package sshx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type AuthSpec struct {
	KeyPath       string `json:"key_path,omitempty"`
	KeyPassphrase string `json:"key_passphrase,omitempty"`
	UseAgent      bool   `json:"use_agent,omitempty"`
	Password      string `json:"password,omitempty"`
}

// closerList lets callers cleanly release auth-time resources (e.g. the
// ssh-agent unix socket) after ssh.Dial has completed.
type closerList []io.Closer

func (cl closerList) Close() {
	for _, c := range cl {
		_ = c.Close()
	}
}

// Build returns the auth methods plus a Closer that releases anything Methods
// opened (currently just the ssh-agent socket). Always defer cl.Close() in
// the caller, even on error — Build closes its own state on error paths but
// the caller must close on the happy path too.
func (a AuthSpec) Build() ([]ssh.AuthMethod, closerList, error) {
	var methods []ssh.AuthMethod
	var closers closerList

	if a.UseAgent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, nil, errors.New("ssh-agent requested but SSH_AUTH_SOCK is empty")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, nil, fmt.Errorf("dial ssh-agent: %w", err)
		}
		closers = append(closers, conn)
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}

	if a.KeyPath != "" {
		data, err := os.ReadFile(a.KeyPath)
		if err != nil {
			closers.Close()
			// Don't echo the key path back to the MCP client; the file is
			// often in a private user directory and the path itself is
			// reconnaissance. Log the path on the daemon if you need details.
			return nil, nil, fmt.Errorf("read private key: %w", err)
		}
		var signer ssh.Signer
		if a.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(a.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(data)
		}
		if err != nil {
			closers.Close()
			return nil, nil, fmt.Errorf("parse private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if a.Password != "" {
		methods = append(methods, ssh.Password(a.Password))
	}

	if len(methods) == 0 {
		closers.Close()
		return nil, nil, errors.New("no authentication method provided (set key_path, use_agent, or password)")
	}
	return methods, closers, nil
}


func HostKeyCallback(insecure bool, knownHostsPath string) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home dir: %w", err)
		}
		knownHostsPath = home + "/.ssh/known_hosts"
	}
	if _, err := os.Stat(knownHostsPath); err != nil {
		// Mention that a known_hosts file is missing without echoing the
		// (possibly home-directory) path back to the MCP client.
		return nil, fmt.Errorf("known_hosts file is not readable: %w (pass insecure=true to skip host key verification)", err)
	}
	return knownhosts.New(knownHostsPath)
}
