package sshx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// perConnDialTimeout bounds how long a forwarded-connection dial can spend
// setting up an SSH-tunneled TCP channel. Without this, a buggy or hostile
// SOCKS client targeting an unreachable host accumulates one stuck goroutine
// per attempt until the session itself is closed.
const perConnDialTimeout = 30 * time.Second

func dialViaClient(client interface {
	DialContext(ctx context.Context, n, addr string) (net.Conn, error)
}, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), perConnDialTimeout)
	defer cancel()
	return client.DialContext(ctx, "tcp", addr)
}

type ForwardKind string

const (
	ForwardLocal   ForwardKind = "local"
	ForwardRemote  ForwardKind = "remote"
	ForwardDynamic ForwardKind = "dynamic"
)

type LocalSpec struct {
	BindAddr   string `json:"bind_addr,omitempty"`
	BindPort   int    `json:"bind_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

type RemoteSpec struct {
	BindAddr  string `json:"bind_addr,omitempty"`
	BindPort  int    `json:"bind_port"`
	LocalHost string `json:"local_host"`
	LocalPort int    `json:"local_port"`
}

type DynamicSpec struct {
	BindAddr string `json:"bind_addr,omitempty"`
	BindPort int    `json:"bind_port"`
}

type Forward struct {
	ID        string
	SessionID string
	Kind      ForwardKind

	LocalSpec   *LocalSpec
	RemoteSpec  *RemoteSpec
	DynamicSpec *DynamicSpec

	session  *Session
	listener net.Listener
	bindMu   sync.Mutex
	bindPort int
	closed   atomic.Bool
	conns    atomic.Int64
}

func (f *Forward) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	f.bindMu.Lock()
	defer f.bindMu.Unlock()
	if f.listener != nil {
		return f.listener.Close()
	}
	return nil
}

func (f *Forward) Info() ForwardInfo {
	info := ForwardInfo{
		ID:          f.ID,
		SessionID:   f.SessionID,
		Kind:        string(f.Kind),
		Connections: f.conns.Load(),
	}
	switch f.Kind {
	case ForwardLocal:
		info.BindAddr = f.LocalSpec.BindAddr
		info.BindPort = f.bindPort
		info.RemoteAddr = f.LocalSpec.RemoteHost
		info.RemotePort = f.LocalSpec.RemotePort
		info.LocalSpec = f.LocalSpec
	case ForwardRemote:
		info.BindAddr = f.RemoteSpec.BindAddr
		info.BindPort = f.bindPort
		info.RemoteAddr = f.RemoteSpec.LocalHost
		info.RemotePort = f.RemoteSpec.LocalPort
		info.RemoteSpec = f.RemoteSpec
	case ForwardDynamic:
		info.BindAddr = f.DynamicSpec.BindAddr
		info.BindPort = f.bindPort
		info.DynamicSpec = f.DynamicSpec
	}
	return info
}

type ForwardInfo struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	Kind        string `json:"kind"`
	BindAddr    string `json:"bind_addr"`
	BindPort    int    `json:"bind_port"` // effective port (may differ from user's BindPort=0)
	RemoteAddr  string `json:"remote_addr,omitempty"`
	RemotePort  int    `json:"remote_port,omitempty"`
	Connections int64  `json:"connections"`

	// User-supplied spec, preserved verbatim so persistence can replay
	// "any free port" (BindPort=0) instead of pinning the ephemeral port.
	LocalSpec   *LocalSpec   `json:"local_spec,omitempty"`
	RemoteSpec  *RemoteSpec  `json:"remote_spec,omitempty"`
	DynamicSpec *DynamicSpec `json:"dynamic_spec,omitempty"`
}

func (m *Manager) OpenLocalForward(sessionID, forwardID string, spec LocalSpec) (*Forward, error) {
	s, err := m.Get(sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := s.getClient(); err != nil {
		return nil, err
	}
	if spec.BindAddr == "" {
		spec.BindAddr = "127.0.0.1"
	}
	if spec.RemoteHost == "" {
		return nil, errors.New("remote_host is required for local forward")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(spec.BindAddr, strconv.Itoa(spec.BindPort)))
	if err != nil {
		return nil, fmt.Errorf("listen %s:%d: %w", spec.BindAddr, spec.BindPort, err)
	}
	f := &Forward{
		ID: forwardID, SessionID: sessionID, Kind: ForwardLocal,
		LocalSpec: &spec, session: s, listener: ln, bindPort: ln.Addr().(*net.TCPAddr).Port,
	}
	if err := s.registerForward(f); err != nil {
		_ = ln.Close()
		return nil, err
	}
	go runLocalForward(f, ln)
	return f, nil
}

func runLocalForward(f *Forward, ln net.Listener) {
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go func(c net.Conn) {
			defer c.Close()
			client, err := f.session.getClient()
			if err != nil {
				return
			}
			remote, err := dialViaClient(client, net.JoinHostPort(f.LocalSpec.RemoteHost, strconv.Itoa(f.LocalSpec.RemotePort)))
			if err != nil {
				return
			}
			defer remote.Close()
			pipe(c, remote)
		}(local)
	}
}

func (m *Manager) OpenRemoteForward(sessionID, forwardID string, spec RemoteSpec) (*Forward, error) {
	s, err := m.Get(sessionID)
	if err != nil {
		return nil, err
	}
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	if spec.BindAddr == "" {
		spec.BindAddr = "127.0.0.1"
	}
	if spec.LocalHost == "" {
		spec.LocalHost = "127.0.0.1"
	}
	ln, err := client.Listen("tcp", net.JoinHostPort(spec.BindAddr, strconv.Itoa(spec.BindPort)))
	if err != nil {
		return nil, fmt.Errorf("remote listen %s:%d: %w", spec.BindAddr, spec.BindPort, err)
	}
	port := spec.BindPort
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}
	f := &Forward{
		ID: forwardID, SessionID: sessionID, Kind: ForwardRemote,
		RemoteSpec: &spec, session: s, listener: ln, bindPort: port,
	}
	if err := s.registerForward(f); err != nil {
		_ = ln.Close()
		return nil, err
	}
	go runRemoteForward(f, ln)
	return f, nil
}

func runRemoteForward(f *Forward, ln net.Listener) {
	for {
		remote, err := ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go func(c net.Conn) {
			defer c.Close()
			local, err := net.Dial("tcp", net.JoinHostPort(f.RemoteSpec.LocalHost, strconv.Itoa(f.RemoteSpec.LocalPort)))
			if err != nil {
				return
			}
			defer local.Close()
			pipe(c, local)
		}(remote)
	}
}

func (m *Manager) OpenDynamicForward(sessionID, forwardID string, spec DynamicSpec) (*Forward, error) {
	s, err := m.Get(sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := s.getClient(); err != nil {
		return nil, err
	}
	if spec.BindAddr == "" {
		spec.BindAddr = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(spec.BindAddr, strconv.Itoa(spec.BindPort)))
	if err != nil {
		return nil, fmt.Errorf("listen %s:%d: %w", spec.BindAddr, spec.BindPort, err)
	}
	f := &Forward{
		ID: forwardID, SessionID: sessionID, Kind: ForwardDynamic,
		DynamicSpec: &spec, session: s, listener: ln, bindPort: ln.Addr().(*net.TCPAddr).Port,
	}
	if err := s.registerForward(f); err != nil {
		_ = ln.Close()
		return nil, err
	}
	go runSocksForward(f, ln)
	return f, nil
}

func runSocksForward(f *Forward, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go handleSocks(f.session, c)
	}
}

func handleSocks(s *Session, c net.Conn) {
	defer c.Close()
	if err := socksHandshake(c); err != nil {
		return
	}
	dest, err := socksReadRequest(c)
	if err != nil {
		_ = socksReply(c, 0x01)
		return
	}
	client, err := s.getClient()
	if err != nil {
		_ = socksReply(c, 0x03)
		return
	}
	upstream, err := dialViaClient(client, dest)
	if err != nil {
		_ = socksReply(c, 0x05)
		return
	}
	defer upstream.Close()
	if err := socksReply(c, 0x00); err != nil {
		return
	}
	pipe(c, upstream)
}

func socksHandshake(c net.Conn) error {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		return err
	}
	if buf[0] != 0x05 {
		return errors.New("socks: bad version")
	}
	methods := make([]byte, int(buf[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	_, err := c.Write([]byte{0x05, 0x00})
	return err
}

func socksReadRequest(c net.Conn) (string, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return "", err
	}
	if hdr[0] != 0x05 || hdr[1] != 0x01 {
		return "", errors.New("socks: unsupported command")
	}
	var host string
	switch hdr[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 0x03:
		ln := make([]byte, 1)
		if _, err := io.ReadFull(c, ln); err != nil {
			return "", err
		}
		if ln[0] == 0 {
			return "", errors.New("socks: empty hostname")
		}
		name := make([]byte, int(ln[0]))
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	default:
		return "", errors.New("socks: unknown atyp")
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(c, portB); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portB)))), nil
}

func socksReply(c net.Conn, rep byte) error {
	_, err := c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

type closeWriter interface{ CloseWrite() error }

func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); halfClose(a) }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); halfClose(b) }()
	wg.Wait()
}

func (s *Session) registerForward(f *Forward) error {
	s.forwardsMu.Lock()
	defer s.forwardsMu.Unlock()
	if s.closed.Load() {
		return fmt.Errorf("session %q is closed", s.ID)
	}
	if _, exists := s.forwards[f.ID]; exists {
		return fmt.Errorf("forward %q already exists on session %q", f.ID, s.ID)
	}
	s.forwards[f.ID] = f
	return nil
}

func (m *Manager) CloseForward(sessionID, forwardID string) error {
	s, err := m.Get(sessionID)
	if err != nil {
		return err
	}
	s.forwardsMu.Lock()
	f, ok := s.forwards[forwardID]
	if !ok {
		s.forwardsMu.Unlock()
		return fmt.Errorf("forward %q not found on session %q", forwardID, sessionID)
	}
	delete(s.forwards, forwardID)
	s.forwardsMu.Unlock()
	return f.Close()
}

func (m *Manager) ListForwards(sessionID string) ([]ForwardInfo, error) {
	if sessionID == "" {
		m.mu.RLock()
		defer m.mu.RUnlock()
		// Empty slice (not nil) so JSON marshals to [] rather than null when
		// there are no forwards. The schema says "list"; null is a lie.
		out := []ForwardInfo{}
		for _, s := range m.sessions {
			s.forwardsMu.Lock()
			for _, f := range s.forwards {
				out = append(out, f.Info())
			}
			s.forwardsMu.Unlock()
		}
		return out, nil
	}
	s, err := m.Get(sessionID)
	if err != nil {
		return nil, err
	}
	s.forwardsMu.Lock()
	defer s.forwardsMu.Unlock()
	out := make([]ForwardInfo, 0, len(s.forwards))
	for _, f := range s.forwards {
		out = append(out, f.Info())
	}
	return out, nil
}

func (s *Session) rebindForwards() {
	s.forwardsMu.Lock()
	forwards := make([]*Forward, 0, len(s.forwards))
	for _, f := range s.forwards {
		forwards = append(forwards, f)
	}
	s.forwardsMu.Unlock()

	for _, f := range forwards {
		if f.Kind != ForwardRemote || f.closed.Load() {
			continue
		}
		client, err := s.getClient()
		if err != nil {
			continue
		}
		ln, err := client.Listen("tcp", net.JoinHostPort(f.RemoteSpec.BindAddr, strconv.Itoa(f.RemoteSpec.BindPort)))
		if err != nil {
			continue
		}
		// Swap in the new listener and close the old one. Closing the old
		// listener causes its accept loop to exit, so we never have two
		// goroutines racing to Accept() on the same forward. Re-check
		// f.closed under bindMu — Forward.Close may have fired while we
		// were dialing, in which case we must drop the new listener too.
		f.bindMu.Lock()
		if f.closed.Load() {
			f.bindMu.Unlock()
			_ = ln.Close()
			continue
		}
		old := f.listener
		f.listener = ln
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			f.bindPort = tcp.Port
		}
		f.bindMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		go runRemoteForward(f, ln)
	}
}
