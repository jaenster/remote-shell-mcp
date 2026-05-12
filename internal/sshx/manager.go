package sshx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

func (m *Manager) Connect(id string, spec ConnectSpec) (*Session, error) {
	if id == "" {
		return nil, errors.New("session id is required")
	}
	if spec.User == "" {
		return nil, errors.New("user is required")
	}

	m.mu.Lock()
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("session %q already exists", id)
	}
	sess := &Session{
		ID:       id,
		Spec:     spec,
		state:    StateConnecting,
		forwards: map[string]*Forward{},
		shells:   map[string]*Shell{},
	}
	m.sessions[id] = sess
	m.mu.Unlock()

	client, jumps, addr, err := dialFinal(spec)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return nil, err
	}
	if !sess.setClient(client, jumps, addr) {
		// Session was disconnected mid-dial. setClient already closed the
		// new connection — clean up the map entry if it's still there.
		m.mu.Lock()
		if existing, ok := m.sessions[id]; ok && existing == sess {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
		return nil, errors.New("session was disconnected during connect")
	}

	if spec.Keepalive.Std() > 0 || spec.AutoReconnect {
		sess.startKeepalive()
	}
	return sess, nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("ssh session %q not found", id)
	}
	return s, nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("ssh session %q not found", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	s.close()
	return nil
}

func (m *Manager) Reconnect(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	s.clearClient()
	s.markState(StateReconnecting, nil)

	client, jumps, addr, err := dialFinal(s.Spec)
	if err != nil {
		s.markState(StateDisconnected, err)
		return nil, err
	}
	if !s.setClient(client, jumps, addr) {
		return nil, errors.New("session was closed during reconnect")
	}
	s.rebindForwards()
	return s, nil
}

func (m *Manager) CloneSession(srcID, newID string) (*Session, error) {
	src, err := m.Get(srcID)
	if err != nil {
		return nil, err
	}
	return m.Connect(newID, src.Spec)
}

func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	return out
}

func (m *Manager) Sessions() map[string]*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*Session, len(m.sessions))
	for k, v := range m.sessions {
		out[k] = v
	}
	return out
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.close()
		delete(m.sessions, id)
	}
}

func (s *Session) startKeepalive() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		cancel()
		return
	}
	s.keepaliveCancel = cancel
	s.mu.Unlock()
	interval := s.Spec.Keepalive.Std()
	if interval == 0 {
		interval = 30 * time.Second
	}
	go s.runKeepalive(ctx, interval)
}

func (s *Session) runKeepalive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			client, err := s.getClient()
			if err != nil {
				if s.Spec.AutoReconnect {
					s.attemptReconnect(ctx)
				}
				continue
			}
			_, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				if s.Spec.AutoReconnect {
					s.attemptReconnect(ctx)
				} else {
					s.markState(StateDisconnected, err)
				}
			}
		}
	}
}

func (s *Session) attemptReconnect(ctx context.Context) {
	// Skip if another reconnect (user-driven or keepalive-driven) is already in flight.
	if !s.reconnectMu.TryLock() {
		return
	}
	defer s.reconnectMu.Unlock()

	s.clearClient()
	s.markState(StateReconnecting, nil)

	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		client, jumps, addr, err := dialFinal(s.Spec)
		if err == nil {
			if s.setClient(client, jumps, addr) {
				s.rebindForwards()
			}
			return
		}
		s.markState(StateReconnecting, err)
		wait := backoff[len(backoff)-1]
		if i < len(backoff) {
			wait = backoff[i]
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
