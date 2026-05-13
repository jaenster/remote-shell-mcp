package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
	"github.com/jaenster/remote-shell-mcp/internal/launcher"
)

// runCLI executes a single MCP tool call against the daemon's /rpc endpoint
// and prints the result. It is the alternative to the stdio proxy: when an
// MCP host (Claude Code, Cursor) isn't in the loop, you can still inspect
// or drive the daemon directly. Returns an exit code.
//
// Arg shapes (httpie-style; the key portion is always a Go-identifier-ish
// name so the operator can never be ambiguous):
//
//	key=value     string
//	key:=value    raw JSON  (numbers, bools, null, objects, arrays)
//	key@=path     string loaded from file
//	key:@=path    JSON loaded from file
//
// First positional is the tool name. `tools` lists tools. `--json` / `--toon`
// switch the on-wire output format the daemon returns. `--help` after a tool
// name prints that tool's input schema.
func runCLI(toolName string, args []string) int {
	var (
		outJSON bool
		outTOON bool
		help    bool
	)
	kvPairs := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "--json":
			outJSON = true
		case "--toon":
			outTOON = true
		case "--help", "-h":
			help = true
		default:
			kvPairs = append(kvPairs, a)
		}
	}
	if outJSON && outTOON {
		fmt.Fprintln(os.Stderr, "cannot use --json and --toon together")
		return 2
	}

	// Resolve daemon addr + token the same way the proxy does.
	addr := envOr("REMOTE_SHELL_MCP_ADDR", "127.0.0.1:7800")
	tokenPath := envOr("REMOTE_SHELL_MCP_TOKEN", "")
	if tokenPath == "" {
		_, _, def, err := daemon.DefaultPaths()
		if err != nil {
			fmt.Fprintln(os.Stderr, "config dir:", err)
			return 1
		}
		tokenPath = def
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := launcher.EnsureDaemon(addr, os.Getenv("REMOTE_SHELL_MCP_DAEMON"), nil); err != nil {
		fmt.Fprintln(os.Stderr, "ensure daemon:", err)
		return 1
	}
	token, err := waitForToken(tokenPath, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read token:", err)
		return 1
	}

	cli := &rpcClient{
		baseURL: "http://" + addr,
		token:   token,
		http:    &http.Client{Timeout: 0},
	}

	// `tools` lists everything we can call.
	if toolName == "tools" {
		return cli.listTools(ctx, outJSON)
	}

	// `<tool> --help` shows that tool's input schema by filtering tools/list.
	if help {
		return cli.toolHelp(ctx, toolName)
	}

	argsObj, err := parseKVArgs(kvPairs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "args:", err)
		return 2
	}

	out := outputAsIs
	if outJSON {
		out = outputJSON
	} else if outTOON {
		out = outputTOON
	}
	return cli.callTool(ctx, toolName, argsObj, out)
}

type rpcClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *rpcClient) do(ctx context.Context, method string, params any) (*rpcResp, error) {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return nil, errors.New("daemon returned 401 (token mismatch)")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out rpcResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode rpc response: %w (body=%s)", err, string(respBody))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	return &out, nil
}

type outputMode int

const (
	outputAsIs outputMode = iota // print whatever the daemon emitted
	outputJSON                   // force JSON, decoding TOON locally if needed
	outputTOON                   // force TOON, decoding JSON locally if needed
)

func (c *rpcClient) callTool(ctx context.Context, name string, args map[string]any, mode outputMode) int {
	resp, err := c.do(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// MCP tool result: { content: [{type:"text", text:"..."}], isError: bool }
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		// Fall back to raw JSON if shape is unexpected.
		fmt.Println(string(resp.Result))
		return 0
	}
	exit := 0
	if result.IsError {
		exit = 1
	}
	for _, c := range result.Content {
		if c.Type != "text" {
			continue
		}
		w := os.Stdout
		if exit != 0 {
			w = os.Stderr
		}
		fmt.Fprintln(w, transcodeOutput(c.Text, mode))
	}
	return exit
}

// transcodeOutput converts the daemon's text payload to the user-requested
// format. The daemon emits whatever its `-format` flag was set to; the CLI
// re-encodes locally so `--json` / `--toon` work regardless of how the daemon
// was started. If decoding fails (the text is neither valid JSON nor valid
// TOON), we pass it through untouched — better an unformatted answer than no
// answer.
func transcodeOutput(text string, mode outputMode) string {
	if mode == outputAsIs {
		return text
	}
	// Try JSON first since it's stricter and faster; fall back to TOON.
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		dv, derr := toon.DecodeString(text)
		if derr != nil {
			return text
		}
		v = dv
	}
	switch mode {
	case outputJSON:
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return text
		}
		return string(buf)
	case outputTOON:
		s, err := toon.MarshalString(v)
		if err != nil {
			return text
		}
		return s
	}
	return text
}

func (c *rpcClient) listTools(ctx context.Context, asJSON bool) int {
	resp, err := c.do(ctx, "tools/list", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if asJSON {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, resp.Result, "", "  ")
		fmt.Println(pretty.String())
		return 0
	}
	var data struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		fmt.Println(string(resp.Result))
		return 0
	}
	for _, t := range data.Tools {
		// Single-line summary: name then the first line of the description.
		desc := strings.TrimSpace(t.Description)
		if idx := strings.IndexByte(desc, '\n'); idx >= 0 {
			desc = desc[:idx]
		}
		fmt.Printf("%-32s %s\n", t.Name, desc)
	}
	return 0
}

func (c *rpcClient) toolHelp(ctx context.Context, name string) int {
	resp, err := c.do(ctx, "tools/list", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var data struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &data); err != nil {
		return 1
	}
	for _, t := range data.Tools {
		if t.Name == name {
			fmt.Println(t.Name)
			fmt.Println()
			fmt.Println(strings.TrimSpace(t.Description))
			fmt.Println()
			fmt.Println("Input schema:")
			var pretty bytes.Buffer
			_ = json.Indent(&pretty, t.InputSchema, "  ", "  ")
			fmt.Println("  " + pretty.String())
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "no tool named %q (try `remote-shell-mcp tools`)\n", name)
	return 1
}

// parseKVArgs turns httpie-style `key=val` / `key:=val` / `key@=path` /
// `key:@=path` into a JSON object suitable for tools/call's arguments field.
func parseKVArgs(args []string) (map[string]any, error) {
	out := make(map[string]any, len(args))
	for _, a := range args {
		k, op, v, ok := splitKVOp(a)
		if !ok {
			return nil, fmt.Errorf("bad arg %q (expected key=value, key:=json, key@=file, or key:@=file)", a)
		}
		switch op {
		case "=":
			out[k] = v
		case ":=":
			var parsed any
			if err := json.Unmarshal([]byte(v), &parsed); err != nil {
				// Allow bare bool/null/number shorthand even without quotes.
				if parsed2, ok := coerceLooseJSON(v); ok {
					out[k] = parsed2
					continue
				}
				return nil, fmt.Errorf("arg %s: not valid JSON: %v", k, err)
			}
			out[k] = parsed
		case "@=":
			data, err := os.ReadFile(v)
			if err != nil {
				return nil, fmt.Errorf("arg %s: %v", k, err)
			}
			out[k] = string(data)
		case ":@=":
			data, err := os.ReadFile(v)
			if err != nil {
				return nil, fmt.Errorf("arg %s: %v", k, err)
			}
			var parsed any
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, fmt.Errorf("arg %s: file is not valid JSON: %v", k, err)
			}
			out[k] = parsed
		}
	}
	return out, nil
}

// splitKVOp finds the longest matching operator in s ("=", ":=", "@=", ":@=")
// and splits around it. The key must be a non-empty Go-identifier-ish token,
// which guarantees the operator chars never appear inside the key.
func splitKVOp(s string) (key, op, val string, ok bool) {
	// Walk forward through the key, stop at first non-identifier char, then
	// match the longest operator that starts there.
	end := 0
	for end < len(s) {
		ch := s[end]
		if ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (end > 0 && ch >= '0' && ch <= '9') {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return "", "", "", false
	}
	key = s[:end]
	rest := s[end:]
	switch {
	case strings.HasPrefix(rest, ":@="):
		return key, ":@=", rest[3:], true
	case strings.HasPrefix(rest, ":="):
		return key, ":=", rest[2:], true
	case strings.HasPrefix(rest, "@="):
		return key, "@=", rest[2:], true
	case strings.HasPrefix(rest, "="):
		return key, "=", rest[1:], true
	}
	return "", "", "", false
}

// coerceLooseJSON accepts true/false/null and bare numbers without quoting so
// `key:=true` works without shell escaping. Anything else falls through to
// strict JSON parsing.
func coerceLooseJSON(v string) (any, bool) {
	switch v {
	case "true":
		return true, true
	case "false":
		return false, true
	case "null":
		return nil, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, true
	}
	return nil, false
}

// printCLIUsage is the top-level usage message shown for `--help`, an unknown
// flag, or no args + a tty (so we don't accidentally spam the stdio proxy
// path with a usage blob).
func printCLIUsage() {
	const u = `Usage: remote-shell-mcp <tool> [key=value ...] [--json|--toon]
       remote-shell-mcp tools                       list available MCP tools
       remote-shell-mcp <tool> --help               show one tool's input schema
       remote-shell-mcp setup [flags]               register with detected MCP clients
       remote-shell-mcp version                     print version

With no arguments and stdin not a tty, run as the MCP stdio proxy
(the default mode for Claude Code / Cursor / Codex CLI).

Tool argument shapes (httpie-style):
  key=value       string
  key:=value      raw JSON: numbers, bools, null, objects, arrays
  key@=path       string loaded from file
  key:@=path      JSON loaded from file

Examples:
  remote-shell-mcp tools
  remote-shell-mcp status
  remote-shell-mcp ssh_list
  remote-shell-mcp ssh_exec session_id=opslagbeest command='uname -a'
  remote-shell-mcp ssh_connect id=opslagbeest 'spec:={"addresses":["opslagbeest"],"auth":{"agent":true}}'
`
	fmt.Print(u)
}
