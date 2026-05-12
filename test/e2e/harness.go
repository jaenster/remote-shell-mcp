//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	imageTag       = "remote-shell-mcp-test-sshd:latest"
	containerLabel = "remote-shell-mcp-e2e=1"
	sshPassword    = "testpassword"
	sshUser        = "root"
)

func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate harness file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var (
	buildOnce sync.Once
	buildErr  error
	builtBins struct{ daemon, launcher string }
)

func BuildBinaries(t *testing.T) (daemonBin, launcherBin string) {
	t.Helper()
	buildOnce.Do(func() {
		root := RepoRoot(t)
		binDir := filepath.Join(root, "bin")
		builtBins.daemon = filepath.Join(binDir, "remote-shell-mcpd")
		builtBins.launcher = filepath.Join(binDir, "remote-shell-mcp")
		// Always rebuild — Go's build cache makes incremental rebuilds cheap, and
		// stale binaries from an earlier `go build` cycle silently mask source changes.
		for _, b := range []struct{ out, pkg string }{
			{"bin/remote-shell-mcpd", "./cmd/remote-shell-mcpd"},
			{"bin/remote-shell-mcp", "./cmd/remote-shell-mcp"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = root
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				buildErr = fmt.Errorf("build %s: %w", b.pkg, err)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return builtBins.daemon, builtBins.launcher
}

func EnsureDockerImage(t *testing.T) {
	t.Helper()
	// Exit code is authoritative; the previous "string contains Error" check
	// false-matched on legitimate JSON whose labels/env happened to contain
	// the substring.
	if err := exec.Command("docker", "image", "inspect", imageTag).Run(); err == nil {
		return
	}
	root := RepoRoot(t)
	ctx := filepath.Join(root, "test", "e2e")
	cmd := exec.Command("docker", "build", "-t", imageTag, "-f", filepath.Join(ctx, "Dockerfile.sshd"), ctx)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker build: %v", err)
	}
}

// SweepStaleContainers removes any leftover containers from previous failed
// runs of this suite. Invoked from setupSession-style helpers to keep CI clean.
func SweepStaleContainers() {
	for _, label := range []string{containerLabel, "remote-shell-mcp-e2e-lifecycle=1"} {
		ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+label).Output()
		if err != nil {
			continue
		}
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
}

type SSHDContainer struct {
	ID      string
	SSHPort int
	t       *testing.T
}

type SSHDOpts struct {
	MountDockerSocket bool // bind /var/run/docker.sock into the container
}

func StartSSHDContainer(t *testing.T) *SSHDContainer {
	return StartSSHDContainerWith(t, SSHDOpts{})
}

func StartSSHDContainerWith(t *testing.T, opts SSHDOpts) *SSHDContainer {
	t.Helper()
	args := []string{"run", "-d", "--rm",
		"--label", containerLabel,
		"-p", "127.0.0.1::22",
	}
	if opts.MountDockerSocket {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	args = append(args, imageTag)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	id := strings.TrimSpace(string(out))
	c := &SSHDContainer{ID: id, t: t}
	port, err := waitForPort(id)
	if err != nil {
		c.Stop()
		t.Fatalf("get sshd port: %v", err)
	}
	c.SSHPort = port
	if err := waitForSSH(port); err != nil {
		c.Stop()
		t.Fatalf("wait for sshd: %v", err)
	}
	return c
}

func waitForPort(id string) (int, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "port", id, "22/tcp").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if idx := strings.LastIndex(line, ":"); idx >= 0 {
				if p, err := strconv.Atoi(strings.TrimSpace(line[idx+1:])); err == nil {
					return p, nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, fmt.Errorf("no port mapping for container")
}

func waitForSSH(port int) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			time.Sleep(200 * time.Millisecond)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("sshd never came up on 127.0.0.1:%d", port)
}

func (c *SSHDContainer) Stop() {
	if c == nil || c.ID == "" {
		return
	}
	_ = exec.Command("docker", "kill", c.ID).Run()
}

func (c *SSHDContainer) Exec(t *testing.T, cmd ...string) string {
	t.Helper()
	args := append([]string{"exec", c.ID}, cmd...)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker exec %v: %v", cmd, err)
	}
	return string(out)
}

func (c *SSHDContainer) ExecAsync(cmd ...string) {
	args := append([]string{"exec", c.ID}, cmd...)
	go func() {
		_ = exec.Command("docker", args...).Run()
	}()
}

func PickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

type Daemon struct {
	cmd       *exec.Cmd
	addr      string
	tokenPath string
	stop      context.CancelFunc
	stderr    *bytes.Buffer
}

type DaemonOpts struct {
	StatePath string
	LockPath  string
	TokenPath string
	Env       []string
	Format    string // override -format flag; empty = json (test default)
}

func StartDaemon(t *testing.T, daemonBin string) *Daemon {
	t.Helper()
	return StartDaemonWith(t, daemonBin, DaemonOpts{})
}

func StartDaemonWith(t *testing.T, daemonBin string, opts DaemonOpts) *Daemon {
	t.Helper()
	port := PickFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	dir := t.TempDir()
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(dir, "state.json")
	}
	if opts.LockPath == "" {
		opts.LockPath = filepath.Join(dir, "daemon.lock")
	}
	if opts.TokenPath == "" {
		opts.TokenPath = filepath.Join(dir, "daemon.token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, daemonBin,
		"-addr", addr,
		"-state", opts.StatePath,
		"-lock", opts.LockPath,
		"-token", opts.TokenPath,
		"-log", "text",
		// The e2e suite parses tool responses as JSON. Pin the format
		// regardless of the new daemon default so tests stay deterministic.
		// A dedicated TestTOONFormat verifies the TOON path separately.
	)
	if opts.Format != "" {
		cmd.Args = append(cmd.Args, "-format", opts.Format)
	} else {
		cmd.Args = append(cmd.Args, "-format", "json")
	}
	cmd.Env = append(os.Environ(), opts.Env...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start daemon: %v", err)
	}
	d := &Daemon{cmd: cmd, addr: addr, tokenPath: opts.TokenPath, stop: cancel, stderr: &stderr}
	if err := waitForHTTP(addr); err != nil {
		t.Logf("daemon stderr:\n%s", stderr.String())
		d.Stop()
		t.Fatalf("daemon never came up: %v", err)
	}
	return d
}

func (d *Daemon) TokenPath() string { return d.tokenPath }

// daemon_ReadToken is a test-only wrapper to avoid importing internal/daemon
// from a build-tagged file (the internal import is allowed but adds noise).
func daemon_ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (d *Daemon) Addr() string { return d.addr }

func (d *Daemon) Stop() {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	// SIGTERM gives the daemon a chance to flush state and call CloseSessions
	// on SSE clients. Escalate to SIGKILL via ctx cancel if it ignores us.
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	graceful := make(chan struct{})
	go func() { _ = d.cmd.Wait(); close(graceful) }()
	select {
	case <-graceful:
	case <-time.After(3 * time.Second):
		if d.stop != nil {
			d.stop()
		}
		<-graceful
	}
}

// GenerateKey creates an ed25519 keypair on disk. Returns the private key path
// (OpenSSH PEM format) and the public key in authorized_keys form (single line).
func GenerateKey(t *testing.T) (privPath, pubAuthorized string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	pubAuthorized = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal priv: %v", err)
	}
	privBytes := pem.EncodeToMemory(pemBlock)
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(privPath, privBytes, 0o600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	return
}

// InstallAuthorizedKey appends an authorized_keys entry inside the container.
func InstallAuthorizedKey(t *testing.T, sshd *SSHDContainer, pub string) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", sshd.ID, "sh", "-c",
		"mkdir -p /root/.ssh && chmod 700 /root/.ssh && cat >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")
	cmd.Stdin = strings.NewReader(pub + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install authorized_keys: %v\n%s", err, string(out))
	}
}

// StartSSHAgent spawns an ssh-agent, adds the given key, and returns the socket
// path. The agent is killed at test cleanup.
func StartSSHAgent(t *testing.T, keyPath string) string {
	t.Helper()
	out, err := exec.Command("ssh-agent", "-c").Output()
	if err != nil {
		t.Skipf("ssh-agent not available: %v", err)
	}
	var sock, pidStr string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "setenv SSH_AUTH_SOCK ") {
			sock = strings.TrimSuffix(strings.TrimPrefix(line, "setenv SSH_AUTH_SOCK "), ";")
		}
		if strings.HasPrefix(line, "setenv SSH_AGENT_PID ") {
			pidStr = strings.TrimSuffix(strings.TrimPrefix(line, "setenv SSH_AGENT_PID "), ";")
		}
	}
	if sock == "" {
		t.Fatalf("could not parse ssh-agent output: %s", string(out))
	}
	add := exec.Command("ssh-add", keyPath)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, string(out))
	}
	t.Cleanup(func() {
		if pidStr != "" {
			_ = exec.Command("kill", pidStr).Run()
		}
	})
	return sock
}

func waitForHTTP(addr string) error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("address %s never accepted connections", addr)
}
