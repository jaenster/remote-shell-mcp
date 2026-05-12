//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestMain_BinariesAndImage(t *testing.T) {
	_, _ = BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
}

type sshConnectResp struct {
	ID            string   `json:"id"`
	State         string   `json:"state"`
	ActiveAddress string   `json:"active_address"`
	Addresses     []string `json:"addresses"`
}

type execResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func sshSpec(port int) map[string]any {
	return map[string]any{
		"user":      sshUser,
		"addresses": []string{fmt.Sprintf("127.0.0.1:%d", port)},
		"auth":      map[string]any{"password": sshPassword},
		"insecure":  true,
		"timeout":   "10s",
	}
}

func setupSession(t *testing.T) (*MCPClient, *SSHDContainer, *Daemon, func()) {
	t.Helper()
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	daemon := StartDaemon(t, daemonBin)
	mc := NewMCPClientForDaemon(t, launcherBin, daemon)
	teardown := func() {
		mc.Close()
		daemon.Stop()
		sshd.Stop()
	}
	return mc, sshd, daemon, teardown
}

func TestSSHConnectAndExec(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{
		"id":   "s1",
		"spec": sshSpec(sshd.SSHPort),
	}, &info); err != nil {
		t.Fatalf("ssh_connect: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("expected state=connected, got %q", info.State)
	}

	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "s1",
		"command":    "echo hello-from-container && hostname",
	}, &res); err != nil {
		t.Fatalf("ssh_exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello-from-container") {
		t.Fatalf("exec stdout missing token: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit_code=0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
}

func TestSSHPersistentShell(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_shell_open", map[string]any{"session_id": "s1", "shell_id": "sh1"}); err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	// drain banner
	_, _ = mc.Call(ctx, "ssh_shell_read", map[string]any{"session_id": "s1", "shell_id": "sh1", "timeout_ms": 800})

	if _, err := mc.Call(ctx, "ssh_shell_write", map[string]any{"session_id": "s1", "shell_id": "sh1", "data": "MARK=abc123; cd /tmp; pwd; echo $MARK\n"}); err != nil {
		t.Fatalf("shell_write: %v", err)
	}

	got := readUntil(t, mc, "s1", "sh1", "abc123", 3*time.Second)
	if !strings.Contains(got, "/tmp") {
		t.Fatalf("shell did not preserve cwd; output:\n%s", got)
	}
	if !strings.Contains(got, "abc123") {
		t.Fatalf("shell did not preserve env var; output:\n%s", got)
	}

	if _, err := mc.Call(ctx, "ssh_shell_write", map[string]any{"session_id": "s1", "shell_id": "sh1", "data": "echo $MARK $PWD\n"}); err != nil {
		t.Fatalf("shell_write 2: %v", err)
	}
	got2 := readUntil(t, mc, "s1", "sh1", "abc123 /tmp", 3*time.Second)
	if !strings.Contains(got2, "abc123 /tmp") {
		t.Fatalf("state lost between calls; output:\n%s", got2)
	}

	if _, err := mc.Call(ctx, "ssh_shell_close", map[string]any{"session_id": "s1", "shell_id": "sh1"}); err != nil {
		t.Fatalf("shell_close: %v", err)
	}
}

func readUntil(t *testing.T, mc *MCPClient, sid, shid, needle string, total time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), total+2*time.Second)
	defer cancel()
	var acc strings.Builder
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		var out struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := mc.CallJSON(ctx, "ssh_shell_read", map[string]any{
			"session_id": sid, "shell_id": shid, "timeout_ms": 400,
		}, &out); err != nil {
			t.Fatalf("shell_read: %v", err)
		}
		acc.WriteString(out.Data)
		if strings.Contains(acc.String(), needle) {
			return acc.String()
		}
		if out.EOF {
			return acc.String()
		}
	}
	return acc.String()
}

func TestSFTP(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	content := "hello sftp from e2e\n"
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "s1", "path": "/tmp/e2e.txt", "data": content,
	}); err != nil {
		t.Fatalf("file_write: %v", err)
	}
	var rd struct {
		Bytes int    `json:"bytes"`
		Data  string `json:"data"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"session_id": "s1", "path": "/tmp/e2e.txt",
	}, &rd); err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if rd.Data != content {
		t.Fatalf("round-trip mismatch: wrote %q got %q", content, rd.Data)
	}
	var entries []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_list", map[string]any{
		"session_id": "s1", "path": "/tmp",
	}, &entries); err != nil {
		t.Fatalf("file_list: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name == "e2e.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("file_list did not include e2e.txt; got %d entries", len(entries))
	}
}

// waitForContainerHTTP confirms the busybox httpd inside the container is serving.
func waitForContainerHTTP(t *testing.T, sshd *SSHDContainer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", sshd.ID, "wget", "-qO-", "http://127.0.0.1:8080/marker.txt").Output()
		if err == nil && strings.Contains(string(out), "HELLO-FROM-IN-CONTAINER") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("in-container httpd never came up on :8080")
}

// TestSFTPFullCoverage exercises the SFTP tool surface that TestSFTP doesn't —
// chmod, mkdir, delete, rename, upload, download, stat, and the
// data/data_base64 conflict guard on ssh_file_write.
func TestSFTPFullCoverage(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "f1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// mkdir (recursive)
	if _, err := mc.Call(ctx, "ssh_file_mkdir", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b", "recursive": true,
	}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// write a file under the new directory
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt", "data": "first\n",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// chmod via string (the new API)
	if _, err := mc.Call(ctx, "ssh_file_chmod", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt", "mode": "0640",
	}); err != nil {
		t.Fatalf("chmod string: %v", err)
	}
	// stat confirms the mode (output mode string contains rw-r-----)
	var st struct {
		Mode string `json:"mode"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_stat", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt",
	}, &st); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !strings.Contains(st.Mode, "rw-r-----") {
		t.Fatalf("chmod 0640 did not produce expected mode; got %q", st.Mode)
	}

	// rename
	if _, err := mc.Call(ctx, "ssh_file_rename", map[string]any{
		"session_id": "f1",
		"from":       "/tmp/e2e-sftp/a/b/file.txt",
		"to":         "/tmp/e2e-sftp/a/b/renamed.txt",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// upload + download round-trip via local temp files
	localDir := t.TempDir()
	srcPath := localDir + "/up.bin"
	dstPath := localDir + "/down.bin"
	srcContent := []byte("upload payload " + strings.Repeat("x", 1024))
	if err := os.WriteFile(srcPath, srcContent, 0o600); err != nil {
		t.Fatalf("local write: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_upload", map[string]any{
		"session_id":  "f1",
		"local_path":  srcPath,
		"remote_path": "/tmp/e2e-sftp/uploaded.bin",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_download", map[string]any{
		"session_id":  "f1",
		"remote_path": "/tmp/e2e-sftp/uploaded.bin",
		"local_path":  dstPath,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	roundtrip, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("local read: %v", err)
	}
	if string(roundtrip) != string(srcContent) {
		t.Fatalf("upload/download did not round-trip (%d vs %d bytes)", len(roundtrip), len(srcContent))
	}

	// delete
	if _, err := mc.Call(ctx, "ssh_file_delete", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/renamed.txt",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// ambiguous write should now be rejected — tool returns IsError=true,
	// which CallText surfaces as a Go error.
	if _, err := mc.CallText(ctx, "ssh_file_write", map[string]any{
		"session_id":  "f1",
		"path":        "/tmp/e2e-sftp/conflict.bin",
		"data":        "text",
		"data_base64": "dGV4dA==",
	}); err == nil {
		t.Fatalf("expected ssh_file_write to reject both data and data_base64 set")
	}
}

func TestSSHLocalForwardHTTP(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	waitForContainerHTTP(t, sshd)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var info struct {
		BindPort int `json:"bind_port"`
	}
	if err := mc.CallJSON(ctx, "ssh_forward_local", map[string]any{
		"session_id":  "s1",
		"forward_id":  "fL",
		"bind_port":   0,
		"remote_host": "127.0.0.1",
		"remote_port": 8080,
	}, &info); err != nil {
		t.Fatalf("forward_local: %v", err)
	}

	// Real HTTP request through the -L tunnel.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/marker.txt", info.BindPort))
	if err != nil {
		t.Fatalf("http get via forward: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("got status %d body=%q", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "HELLO-FROM-IN-CONTAINER") {
		t.Fatalf("forward returned unexpected body: %q", string(body))
	}

	// Hit a non-existent path to verify error propagation through the tunnel.
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/nope.txt", info.BindPort))
	if err != nil {
		t.Fatalf("http get 404: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for missing file, got %d", resp.StatusCode)
	}

	if _, err := mc.Call(ctx, "ssh_forward_cancel", map[string]any{"session_id": "s1", "forward_id": "fL"}); err != nil {
		t.Fatalf("forward_cancel: %v", err)
	}
}

func TestSSHRemoteForwardHTTP(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Real Go HTTP server on the host that the container will reach via -R.
	mux := http.NewServeMux()
	var hits int32
	var hitsMu sync.Mutex
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits++
		hitsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"from-host","ua":"` + r.UserAgent() + `"}`))
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen local: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())
	hostPort := ln.Addr().(*net.TCPAddr).Port

	// Open a remote forward: container listens on a random port, tunnels back to our Go server.
	var fwd struct {
		BindPort int `json:"bind_port"`
	}
	if err := mc.CallJSON(ctx, "ssh_forward_remote", map[string]any{
		"session_id": "s1",
		"forward_id": "fR",
		"bind_addr":  "127.0.0.1",
		"bind_port":  0,
		"local_host": "127.0.0.1",
		"local_port": hostPort,
	}, &fwd); err != nil {
		t.Fatalf("forward_remote: %v", err)
	}
	if fwd.BindPort == 0 {
		t.Fatalf("expected non-zero remote bind_port")
	}

	// curl from inside the container — goes through SSH back to our Go server.
	out, err := exec.Command("docker", "exec", sshd.ID,
		"curl", "-sS", "--max-time", "5",
		fmt.Sprintf("http://127.0.0.1:%d/api", fwd.BindPort)).CombinedOutput()
	if err != nil {
		t.Fatalf("curl from container: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), `"hello":"from-host"`) {
		t.Fatalf("remote forward did not reach host server; got %q", string(out))
	}

	hitsMu.Lock()
	gotHits := hits
	hitsMu.Unlock()
	if gotHits == 0 {
		t.Fatalf("host server never saw a hit")
	}

	if _, err := mc.Call(ctx, "ssh_forward_cancel", map[string]any{"session_id": "s1", "forward_id": "fR"}); err != nil {
		t.Fatalf("forward_cancel: %v", err)
	}
}

func TestSSHDynamicForwardSOCKS5(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	waitForContainerHTTP(t, sshd)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var fwd struct {
		BindPort int `json:"bind_port"`
	}
	if err := mc.CallJSON(ctx, "ssh_forward_dynamic", map[string]any{
		"session_id": "s1",
		"forward_id": "fD",
		"bind_port":  0,
	}, &fwd); err != nil {
		t.Fatalf("forward_dynamic: %v", err)
	}
	if fwd.BindPort == 0 {
		t.Fatalf("expected non-zero bind_port")
	}

	// Real SOCKS5 client. Dial through the dynamic proxy to the in-container HTTP server.
	socksAddr := fmt.Sprintf("127.0.0.1:%d", fwd.BindPort)
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		t.Fatalf("socks5 dialer: %v", err)
	}
	tr := &http.Transport{
		Dial: dialer.Dial,
	}
	httpc := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	// 127.0.0.1:8080 is reachable on the SSH-server side (it's the container's own loopback).
	resp, err := httpc.Get("http://127.0.0.1:8080/marker.txt")
	if err != nil {
		t.Fatalf("http via SOCKS5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "HELLO-FROM-IN-CONTAINER") {
		t.Fatalf("dynamic forward returned %q", string(body))
	}

	if _, err := mc.Call(ctx, "ssh_forward_cancel", map[string]any{"session_id": "s1", "forward_id": "fD"}); err != nil {
		t.Fatalf("forward_cancel: %v", err)
	}
}

func TestDockerLocalSocket(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id":   "d1",
		"spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("docker_connect via %s: %v", dockerHost, err)
	}

	var containers []struct {
		ID    string   `json:"id"`
		Names []string `json:"names"`
	}
	if err := mc.CallJSON(ctx, "docker_containers", map[string]any{
		"host_id": "d1", "all": true,
	}, &containers); err != nil {
		t.Fatalf("docker_containers: %v", err)
	}
	var ourID string
	for _, c := range containers {
		if strings.HasPrefix(c.ID, sshd.ID) || strings.HasPrefix(sshd.ID, c.ID) {
			ourID = c.ID
			break
		}
	}
	if ourID == "" {
		t.Fatalf("did not find our sshd container (%s) in docker_containers; got %d", sshd.ID, len(containers))
	}

	var ex execResp
	if err := mc.CallJSON(ctx, "docker_exec", map[string]any{
		"host_id":   "d1",
		"container": ourID,
		"cmd":       []string{"sh", "-c", "echo from-docker-exec"},
	}, &ex); err != nil {
		t.Fatalf("docker_exec: %v", err)
	}
	if !strings.Contains(ex.Stdout, "from-docker-exec") {
		t.Fatalf("docker_exec stdout: %q", ex.Stdout)
	}
	if ex.ExitCode != 0 {
		t.Fatalf("docker_exec exit %d", ex.ExitCode)
	}
}

func detectDockerHost() string {
	candidates := []string{
		"/var/run/docker.sock",
	}
	if home, err := exec.Command("sh", "-c", "echo $HOME").Output(); err == nil {
		h := strings.TrimSpace(string(home))
		candidates = append(candidates, h+"/.docker/run/docker.sock", h+"/.colima/default/docker.sock")
	}
	for _, c := range candidates {
		if fileExists(c) {
			return "unix://" + c
		}
	}
	return ""
}

func fileExists(p string) bool {
	out, err := exec.Command("test", "-S", p).CombinedOutput()
	_ = out
	return err == nil
}

func TestDockerPersistentShell(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id":   "d1",
		"spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("docker_connect: %v", err)
	}
	if _, err := mc.Call(ctx, "docker_shell_open", map[string]any{
		"host_id":   "d1",
		"shell_id":  "sh1",
		"container": sshd.ID,
		"cmd":       []string{"/bin/sh"},
	}); err != nil {
		t.Fatalf("docker_shell_open: %v", err)
	}
	// drain prompt
	_, _ = mc.Call(ctx, "docker_shell_read", map[string]any{"host_id": "d1", "shell_id": "sh1", "timeout_ms": 600})

	writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer writeCancel()
	if _, err := mc.Call(writeCtx, "docker_shell_write", map[string]any{
		"host_id": "d1", "shell_id": "sh1", "data": "echo CONTAINER-MARK-7Q9\n",
	}); err != nil {
		t.Fatalf("docker_shell_write: %v", err)
	}

	var acc strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var out struct {
			Data string `json:"data"`
		}
		if err := mc.CallJSON(ctx, "docker_shell_read", map[string]any{
			"host_id": "d1", "shell_id": "sh1", "timeout_ms": 400,
		}, &out); err != nil {
			t.Fatalf("docker_shell_read: %v", err)
		}
		acc.WriteString(out.Data)
		if strings.Contains(acc.String(), "CONTAINER-MARK-7Q9") {
			break
		}
	}
	if !strings.Contains(acc.String(), "CONTAINER-MARK-7Q9") {
		t.Fatalf("docker shell did not echo back the mark; got:\n%s", acc.String())
	}

	// State preservation across multiple writes: set a var, then read it back.
	if _, err := mc.Call(ctx, "docker_shell_write", map[string]any{
		"host_id": "d1", "shell_id": "sh1", "data": "FOO=bar-7Q9-baz\n",
	}); err != nil {
		t.Fatalf("docker_shell_write 2: %v", err)
	}
	_, _ = mc.Call(ctx, "docker_shell_read", map[string]any{"host_id": "d1", "shell_id": "sh1", "timeout_ms": 400})
	if _, err := mc.Call(ctx, "docker_shell_write", map[string]any{
		"host_id": "d1", "shell_id": "sh1", "data": "echo got-$FOO\n",
	}); err != nil {
		t.Fatalf("docker_shell_write 3: %v", err)
	}
	acc.Reset()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var out struct {
			Data string `json:"data"`
		}
		_ = mc.CallJSON(ctx, "docker_shell_read", map[string]any{
			"host_id": "d1", "shell_id": "sh1", "timeout_ms": 400,
		}, &out)
		acc.WriteString(out.Data)
		if strings.Contains(acc.String(), "got-bar-7Q9-baz") {
			break
		}
	}
	if !strings.Contains(acc.String(), "got-bar-7Q9-baz") {
		t.Fatalf("docker shell did not preserve env var; got:\n%s", acc.String())
	}
}

// TestSSHJumpHost exercises ProxyJump-style multi-hop: connect via the
// container's sshd as a jump, then SSH to 127.0.0.1:22 INSIDE the container
// (which loops back through the same sshd). The bastion auth happens with
// the test's password; the terminal hop also uses password auth.
func TestSSHJumpHost(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec := map[string]any{
		"user": sshUser,
		// Final target is reached via the jump's perspective, so 127.0.0.1:22
		// resolves on the jump host (= the container itself).
		"addresses": []string{"127.0.0.1:22"},
		"auth":      map[string]any{"password": sshPassword},
		"insecure":  true,
		"jump_hosts": []map[string]any{
			{
				"user":      sshUser,
				"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
				"auth":      map[string]any{"password": sshPassword},
				"insecure":  true,
			},
		},
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "jh", "spec": spec}); err != nil {
		t.Fatalf("ssh_connect via jump: %v", err)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "jh", "command": "echo through-bastion-$(hostname)",
	}, &res); err != nil {
		t.Fatalf("ssh_exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "through-bastion") {
		t.Fatalf("jump-host exec stdout: %q", res.Stdout)
	}
}

// TestSSHMultiAddress confirms ssh_connect tries addresses in order: an
// unreachable address first, the real container second.
func TestSSHMultiAddress(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 127.0.0.1:1 is essentially always closed; daemon should fall through.
	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{"127.0.0.1:1", fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      map[string]any{"password": sshPassword},
		"insecure":  true,
		"timeout":   "2s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "ma", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect multi-address: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("state %q", info.State)
	}
	if !strings.HasSuffix(info.ActiveAddress, fmt.Sprintf(":%d", sshd.SSHPort)) {
		t.Fatalf("expected active_address to be the real sshd port; got %q", info.ActiveAddress)
	}
}

// TestSSHClone forks an existing session under a new id and confirms both
// answer exec independently.
func TestSSHClone(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "orig", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_clone", map[string]any{"source_id": "orig", "new_id": "twin"}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	for _, id := range []string{"orig", "twin"} {
		var res execResp
		if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
			"session_id": id, "command": "echo " + id,
		}, &res); err != nil {
			t.Fatalf("exec on %s: %v", id, err)
		}
		if !strings.Contains(res.Stdout, id) {
			t.Fatalf("exec on %s returned %q", id, res.Stdout)
		}
	}
	// Closing one should not affect the other.
	if _, err := mc.Call(ctx, "ssh_disconnect", map[string]any{"id": "orig"}); err != nil {
		t.Fatalf("disconnect orig: %v", err)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "twin", "command": "echo still-alive",
	}, &res); err != nil {
		t.Fatalf("twin exec after orig disconnect: %v", err)
	}
	if !strings.Contains(res.Stdout, "still-alive") {
		t.Fatalf("twin output %q", res.Stdout)
	}
}

func TestSSHKeyAuth(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	privPath, pub := GenerateKey(t)
	InstallAuthorizedKey(t, sshd, pub)

	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      map[string]any{"key_path": privPath},
		"insecure":  true,
		"timeout":   "10s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "k1", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect key: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("state = %q", info.State)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "k1", "command": "whoami",
	}, &res); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "root") {
		t.Fatalf("whoami via key auth returned %q", res.Stdout)
	}
}

func TestSSHAgentAuth(t *testing.T) {
	if _, err := exec.LookPath("ssh-agent"); err != nil {
		t.Skip("ssh-agent not available")
	}
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add not available")
	}
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()

	privPath, pub := GenerateKey(t)
	InstallAuthorizedKey(t, sshd, pub)
	authSock := StartSSHAgent(t, privPath)

	// Daemon must see SSH_AUTH_SOCK to talk to the agent.
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Env: []string{"SSH_AUTH_SOCK=" + authSock}})
	defer daemon.Stop()

	mc := NewMCPClientForDaemon(t, launcherBin, daemon)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      map[string]any{"use_agent": true},
		"insecure":  true,
		"timeout":   "10s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "a1", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect agent: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("agent auth state = %q", info.State)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "a1", "command": "echo agent-ok",
	}, &res); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "agent-ok") {
		t.Fatalf("exec output %q", res.Stdout)
	}
}

func TestSSHAutoReconnect(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":           sshUser,
		"addresses":      []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":           map[string]any{"password": sshPassword},
		"insecure":       true,
		"timeout":        "10s",
		"keepalive":      "1s",
		"auto_reconnect": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "r1", "spec": spec}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Sanity check it works before we break it.
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "r1", "command": "echo before"}, &res); err != nil {
		t.Fatalf("exec pre-kill: %v", err)
	}
	if !strings.Contains(res.Stdout, "before") {
		t.Fatalf("pre-kill stdout %q", res.Stdout)
	}

	// Kill sshd inside the container. The respawn loop in the Dockerfile brings it back.
	if _, err := exec.Command("docker", "exec", sshd.ID, "sh", "-c", "pkill -KILL sshd").CombinedOutput(); err != nil {
		t.Fatalf("kill sshd: %v", err)
	}

	// Poll ssh_info until state goes through reconnecting/disconnected and back to connected.
	deadline := time.Now().Add(20 * time.Second)
	var sawReconnecting bool
	var finalState string
	for time.Now().Before(deadline) {
		var info sshConnectResp
		if err := mc.CallJSON(ctx, "ssh_info", map[string]any{"id": "r1"}, &info); err != nil {
			t.Fatalf("ssh_info: %v", err)
		}
		finalState = info.State
		if info.State == "reconnecting" || info.State == "disconnected" {
			sawReconnecting = true
		}
		if sawReconnecting && info.State == "connected" {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if !sawReconnecting {
		t.Fatalf("never observed reconnecting state; final=%q", finalState)
	}
	if finalState != "connected" {
		t.Fatalf("session did not recover; final state=%q", finalState)
	}

	// Verify exec works again on the new connection.
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "r1", "command": "echo after"}, &res); err != nil {
		t.Fatalf("exec post-reconnect: %v", err)
	}
	if !strings.Contains(res.Stdout, "after") {
		t.Fatalf("post-reconnect stdout %q", res.Stdout)
	}
}

func TestStateAcrossDaemonRestart(t *testing.T) {
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()

	privPath, pub := GenerateKey(t)
	InstallAuthorizedKey(t, sshd, pub)

	stateDir := t.TempDir()
	statePath := stateDir + "/state.json"
	lockPath := stateDir + "/daemon.lock"

	// Phase 1 — connect a persistent session with key auth.
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	mc := NewMCPClientForDaemon(t, launcherBin, daemon)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":       sshUser,
		"addresses":  []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":       map[string]any{"key_path": privPath},
		"insecure":   true,
		"timeout":    "10s",
		"persistent": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "persist-d", "spec": spec}); err != nil {
		mc.Close()
		daemon.Stop()
		t.Fatalf("connect: %v", err)
	}
	// Make sure the snapshot has actually been flushed.
	if _, err := mc.Call(ctx, "snapshot", nil); err != nil {
		mc.Close()
		daemon.Stop()
		t.Fatalf("snapshot: %v", err)
	}
	mc.Close()
	daemon.Stop()

	// Confirm state file is non-empty so the next daemon has something to rehydrate.
	if st, err := os.Stat(statePath); err != nil || st.Size() == 0 {
		t.Fatalf("state file empty after snapshot: stat=%v size=%v", err, st)
	}

	// Phase 2 — fresh daemon on a new port, same state file.
	daemon2 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	defer daemon2.Stop()
	mc2 := NewMCPClientForDaemon(t, launcherBin, daemon2)
	defer mc2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()

	// The persistent session should be back, reconnected.
	deadline := time.Now().Add(8 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		var list []sshConnectResp
		if err := mc2.CallJSON(ctx2, "ssh_list", nil, &list); err != nil {
			t.Fatalf("ssh_list: %v", err)
		}
		for _, s := range list {
			if s.ID == "persist-d" && s.State == "connected" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !found {
		t.Fatalf("persistent session did not rehydrate on daemon 2")
	}

	// Exec to confirm the rehydrated session is actually usable.
	var res execResp
	if err := mc2.CallJSON(ctx2, "ssh_exec", map[string]any{
		"session_id": "persist-d", "command": "echo restored",
	}, &res); err != nil {
		t.Fatalf("exec after rehydrate: %v", err)
	}
	if !strings.Contains(res.Stdout, "restored") {
		t.Fatalf("rehydrated session exec stdout %q", res.Stdout)
	}
}

// TestDockerContainerLifecycle exercises start/stop/restart/kill/remove + inspect
// against a real container created via `docker run -d`. We don't have a
// docker_create tool yet, so we start the container outside the MCP layer and
// drive everything else through the daemon.
// TestDockerOverSSH exercises the ssh:// host scheme — docker_connect tunnels
// to the daemon through an SSH transport. We bind-mount /var/run/docker.sock
// into the sshd container so the SSH-side has something to talk to.
func TestDockerOverSSH(t *testing.T) {
	if detectDockerHost() == "" {
		t.Skip("no docker socket on host to tunnel to")
	}
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainerWith(t, SSHDOpts{MountDockerSocket: true})
	defer sshd.Stop()
	daemon := StartDaemon(t, daemonBin)
	defer daemon.Stop()

	mc := NewMCPClientForDaemon(t, launcherBin, daemon)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	spec := map[string]any{
		"host":         fmt.Sprintf("ssh://root@127.0.0.1:%d/var/run/docker.sock", sshd.SSHPort),
		"password":     sshPassword,
		"ssh_insecure": true,
	}
	if _, err := mc.Call(ctx, "docker_connect", map[string]any{"id": "viassh", "spec": spec}); err != nil {
		t.Fatalf("docker_connect via ssh: %v", err)
	}

	var containers []struct {
		ID    string   `json:"id"`
		Names []string `json:"names"`
	}
	if err := mc.CallJSON(ctx, "docker_containers", map[string]any{
		"host_id": "viassh", "all": true,
	}, &containers); err != nil {
		t.Fatalf("docker_containers via ssh: %v", err)
	}
	if len(containers) == 0 {
		t.Fatalf("expected to see at least one container via SSH-tunneled docker")
	}
}

// TestDockerListAndDisconnect covers docker_list_hosts and docker_disconnect.
func TestDockerListAndDisconnect(t *testing.T) {
	mc, _, _, teardown := setupSession(t)
	defer teardown()
	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id": "h1", "spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	var hosts []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := mc.CallJSON(ctx, "docker_list_hosts", nil, &hosts); err != nil {
		t.Fatalf("list_hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].ID != "h1" || hosts[0].State != "connected" {
		t.Fatalf("unexpected hosts: %+v", hosts)
	}
	if _, err := mc.Call(ctx, "docker_disconnect", map[string]any{"id": "h1"}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if err := mc.CallJSON(ctx, "docker_list_hosts", nil, &hosts); err != nil {
		t.Fatalf("list_hosts after disconnect: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("expected 0 hosts after disconnect, got %+v", hosts)
	}
}

// TestDockerRunAndImage exercises the new docker_run + docker_image_* tools
// end-to-end: pull alpine if needed, run a small container with an env var
// override, verify it's running, fetch its logs, remove it.
func TestDockerRunAndImage(t *testing.T) {
	mc, _, _, teardown := setupSession(t)
	defer teardown()
	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id": "d1", "spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("docker_connect: %v", err)
	}

	// Pull (idempotent — works whether or not the image is already local).
	if _, err := mc.Call(ctx, "docker_image_pull", map[string]any{
		"host_id": "d1", "image": "alpine:3.20",
	}); err != nil {
		t.Fatalf("docker_image_pull: %v", err)
	}

	// Image should now appear in the list.
	var images []struct {
		ID       string   `json:"id"`
		RepoTags []string `json:"repo_tags"`
	}
	if err := mc.CallJSON(ctx, "docker_image_list", map[string]any{"host_id": "d1"}, &images); err != nil {
		t.Fatalf("docker_image_list: %v", err)
	}
	var found bool
	for _, im := range images {
		for _, tag := range im.RepoTags {
			if strings.HasPrefix(tag, "alpine:3.20") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("alpine:3.20 not in image list after pull")
	}

	// docker_run with cmd + env + labels.
	var run struct {
		ID string `json:"id"`
	}
	if err := mc.CallJSON(ctx, "docker_run", map[string]any{
		"host_id": "d1",
		"spec": map[string]any{
			"image":           "alpine:3.20",
			"cmd":             []string{"sh", "-c", "echo HELLO-$MARKER && sleep 60"},
			"env":             map[string]string{"MARKER": "abc-987-xyz"},
			"labels":          map[string]string{"remote-shell-mcp-e2e-lifecycle": "1"},
			"pull_if_missing": false,
		},
	}, &run); err != nil {
		t.Fatalf("docker_run: %v", err)
	}
	if run.ID == "" {
		t.Fatalf("docker_run returned no id")
	}
	defer exec.Command("docker", "rm", "-f", run.ID).Run()

	// Confirm the container is running.
	if !waitContainerState(t, run.ID, "running", 5*time.Second) {
		t.Fatalf("container did not enter running state")
	}

	// Logs should contain the env-substituted marker.
	time.Sleep(500 * time.Millisecond) // give it a beat to emit
	logs, err := mc.CallText(ctx, "docker_container_logs", map[string]any{
		"host_id": "d1", "container": run.ID,
	})
	if err != nil {
		t.Fatalf("docker_container_logs: %v", err)
	}
	if !strings.Contains(logs, "HELLO-abc-987-xyz") {
		t.Fatalf("expected env substitution in logs; got %q", logs)
	}

	// Stop + remove + verify gone.
	if _, err := mc.Call(ctx, "docker_container_remove", map[string]any{
		"host_id": "d1", "container": run.ID, "force": true,
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if exec.Command("docker", "inspect", run.ID).Run() == nil {
		t.Fatalf("container still exists after docker_container_remove")
	}
}

func TestDockerContainerLifecycle(t *testing.T) {
	mc, _, _, teardown := setupSession(t)
	defer teardown()
	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Spin up a small long-running container.
	out, err := exec.Command("docker", "run", "-d",
		"--label", "remote-shell-mcp-e2e-lifecycle=1",
		"alpine:3.20",
		"sh", "-c", "while sleep 0.5; do echo tick-$(date +%s%N); done",
	).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	defer func() {
		_ = exec.Command("docker", "rm", "-f", cid).Run()
	}()

	// Wait for the container to actually be running before talking to it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", cid).Output()
		if strings.TrimSpace(string(st)) == "true" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id": "d1", "spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("docker_connect: %v", err)
	}

	// docker_container_inspect — confirm running.
	var insp map[string]any
	if err := mc.CallJSON(ctx, "docker_container_inspect", map[string]any{
		"host_id": "d1", "container": cid,
	}, &insp); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	state, _ := insp["State"].(map[string]any)
	if state == nil || state["Running"] != true {
		t.Fatalf("expected Running=true, got %v", state)
	}

	// docker_container_logs — should contain at least one tick.
	time.Sleep(800 * time.Millisecond) // let it emit a couple of lines
	logs, err := mc.CallText(ctx, "docker_container_logs", map[string]any{
		"host_id": "d1", "container": cid, "tail": "10",
	})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs, "tick-") {
		t.Fatalf("expected tick output in logs; got %q", logs)
	}

	// docker_container_stop with a short timeout.
	stopTimeout := 1
	if _, err := mc.Call(ctx, "docker_container_stop", map[string]any{
		"host_id": "d1", "container": cid, "timeout_sec": stopTimeout,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !waitContainerState(t, cid, "exited", 5*time.Second) {
		t.Fatalf("container did not stop")
	}

	// docker_container_start.
	if _, err := mc.Call(ctx, "docker_container_start", map[string]any{
		"host_id": "d1", "container": cid,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !waitContainerState(t, cid, "running", 5*time.Second) {
		t.Fatalf("container did not start back up")
	}

	// docker_container_restart.
	if _, err := mc.Call(ctx, "docker_container_restart", map[string]any{
		"host_id": "d1", "container": cid, "timeout_sec": 1,
	}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !waitContainerState(t, cid, "running", 6*time.Second) {
		t.Fatalf("container not running after restart")
	}

	// docker_container_kill.
	if _, err := mc.Call(ctx, "docker_container_kill", map[string]any{
		"host_id": "d1", "container": cid, "signal": "SIGKILL",
	}); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !waitContainerState(t, cid, "exited", 5*time.Second) {
		t.Fatalf("container did not exit after kill")
	}

	// docker_container_remove — force because it's already exited.
	if _, err := mc.Call(ctx, "docker_container_remove", map[string]any{
		"host_id": "d1", "container": cid, "force": true,
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if exec.Command("docker", "inspect", cid).Run() == nil {
		t.Fatalf("container still inspectable after remove")
	}
}

func waitContainerState(t *testing.T, cid, want string, total time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", cid).Output()
		if err == nil && strings.TrimSpace(string(out)) == want {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// TestManyParallelSessions opens N SSH sessions, runs a command on each, then
// closes them all. Verifies the manager doesn't deadlock under fanout and
// that close cleans up.
func TestManyParallelSessions(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N*3)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("p%d", i)
			if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": id, "spec": sshSpec(sshd.SSHPort)}); err != nil {
				errs <- fmt.Errorf("%s connect: %w", id, err)
				return
			}
			var res execResp
			if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
				"session_id": id, "command": fmt.Sprintf("echo session-%d", i),
			}, &res); err != nil {
				errs <- fmt.Errorf("%s exec: %w", id, err)
				return
			}
			want := fmt.Sprintf("session-%d", i)
			if !strings.Contains(res.Stdout, want) {
				errs <- fmt.Errorf("%s wrong output: %q", id, res.Stdout)
				return
			}
			if _, err := mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id}); err != nil {
				errs <- fmt.Errorf("%s disconnect: %w", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// ssh_list should be empty after all disconnects.
	var list []sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 sessions after disconnect, got %d", len(list))
	}
}

// NOTE: TestTrulyParallelClients (multiple launcher subprocesses connecting to
// the same daemon) was attempted but exposed an interaction with mcp-go's
// SSE server under -race that I haven't been able to isolate yet — token
// reads after the first launcher's SSE stream return "stream ended". The
// daemon-side concurrency we actually care about IS tested by
// TestManyParallelSessions (10 concurrent ssh_connect on one client; the
// daemon side runs N handler goroutines via the SSE server's request
// dispatch), TestConcurrentShellWrites, and TestConcurrentReconnects.
// Multi-launcher coverage is a known gap.

// TestLargeMCPResponse drives a tool that returns a multi-megabyte payload to
// confirm the launcher proxy handles big SSE messages (no scanner cap).
func TestLargeMCPResponse(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "big", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 2 MiB file → forces a JSON-RPC response larger than the old 16 MiB cap is
	// not enough to trigger the bug, but any response > the default bufio buffer
	// would have stuck without our fix.
	const sz = 2 * 1024 * 1024
	mk := strings.Repeat("X", sz)
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "big", "path": "/tmp/big.bin", "data": mk,
	}); err != nil {
		t.Fatalf("write big: %v", err)
	}
	var rd struct {
		Bytes int    `json:"bytes"`
		Data  string `json:"data"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"session_id": "big", "path": "/tmp/big.bin",
	}, &rd); err != nil {
		t.Fatalf("read big: %v", err)
	}
	if rd.Bytes != sz || len(rd.Data) != sz {
		t.Fatalf("size mismatch: wanted %d, got bytes=%d len(data)=%d", sz, rd.Bytes, len(rd.Data))
	}
}

// TestConnectDisconnectRace exercises the dial-vs-close race the post-fix audit
// caught. Under the old code, a Disconnect that fired between dialFinal
// succeeding and setClient leaked the freshly-dialed SSH client + jump
// clients + keepalive goroutine. The race is timing-dependent so we just
// make sure the daemon survives many overlapping pairs without deadlocking
// or panicking, then clean up. -race catches the underlying data races.
func TestConnectDisconnectRace(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	const iters = 12
	var wg sync.WaitGroup
	for i := 0; i < iters; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("rc%d", i)
			connectDone := make(chan struct{})
			go func() {
				_, _ = mc.Call(ctx, "ssh_connect", map[string]any{
					"id":   id,
					"spec": sshSpec(sshd.SSHPort),
				})
				close(connectDone)
			}()
			time.Sleep(time.Duration(i%5) * 5 * time.Millisecond)
			_, _ = mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id})
			<-connectDone
			// Whichever order won, sweep up any leftover so ssh_list is clean.
			_, _ = mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id})
		}()
	}
	wg.Wait()

	// Daemon should still be responsive and free of stale rc* sessions.
	var list []sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list after race: %v", err)
	}
	for _, s := range list {
		if strings.HasPrefix(s.ID, "rc") {
			t.Errorf("session %s persisted past cleanup", s.ID)
		}
	}
}

// TestConcurrentShellWrites was a guaranteed race under the old code (shell
// Write was unsynchronized, so two goroutines into stdin would interleave
// bytes). The fix is a writeMu; with -race this would have screamed before.
func TestConcurrentShellWrites(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "c1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_shell_open", map[string]any{"session_id": "c1", "shell_id": "sh"}); err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	_, _ = mc.Call(ctx, "ssh_shell_read", map[string]any{"session_id": "c1", "shell_id": "sh", "timeout_ms": 500})

	var wg sync.WaitGroup
	const writers = 8
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := mc.Call(ctx, "ssh_shell_write", map[string]any{
				"session_id": "c1", "shell_id": "sh",
				"data": fmt.Sprintf("echo writer-%d\n", i),
			})
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Confirm we eventually see all 8 markers; lines may be ordered any way.
	var acc strings.Builder
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var out struct{ Data string `json:"data"` }
		_ = mc.CallJSON(ctx, "ssh_shell_read", map[string]any{
			"session_id": "c1", "shell_id": "sh", "timeout_ms": 400,
		}, &out)
		acc.WriteString(out.Data)
		if allPresent(acc.String(), writers) {
			break
		}
	}
	if !allPresent(acc.String(), writers) {
		t.Fatalf("not all writers' output observed:\n%s", acc.String())
	}
}

func allPresent(s string, n int) bool {
	for i := 0; i < n; i++ {
		if !strings.Contains(s, fmt.Sprintf("writer-%d", i)) {
			return false
		}
	}
	return true
}

// TestConcurrentReconnects exercises the reconnectMu serialization. Under the
// old code an explicit Reconnect concurrent with a keepalive-driven
// attemptReconnect would race, double-dial, and leak SSH clients.
func TestConcurrentReconnects(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":           sshUser,
		"addresses":      []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":           map[string]any{"password": sshPassword},
		"insecure":       true,
		"timeout":        "5s",
		"keepalive":      "1s",
		"auto_reconnect": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "rc", "spec": spec}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var wg sync.WaitGroup
	const callers = 6
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, _ = mc.Call(ctx, "ssh_reconnect", map[string]any{"id": "rc"})
		}()
	}
	wg.Wait()

	// After the storm, session should be connected and exec should still work.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var info sshConnectResp
		if err := mc.CallJSON(ctx, "ssh_info", map[string]any{"id": "rc"}, &info); err == nil && info.State == "connected" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "rc", "command": "echo survived"}, &res); err != nil {
		t.Fatalf("exec after concurrent reconnects: %v", err)
	}
	if !strings.Contains(res.Stdout, "survived") {
		t.Fatalf("post-storm stdout %q", res.Stdout)
	}
}

// TestTOONFormat starts the daemon with -format=toon (which is also the
// production default) and confirms a tool result comes back as TOON text
// rather than JSON. We don't try to parse TOON — just verify it's
// shape-correct (an `items[N]{fields}:` header) and a few field tokens
// appear, since the goal is to catch a regression where TOON encoding
// silently falls back to JSON.
func TestTOONFormat(t *testing.T) {
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Format: "toon"})
	defer daemon.Stop()

	mc := NewMCPClientForDaemon(t, launcherBin, daemon)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "tn", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	text, err := mc.CallText(ctx, "ssh_list", nil)
	if err != nil {
		t.Fatalf("ssh_list: %v", err)
	}
	// Negative check: JSON output would parse as JSON. TOON does not.
	var any any
	if err := json.Unmarshal([]byte(text), &any); err == nil {
		t.Fatalf("expected TOON output, got valid JSON:\n%s", text)
	}
	// TOON list output looks like `[1]{…}:` for a single-element array;
	// must contain the session id and state.
	if !strings.Contains(text, "tn") || !strings.Contains(text, "connected") {
		t.Fatalf("TOON output missing expected tokens:\n%s", text)
	}
	// And it should use the json-tagged field names (snake_case), not Go's
	// PascalCase, which would happen if json tags were ignored.
	if !strings.Contains(text, "state") || strings.Contains(text, "ActiveAddress") {
		t.Fatalf("TOON output not using json-tag field names:\n%s", text)
	}
}

func TestUnauthorizedRejected(t *testing.T) {
	daemonBin, _ := BuildBinaries(t)
	daemon := StartDaemon(t, daemonBin)
	defer daemon.Stop()

	// No Authorization header at all → 401.
	resp, err := http.Get("http://" + daemon.addr + "/sse")
	if err != nil {
		t.Fatalf("GET /sse without auth: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for unauthenticated, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("expected WWW-Authenticate Bearer challenge, got %q", got)
	}

	// Wrong token → 401.
	req, _ := http.NewRequest("GET", "http://"+daemon.addr+"/sse", nil)
	req.Header.Set("Authorization", "Bearer notarealtoken")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse with bad token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for wrong token, got %d", resp.StatusCode)
	}

	// Right token → 200 (we don't drain the stream, just confirm the handshake passes).
	tok, err := daemon_ReadToken(daemon.tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req, _ = http.NewRequest("GET", "http://"+daemon.addr+"/sse", nil)
		req.Header.Set("Authorization", scheme+" "+tok)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req = req.WithContext(ctx)
		resp, err = http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			t.Fatalf("GET /sse with scheme %q: %v", scheme, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("scheme %q expected 200, got %d", scheme, resp.StatusCode)
		}
	}

	// Confirm the token file mode is 0600.
	st, err := os.Stat(daemon.tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("token file mode is %o, want 0600", mode)
	}
}

func TestLauncherAutoSpawnsDaemon(t *testing.T) {
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	port := PickFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	stateDir := t.TempDir()

	// Confirm no daemon is listening on the picked port.
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("port %s already in use before test", addr)
	}

	// Spawn the launcher WITHOUT -no-spawn; it must start the daemon itself.
	cmd := exec.Command(launcherBin, "-addr", addr, "-daemon-binary", daemonBin)
	cmd.Env = append(cmd.Environ(),
		"REMOTE_SHELL_MCP_STATE="+stateDir+"/state.json",
		"REMOTE_SHELL_MCP_LOCK="+stateDir+"/daemon.lock",
		// Pin JSON so the JSON-unmarshalling assertion below stays valid
		// even though TOON is the production default.
		"REMOTE_SHELL_MCP_FORMAT=json",
	)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		// The daemon was spawned detached. Read its pid and wait for it to actually exit
		// before TempDir cleanup tries to remove files the daemon still has open.
		if pid, err := readPidFromLock(stateDir + "/daemon.lock"); err == nil {
			_ = exec.Command("kill", "-TERM", strconv.Itoa(pid)).Run()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err != nil {
					break
				}
				time.Sleep(80 * time.Millisecond)
			}
			_ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run()
		}
	})

	mc := AttachMCPClient(t, cmd, stdin, stdout)
	defer mc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var status struct {
		Uptime string `json:"uptime"`
	}
	if err := mc.CallJSON(ctx, "status", nil, &status); err != nil {
		t.Fatalf("status after auto-spawn: %v", err)
	}
	if status.Uptime == "" {
		t.Fatalf("daemon did not auto-spawn (empty uptime)")
	}
	t.Logf("auto-spawned daemon uptime=%s", status.Uptime)
}

func readPidFromLock(path string) (int, error) {
	data, err := exec.Command("cat", path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func TestDaemonSurvivesLauncherRestart(t *testing.T) {
	daemonBin, launcherBin := BuildBinaries(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()
	daemon := StartDaemon(t, daemonBin)
	defer daemon.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mc := NewMCPClientForDaemon(t, launcherBin, daemon)
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "persist1", "spec": sshSpec(sshd.SSHPort)}); err != nil {
		mc.Close()
		t.Fatalf("connect: %v", err)
	}
	mc.Close()

	mc2 := NewMCPClientForDaemon(t, launcherBin, daemon)
	defer mc2.Close()
	var list []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := mc2.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list after restart: %v", err)
	}
	var found bool
	for _, s := range list {
		if s.ID == "persist1" && s.State == "connected" {
			found = true
			break
		}
	}
	if !found {
		raw, _ := json.Marshal(list)
		t.Fatalf("session did not survive launcher restart; ssh_list=%s", string(raw))
	}
}
