package mcptools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	toon "github.com/toon-format/toon-go"

	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
)

// OutputFormat controls how tool results are serialized into MCP text content.
// LLM consumers paying per token may prefer TOON for arrays of uniform
// objects (docker_containers, ssh_file_list, etc.) where it cuts ~30-50%
// vs. JSON.
type OutputFormat string

const (
	FormatJSON OutputFormat = "json"
	FormatTOON OutputFormat = "toon"
)

func ParseFormat(s string) (OutputFormat, error) {
	switch OutputFormat(s) {
	case "":
		return FormatJSON, nil
	case FormatJSON, FormatTOON:
		return OutputFormat(s), nil
	default:
		return "", fmt.Errorf("unknown output format %q (want json or toon)", s)
	}
}

type State struct {
	SSH    *sshx.Manager
	Docker *dockerx.Manager
	Store  *state.Store
	Log    *slog.Logger
	Format OutputFormat // json (default) or toon

	flushStartOnce sync.Once
	flushReqCh     chan struct{} // capacity 1; coalesced "please flush" signal
	flushStopCh    chan struct{}
	flushDoneCh    chan struct{}
}

// Start kicks off the background state flusher. Tool handlers call Persist()
// which is now a debounced async signal; the actual disk write happens here.
// Idempotent; safe to call from main() before serving traffic.
func (s *State) Start() {
	s.flushStartOnce.Do(func() {
		s.flushReqCh = make(chan struct{}, 1)
		s.flushStopCh = make(chan struct{})
		s.flushDoneCh = make(chan struct{})
		go s.flushLoop()
	})
}

// Stop halts the flusher and performs one final synchronous flush so a clean
// shutdown can't leave a pending write on the floor.
func (s *State) Stop() {
	if s.flushStopCh == nil {
		_ = s.PersistNow()
		return
	}
	close(s.flushStopCh)
	<-s.flushDoneCh
	_ = s.PersistNow()
}

// Persist requests an async flush. Multiple calls inside the debounce window
// (200ms) coalesce into a single disk write. Tool handlers call this on
// every state-mutating op; the cost is now bounded regardless of churn.
func (s *State) Persist() error {
	if s.Store == nil {
		return nil
	}
	if s.flushReqCh == nil {
		// Not started — fall back to synchronous so anyone using State as a
		// library outside the daemon still gets persistence semantics.
		return s.PersistNow()
	}
	select {
	case s.flushReqCh <- struct{}{}:
	default:
	}
	return nil
}

// PersistNow blocks until the current state is on disk. Use this from the
// `snapshot` tool and from the daemon shutdown path; otherwise prefer Persist.
func (s *State) PersistNow() error {
	if s.Store == nil {
		return nil
	}
	return s.Store.Save(state.Capture(s.SSH, s.Docker))
}

func (s *State) flushLoop() {
	defer close(s.flushDoneCh)
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	fire := func() {
		if err := s.PersistNow(); err != nil && s.Log != nil {
			s.Log.Warn("flush state", "err", err)
		}
	}
	for {
		select {
		case <-s.flushStopCh:
			return
		case <-s.flushReqCh:
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			timer = nil
			fire()
		}
	}
}

func Build(st *State, name, version string) *server.MCPServer {
	srv := server.NewMCPServer(
		name, version,
		server.WithToolCapabilities(true),
		server.WithLogging(),
	)
	RegisterSSH(srv, st)
	RegisterSSHShell(srv, st)
	RegisterSFTP(srv, st)
	RegisterForward(srv, st)
	RegisterDocker(srv, st)
	RegisterDockerShell(srv, st)
	RegisterMeta(srv, st)
	return srv
}

// resultJSON serializes a result for an MCP text-content reply. Despite the
// name, the actual format respects State.Format — JSON (default) or TOON.
// Renamed call sites would be churn; the function semantic is "structured
// result, encoded according to the daemon's output-format setting".
func (s *State) resultJSON(v any) (*mcp.CallToolResult, error) {
	switch s.Format {
	case FormatTOON:
		// toon-go reads `toon:"..."` struct tags, not `json:"..."`. We
		// already json-tagged every result type, so the cheapest way to
		// make TOON output use the same field names a JSON consumer sees
		// is to marshal → unmarshal-to-generic → toon-marshal. The extra
		// pass costs microseconds for our payload sizes.
		raw, err := json.Marshal(v)
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("json encode for toon path failed; falling back to JSON", "err", err)
			}
			return jsonResult(v)
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			if s.Log != nil {
				s.Log.Warn("json reparse for toon path failed; falling back to JSON", "err", err)
			}
			return jsonResult(v)
		}
		data, err := toon.MarshalString(generic)
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("toon encode failed; falling back to JSON", "err", err)
			}
			return jsonResult(v)
		}
		return mcp.NewToolResultText(data), nil
	default:
		return jsonResult(v)
	}
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("encode result", err), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// resultJSON is kept as a package-level fallback for handlers that don't have
// State plumbed through. New handlers should call (*State).resultJSON.
func resultJSON(v any) (*mcp.CallToolResult, error) {
	return jsonResult(v)
}

func resultErr(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(err.Error()), nil
}

func bind[T any](req mcp.CallToolRequest, out *T) error {
	return req.BindArguments(out)
}

// clampReadTimeout converts a client-supplied milliseconds value into a
// time.Duration, defending against negative values, huge values that would
// overflow the multiplication, and "wait forever" requests that would tie up
// the MCP transport.
const maxReadTimeout = 5 * 60 * 1000 // 5 minutes

func clampReadTimeout(ms int) int {
	if ms < 0 {
		return 0
	}
	if ms > maxReadTimeout {
		return maxReadTimeout
	}
	return ms
}
