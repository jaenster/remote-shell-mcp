package mcptools

import (
	"context"
	"runtime"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var startedAt = time.Now()

func RegisterMeta(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("snapshot",
		mcp.WithDescription("Persist current state (sessions/forwards/docker hosts marked persistent) to disk. Normally called automatically on changes."),
	), handleSnapshot(st))

	srv.AddTool(mcp.NewTool("status",
		mcp.WithDescription("Daemon status: counts, uptime, persistence path, goroutines."),
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
		return resultJSON(map[string]any{"saved": true, "path": path})
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
		return resultJSON(map[string]any{
			"uptime":         time.Since(startedAt).String(),
			"ssh_sessions":   len(sshList),
			"docker_hosts":   len(dkList),
			"forwards":       len(fwds),
			"goroutines":     runtime.NumGoroutine(),
			"state_path":     path,
			"sessions":       sshList,
			"hosts":          dkList,
			"forwards_list":  fwds,
		})
	}
}
