package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
	"github.com/jaenster/remote-shell-mcp/internal/mcptools"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
)

const (
	defaultAddr = "127.0.0.1:7800"
	serverName  = "remote-shell-mcp"
	serverVer   = "0.1.0"
)

func main() {
	addr := flag.String("addr", envOr("REMOTE_SHELL_MCP_ADDR", defaultAddr), "Bind address for the SSE MCP server (host:port).")
	statePath := flag.String("state", envOr("REMOTE_SHELL_MCP_STATE", ""), "Path to state.json (default: $XDG_CONFIG_HOME/remote-shell-mcp/state.json).")
	lockPath := flag.String("lock", envOr("REMOTE_SHELL_MCP_LOCK", ""), "Path to daemon lock file.")
	tokenPath := flag.String("token", envOr("REMOTE_SHELL_MCP_TOKEN", ""), "Path to the auth token file. Daemon writes a fresh random token here on startup.")
	logFmt := flag.String("log", envOr("REMOTE_SHELL_MCP_LOG", "text"), "Log format: text or json.")
	flag.Parse()

	defLock, defState, defToken, err := daemon.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config dir:", err)
		os.Exit(1)
	}
	if *lockPath == "" {
		*lockPath = defLock
	}
	if *statePath == "" {
		*statePath = defState
	}
	if *tokenPath == "" {
		*tokenPath = defToken
	}
	_ = os.MkdirAll(filepath.Dir(*lockPath), 0o700)

	var handler slog.Handler
	if *logFmt == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	lock, err := daemon.AcquireLock(*lockPath)
	if err != nil {
		log.Error("acquire lock", "err", err, "path", *lockPath)
		os.Exit(1)
	}
	defer lock.Release()

	token, err := daemon.GenerateToken()
	if err != nil {
		log.Error("generate token", "err", err)
		os.Exit(1)
	}
	if err := daemon.WriteToken(*tokenPath, token); err != nil {
		log.Error("write token", "err", err, "path", *tokenPath)
		os.Exit(1)
	}
	defer os.Remove(*tokenPath)

	store, err := state.NewStore(*statePath)
	if err != nil {
		log.Error("init state store", "err", err)
		os.Exit(1)
	}
	sshMgr := sshx.NewManager()
	dkMgr := dockerx.NewManager()
	st := &mcptools.State{SSH: sshMgr, Docker: dkMgr, Store: store, Log: log}

	if snap, err := store.Load(); err != nil {
		log.Warn("load state", "err", err)
	} else if snap != nil {
		state.Restore(snap, sshMgr, dkMgr, log)
	}

	// Start the debounced state flusher before serving traffic so the first
	// tool calls don't fall back to synchronous writes. We do NOT defer Stop()
	// here — Stop calls PersistNow as its last act, and we need that to happen
	// BEFORE CloseAll empties the managers (otherwise the final write
	// captures empty state and clobbers everything that was just saved).
	st.Start()

	mcpServer := mcptools.Build(st, serverName, serverVer)
	sseServer := server.NewSSEServer(mcpServer,
		server.WithKeepAlive(true),
		server.WithKeepAliveInterval(15*time.Second),
	)
	authed := daemon.AuthMiddleware(token, sseServer)
	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: authed,
		// Defend against slowloris from a local process holding a connection
		// open without sending headers. SSE handlers explicitly want long-lived
		// connections, so we don't set ReadTimeout / WriteTimeout (which would
		// kill the stream); ReadHeaderTimeout only applies to the request line
		// + headers.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "sse", "/sse", "message", "/message",
			"state", store.Path(), "token", *tokenPath)
		errCh <- httpSrv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Error("server stopped", "err", err)
		}
	case sig := <-sigCh:
		log.Info("signal received", "sig", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sseServer.CloseSessions()
	_ = httpSrv.Shutdown(ctx)

	// Stop the flusher (drains its goroutine and does one final synchronous
	// write of CURRENT state, before we tear sessions down). Order matters:
	// Stop → CloseAll, never the reverse.
	st.Stop()
	sshMgr.CloseAll()
	dkMgr.CloseAll()
	log.Info("shutdown complete")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
