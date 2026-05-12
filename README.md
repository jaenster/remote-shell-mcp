# remote-shell-mcp

**Persistent SSH and Docker for your LLM, over MCP. Open the connection once; sessions, port forwards, and shells survive across client restarts.**

`remote-shell-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server that gives any MCP client (Claude Code, Claude Desktop, Cursor, …) a real toolbox for working on remote machines and containers — `ssh_exec`, persistent PTY shells with state preserved between calls, `-L`/`-R`/`-D` port forwards, SFTP round-trips, Docker over `unix://`, `tcp://`, or `ssh://`, container lifecycle, image pulls, and `docker_run`.

It runs as a daemon. Your MCP client talks to a tiny stdio launcher that auto-spawns the daemon on first use and proxies over SSE. The daemon outlives every client restart, every Claude Code reload, every "did the bridge just hang up?" — so the `vim` you opened over a PTY, the SOCKS proxy you set up to reach a database, the keepalive on a flaky link, all keep running.

```
┌──────────────┐   stdio    ┌──────────────────────┐    SSE/HTTP    ┌────────────────────────┐
│ Claude Code  │ ◀────────▶ │ remote-shell-mcp     │ ◀───────────▶ │ remote-shell-mcpd      │
│ (or Cursor,  │   JSON-RPC │ stdio launcher       │  Bearer token │ daemon: SSH sessions,  │
│ Desktop, …)  │            │ auto-spawns + retries │                │ port forwards, Docker  │
└──────────────┘            └──────────────────────┘                └────────────────────────┘
```

## Features

|-|-|
| **SSH** | named long-lived sessions, multi-address try-in-order, ProxyJump-style jump hosts, `ssh-agent`/key/password auth, keepalive + auto-reconnect with backoff, persistent across daemon restart, `ssh_clone` |
| **Persistent shells** | full PTY, `cd` + env + `vim` survive between MCP calls, write/read/resize/close, parallel-safe writes |
| **Port forwards** | `-L`, `-R`, `-D` (SOCKS5); local forwards auto-rebound after reconnect; 30s per-conn dial timeout |
| **SFTP** | read / write / list / stat / mkdir / chmod (octal string) / rename / delete / upload / download, 64 MiB read cap |
| **Docker** | `unix://`, `tcp://` (TLS), or `ssh://user@host[:port][/path/to/docker.sock]`; multiple hosts per daemon |
| **Containers** | list / inspect / start / stop / restart / kill / remove / logs, `docker_exec`, `docker_run` with image/cmd/env/ports/volumes/labels and optional auto-pull |
| **Container shells** | persistent TTY shells inside containers, same model as SSH shells |
| **Images** | `docker_image_list`, `docker_image_pull` (blocks until done), `docker_image_remove` |
| **Persistence** | session and forward specs (no secrets) saved to `$XDG_CONFIG_HOME/remote-shell-mcp/state.json`; rehydrated on daemon startup |
| **Auth** | 32-byte random Bearer token on the SSE endpoint, rotated each daemon restart, stored 0600 in the same config dir |
| **Bridge** | launcher reconnects with exponential backoff if the daemon flaps; survives token rotation; parallel POST dispatch (up to 128 in flight) |

## Quick start

```
# 1. Build both binaries
go build -o bin/remote-shell-mcpd ./cmd/remote-shell-mcpd
go build -o bin/remote-shell-mcp  ./cmd/remote-shell-mcp

# 2. Put them on PATH (or keep side-by-side; the launcher locates the daemon either way)
cp bin/remote-shell-mcp{,d} ~/.local/bin/

# 3. Add it to your MCP client. Claude Code:
claude mcp add remote-shell ~/.local/bin/remote-shell-mcp
```

Same shape for other clients:

```json
{
  "mcpServers": {
    "remote-shell": { "command": "/absolute/path/to/remote-shell-mcp" }
  }
}
```

The launcher takes no required flags. The first MCP call auto-spawns the daemon detached.

## Why a daemon?

A stdio MCP server lives and dies with each client connection. That's fine for stateless tools but ruinous for SSH: every Claude Code reload, every `ctrl-C`, every transient shutdown, drops every session, every tunnel, every PTY. With a long-running daemon:

- An `ssh_shell` you `cd /var/log`'d into is still there when the client reconnects.
- A `-L 5432:db:5432` tunnel into a remote database stays bound for the day.
- An `auto_reconnect: true` session that lost its TCP transport at 2am is back by 2:05am.
- A `persistent: true` session written to disk is back after `kill -9 daemon`.

## Configuration

The launcher takes no required flags. Environment overrides (also accepted as `-flags` on either binary):

|-|-|
| `REMOTE_SHELL_MCP_ADDR` | daemon bind address (default `127.0.0.1:7800`) |
| `REMOTE_SHELL_MCP_DAEMON` | path to the daemon binary (launcher) |
| `REMOTE_SHELL_MCP_STATE` | state file path (daemon-side) |
| `REMOTE_SHELL_MCP_LOCK` | lock file path (daemon-side) |
| `REMOTE_SHELL_MCP_TOKEN` | auth token file path (both sides) |

Defaults live in `$XDG_CONFIG_HOME/remote-shell-mcp/` (`~/.config/remote-shell-mcp/` on Linux, `~/Library/Application Support/remote-shell-mcp/` on macOS).

## Auth

The daemon generates a fresh 32-byte random token on startup and writes it to `daemon.token` (mode 0600). Every request to the SSE endpoint requires `Authorization: Bearer <token>` (RFC 7235 case-insensitive scheme; constant-time compare). Unauthenticated requests get `401 Unauthorized`. The launcher reads the token before connecting and re-reads on every reconnect, so a daemon restart that rotates the token doesn't break the bridge.

A local non-root attacker on the same host can't drive your SSH/Docker sessions just by hitting `127.0.0.1:7800` — they'd also need read access to the token file in your home directory.

## End-to-end tests

Requires Docker. First run builds a small `alpine + openssh + busybox-httpd` test image (~20 MB).

```
go test -race -tags e2e -count=1 ./test/e2e/
```

The suite (30+ tests, race-clean) exercises:

- Auth: password, key file, ssh-agent, daemon bearer token (incl. case-insensitive scheme)
- SSH: exec, persistent PTY shell with state preservation, jump hosts, multi-address fallback, clone, auto-reconnect after sshd kill, persistence across full daemon restart
- Forwards: `-L` via real `http.Get`, `-R` with the container `curl`ing back to a Go server in the test process, `-D` SOCKS5 via `golang.org/x/net/proxy`
- SFTP: mkdir / chmod / stat / rename / upload / download / delete; rejects `data` + `data_base64` set together
- Docker: unix socket and `ssh://` schemes, list/disconnect, container lifecycle, persistent in-container shell, image pull + run + logs + remove
- Concurrency: 8 parallel writers to the same shell, 6 simultaneous reconnects, 10 parallel sessions, 6 truly-parallel MCP clients (separate launchers), 12 overlapping connect/disconnect pairs
- Bridge: launcher auto-spawn, daemon survives launcher restart, 2 MiB MCP response round-trip
- Auth: 401 path with multiple `Bearer` casings, 0600 token file mode

## Project layout

```
cmd/
  remote-shell-mcpd/   daemon entry point
  remote-shell-mcp/    stdio launcher entry point
internal/
  sshx/                SSH manager, dial chain, forwards, PTY shells, SFTP, keepalive
  dockerx/             Docker manager (unix/tcp/ssh hosts), container/image ops, shells, run
  mcptools/            MCP tool registrations, debounced state flusher
  state/               on-disk snapshot/restore
  daemon/              pidfile lock, auth token, default paths
  launcher/            stdio↔SSE proxy with parallel POST + reconnect backoff
test/e2e/              end-to-end tests + sshd container Dockerfile
```

## License

MIT. See `LICENSE`.

## Status

Built iteratively with [Claude Code](https://claude.com/claude-code) across ~11 rounds of write/test/audit. Each audit round was an independent agent looking for bugs; every round through 11 surfaced real issues that the previous round missed, and all P0/P1 findings are tracked and fixed in-tree.
