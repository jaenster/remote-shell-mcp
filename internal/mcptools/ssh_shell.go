package mcptools

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func RegisterSSHShell(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_shell_open",
		mcp.WithDescription("Open a persistent PTY shell on an SSH session. State (cd, env, vim) survives between calls until ssh_shell_close."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Parent SSH session id.")),
		mcp.WithString("shell_id", mcp.Required(), mcp.Description("Identifier for this shell.")),
		mcp.WithString("term", mcp.Description("TERM value (default: xterm-256color).")),
		mcp.WithNumber("rows", mcp.Description("PTY rows (default 24).")),
		mcp.WithNumber("cols", mcp.Description("PTY cols (default 80).")),
		mcp.WithString("command", mcp.Description("Command to run instead of an interactive login shell.")),
		mcp.WithObject("env", mcp.Description("Environment variables.")),
	), handleShellOpen(st))

	srv.AddTool(mcp.NewTool("ssh_shell_write",
		mcp.WithDescription("Write data to a shell's stdin. Include trailing \\n to send a line."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithString("data", mcp.Required(), mcp.Description("Bytes to send to the shell stdin.")),
	), handleShellWrite(st))

	srv.AddTool(mcp.NewTool("ssh_shell_read",
		mcp.WithDescription("Read buffered output from a shell. Returns immediately if data is buffered; otherwise waits up to timeout_ms for new output."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("timeout_ms", mcp.Description("How long to wait for new output if buffer is empty. Default 1000.")),
	), handleShellRead(st))

	srv.AddTool(mcp.NewTool("ssh_shell_resize",
		mcp.WithDescription("Resize the shell's PTY."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("rows", mcp.Required()),
		mcp.WithNumber("cols", mcp.Required()),
	), handleShellResize(st))

	srv.AddTool(mcp.NewTool("ssh_shell_close",
		mcp.WithDescription("Close a persistent shell."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
	), handleShellClose(st))

	srv.AddTool(mcp.NewTool("ssh_shell_list",
		mcp.WithDescription("List shells on a session, or across all sessions if session_id is omitted."),
		mcp.WithString("session_id", mcp.Description("Optional session id.")),
	), handleShellList(st))
}

type shellOpenArgs struct {
	SessionID string            `json:"session_id"`
	ShellID   string            `json:"shell_id"`
	Term      string            `json:"term,omitempty"`
	Rows      int               `json:"rows,omitempty"`
	Cols      int               `json:"cols,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func handleShellOpen(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args shellOpenArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(args.SessionID)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.OpenShell(args.ShellID, sshx.ShellOptions{
			Term: args.Term, Rows: args.Rows, Cols: args.Cols,
			Command: args.Command, Env: args.Env,
		})
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(sh.Info())
	}
}

func handleShellWrite(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		data, err := req.RequireString("data")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid)
		if err != nil {
			return resultErr(err)
		}
		n, err := sh.Write([]byte(data))
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes_written": n})
	}
}

func handleShellRead(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		timeoutMs := clampReadTimeout(req.GetInt("timeout_ms", 1000))
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid)
		if err != nil {
			return resultErr(err)
		}
		out, err := sh.Read(time.Duration(timeoutMs) * time.Millisecond)
		if err != nil {
			return st.resultJSON(map[string]any{"data": string(out), "eof": true})
		}
		return st.resultJSON(map[string]any{"data": string(out), "eof": false})
	}
}

func handleShellResize(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		rows, err := req.RequireInt("rows")
		if err != nil {
			return resultErr(err)
		}
		cols, err := req.RequireInt("cols")
		if err != nil {
			return resultErr(err)
		}
		if rows < 1 || rows > 1000 || cols < 1 || cols > 1000 {
			return resultErr(errors.New("rows/cols must be in [1, 1000]"))
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid)
		if err != nil {
			return resultErr(err)
		}
		if err := sh.Resize(rows, cols); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("ok"), nil
	}
}

func handleShellClose(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.CloseShell(shid); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("closed " + shid), nil
	}
}

func handleShellList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid := req.GetString("session_id", "")
		if sid == "" {
			var all []sshx.ShellInfo
			for _, s := range st.SSH.Sessions() {
				all = append(all, s.ListShells()...)
			}
			return st.resultJSON(all)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(s.ListShells())
	}
}
