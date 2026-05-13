package dockerx

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"
)

type Host struct {
	ID   string
	Spec ConnectSpec

	mu          sync.RWMutex
	conn        *hostConn
	connectedAt time.Time
	lastError   string

	shellsMu sync.Mutex
	shells   map[string]*Shell

	closed atomic.Bool
}

type HostInfo struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	State       string    `json:"state"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Persistent  bool      `json:"persistent"`
	Shells      []string  `json:"shells"`
}

// HostRow is the compact, primitive-only projection used by docker_list_hosts
// — see sshx.SessionRow for the rationale.
type HostRow struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	State      string `json:"state"`
	LastError  string `json:"last_error"` // empty when none; kept for tabular-form uniformity
	Persistent bool   `json:"persistent"`
	Shells     int    `json:"shells"`
}

func (i HostInfo) Row() HostRow {
	return HostRow{ID: i.ID, Host: i.Host, State: i.State, LastError: i.LastError, Persistent: i.Persistent, Shells: len(i.Shells)}
}

func (h *Host) Info() HostInfo {
	h.mu.RLock()
	state := "disconnected"
	if h.conn != nil {
		state = "connected"
	}
	info := HostInfo{
		ID:          h.ID,
		Host:        h.Spec.Host,
		State:       state,
		ConnectedAt: h.connectedAt,
		LastError:   h.lastError,
		Persistent:  h.Spec.Persistent,
	}
	h.mu.RUnlock()

	h.shellsMu.Lock()
	shells := make([]string, 0, len(h.shells))
	for id := range h.shells {
		shells = append(shells, id)
	}
	h.shellsMu.Unlock()
	info.Shells = shells
	return info
}

func (h *Host) client() (*client.Client, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.conn == nil {
		return nil, fmt.Errorf("docker host %q is disconnected", h.ID)
	}
	return h.conn.docker, nil
}

func (h *Host) close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	h.shellsMu.Lock()
	for id, s := range h.shells {
		_ = s.Close()
		delete(h.shells, id)
	}
	h.shellsMu.Unlock()

	h.mu.Lock()
	if h.conn != nil {
		h.conn.close()
		h.conn = nil
	}
	h.mu.Unlock()
}

type Manager struct {
	mu    sync.RWMutex
	hosts map[string]*Host
}

func NewManager() *Manager {
	return &Manager{hosts: map[string]*Host{}}
}

func (m *Manager) Connect(id string, spec ConnectSpec) (*Host, error) {
	if id == "" {
		return nil, errors.New("docker host id is required")
	}
	m.mu.Lock()
	if _, ok := m.hosts[id]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("docker host %q already exists", id)
	}
	m.mu.Unlock()

	conn, err := dialDocker(spec)
	if err != nil {
		return nil, err
	}
	h := &Host{
		ID: id, Spec: spec,
		conn:        conn,
		connectedAt: time.Now(),
		shells:      map[string]*Shell{},
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.hosts[id]; ok {
		conn.close()
		return nil, fmt.Errorf("docker host %q created concurrently", id)
	}
	m.hosts[id] = h
	return h, nil
}

func (m *Manager) Get(id string) (*Host, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.hosts[id]
	if !ok {
		return nil, fmt.Errorf("docker host %q not found", id)
	}
	return h, nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	h, ok := m.hosts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("docker host %q not found", id)
	}
	delete(m.hosts, id)
	m.mu.Unlock()
	h.close()
	return nil
}

func (m *Manager) List() []HostInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]HostInfo, 0, len(m.hosts))
	for _, h := range m.hosts {
		out = append(out, h.Info())
	}
	return out
}

func (m *Manager) Hosts() map[string]*Host {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*Host, len(m.hosts))
	for k, v := range m.hosts {
		out[k] = v
	}
	return out
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.hosts {
		h.close()
		delete(m.hosts, id)
	}
}
