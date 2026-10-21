# agents-manager (`am`)

Local CLI that lists session history from codex / claude code / cursor-agent / antigravity by reading their on-disk logs. Go program — binary is `am`, full name `agents-manager`. No external deps (except Bubble Tea and Lip Gloss for TUI styling and interaction).

## Data sources (macOS)

Each run scans these paths fresh — no caching, no daemon.

- **codex**: `~/.codex/session_index.jsonl` (id, thread_name, updated_at) + `~/.codex/sessions/**/rollout-*.jsonl` for `session_meta.payload.cwd` and the first real user message. Rollout scan also recovers sessions missing from the index.
- **claude code**: `~/.claude/projects/*/*.jsonl` (supports paths resolved via `CLAUDE_CONFIG_DIR` or matches for `~/.claude*/projects/*/*.jsonl` to support multiple profiles). Each file = one session. `updatedAt` from tail JSONL `timestamp`, falls back to mtime. `cwd` from tail, project label from folder name.
- **cursor-agent**: `~/.cursor/projects/<projectDir>/agent-transcripts/<uuid>/<uuid>.jsonl`. `updatedAt` = file mtime. Title = first `role:"user"` message, `<user_query>` tags stripped.
- **antigravity**: `~/.gemini/antigravity-cli/brain/<uuid>/.system_generated/logs/transcript.jsonl`. `updatedAt` = file mtime. Title = first `USER_INPUT` message with content extracted from `<USER_REQUEST>` tags. Project path is resolved from `~/.gemini/antigravity-cli/history.jsonl` matching `conversationId`.

## Title filtering (codex)

When extracting titles from codex rollout files, skip these as system/bootstrap noise:
- `# AGENTS.md instructions ...`
- `<INSTRUCTIONS> ... </INSTRUCTIONS>`
- `<environment_context> ...`

Only the first genuine user message counts as the title.

## CLI

```
am sessions [--provider P]... [-n|--limit N] [-p|--period D] [--all] [--project SUB] [--json] [--debug]
am history [<dir>] [options]
am sessions <dir> [options]
am search <keyword> [<dir>] [options]
am launch codex [args...]
am launch config
am remote --telegram <token>  # experimental
am completion <shell>
```

Commands support prefix and fuzzy (subsequence) matching (`am l codex`, `am lch codex` -> `am launch codex`, `am ses` -> `am sessions`, `am sea` -> `am search`). Ambiguous abbreviations list matching candidates.

When cleaning up `am remote` tests, terminate only the `./am remote` process tree and its child `codex app-server --stdio` / MCP helper processes. Do not kill `/Applications/Codex.app/.../codex app-server --analytics-default-enabled`; that belongs to the desktop app.

`am remote` renders agent output as formatted text in Telegram. Messages and drafts are converted from the CommonMark the agents emit to Telegram **HTML** (`parse_mode: "HTML"`) by `telegramMarkdownToHTML`: `**bold**`, `*italic*`, `~~strike~~`, `` `code` ``, ```` ```fences``` ````, `#` headings (rendered as bold) and `[text](url)` links. Code spans/blocks are stashed before conversion so their contents are never parsed as markdown, and `& < >` are HTML-escaped. Underscores are left alone (so `snake_case` survives) and unbalanced markers (common in streamed drafts) stay literal. If Telegram rejects a message (e.g. a malformed entity), the send is retried once as plain text so delivery is never blocked.

`am remote` is single-instance: on start it acquires a pid lock at `$TMPDIR/am-remote.pid.lock` and refuses to launch (exit 1) if a live instance already holds it — this prevents multiple pollers from competing for the same bot's `getUpdates`. The lock is removed on graceful exit and on SIGINT/SIGTERM; a stale lock left by a `kill -9` is detected (pid not alive) and overwritten by the next start.

Agent child processes (codex app-server, the claude ACP `npx` tree, the per-prompt `agy` command) are started with `Setpgid: true` and tracked in `activeChild`. On SIGINT/SIGTERM the signal handler kills the active child's whole process group (`kill(-pid)`), so grandchildren are reaped too — important for claude, where `npx -> node -> agent` would otherwise be orphaned (npx does not forward signals). So Ctrl-C / `kill <pid>` / a detached run all leave no orphaned agent behind.

Sessions can be started and stopped repeatedly inside one running `am remote` (e.g. `/start_codex`→`/stop`→`/start_agy`→`/stop`→`/start_claude`→`/stop`): the outer loop returns to waiting for the next `/start_*` after each `/stop`. `/stop` for codex/claude closes the reader's done channel, kills the process group, reaps it (`Process.Wait` in a goroutine, so repeated cycles don't accumulate zombies) and clears `activeChild`. Because the session loop only exits via `/stop`, the post-loop cleanup just clears `activeChild` and performs no pid operations (the pid may already be reused).

When running or testing `am remote`, the Telegram Bot Token should be loaded from `.env.test` if present. When initialized, the bot automatically registers commands (`/start`, `/start_codex`, `/start_agy`, `/start_claude`, `/stop`) to the Telegram server.

If every codex turn fails with `The '<model>' model requires a newer version of Codex` (400 invalid_request_error), upgrade the CLI (`brew upgrade codex`) and restart the bridged session with `/stop` → `/start_codex` — the running `codex app-server` child keeps the old binary until respawned. Note `thread/start` can still succeed while every `turn/start` fails, so a live thread does not prove the model works. Never call `getUpdates` with the bot token while `am remote` is running (it steals updates from the poller); `sendMessage`/`getMe` are safe.

`am remote` claude mode bridges Claude via the Agent Client Protocol (ACP). `/start claude` spawns `npx -y @agentclientprotocol/claude-agent-acp` over stdio. ACP is JSON-RPC 2.0 and keeps the `"jsonrpc":"2.0"` envelope (codex app-server omits it). Lifecycle: `initialize` (id 0, `protocolVersion: 1`, client advertises `fs`/`terminal` capabilities as false so the agent uses its own built-in tools) -> `session/new` (id 1, returns `sessionId`) -> `session/set_mode` to `bypassPermissions` so the session runs unattended without the default plan-mode detour -> per-prompt `session/prompt` (`sessionId` + `prompt` content blocks). Streamed `session/update` notifications: `agent_message_chunk` (`update.content.text`) goes to the draft buffer; `tool_call` is sent as one immediate Telegram message (like agy); other update types are ignored. The agent-initiated `session/request_permission` request is auto-approved by selecting an `allow_always` option (falling back to `allow_once`, then the first option). The prompt response's `stopReason` flushes the draft as `turn_completed`. On `/stop` the npx process tree is killed via the shared done channel.

For `am remote` agy tests, do not rely only on `~/.gemini/antigravity-cli/history.jsonl`: `agy --print` can create/update `brain/<uuid>/.system_generated/logs/transcript.jsonl` without appending a new history row. Detect the active conversation from transcript mtimes as a fallback, match the transcript's `<USER_REQUEST>` to the prompt, and allow enough time for the transcript to appear before killing `agy`.

`am remote` agy mode should keep the active conversation for the lifetime of the remote process. `/start agy` initializes that conversation with an internal bootstrap prompt, stores the conversation id and last step, then later prompts use `agy --conversation <id> --print ...` and stream only transcript steps after the previously sent step index.

In `am remote` agy mode, tool-call notices are not surfaced to Telegram at all — only `PLANNER_RESPONSE` answer text (excluding pure "I will..." narration) is streamed into the draft buffer. (claude/ACP mode still sends `tool_call` notices as immediate messages.)

`am search` scans transcripts (plus title/project/projectRoot) across all selected providers. Default is case-insensitive. Non-TUI output displays the last N lines (default 10) of the transcript indented under the session header, which can be configured using the `--snippet <lines>` option. `--json` includes this snippet as a field. TUI selection (with `c` to copy context) works the same as `sessions <dir>`.

- `<dir>`: exact-match filter on session cwd (not prefix, not recursive). When provided, output is merged across providers into a single table sorted by recency.
- `-n, --limit` defaults to 20 per provider; in merged-dir mode it applies to the combined result.
- `-p, --period` filters results to the last N days (defaults to 7).
- `--all` disables both the limit and period filters, showing all history.
- All timestamps normalized to UTC-aware datetimes for consistent sorting.

## TUI (interactive selection)

Active by default when `am sessions` or `am sessions <dir>` runs in a TTY. Disable with `--no-tui`.

- Shows at most 10 rows, anchored to bottom of terminal.
- `am sessions` (without `<dir>`) displays sessions grouped by provider in tabs.
- `←/→` or `h/l` or `Tab/Shift+Tab` switch active tabs (providers).
- `↑/↓` or `k/j` move inside list, `Enter` prints the selected session id to stdout, `q`/`Esc` cancel.
- Autowrap is disabled (`ESC[?7l`) and restored on exit to prevent layout shift.
- Column padding uses display width (CJK = 2 cells), not character count.

## Output rules

- `Id` column is always full (never truncated/ellipsized) and is the leftmost column.
- Provider-grouped table for plain `am sessions`; single merged table for `am sessions <dir>`.
- ASCII-only in TUI strings (no `…`, no unicode arrows) to keep width math reliable.

## Codex launch wrapper

The status bar's `in`/`out` token counts use one decimal place with `k` (1,000) and `M` (1,000,000); counts below 1,000 remain integers.

`am launch codex [args...]` lives in `internal/codexlaunch`. Each invocation owns a unique `am-codex-*` tmux server; cleanup must target only that server. Enable `mouse` on that dedicated server for wheel scrolling; `Ctrl-b [` opens tmux scrollback and `q` exits it. The private `_launch-codex` entry point supervises Codex inside its pane. Forward arguments as argv, never interpolate them into shell commands. Use `--no-daemon` so rollout ownership can be resolved through the launched process tree using `lsof`; never choose the globally newest log or match cwd alone. Parse rate-limit windows by `window_minutes`, not primary/secondary order. Unavailable quota windows are omitted; other missing telemetry stays `--`. Do not render plan identifiers. `tasks` counts active top-level turns, not subagents. Escape tmux `#` sequences in each log-derived segment before adding trusted color/style markup; do not escape the fully styled line.

`am launch config` edits which status segments are shown and in what order (Bubble Tea editor in `configtui.go`). Segments are keyed by `segmentSpecs` in `config.go` (26 keys; 8 enabled by default, the rest off). New segments go in `segmentSpecs` plus `status.segments()` and the TUI sample. Segment data comes from the rollout log (`turn_context`, `task_started`/`task_complete` with `root_turn_id` for top-level turns, `response_item` tool call types, `item_completed` CommandExecution, `thread_settings_applied`) except branch/dirty (`git` in `git.go`, polled only when enabled) and uptime (launcher start). The config lives at `~/.config/am/launch.json` (`$XDG_CONFIG_HOME/am/launch.json`; tests override with `AM_CONFIG_DIR`). Loading normalizes the file: unknown/duplicate keys are dropped and segments missing from the file are appended with their default visibility, so adding a segment never requires users to edit their config. The config is read once in `RunPane` at launch; it never changes a running session. Saving with every item disabled is refused. The same file holds `fast_mode` (toggled with `f` in the editor); when true, `codexArgs` inserts `-c service_tier="fast"` right after `--no-daemon`, ahead of user arguments, so a user-supplied `-c service_tier=...` overrides it. When false nothing is inserted. The launch integration test sets `AM_CONFIG_DIR` so the real user config never leaks into it.

Build the whole package (`go build -o am .`), not just `am.go`. Local verification: `go test ./... -skip 'TestAgy(RemoteCommunicationFlow|ConversationResumeFlow)' -timeout 60s` and `go vet ./...`. The excluded existing tests make live external agy requests. The launch integration test uses a fake Codex process and isolated real tmux servers.

Startup telemetry uses a short-lived read-only `codex app-server --stdio` (`config/read`, `account/rateLimits/read`) with a bounded timeout and process-group cleanup. Never start a model turn just to populate the status line. Bootstrap values only fill missing live fields; a late startup response must not replace an active session model or usage.
