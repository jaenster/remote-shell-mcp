package main

import (
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
)

func main() {
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

	// Reconnect-on-failure loop: if the daemon's SSE stream dies (daemon was
	// restarted, crashed, etc.), don't exit — the parent MCP client would
	// just re-spawn us in a tight loop. Instead, wait with backoff and
	// re-attach. Only exit on context cancel (signal) or repeated failure.
	backoff := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
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
			Stdin:   os.Stdin,
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
