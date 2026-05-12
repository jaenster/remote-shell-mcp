package dockerx

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type RunOptions struct {
	Image         string            `json:"image"`
	Name          string            `json:"name,omitempty"`
	Cmd           []string          `json:"cmd,omitempty"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	WorkingDir    string            `json:"working_dir,omitempty"`
	User          string            `json:"user,omitempty"`
	Hostname      string            `json:"hostname,omitempty"`
	Ports         []string          `json:"ports,omitempty"`   // Docker-CLI form: "8080:80", "127.0.0.1:8080:80/tcp"
	Volumes       []string          `json:"volumes,omitempty"` // "/host:/container[:ro]"
	RestartPolicy string            `json:"restart_policy,omitempty"`
	AutoRemove    bool              `json:"auto_remove,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	NetworkMode   string            `json:"network_mode,omitempty"`
	Tty           bool              `json:"tty,omitempty"`
	Detach        bool              `json:"detach,omitempty"` // currently always true; reserved
	PullIfMissing bool              `json:"pull_if_missing,omitempty"`
}

type RunResult struct {
	ID       string   `json:"id"`
	Warnings []string `json:"warnings,omitempty"`
}

// Run creates and starts a container from scratch — the closest thing the MCP
// surface has to `docker run`. Unlike the CLI it does NOT attach by default;
// the caller can follow up with docker_container_logs or docker_shell_open.
func (h *Host) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	c, err := h.client()
	if err != nil {
		return nil, err
	}
	if opts.Image == "" {
		return nil, fmt.Errorf("image is required")
	}

	if opts.PullIfMissing {
		// Probe via ImageList filter; if the image isn't local, pull it.
		filters := client.Filters{}.Add("reference", opts.Image)
		res, lerr := c.ImageList(ctx, client.ImageListOptions{Filters: filters})
		if lerr != nil || len(res.Items) == 0 {
			if _, err := h.PullImage(ctx, opts.Image); err != nil {
				return nil, fmt.Errorf("pull %s: %w", opts.Image, err)
			}
		}
	}

	env := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	exposed := network.PortSet{}
	bindings := network.PortMap{}
	for _, spec := range opts.Ports {
		port, binding, err := parsePortSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("parse port %q: %w", spec, err)
		}
		exposed[port] = struct{}{}
		if binding != nil {
			bindings[port] = append(bindings[port], *binding)
		}
	}

	cfg := &container.Config{
		Image:        opts.Image,
		Cmd:          opts.Cmd,
		Entrypoint:   opts.Entrypoint,
		Env:          env,
		WorkingDir:   opts.WorkingDir,
		User:         opts.User,
		Hostname:     opts.Hostname,
		Labels:       opts.Labels,
		Tty:          opts.Tty,
		ExposedPorts: exposed,
	}

	host := &container.HostConfig{
		PortBindings: bindings,
		Binds:        opts.Volumes,
		AutoRemove:   opts.AutoRemove,
		NetworkMode:  container.NetworkMode(opts.NetworkMode),
	}
	if opts.RestartPolicy != "" {
		host.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyMode(opts.RestartPolicy)}
	}

	create, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: host,
		Name:       opts.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	if _, err := c.ContainerStart(ctx, create.ID, client.ContainerStartOptions{}); err != nil {
		// Best-effort cleanup so a failed start doesn't leave a stopped husk.
		_, _ = c.ContainerRemove(ctx, create.ID, client.ContainerRemoveOptions{Force: true})
		return nil, fmt.Errorf("start: %w", err)
	}
	return &RunResult{ID: create.ID, Warnings: create.Warnings}, nil
}

// parsePortSpec accepts Docker-CLI port forms:
//
//	"80"                         → expose 80/tcp, no host binding
//	"80/udp"                     → expose 80/udp, no host binding
//	"8080:80"                    → bind host 8080 → container 80/tcp
//	"127.0.0.1:8080:80/tcp"      → bind host 127.0.0.1:8080 → container 80/tcp
//
// Returns the container-side Port plus an optional host binding.
func parsePortSpec(spec string) (network.Port, *network.PortBinding, error) {
	proto := "tcp"
	if idx := strings.LastIndex(spec, "/"); idx >= 0 {
		proto = strings.ToLower(spec[idx+1:])
		spec = spec[:idx]
	}
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return network.Port{}, nil, fmt.Errorf("unknown protocol %q", proto)
	}

	parts := strings.Split(spec, ":")
	var hostIP, hostPort, cport string
	switch len(parts) {
	case 1:
		cport = parts[0]
	case 2:
		hostPort, cport = parts[0], parts[1]
	case 3:
		hostIP, hostPort, cport = parts[0], parts[1], parts[2]
	default:
		return network.Port{}, nil, fmt.Errorf("too many `:` separators")
	}
	cn, err := strconv.ParseUint(cport, 10, 16)
	if err != nil || cn == 0 {
		return network.Port{}, nil, fmt.Errorf("invalid container port %q", cport)
	}
	port, ok := network.PortFrom(uint16(cn), network.IPProtocol(proto))
	if !ok {
		return network.Port{}, nil, fmt.Errorf("invalid container port spec")
	}
	if hostPort == "" {
		return port, nil, nil
	}
	// Allow a numeric host port; ranges and "0" (random) are passed through as-is.
	if _, err := strconv.ParseUint(strings.Split(hostPort, "-")[0], 10, 16); err != nil {
		return network.Port{}, nil, fmt.Errorf("invalid host port %q", hostPort)
	}
	binding := &network.PortBinding{HostPort: hostPort}
	if hostIP != "" {
		ip, err := netip.ParseAddr(hostIP)
		if err != nil {
			return network.Port{}, nil, fmt.Errorf("invalid host ip %q: %w", hostIP, err)
		}
		binding.HostIP = ip
	}
	return port, binding, nil
}
