package mcptools

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
)

func RegisterDockerShell(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("docker_shell_open",
		mcp.WithDescription("Open a persistent interactive shell inside a container (docker exec with a TTY). State (cd, env vars, vim, tail -f) survives between calls until docker_shell_close. cmd is argv-style: [\"/bin/sh\"] not \"/bin/sh -i\"; use [\"sh\", \"-c\", \"…\"] for shell-style invocations."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required(), mcp.Description("Identifier for this shell.")),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithArray("cmd", mcp.Description("Argv array. Default [\"/bin/sh\"]."), mcp.Items(map[string]any{"type": "string"})),
		mcp.WithObject("env", mcp.Description("Environment variables as {KEY: value}.")),
		mcp.WithString("user", mcp.Description("UID, name, or UID:GID.")),
		mcp.WithString("working_dir", mcp.Description("Working directory inside the container.")),
		mcp.WithNumber("rows", mcp.Description("PTY rows. Default 24.")),
		mcp.WithNumber("cols", mcp.Description("PTY cols. Default 80.")),
	), handleDockerShellOpen(st))

	srv.AddTool(mcp.NewTool("docker_shell_write",
		mcp.WithDescription("Write data to a Docker shell's stdin. Include trailing \\n to send a line."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithString("data", mcp.Required(), mcp.Description("Bytes to send to the shell's stdin.")),
	), handleDockerShellWrite(st))

	srv.AddTool(mcp.NewTool("docker_shell_read",
		mcp.WithDescription("Read buffered output from a Docker shell. Returns immediately if data is buffered; otherwise waits up to timeout_ms for new output."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("timeout_ms", mcp.Description("How long to wait for new output if the buffer is empty (default 1000, capped at 300000).")),
	), handleDockerShellRead(st))

	srv.AddTool(mcp.NewTool("docker_shell_resize",
		mcp.WithDescription("Resize the Docker shell's PTY."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("rows", mcp.Required()),
		mcp.WithNumber("cols", mcp.Required()),
	), handleDockerShellResize(st))

	srv.AddTool(mcp.NewTool("docker_shell_close",
		mcp.WithDescription("Close a Docker shell."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
	), handleDockerShellClose(st))

	srv.AddTool(mcp.NewTool("docker_shell_list",
		mcp.WithDescription("List Docker shells on a host, or across all hosts if host_id is omitted."),
		mcp.WithString("host_id", mcp.Description("Optional host id; omit for all hosts.")),
	), handleDockerShellList(st))
}

type dockerShellOpenArgs struct {
	HostID     string            `json:"host_id"`
	ShellID    string            `json:"shell_id"`
	Container  string            `json:"container"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Rows       uint              `json:"rows,omitempty"`
	Cols       uint              `json:"cols,omitempty"`
}

func handleDockerShellOpen(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args dockerShellOpenArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(args.HostID)
		if err != nil {
			return resultErr(err)
		}
		sh, err := h.OpenShell(ctx, args.ShellID, dockerx.ShellOptions{
			Container: args.Container, Cmd: args.Cmd, Env: args.Env, User: args.User,
			WorkingDir: args.WorkingDir, Rows: args.Rows, Cols: args.Cols,
		})
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(sh.Info())
	}
}

func handleDockerShellWrite(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
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
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := h.GetShell(shid)
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

func handleDockerShellRead(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		timeoutMs := clampReadTimeout(req.GetInt("timeout_ms", 1000))
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := h.GetShell(shid)
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

func handleDockerShellResize(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
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
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := h.GetShell(shid)
		if err != nil {
			return resultErr(err)
		}
		if err := sh.Resize(uint(rows), uint(cols)); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("ok"), nil
	}
}

func handleDockerShellClose(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		if err := h.CloseShell(shid); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("closed " + shid), nil
	}
}

func handleDockerShellList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid := req.GetString("host_id", "")
		if hid == "" {
			var all []dockerx.ShellInfo
			for _, h := range st.Docker.Hosts() {
				all = append(all, h.ListShells()...)
			}
			return st.resultJSON(all)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(h.ListShells())
	}
}
