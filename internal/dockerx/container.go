package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type ContainerSummary struct {
	ID      string            `json:"id"`
	Names   []string          `json:"names"`
	Image   string            `json:"image"`
	Command string            `json:"command"`
	State   string            `json:"state"`
	Status  string            `json:"status"`
	Created time.Time         `json:"created"`
	Ports   []ContainerPort   `json:"ports,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// ContainerRow is the primitive-only projection used by docker_containers so
// the tool's response stays in TOON's compact tabular form. The trade-off:
// ports collapse to a single comma-joined string and labels are dropped (use
// docker_container_inspect for full detail). No `omitempty` here — TOON's
// tabular form requires every row to have the same field set, so we keep
// empty strings instead of omitting fields.
type ContainerRow struct {
	ID     string `json:"id"`     // shortened to 12 hex chars
	Name   string `json:"name"`   // first name, leading slash stripped
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
	Ports  string `json:"ports"` // e.g. "0.0.0.0:80->80/tcp,0.0.0.0:443->443/tcp"
}

func (c ContainerSummary) Row() ContainerRow {
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	ports := make([]string, 0, len(c.Ports))
	for _, p := range c.Ports {
		var s string
		if p.PublicPort != 0 {
			if p.IP != "" {
				s = fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type)
			} else {
				s = fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, p.Type)
			}
		} else {
			s = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		ports = append(ports, s)
	}
	return ContainerRow{
		ID: id, Name: name, Image: c.Image, State: c.State, Status: c.Status,
		Ports: strings.Join(ports, ","),
	}
}

type ContainerPort struct {
	IP          string `json:"ip,omitempty"`
	PrivatePort uint16 `json:"private_port"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	Type        string `json:"type"`
}

type ListContainersOptions struct {
	All     bool
	Limit   int
	Filters map[string][]string
}

func (h *Host) ListContainers(ctx context.Context, opts ListContainersOptions) ([]ContainerSummary, error) {
	c, err := h.client()
	if err != nil {
		return nil, err
	}
	var filters client.Filters
	if len(opts.Filters) > 0 {
		filters = client.Filters{}
		for k, vs := range opts.Filters {
			filters = filters.Add(k, vs...)
		}
	}
	res, err := c.ContainerList(ctx, client.ContainerListOptions{
		All:     opts.All,
		Limit:   opts.Limit,
		Filters: filters,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, summaryToOurs(it))
	}
	return out, nil
}

func summaryToOurs(s container.Summary) ContainerSummary {
	ports := make([]ContainerPort, 0, len(s.Ports))
	for _, p := range s.Ports {
		cp := ContainerPort{PrivatePort: p.PrivatePort, PublicPort: p.PublicPort, Type: string(p.Type)}
		if p.IP.IsValid() {
			cp.IP = p.IP.String()
		}
		ports = append(ports, cp)
	}
	return ContainerSummary{
		ID:      s.ID,
		Names:   s.Names,
		Image:   s.Image,
		Command: s.Command,
		State:   string(s.State),
		Status:  s.Status,
		Created: time.Unix(s.Created, 0),
		Ports:   ports,
		Labels:  s.Labels,
	}
}

func (h *Host) Inspect(ctx context.Context, containerID string) (container.InspectResponse, error) {
	c, err := h.client()
	if err != nil {
		return container.InspectResponse{}, err
	}
	res, err := c.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return res.Container, nil
}

func (h *Host) Start(ctx context.Context, id string) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	_, err = c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (h *Host) Stop(ctx context.Context, id string, timeoutSec *int) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	_, err = c.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: timeoutSec})
	return err
}

func (h *Host) Restart(ctx context.Context, id string, timeoutSec *int) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	_, err = c.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: timeoutSec})
	return err
}

func (h *Host) Kill(ctx context.Context, id, signal string) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	_, err = c.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: signal})
	return err
}

func (h *Host) Remove(ctx context.Context, id string, force, removeVolumes bool) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	_, err = c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: force, RemoveVolumes: removeVolumes})
	return err
}

type LogsOptions struct {
	Tail       string
	Since      string
	Until      string
	Timestamps bool
	Stdout     bool
	Stderr     bool
}

func (h *Host) Logs(ctx context.Context, id string, opts LogsOptions) (string, error) {
	c, err := h.client()
	if err != nil {
		return "", err
	}
	if !opts.Stdout && !opts.Stderr {
		opts.Stdout = true
		opts.Stderr = true
	}
	rc, err := c.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: opts.Stdout,
		ShowStderr: opts.Stderr,
		Tail:       opts.Tail,
		Since:      opts.Since,
		Until:      opts.Until,
		Timestamps: opts.Timestamps,
		Follow:     false,
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()

	insp, err := c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	tty := insp.Container.Config != nil && insp.Container.Config.Tty

	var out bytes.Buffer
	if tty {
		if _, err := io.Copy(&out, rc); err != nil {
			return "", err
		}
		return out.String(), nil
	}
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &stderr, rc); err != nil {
		return "", err
	}
	if stderr.Len() > 0 {
		return out.String() + "\n--- STDERR ---\n" + stderr.String(), nil
	}
	return out.String(), nil
}

type ExecOptions struct {
	Cmd        []string
	WorkingDir string
	Env        map[string]string
	User       string
	Stdin      string
}

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (h *Host) Exec(ctx context.Context, containerID string, opts ExecOptions) (*ExecResult, error) {
	c, err := h.client()
	if err != nil {
		return nil, err
	}
	if len(opts.Cmd) == 0 {
		return nil, fmt.Errorf("cmd is required")
	}
	env := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}
	create, err := c.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          opts.Cmd,
		Env:          env,
		User:         opts.User,
		WorkingDir:   opts.WorkingDir,
		AttachStdin:  opts.Stdin != "",
		AttachStdout: true,
		AttachStderr: true,
		TTY:          false,
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	attach, err := c.ExecAttach(ctx, create.ID, client.ExecAttachOptions{TTY: false})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	if opts.Stdin != "" {
		go func() {
			_, _ = io.WriteString(attach.Conn, opts.Stdin)
			_ = attach.CloseWrite()
		}()
	}

	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader)
		copyDone <- err
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-copyDone:
		if err != nil && err != io.EOF {
			return nil, err
		}
	}
	insp, err := c.ExecInspect(ctx, create.ID, client.ExecInspectOptions{})
	if err != nil {
		return nil, err
	}
	return &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: insp.ExitCode}, nil
}
