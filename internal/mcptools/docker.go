package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
)

func RegisterDocker(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("docker_connect",
		mcp.WithDescription("Connect to a Docker daemon. host accepts unix:// (local socket), tcp:// (with optional TLS), or ssh://user@host[:port][/path/to/docker.sock] for tunneling Docker over SSH. Returns the connection id and state."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Identifier for this docker host connection. Reuse in subsequent docker_* tool calls.")),
		mcp.WithObject("spec", mcp.Required(),
			mcp.Description("Docker connection spec."),
			mcp.Properties(map[string]any{
				"host":            map[string]any{"type": "string", "description": "Daemon URL: unix:///var/run/docker.sock | tcp://1.2.3.4:2376 | ssh://user@host[:port][/path/to/docker.sock]"},
				"tls_cert_path":   map[string]any{"type": "string", "description": "Path to client cert PEM (for tcp:// + TLS). Local to the daemon's filesystem."},
				"tls_key_path":    map[string]any{"type": "string", "description": "Path to client key PEM. Local to the daemon's filesystem."},
				"tls_ca_path":     map[string]any{"type": "string", "description": "Path to CA cert PEM. Local to the daemon's filesystem."},
				"api_version":     map[string]any{"type": "string", "description": "Pin a Docker API version, e.g. \"1.41\". Default: negotiate."},
				"key_path":        map[string]any{"type": "string", "description": "(ssh:// only) Path to SSH private key for tunneling auth."},
				"key_passphrase":  map[string]any{"type": "string", "description": "(ssh:// only) Passphrase for an encrypted key, if any."},
				"use_agent":       map[string]any{"type": "boolean", "description": "(ssh:// only) Authenticate via SSH_AUTH_SOCK."},
				"password":        map[string]any{"type": "string", "description": "(ssh:// only) Password auth."},
				"known_hosts_path": map[string]any{"type": "string", "description": "(ssh:// only) Override ~/.ssh/known_hosts location."},
				"ssh_insecure":    map[string]any{"type": "boolean", "description": "(ssh:// only) Skip SSH host key verification."},
				"persistent":      map[string]any{"type": "boolean", "description": "Survive daemon restart (spec is rehydrated; secrets are scrubbed and re-resolved)."},
			}),
		),
	), handleDockerConnect(st))

	srv.AddTool(mcp.NewTool("docker_disconnect",
		mcp.WithDescription("Disconnect from a Docker host."),
		mcp.WithString("id", mcp.Required()),
	), handleDockerDisconnect(st))

	srv.AddTool(mcp.NewTool("docker_list_hosts",
		mcp.WithDescription("List connected Docker hosts."),
	), handleDockerListHosts(st))

	srv.AddTool(mcp.NewTool("docker_containers",
		mcp.WithDescription("List containers on a Docker host."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithBoolean("all", mcp.Description("Include stopped containers.")),
		mcp.WithNumber("limit", mcp.Description("Max containers to return.")),
		mcp.WithObject("filters", mcp.Description("Filter map e.g. {\"name\": [\"db\"], \"status\": [\"running\"]}.")),
	), handleDockerContainers(st))

	srv.AddTool(mcp.NewTool("docker_container_inspect",
		mcp.WithDescription("Inspect a container."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required()),
	), handleDockerInspect(st))

	srv.AddTool(mcp.NewTool("docker_container_start",
		mcp.WithDescription("Start a stopped container."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
	), handleDockerStart(st))

	srv.AddTool(mcp.NewTool("docker_container_stop",
		mcp.WithDescription("Stop a running container with SIGTERM, then SIGKILL after timeout."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithNumber("timeout_sec", mcp.Description("Seconds to wait for graceful shutdown before SIGKILL. Default 10.")),
	), handleDockerStop(st))

	srv.AddTool(mcp.NewTool("docker_container_restart",
		mcp.WithDescription("Stop then start a container."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithNumber("timeout_sec", mcp.Description("Grace period before SIGKILL on the stop phase. Default 10.")),
	), handleDockerRestart(st))

	srv.AddTool(mcp.NewTool("docker_container_kill",
		mcp.WithDescription("Send a signal to a running container's main process."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithString("signal", mcp.Description("Signal name, e.g. SIGTERM, SIGKILL, SIGHUP. Default SIGKILL.")),
	), handleDockerKill(st))

	srv.AddTool(mcp.NewTool("docker_container_remove",
		mcp.WithDescription("Delete a container. Must be stopped, or pass force=true."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithBoolean("force", mcp.Description("Kill and remove a running container.")),
		mcp.WithBoolean("remove_volumes", mcp.Description("Also remove anonymous volumes attached to the container.")),
	), handleDockerRemove(st))

	srv.AddTool(mcp.NewTool("docker_container_logs",
		mcp.WithDescription("Fetch container logs. Returns a snapshot; this tool does not follow."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithString("tail", mcp.Description("Number of lines from the end, or 'all'. Default 'all'.")),
		mcp.WithString("since", mcp.Description("RFC3339 timestamp or duration like '5m' (only logs after this point).")),
		mcp.WithString("until", mcp.Description("RFC3339 timestamp or duration (only logs before this point).")),
		mcp.WithBoolean("timestamps", mcp.Description("Prefix each line with its UTC timestamp.")),
		mcp.WithBoolean("stdout", mcp.Description("Include stdout (default true).")),
		mcp.WithBoolean("stderr", mcp.Description("Include stderr (default true).")),
	), handleDockerLogs(st))

	srv.AddTool(mcp.NewTool("docker_image_list",
		mcp.WithDescription("List images on a Docker host."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithBoolean("all", mcp.Description("Include intermediate/dangling images.")),
	), handleDockerImageList(st))

	srv.AddTool(mcp.NewTool("docker_image_pull",
		mcp.WithDescription("Pull an image from a registry. Blocks until the pull is complete; the call may take a while for large images."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("image", mcp.Required(), mcp.Description("Image reference, e.g. alpine:3.20, docker.io/library/postgres:16.")),
	), handleDockerImagePull(st))

	srv.AddTool(mcp.NewTool("docker_image_remove",
		mcp.WithDescription("Delete a local image."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("image", mcp.Required(), mcp.Description("Image reference or id.")),
		mcp.WithBoolean("force", mcp.Description("Remove even if the image has containers using it.")),
		mcp.WithBoolean("prune_children", mcp.Description("Also remove dangling parent layers.")),
	), handleDockerImageRemove(st))

	srv.AddTool(mcp.NewTool("docker_run",
		mcp.WithDescription("Create and start a container — the MCP equivalent of `docker run -d`. Returns the new container id. Use docker_container_logs / docker_shell_open to attach. Set pull_if_missing=true to auto-pull the image if it's not local."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithObject("spec", mcp.Required(),
			mcp.Description("Run spec."),
			mcp.Properties(map[string]any{
				"image":           map[string]any{"type": "string", "description": "Image reference, e.g. alpine:3.20. Required."},
				"name":            map[string]any{"type": "string", "description": "Container name. Optional; daemon assigns one if omitted."},
				"cmd":             map[string]any{"type": "array", "description": "Argv to run inside the container. Overrides the image CMD. Each element is one argv entry.", "items": map[string]any{"type": "string"}},
				"entrypoint":      map[string]any{"type": "array", "description": "Override the image ENTRYPOINT.", "items": map[string]any{"type": "string"}},
				"env":             map[string]any{"type": "object", "description": "Environment variables as {KEY: value}."},
				"working_dir":     map[string]any{"type": "string"},
				"user":            map[string]any{"type": "string", "description": "UID, name, or UID:GID."},
				"hostname":        map[string]any{"type": "string"},
				"ports":           map[string]any{"type": "array", "description": "Docker-CLI port forms: \"8080:80\", \"127.0.0.1:8080:80/tcp\", \"53/udp\".", "items": map[string]any{"type": "string"}},
				"volumes":         map[string]any{"type": "array", "description": "Bind mounts: \"/host:/container[:ro]\".", "items": map[string]any{"type": "string"}},
				"restart_policy":  map[string]any{"type": "string", "description": "no | always | unless-stopped | on-failure"},
				"auto_remove":     map[string]any{"type": "boolean", "description": "Equivalent to --rm: remove the container when it exits."},
				"labels":          map[string]any{"type": "object", "description": "Container labels as {key: value}."},
				"network_mode":    map[string]any{"type": "string", "description": "host, none, bridge, container:<id>, etc."},
				"tty":             map[string]any{"type": "boolean", "description": "Allocate a TTY in the container."},
				"pull_if_missing": map[string]any{"type": "boolean", "description": "Pull the image first if it's not already on the daemon."},
			}),
		),
	), handleDockerRun(st))

	srv.AddTool(mcp.NewTool("docker_exec",
		mcp.WithDescription("Run a one-shot command in a container. The cmd is an argv array — each element is ONE argv entry; \"ls -la /tmp\" as a single string will NOT be parsed by a shell. To run shell-style, use [\"sh\", \"-c\", \"ls -la /tmp\"]. For interactive/stateful work use docker_shell_open instead."),
		mcp.WithString("host_id", mcp.Required()),
		mcp.WithString("container", mcp.Required(), mcp.Description("Container id or name.")),
		mcp.WithArray("cmd", mcp.Required(),
			mcp.Description("Argv array, e.g. [\"ls\", \"-la\", \"/tmp\"] or [\"sh\", \"-c\", \"ls -la /tmp\"]."),
			mcp.Items(map[string]any{"type": "string"})),
		mcp.WithString("working_dir", mcp.Description("Working directory inside the container.")),
		mcp.WithString("user", mcp.Description("UID, name, or UID:GID.")),
		mcp.WithObject("env", mcp.Description("Environment variables as {KEY: value}.")),
		mcp.WithString("stdin", mcp.Description("Data piped to the command's stdin.")),
	), handleDockerExec(st))
}

type dockerConnectArgs struct {
	ID   string              `json:"id"`
	Spec dockerx.ConnectSpec `json:"spec"`
}

func handleDockerConnect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args dockerConnectArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Connect(args.ID, args.Spec)
		if err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return resultJSON(h.Info())
	}
}

func handleDockerDisconnect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return resultErr(err)
		}
		if err := st.Docker.Disconnect(id); err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return mcp.NewToolResultText("disconnected " + id), nil
	}
}

func handleDockerListHosts(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return resultJSON(st.Docker.List())
	}
}

type dockerContainersArgs struct {
	HostID  string              `json:"host_id"`
	All     bool                `json:"all,omitempty"`
	Limit   int                 `json:"limit,omitempty"`
	Filters map[string][]string `json:"filters,omitempty"`
}

func handleDockerContainers(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args dockerContainersArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(args.HostID)
		if err != nil {
			return resultErr(err)
		}
		list, err := h.ListContainers(ctx, dockerx.ListContainersOptions{All: args.All, Limit: args.Limit, Filters: args.Filters})
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(list)
	}
}

func handleDockerInspect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		insp, err := h.Inspect(ctx, c)
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(insp)
	}
}

func handleDockerStart(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		if err := h.Start(ctx, c); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("started " + c), nil
	}
}

func handleDockerStop(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		var t *int
		if req.GetInt("timeout_sec", -1) >= 0 {
			v := req.GetInt("timeout_sec", 0)
			t = &v
		}
		if err := h.Stop(ctx, c, t); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("stopped " + c), nil
	}
}

func handleDockerRestart(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		var t *int
		if req.GetInt("timeout_sec", -1) >= 0 {
			v := req.GetInt("timeout_sec", 0)
			t = &v
		}
		if err := h.Restart(ctx, c, t); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("restarted " + c), nil
	}
}

func handleDockerKill(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		signal := req.GetString("signal", "")
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		if err := h.Kill(ctx, c, signal); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("killed " + c), nil
	}
}

func handleDockerRemove(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		force := req.GetBool("force", false)
		removeVolumes := req.GetBool("remove_volumes", false)
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		if err := h.Remove(ctx, c, force, removeVolumes); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("removed " + c), nil
	}
}

func handleDockerLogs(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		c, err := req.RequireString("container")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		opts := dockerx.LogsOptions{
			Tail:       req.GetString("tail", "all"),
			Since:      req.GetString("since", ""),
			Until:      req.GetString("until", ""),
			Timestamps: req.GetBool("timestamps", false),
			Stdout:     req.GetBool("stdout", true),
			Stderr:     req.GetBool("stderr", true),
		}
		logs, err := h.Logs(ctx, c, opts)
		if err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText(logs), nil
	}
}

type dockerExecArgs struct {
	HostID     string            `json:"host_id"`
	Container  string            `json:"container"`
	Cmd        []string          `json:"cmd"`
	WorkingDir string            `json:"working_dir,omitempty"`
	User       string            `json:"user,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Stdin      string            `json:"stdin,omitempty"`
}

func handleDockerImageList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		out, err := h.ListImages(ctx, req.GetBool("all", false))
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(out)
	}
}

func handleDockerImagePull(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		ref, err := req.RequireString("image")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		status, err := h.PullImage(ctx, ref)
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(map[string]any{"image": ref, "status": status})
	}
}

func handleDockerImageRemove(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hid, err := req.RequireString("host_id")
		if err != nil {
			return resultErr(err)
		}
		ref, err := req.RequireString("image")
		if err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(hid)
		if err != nil {
			return resultErr(err)
		}
		force := req.GetBool("force", false)
		prune := req.GetBool("prune_children", false)
		if err := h.RemoveImage(ctx, ref, force, prune); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("removed " + ref), nil
	}
}

type dockerRunArgs struct {
	HostID string             `json:"host_id"`
	Spec   dockerx.RunOptions `json:"spec"`
}

func handleDockerRun(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args dockerRunArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(args.HostID)
		if err != nil {
			return resultErr(err)
		}
		res, err := h.Run(ctx, args.Spec)
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(res)
	}
}

func handleDockerExec(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args dockerExecArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		h, err := st.Docker.Get(args.HostID)
		if err != nil {
			return resultErr(err)
		}
		res, err := h.Exec(ctx, args.Container, dockerx.ExecOptions{
			Cmd: args.Cmd, WorkingDir: args.WorkingDir, User: args.User,
			Env: args.Env, Stdin: args.Stdin,
		})
		if err != nil {
			return resultErr(err)
		}
		return resultJSON(res)
	}
}
