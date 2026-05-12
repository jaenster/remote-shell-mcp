package dockerx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

type hostConn struct {
	docker    *client.Client
	sshClient *ssh.Client
}

func dialDocker(spec ConnectSpec) (*hostConn, error) {
	host := strings.TrimSpace(spec.Host)
	if host == "" {
		return nil, errors.New("host is required")
	}

	if strings.HasPrefix(host, "ssh://") {
		return dialDockerOverSSH(host, spec)
	}

	opts := []client.Opt{
		client.WithHost(host),
		client.WithAPIVersionNegotiation(),
	}
	if spec.APIVersion != "" {
		opts = append(opts, client.WithVersion(spec.APIVersion))
	}
	if spec.TLSCertPath != "" || spec.TLSCAPath != "" {
		opts = append(opts, client.WithTLSClientConfig(spec.TLSCAPath, spec.TLSCertPath, spec.TLSKeyPath))
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client %q: %w", host, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping docker daemon: %w", err)
	}
	return &hostConn{docker: c}, nil
}

func dialDockerOverSSH(rawURL string, spec ConnectSpec) (*hostConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse ssh url: %w", err)
	}
	user := u.User.Username()
	if user == "" {
		return nil, errors.New("ssh:// docker host requires a user (ssh://user@host)")
	}
	hostname := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "22"
	}
	addr := net.JoinHostPort(hostname, port)

	auth := sshx.AuthSpec{
		KeyPath:       spec.KeyPath,
		KeyPassphrase: spec.KeyPassphrase,
		UseAgent:      spec.UseAgent,
		Password:      spec.Password,
	}
	methods, authClosers, err := auth.Build()
	if err != nil {
		return nil, err
	}
	defer authClosers.Close()
	hostKey, err := sshx.HostKeyCallback(spec.SSHInsecure, spec.KnownHostsPath)
	if err != nil {
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         15 * time.Second,
	}
	sc, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	socket := strings.TrimPrefix(u.Path, "/")
	if socket == "" {
		socket = "/var/run/docker.sock"
	} else {
		socket = "/" + socket
	}

	dialContext := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return sc.Dial("unix", socket)
	}

	opts := []client.Opt{
		client.WithHost("http://docker"),
		client.WithDialContext(dialContext),
		client.WithAPIVersionNegotiation(),
	}
	if spec.APIVersion != "" {
		opts = append(opts, client.WithVersion(spec.APIVersion))
	}
	c, err := client.New(opts...)
	if err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("docker client over ssh: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		_ = c.Close()
		_ = sc.Close()
		return nil, fmt.Errorf("ping docker daemon over ssh: %w", err)
	}
	return &hostConn{docker: c, sshClient: sc}, nil
}

func (h *hostConn) close() {
	if h.docker != nil {
		_ = h.docker.Close()
	}
	if h.sshClient != nil {
		_ = h.sshClient.Close()
	}
}
