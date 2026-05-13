package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
	"github.com/jaenster/remote-shell-mcp/internal/launcher"
	"github.com/jaenster/remote-shell-mcp/internal/setup"
)

func main() {
	// Subcommand dispatch. The hierarchy:
	//   - `setup` / `version`: built-in subcommands.
	//   - First arg is `--help` / `-h`: print CLI usage and exit.
	//   - First arg starts with a letter (i.e., a positional, not a `-flag`):
	//     treat it as an MCP tool name and run the one-shot CLI against the
	//     daemon's /rpc endpoint.
	//   - Anything else (no args, or `-flag` style options): stdio proxy mode,
	//     so MCP hosts that exec us with no args get the JSON-RPC stream.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup(os.Args[2:])
			return
		case "version":
			fmt.Println("remote-shell-mcp", version)
			return
		case "--help", "-h", "help":
			printCLIUsage()
			return
		}
		if first := os.Args[1]; len(first) > 0 && first[0] != '-' {
			os.Exit(runCLI(first, os.Args[2:]))
		}
	}

	addr := flag.String("addr", envOr("REMOTE_SHELL_MCP_ADDR", "127.0.0.1:7800"), "Daemon SSE address (host:port).")
	daemonBin := flag.String("daemon-binary", envOr("REMOTE_SHELL_MCP_DAEMON", ""), "Path to the remote-shell-mcpd binary. If empty, looks on PATH and next to this binary.")
	tokenPath := flag.String("token", envOr("REMOTE_SHELL_MCP_TOKEN", ""), "Path to the daemon's auth token file. Defaults to $XDG_CONFIG_HOME/remote-shell-mcp/daemon.token.")
	noSpawn := flag.Bool("no-spawn", false, "Do not start the daemon; fail if it is not already running.")
	flag.Parse()

	if *tokenPath == "" {
		_, _, def, err := daemon.DefaultPaths()
		if err != nil {
			fmt.Fprintln(os.Stderr, "config dir:", err)
			os.Exit(1)
		}
		*tokenPath = def
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Hoist stdin reading into a SINGLE long-lived goroutine. Without this
	// every Proxy.Run() spawned a fresh stdin reader; after a daemon flap
	// + reconnect, two goroutines would race for bytes from os.Stdin and
	// half the parent's writes would be eaten by the dead reader.
	linesCh := make(chan []byte, 32)
	go func() {
		defer close(linesCh)
		r := bufio.NewReaderSize(os.Stdin, 64*1024)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				buf := append([]byte(nil), line...)
				select {
				case linesCh <- buf:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return // EOF or read error → channel closes via defer
			}
		}
	}()

	// Reconnect-on-failure loop: if the daemon's SSE stream dies (daemon was
	// restarted, crashed, etc.), don't exit — the parent MCP client would
	// just re-spawn us in a tight loop. Instead, wait with backoff and
	// re-attach. Only exit on context cancel (signal) or repeated failure.
	// Tight initial reconnect window so a daemon restart isn't visible to
	// the parent MCP client as a multi-second stall; ramp if the daemon
	// stays unreachable.
	backoff := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 5 * time.Second}
	attempt := 0
	// If a Run() call survives this long, treat the bridge as healthy and
	// reset the backoff index. Without this, after a few flaps every
	// subsequent reconnect always waits the max delay even if the daemon
	// became healthy in between.
	const stableThreshold = 30 * time.Second
	for ctx.Err() == nil {
		if !*noSpawn {
			if err := launcher.EnsureDaemon(*addr, *daemonBin, nil); err != nil {
				fmt.Fprintln(os.Stderr, "ensure daemon:", err)
				sleepWithCtx(ctx, backoff[min(attempt, len(backoff)-1)])
				attempt++
				continue
			}
		}
		tok, err := waitForToken(*tokenPath, 5*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read token:", err)
			sleepWithCtx(ctx, backoff[min(attempt, len(backoff)-1)])
			attempt++
			continue
		}

		p := &launcher.Proxy{
			BaseURL: "http://" + *addr,
			Token:   tok,
			Lines:   linesCh,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
		}
		started := time.Now()
		err = p.Run(ctx)
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, errStdinClosed) {
			return
		}
		if time.Since(started) > stableThreshold {
			attempt = 0
		}
		fmt.Fprintln(os.Stderr, "proxy error (will retry):", err)
		sleepWithCtx(ctx, backoff[min(attempt, len(backoff)-1)])
		attempt++
	}
}

// errStdinClosed is a sentinel that Proxy.Run currently doesn't return; it's
// declared so future versions of the proxy can distinguish a stdin EOF (parent
// closed us) from an SSE EOF (daemon went away). Today, stdin EOF is reported
// as a normal nil exit, so we'll just observe nil and return cleanly.
var errStdinClosed = errors.New("stdin closed")

func sleepWithCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func waitForToken(path string, total time.Duration) (string, error) {
	deadline := time.Now().Add(total)
	for {
		if tok, err := daemon.ReadToken(path); err == nil && tok != "" {
			return tok, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("token file %s never appeared", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// version is overridden via -ldflags at release-time; ldflags target is
// `main.version`.
var version = "dev"

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	name := fs.String("name", "remote-shell", "MCP server name to register.")
	binary := fs.String("binary", "", "Path to write into the MCP config's `command` field (default: this binary).")
	only := fs.String("client", "", "Only install into clients whose name contains this substring (e.g. \"claude\" or \"codex\"). Default: ask about each detected client.")
	yes := fs.Bool("yes", false, "Don't prompt — install into every detected client.")
	dryRun := fs.Bool("dry-run", false, "Print what would be written without touching any files.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: remote-shell-mcp setup [flags]")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Detects supported MCP clients (Claude Code, Claude Desktop, Codex CLI)")
		fmt.Fprintln(fs.Output(), "and registers this binary as an MCP server in each one's config.")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	opts := setup.Options{
		ServerName: *name,
		SelfBinary: *binary,
		OnlyClient: *only,
		Yes:        *yes,
		DryRun:     *dryRun,
	}
	results, err := setup.Run(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	var failed int
	for _, r := range results {
		if r.Err != nil {
			failed++
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}
