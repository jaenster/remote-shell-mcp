package sshx

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const defaultPort = 22

func normalizeAddrs(addrs []string) ([]string, error) {
	if len(addrs) == 0 {
		return nil, errors.New("at least one address is required")
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a); err != nil {
			a = net.JoinHostPort(a, strconv.Itoa(defaultPort))
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, errors.New("addresses cannot be empty")
	}
	return out, nil
}

func dialFinal(spec ConnectSpec) (*ssh.Client, []*ssh.Client, string, error) {
	addrs, err := normalizeAddrs(spec.Addresses)
	if err != nil {
		return nil, nil, "", err
	}

	methods, authClosers, err := spec.Auth.Build()
	if err != nil {
		return nil, nil, "", err
	}
	defer authClosers.Close()
	hostKey, err := HostKeyCallback(spec.Insecure, spec.KnownHostsPath)
	if err != nil {
		return nil, nil, "", err
	}
	timeout := spec.Timeout.Std()
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            spec.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}

	jumpClients, err := dialJumpChain(spec.JumpHosts)
	if err != nil {
		return nil, nil, "", err
	}

	var lastErr error
	for _, addr := range addrs {
		client, err := dialThrough(jumpClients, addr, cfg)
		if err == nil {
			return client, jumpClients, addr, nil
		}
		lastErr = err
	}
	for _, jc := range jumpClients {
		_ = jc.Close()
	}
	return nil, nil, "", fmt.Errorf("all addresses failed: %w", lastErr)
}

func dialJumpChain(jumps []JumpHost) ([]*ssh.Client, error) {
	if len(jumps) == 0 {
		return nil, nil
	}
	clients := make([]*ssh.Client, 0, len(jumps))
	for _, j := range jumps {
		dialed, err := dialOneJump(j, clients)
		if err != nil {
			closeAll(clients)
			return nil, fmt.Errorf("jump host %s: %w", j.User, err)
		}
		clients = append(clients, dialed)
	}
	return clients, nil
}

// dialOneJump exists as a function so `defer closers.Close()` releases the
// ssh-agent socket after the dial completes rather than leaking until the
// session ends.
func dialOneJump(j JumpHost, chain []*ssh.Client) (*ssh.Client, error) {
	addrs, err := normalizeAddrs(j.Addresses)
	if err != nil {
		return nil, err
	}
	methods, closers, err := j.Auth.Build()
	if err != nil {
		return nil, err
	}
	defer closers.Close()
	hostKey, err := HostKeyCallback(j.Insecure, j.KnownHostsPath)
	if err != nil {
		return nil, err
	}
	timeout := j.Timeout.Std()
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            j.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}
	var lastErr error
	for _, addr := range addrs {
		dialed, err := dialThrough(chain, addr, cfg)
		if err == nil {
			return dialed, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial failed: %w", lastErr)
}

func dialThrough(chain []*ssh.Client, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	if len(chain) == 0 {
		return ssh.Dial("tcp", addr, cfg)
	}
	parent := chain[len(chain)-1]
	conn, err := parent.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

func closeAll(clients []*ssh.Client) {
	for _, c := range clients {
		_ = c.Close()
	}
}
