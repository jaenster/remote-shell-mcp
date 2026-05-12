package mcptools

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func RegisterForward(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_forward_local",
		mcp.WithDescription("Open a local port forward (ssh -L). Listens on bind_addr:bind_port locally and tunnels to remote_host:remote_port through the SSH session. bind_port=0 picks a free port."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("forward_id", mcp.Required(), mcp.Description("Identifier for this forward.")),
		mcp.WithString("bind_addr", mcp.Description("Local bind address (default 127.0.0.1).")),
		mcp.WithNumber("bind_port", mcp.Required(), mcp.Description("Local port to listen on (0 = pick free).")),
		mcp.WithString("remote_host", mcp.Required(), mcp.Description("Host to reach on the SSH server's side.")),
		mcp.WithNumber("remote_port", mcp.Required(), mcp.Description("Port on remote_host.")),
	), handleForwardLocal(st))

	srv.AddTool(mcp.NewTool("ssh_forward_remote",
		mcp.WithDescription("Open a remote port forward (ssh -R). Listens on bind_addr:bind_port on the SSH server, tunnels back to local_host:local_port."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("forward_id", mcp.Required()),
		mcp.WithString("bind_addr", mcp.Description("Remote bind address (default 127.0.0.1).")),
		mcp.WithNumber("bind_port", mcp.Required()),
		mcp.WithString("local_host", mcp.Description("Local host the daemon should dial (default 127.0.0.1).")),
		mcp.WithNumber("local_port", mcp.Required()),
	), handleForwardRemote(st))

	srv.AddTool(mcp.NewTool("ssh_forward_dynamic",
		mcp.WithDescription("Open a dynamic SOCKS5 forward (ssh -D). Starts a local SOCKS5 proxy whose CONNECTs go through the SSH session."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("forward_id", mcp.Required()),
		mcp.WithString("bind_addr", mcp.Description("Local bind address (default 127.0.0.1).")),
		mcp.WithNumber("bind_port", mcp.Required()),
	), handleForwardDynamic(st))

	srv.AddTool(mcp.NewTool("ssh_forwards_list",
		mcp.WithDescription("List forwards on a session, or across all sessions if session_id is omitted."),
		mcp.WithString("session_id", mcp.Description("Optional session id.")),
	), handleForwardList(st))

	srv.AddTool(mcp.NewTool("ssh_forward_cancel",
		mcp.WithDescription("Close a single forward."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("forward_id", mcp.Required()),
	), handleForwardCancel(st))
}

func handleForwardLocal(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		fid, err := req.RequireString("forward_id")
		if err != nil {
			return resultErr(err)
		}
		bindPort, err := req.RequireInt("bind_port")
		if err != nil {
			return resultErr(err)
		}
		remoteHost, err := req.RequireString("remote_host")
		if err != nil {
			return resultErr(err)
		}
		remotePort, err := req.RequireInt("remote_port")
		if err != nil {
			return resultErr(err)
		}
		if bindPort < 0 || bindPort > 65535 || remotePort < 1 || remotePort > 65535 {
			return resultErr(errors.New("ports must be in [1, 65535] (bind_port also accepts 0 = pick free)"))
		}
		spec := sshx.LocalSpec{
			BindAddr:   req.GetString("bind_addr", ""),
			BindPort:   bindPort,
			RemoteHost: remoteHost,
			RemotePort: remotePort,
		}
		f, err := st.SSH.OpenLocalForward(sid, fid, spec)
		if err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return st.resultJSON(f.Info())
	}
}

func handleForwardRemote(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		fid, err := req.RequireString("forward_id")
		if err != nil {
			return resultErr(err)
		}
		bindPort, err := req.RequireInt("bind_port")
		if err != nil {
			return resultErr(err)
		}
		localPort, err := req.RequireInt("local_port")
		if err != nil {
			return resultErr(err)
		}
		if bindPort < 0 || bindPort > 65535 || localPort < 1 || localPort > 65535 {
			return resultErr(errors.New("ports must be in [1, 65535] (bind_port also accepts 0 = pick free)"))
		}
		spec := sshx.RemoteSpec{
			BindAddr:  req.GetString("bind_addr", ""),
			BindPort:  bindPort,
			LocalHost: req.GetString("local_host", ""),
			LocalPort: localPort,
		}
		f, err := st.SSH.OpenRemoteForward(sid, fid, spec)
		if err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return st.resultJSON(f.Info())
	}
}

func handleForwardDynamic(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		fid, err := req.RequireString("forward_id")
		if err != nil {
			return resultErr(err)
		}
		bindPort, err := req.RequireInt("bind_port")
		if err != nil {
			return resultErr(err)
		}
		if bindPort < 0 || bindPort > 65535 {
			return resultErr(errors.New("bind_port must be in [0, 65535]"))
		}
		spec := sshx.DynamicSpec{
			BindAddr: req.GetString("bind_addr", ""),
			BindPort: bindPort,
		}
		f, err := st.SSH.OpenDynamicForward(sid, fid, spec)
		if err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return st.resultJSON(f.Info())
	}
}

func handleForwardList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid := req.GetString("session_id", "")
		list, err := st.SSH.ListForwards(sid)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(list)
	}
}

func handleForwardCancel(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		fid, err := req.RequireString("forward_id")
		if err != nil {
			return resultErr(err)
		}
		if err := st.SSH.CloseForward(sid, fid); err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return mcp.NewToolResultText("cancelled " + fid), nil
	}
}
