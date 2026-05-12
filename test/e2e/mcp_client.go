//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type MCPClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan rawResponse
	closed   atomic.Bool
	readDone chan struct{}
}

type rawResponse struct {
	raw json.RawMessage
	err error
}

func NewMCPClient(t *testing.T, launcherBin, addr string) *MCPClient {
	t.Helper()
	return NewMCPClientForDaemon(t, launcherBin, &Daemon{addr: addr})
}

func NewMCPClientForDaemon(t *testing.T, launcherBin string, d *Daemon) *MCPClient {
	t.Helper()
	args := []string{"-addr", d.addr, "-no-spawn"}
	if d.tokenPath != "" {
		args = append(args, "-token", d.tokenPath)
	}
	cmd := exec.Command(launcherBin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	return attachMCP(t, cmd, stdin, stdout)
}

func AttachMCPClient(t *testing.T, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader) *MCPClient {
	t.Helper()
	return attachMCP(t, cmd, stdin, stdout)
}

func attachMCP(t *testing.T, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader) *MCPClient {
	c := &MCPClient{
		t:        t,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReaderSize(stdout, 1<<20),
		pending:  map[int64]chan rawResponse{},
		readDone: make(chan struct{}),
	}
	go c.readLoop()
	c.initialize()
	return c
}

func (c *MCPClient) readLoop() {
	defer close(c.readDone)
	for {
		line, err := c.stdout.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			c.dispatch(line)
		}
		if err != nil {
			c.mu.Lock()
			for _, ch := range c.pending {
				ch <- rawResponse{err: err}
			}
			c.pending = nil
			c.mu.Unlock()
			return
		}
	}
}

func (c *MCPClient) dispatch(line string) {
	var env struct {
		ID     *int64           `json:"id"`
		Method *string          `json:"method"`
		Result json.RawMessage  `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return
	}
	if env.ID == nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[*env.ID]
	if ok {
		delete(c.pending, *env.ID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if env.Error != nil {
		ch <- rawResponse{err: fmt.Errorf("rpc error: %s", string(*env.Error))}
		return
	}
	ch <- rawResponse{raw: env.Result}
}

func (c *MCPClient) send(payload any) error {
	if c.closed.Load() {
		return fmt.Errorf("client closed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (c *MCPClient) initialize() {
	c.t.Helper()
	id := c.nextID.Add(1)
	ch := c.expect(id)
	_ = c.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e", "version": "1"},
		},
	})
	select {
	case resp := <-ch:
		if resp.err != nil {
			c.t.Fatalf("initialize: %v", resp.err)
		}
	case <-time.After(10 * time.Second):
		c.t.Fatalf("initialize: timeout")
	}
	_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (c *MCPClient) expect(id int64) chan rawResponse {
	ch := make(chan rawResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *MCPClient) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	c.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	id := c.nextID.Add(1)
	ch := c.expect(id)
	if err := c.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	}); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *MCPClient) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
	raw, err := c.Call(ctx, tool, args)
	if err != nil {
		return "", err
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", nil
	}
	if result.IsError {
		return result.Content[0].Text, fmt.Errorf("tool error: %s", result.Content[0].Text)
	}
	return result.Content[0].Text, nil
}

func (c *MCPClient) CallJSON(ctx context.Context, tool string, args map[string]any, out any) error {
	text, err := c.CallText(ctx, tool, args)
	if err != nil {
		return err
	}
	if text == "" || out == nil {
		return nil
	}
	return json.Unmarshal([]byte(text), out)
}

func (c *MCPClient) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	_ = c.stdin.Close()
	select {
	case <-c.readDone:
	case <-time.After(2 * time.Second):
	}
	_ = c.cmd.Wait()
}
