package sshx

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type ShellOptions struct {
	Term       string            `json:"term,omitempty"`
	Rows       int               `json:"rows,omitempty"`
	Cols       int               `json:"cols,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Command    string            `json:"command,omitempty"`
	BufferSize int               `json:"buffer_size,omitempty"`
}

type Shell struct {
	ID        string
	SessionID string

	parent  *Session // back-ref so the shell can deregister itself when the remote process exits
	session *ssh.Session

	writeMu sync.Mutex // serializes concurrent Write callers
	stdin   io.WriteCloser

	wakeCh chan struct{} // capacity 1; pump signals "buffer changed"

	bufMu  sync.Mutex
	buffer []byte
	maxBuf int

	closed atomic.Bool
	doneCh chan struct{}

	OpenedAt time.Time
}

type ShellInfo struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	OpenedAt   time.Time `json:"opened_at"`
	BufferSize int       `json:"buffered_bytes"`
	Closed     bool      `json:"closed"`
}

func (s *Shell) Info() ShellInfo {
	s.bufMu.Lock()
	size := len(s.buffer)
	s.bufMu.Unlock()
	return ShellInfo{
		ID:         s.ID,
		SessionID:  s.SessionID,
		OpenedAt:   s.OpenedAt,
		BufferSize: size,
		Closed:     s.closed.Load(),
	}
}

func (s *Shell) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return 0, errors.New("shell closed")
	}
	return s.stdin.Write(p)
}

// Read drains any buffered output. If the buffer is empty, it waits up to
// `timeout` for new data and re-checks the buffer after each wake — the wake
// channel is a hint, the buffer is the source of truth.
func (s *Shell) Read(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		s.bufMu.Lock()
		if len(s.buffer) > 0 {
			out := s.buffer
			s.buffer = nil
			s.bufMu.Unlock()
			return out, nil
		}
		s.bufMu.Unlock()

		if s.closed.Load() {
			return nil, io.EOF
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-s.wakeCh:
			timer.Stop()
		case <-timer.C:
			return nil, nil
		case <-s.doneCh:
			timer.Stop()
			s.bufMu.Lock()
			out := s.buffer
			s.buffer = nil
			s.bufMu.Unlock()
			if len(out) > 0 {
				return out, nil
			}
			return nil, io.EOF
		}
	}
}

func (s *Shell) Resize(rows, cols int) error {
	if s.closed.Load() {
		return errors.New("shell closed")
	}
	return s.session.WindowChange(rows, cols)
}

func (s *Shell) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = s.stdin.Close()
	err := s.session.Close()
	select {
	case <-s.doneCh:
	default:
		close(s.doneCh)
	}
	return err
}

func (s *Shell) appendBuffer(chunk []byte) {
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	s.buffer = append(s.buffer, chunk...)
	if s.maxBuf > 0 && len(s.buffer) > s.maxBuf {
		drop := len(s.buffer) - s.maxBuf
		s.buffer = s.buffer[drop:]
	}
}

func (sess *Session) OpenShell(shellID string, opts ShellOptions) (*Shell, error) {
	client, err := sess.getClient()
	if err != nil {
		return nil, err
	}
	chSess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new ssh channel: %w", err)
	}
	for k, v := range opts.Env {
		_ = chSess.Setenv(k, v)
	}
	rows, cols := opts.Rows, opts.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	term := opts.Term
	if term == "" {
		term = "xterm-256color"
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	sess.mu.RLock()
	fwd := sess.agentForwardOn
	sess.mu.RUnlock()
	if fwd {
		_ = agent.RequestAgentForwarding(chSess)
	}
	if err := chSess.RequestPty(term, rows, cols, modes); err != nil {
		_ = chSess.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := chSess.StdinPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	stdout, err := chSess.StdoutPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	stderr, err := chSess.StderrPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	if opts.Command != "" {
		if err := chSess.Start(opts.Command); err != nil {
			_ = chSess.Close()
			return nil, fmt.Errorf("start command: %w", err)
		}
	} else {
		if err := chSess.Shell(); err != nil {
			_ = chSess.Close()
			return nil, fmt.Errorf("start shell: %w", err)
		}
	}

	bufSize := opts.BufferSize
	if bufSize == 0 {
		bufSize = 1024 * 1024
	}
	sh := &Shell{
		ID: shellID, SessionID: sess.ID,
		parent:   sess,
		session:  chSess,
		stdin:    stdin,
		wakeCh:   make(chan struct{}, 1),
		maxBuf:   bufSize,
		doneCh:   make(chan struct{}),
		OpenedAt: time.Now(),
	}

	go sh.pumpReader(stdout)
	go sh.pumpReader(stderr)
	go func() {
		_ = chSess.Wait()
		sh.closed.Store(true)
		select {
		case <-sh.doneCh:
		default:
			close(sh.doneCh)
		}
		// Auto-remove from the parent session so ssh_shell_list doesn't keep
		// reporting dead shells.
		sess.shellsMu.Lock()
		if cur, ok := sess.shells[shellID]; ok && cur == sh {
			delete(sess.shells, shellID)
		}
		sess.shellsMu.Unlock()
	}()

	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	if sess.closed.Load() {
		_ = sh.Close()
		return nil, fmt.Errorf("session %q is closed", sess.ID)
	}
	if _, exists := sess.shells[shellID]; exists {
		_ = sh.Close()
		return nil, fmt.Errorf("shell %q already exists on session %q", shellID, sess.ID)
	}
	sess.shells[shellID] = sh
	return sh, nil
}

func (sh *Shell) pumpReader(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sh.appendBuffer(chunk)
			// Non-blocking wake. The wakeCh has capacity 1 — if a wake is
			// already pending the reader will see the data on its next loop.
			select {
			case sh.wakeCh <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (sess *Session) GetShell(shellID string) (*Shell, error) {
	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	sh, ok := sess.shells[shellID]
	if !ok {
		return nil, fmt.Errorf("shell %q not found on session %q", shellID, sess.ID)
	}
	return sh, nil
}

func (sess *Session) CloseShell(shellID string) error {
	sess.shellsMu.Lock()
	sh, ok := sess.shells[shellID]
	if !ok {
		sess.shellsMu.Unlock()
		return fmt.Errorf("shell %q not found on session %q", shellID, sess.ID)
	}
	delete(sess.shells, shellID)
	sess.shellsMu.Unlock()
	return sh.Close()
}

func (sess *Session) ListShells() []ShellInfo {
	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	out := make([]ShellInfo, 0, len(sess.shells))
	for _, sh := range sess.shells {
		out = append(out, sh.Info())
	}
	return out
}
