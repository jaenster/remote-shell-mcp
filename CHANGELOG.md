# Changelog

All notable changes to remote-shell-mcp. Versions follow [Semantic Versioning](https://semver.org/).

## v0.1.6 — 2026-05-13

- **Windows is a first-class target now.** Daemon lock split into `lock_unix.go` (flock) and `lock_windows.go` (`LockFileEx` with `LOCKFILE_FAIL_IMMEDIATELY` — the moral equivalent of `flock(LOCK_EX|LOCK_NB)`). Same "already-running → error" semantics either way. Also fixed launcher's same-directory probe for the daemon binary to honor `.exe` on Windows (`exec.LookPath` already does this via PATHEXT, but a manual `os.Stat` does not).
- **Release matrix expanded** to: darwin amd64 / arm64 / universal, linux amd64 / arm64 / armv7 / riscv64, windows amd64 / arm64, freebsd amd64 / arm64. Eleven archives per tag. `linux/armv7` covers Raspberry Pi 2/3/Zero 2; `darwin_universal` is a fat Mach-O so one tarball runs on both Intel and Apple Silicon Macs.
- **`install.ps1`** mirrors `install.sh` for Windows: zip download with SHA-256 check, drops binaries into `%LOCALAPPDATA%\Programs\remote-shell-mcp`, adds them to the user PATH, runs `setup`. One-liner: `irm https://raw.githubusercontent.com/jaenster/remote-shell-mcp/main/install.ps1 | iex`.
- **`POST /rpc` endpoint on the daemon** — single JSON-RPC request → single JSON-RPC response, no SSE bookkeeping. Same bearer auth as `/sse`/`/message`. Useful when an MCP host is wedged and you need to talk to the daemon directly.
- **CLI mode on the launcher binary.** Run `remote-shell-mcp <tool> [key=value ...]` to invoke any MCP tool from the shell. Args use httpie typing: `key=string`, `key:=raw-json`, `key@=file-as-string`, `key:@=file-as-json`. `tools` lists them, `<tool> --help` prints the input schema. Output is the daemon's default format; `--json`/`--toon` transcode locally so piping to `jq` always works. Stdio-proxy mode (no args) is unchanged.
- **`install-smoke` CI workflow.** Builds the binaries on ubuntu / macos / windows, runs `setup --yes`, then asserts `claude mcp list` actually sees `remote-shell` — and hits the new `/rpc` surface to close the loop.

## v0.1.5 — 2026-05-13

- **TOON list output is now compact-tabular.** `ssh_list`, `status.sessions`, `status.hosts`, `docker_list_hosts`, `docker_containers`, and `docker_image_list` project into primitive-only "row" types so TOON renders them as `[N]{fields}: row,row,...` instead of expanded per-element form. ~5× smaller output for `docker_containers` against a busy host. Detailed nested info still available via `ssh_info` and `docker_container_inspect`.

## v0.1.4 — 2026-05-13

- **`ForwardAgent` actually does something now.** Previously a JSON-tagged dead field. Sessions opened with `forward_agent: true` register the local agent (incl. 1Password / gpg-agent) and call `agent.RequestAgentForwarding` on each new channel, so the remote `SSH_AUTH_SOCK` points back at your laptop.
- **`ssh_exec` / `docker_exec` enforce a 30-second timeout by default**, configurable via `timeout_ms` up to 1h. Long-running work belongs in `ssh_shell_open` / `docker_shell_open`, which still have no timeout.
- **Launcher SSE watchdog**: if no SSE event arrives for 45s the launcher force-closes the body so the outer reconnect loop fires. Catches the case where TCP doesn't propagate EOF after a daemon restart.
- **Tighter reconnect backoff** (200ms first retry, was 500ms). Most daemon restarts complete inside the first backoff window.
- **Single stdin reader for the launcher's whole lifetime**. Reconnects no longer spawn dueling `os.Stdin` readers fighting over the parent client's bytes.

## v0.1.3 — 2026-05-13

- **Smart `ssh_connect`**: missing fields are filled from `~/.ssh/config`, the same way `ssh` does it. Pass `{"addresses": ["myhost"]}` and the daemon resolves `Hostname`, `Port`, `User`, `IdentityFile`, `IdentityAgent`, `ProxyJump`. Explicit fields always win; `disable_ssh_config: true` opts out.
- **`AuthSpec.AgentSocket`** override — explicit path supersedes `$SSH_AUTH_SOCK`. Required when the daemon doesn't inherit user env (e.g. 1Password's `IdentityAgent` socket).
- **`HostKeyAlgorithmsFor` derives the algorithm list from `known_hosts`** so the server can't pick a key type for which we have no entry and trip a spurious `knownhosts: key mismatch`.

## v0.1.2 — 2026-05-13

- TOON is now the default output format. `-format json` reverts.
- Round-tripping through `json.Marshal` so `toon-go` (which reads `toon:` tags) honors our `json:` tags transparently.

## v0.1.1 — 2026-05-12

- Added `install.sh` with checksum verification.
- Added `remote-shell-mcp setup` subcommand: detects Claude Code / Claude Desktop / Codex CLI configs and offers to register the daemon. Idempotent, backs up to `.bak`, supports `--dry-run`/`--yes`/`--client`/`--name`.
- GoReleaser-driven release workflow producing darwin/linux × amd64/arm64 archives with `checksums.txt`.

## v0.1.0 — 2026-05-12

Initial release. SSH (password / key / agent / multi-address / jump hosts / keepalive + auto-reconnect / persistent), SFTP, three forward modes, persistent PTY shells, Docker over unix/tcp/ssh, container lifecycle, image management, daemon + stdio launcher with bearer-token auth on the SSE endpoint.
