package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"
)

type ShellOptions struct {
	Container  string            `json:"container"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Rows       uint              `json:"rows,omitempty"`
	Cols       uint              `json:"cols,omitempty"`
	BufferSize int               `json:"buffer_size,omitempty"`
}

type Shell struct {
	ID       string
	HostID   string
	Spec     ShellOptions
	OpenedAt time.Time

	execID  string
	host    *Host
	attach  client.HijackedResponse
	writeMu sync.Mutex // serializes concurrent Write callers

	wakeCh chan struct{} // capacity 1; pump signals "buffer changed"
	bufMu  sync.Mutex
	buffer []byte
	maxBuf int

	closed atomic.Bool
	doneCh chan struct{}
}

type ShellInfo struct {
	ID         string    `json:"id"`
	HostID     string    `json:"host_id"`
	Container  string    `json:"container"`
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
		HostID:     s.HostID,
		Container:  s.Spec.Container,
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
	return s.attach.Conn.Write(p)
}

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

func (s *Shell) Resize(rows, cols uint) error {
	if s.closed.Load() {
		return errors.New("shell closed")
	}
	c, err := s.host.client()
	if err != nil {
		return err
	}
	_, err = c.ExecResize(context.Background(), s.execID, client.ExecResizeOptions{Height: rows, Width: cols})
	return err
}

func (s *Shell) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.attach.Close()
	select {
	case <-s.doneCh:
	default:
		close(s.doneCh)
	}
	return nil
}

func (s *Shell) appendBuffer(chunk []byte) {
	s.bufMu.Lock()
	s.buffer = append(s.buffer, chunk...)
	if s.maxBuf > 0 && len(s.buffer) > s.maxBuf {
		drop := len(s.buffer) - s.maxBuf
		s.buffer = s.buffer[drop:]
	}
	s.bufMu.Unlock()
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (h *Host) OpenShell(ctx context.Context, shellID string, opts ShellOptions) (*Shell, error) {
	c, err := h.client()
	if err != nil {
		return nil, err
	}
	if opts.Container == "" {
		return nil, fmt.Errorf("container is required")
	}
	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh"}
	}
	env := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	create, err := c.ExecCreate(ctx, opts.Container, client.ExecCreateOptions{
		Cmd:          cmd,
		Env:          env,
		User:         opts.User,
		WorkingDir:   opts.WorkingDir,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          true,
		ConsoleSize:  client.ConsoleSize{Height: maxUint(opts.Rows, 24), Width: maxUint(opts.Cols, 80)},
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	attach, err := c.ExecAttach(ctx, create.ID, client.ExecAttachOptions{
		TTY:         true,
		ConsoleSize: client.ConsoleSize{Height: maxUint(opts.Rows, 24), Width: maxUint(opts.Cols, 80)},
	})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	bufSize := opts.BufferSize
	if bufSize == 0 {
		bufSize = 1024 * 1024
	}
	sh := &Shell{
		ID: shellID, HostID: h.ID, Spec: opts,
		execID:   create.ID,
		host:     h,
		attach:   attach.HijackedResponse,
		wakeCh:   make(chan struct{}, 1),
		maxBuf:   bufSize,
		doneCh:   make(chan struct{}),
		OpenedAt: time.Now(),
	}

	go sh.pump()

	h.shellsMu.Lock()
	defer h.shellsMu.Unlock()
	if h.closed.Load() {
		_ = sh.Close()
		return nil, fmt.Errorf("docker host %q is closed", h.ID)
	}
	if _, exists := h.shells[shellID]; exists {
		_ = sh.Close()
		return nil, fmt.Errorf("shell %q already exists on host %q", shellID, h.ID)
	}
	h.shells[shellID] = sh
	return sh, nil
}

func (s *Shell) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := s.attach.Reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.appendBuffer(chunk)
		}
		if err != nil {
			s.closed.Store(true)
			select {
			case <-s.doneCh:
			default:
				close(s.doneCh)
			}
			// Auto-remove from the host so docker_shell_list doesn't keep
			// reporting dead shells.
			s.host.shellsMu.Lock()
			if cur, ok := s.host.shells[s.ID]; ok && cur == s {
				delete(s.host.shells, s.ID)
			}
			s.host.shellsMu.Unlock()
			return
		}
	}
}

func (h *Host) GetShell(id string) (*Shell, error) {
	h.shellsMu.Lock()
	defer h.shellsMu.Unlock()
	s, ok := h.shells[id]
	if !ok {
		return nil, fmt.Errorf("shell %q not found on host %q", id, h.ID)
	}
	return s, nil
}

func (h *Host) CloseShell(id string) error {
	h.shellsMu.Lock()
	s, ok := h.shells[id]
	if !ok {
		h.shellsMu.Unlock()
		return fmt.Errorf("shell %q not found on host %q", id, h.ID)
	}
	delete(h.shells, id)
	h.shellsMu.Unlock()
	return s.Close()
}

func (h *Host) ListShells() []ShellInfo {
	h.shellsMu.Lock()
	defer h.shellsMu.Unlock()
	out := make([]ShellInfo, 0, len(h.shells))
	for _, s := range h.shells {
		out = append(out, s.Info())
	}
	return out
}

func maxUint(v, def uint) uint {
	if v == 0 {
		return def
	}
	return v
}
