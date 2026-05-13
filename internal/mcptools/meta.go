package mcptools

import (
	"context"
	"runtime"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

var startedAt = time.Now()

func RegisterMeta(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("snapshot",
		mcp.WithDescription("Persist current state (sessions/forwards/docker hosts marked persistent) to disk. Normally called automatically on changes."),
	), handleSnapshot(st))

	srv.AddTool(mcp.NewTool("status",
		mcp.WithDescription("Daemon snapshot: uptime, persistence path, runtime stats, and the current ssh sessions / docker hosts / port forwards. Use it to check whether the daemon is alive and what state it's holding before running anything else."),
	), handleStatus(st))
}

func handleSnapshot(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Snapshot must wait for the write to land — callers (and tests) rely
		// on this being a fence, not a debounced hint.
		if err := st.PersistNow(); err != nil {
			return resultErr(err)
		}
		path := ""
		if st.Store != nil {
			path = st.Store.Path()
		}
		return st.resultJSON(map[string]any{"saved": true, "path": path})
	}
}

func handleStatus(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sshList := st.SSH.List()
		dkList := st.Docker.List()
		fwds, _ := st.SSH.ListForwards("")
		path := ""
		if st.Store != nil {
			path = st.Store.Path()
		}
		// Project list contents into row-form so TOON renders compact tables.
		// All three are explicit make-then-append so an empty result lands as
		// `[0]:` (TOON) / `[]` (JSON), never null.
		sessionRows := make([]sshx.SessionRow, 0, len(sshList))
		for _, s := range sshList {
			sessionRows = append(sessionRows, s.Row())
		}
		hostRows := make([]dockerx.HostRow, 0, len(dkList))
		for _, h := range dkList {
			hostRows = append(hostRows, h.Row())
		}
		// Counts (`ssh_sessions`, `docker_hosts`, `forwards: <int>`) are
		// derivable from the list lengths; the TOON tabular header even
		// surfaces them as `[N]{...}`. Returning them again was just noise.
		return st.resultJSON(map[string]any{
			"uptime":     time.Since(startedAt).String(),
			"goroutines": runtime.NumGoroutine(),
			"state_path": path,
			"sessions":   sessionRows,
			"hosts":      hostRows,
			"forwards":   fwds,
		})
	}
}
