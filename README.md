# recall

`recall` searches local coding-agent sessions and explicit, durable memories from one command. It supports session data from [Pi](https://github.com/badlogic/pi-mono), [Codex](https://github.com/openai/codex), [OpenCode](https://github.com/anomalyco/opencode), and [Claude Code](https://github.com/anthropics/claude-code).

Memory persistence is always manual. `recall` does not extract memories, summarize sessions, inject prompts, or write memory unless a user runs `remember`, `edit`, `forget`, or `restore`.

## Features

- Search sessions and memories together while keeping their results and scores separate.
- Search exact IDs, SQLite FTS5 text, character n-grams, and local Ollama embeddings.
- Keep durable memories as versioned JSON files.
- Rebuild the disposable local memory search index from canonical JSON.
- Scope memories to a normalized Git remote or make them global.
- Detect Syncthing conflict copies without changing them.
- Preserve the existing `agent-sessions` command and output.
- Keep all data and embedding requests on the local machine.

Lexical and fuzzy search work without Ollama. Failed embeddings remain pending for a later indexing run.

## Requirements

- Go 1.25 or later
- Git for project-scoped memory and project-default searches
- [Ollama](https://ollama.com/) for optional semantic search
- The `all-minilm` model for the default embedding configuration

Install the default model:

```sh
ollama pull all-minilm
```

## Installation

Build and install both commands:

```sh
git clone https://github.com/Thelost77/agent-sessions.git
cd agent-sessions
make check
make install
```

`make install` installs `recall` and `agent-sessions` to `~/.local/bin` by default. Set `PREFIX` or `INSTALL_DIR` to use another path.

To install with Go:

```sh
go install github.com/Thelost77/agent-sessions/cmd/recall@latest
go install github.com/Thelost77/agent-sessions/cmd/agent-sessions@latest
```

## Search

Index sessions and refresh memories:

```sh
recall index
```

Search both sources. Each source receives its own result limit and ranking pass:

```sh
recall "database migration decision"
```

Search sessions only. The short and long forms are equivalent:

```sh
recall -s "database migration decision"
recall session "database migration decision"
```

Search memories only:

```sh
recall -m "database migration decision"
recall memory "database migration decision"
```

Use `--` when a bare query starts with a command word:

```sh
recall -- memory
```

Search output uses separate sections:

```text
MEMORIES

1. m_ab12cd [project]
   We avoided SQLite triggers because...

SESSIONS

1. [pi] Database investigation
   2026-08-03
   ...
```

Combined JSON also keeps sources separate:

```json
{
  "query": "query",
  "memories": { "results": [] },
  "sessions": { "results": [] },
  "warnings": []
}
```

Common search options are:

```text
--limit
--json
--explain
--lexical-only
--semantic-threshold
--all-projects
```

Session filters are:

```text
--path
--harness
--role
--since
--before
```

Memory search supports `--project PATH`, `--global`, and `--include-archived`. Options that do not apply to the selected source are rejected. Put flags before the query; the CLI uses Go standard flag syntax.

Inside a Git repository, new `recall` session searches default to that repository path. Memory searches include the current project and global memories. Outside Git, memory searches include global memories by default. Use `--all-projects` to search every project.

## Explicit memory commands

Create a project memory in the current Git repository:

```sh
recall remember "We keep migrations reversible until the next release."
```

Select another project, create a global memory, or include a source reference:

```sh
recall remember --project ~/work/api --source issue:481 "The API uses cursor pagination."
recall remember --global "Use UTC in operational runbooks."
```

Memory content can come from arguments or standard input:

```sh
printf '%s\n' "The release job requires an annotated tag." | recall remember --global
```

Inspect and revise memories:

```sh
recall list
recall list --all-projects --include-archived
recall show m_ab12cd
recall edit m_ab12cd "Updated content"
recall history m_ab12cd
recall forget m_ab12cd
recall restore m_ab12cd
```

`edit` appends a revision and preserves prior content. `forget` archives without deleting. `restore` removes the archived state. List and search omit archived memories unless `--include-archived` is set. A unique memory ID prefix is accepted; an ambiguous prefix is rejected.

Project identity comes from Git. Common SSH and HTTPS origin forms normalize to the same key, such as `github.com/owner/repository`. A project without an origin uses a `local/<repository-name>` key rather than an absolute path. Outside Git, use `--global` or select a Git repository with `--project`.

## Storage and Syncthing

Canonical memory records and local search data have different lifecycles:

```text
Canonical records:          ~/.local/share/recall/memories/records/*.json
Disposable memory index:    ~/.local/share/recall/memory-index.sqlite
Existing session index:     ~/.local/share/agent-sessions/index.sqlite
```

Each canonical JSON file contains its schema version, opaque `m_` ID, scope, project identity, timestamps, archive state, and complete revision history. Writes use a temporary file, file sync, and atomic rename. Embeddings and search data never enter canonical JSON.

The SQLite memory index is derived data. Delete it to rebuild search from JSON:

```sh
rm -f ~/.local/share/recall/memory-index.sqlite{,-wal,-shm}
recall index
```

Index rebuilds never delete canonical records. `recall index --rebuild` rebuilds both derived indexes. `agent-sessions index --rebuild` continues to rebuild only the session index.

A Syncthing directory can hold canonical records:

```toml
memory_directory = "~/Sync/recall"
memory_index = "~/.local/share/recall/memory-index.sqlite"
```

Do **not** put the SQLite memory index in Syncthing. SQLite databases, WAL and SHM files, FTS tables, embeddings, and caches are device-local derived data. Synchronizing a live SQLite database can corrupt it and create needless conflicts.

`recall` detects standard Syncthing conflict filenames. Search indexes only the canonical JSON file and reports the canonical path plus every conflict-copy path. `edit`, `forget`, and `restore` refuse to change an affected memory. `status` and `doctor` report conflicts as warnings. Resolve conflict files manually; `recall` never merges, renames, overwrites, or deletes them.

An invalid changed canonical file is reported and retried on the next refresh. If the local index contains an older valid version, search keeps that last good row. Canonical JSON remains the source of truth.

## Configuration

The preferred configuration file is `~/.config/recall/config.toml`:

```toml
index = "~/.local/share/agent-sessions/index.sqlite"
memory_directory = "~/.local/share/recall/memories"
memory_index = "~/.local/share/recall/memory-index.sqlite"

[embedding]
url = "http://127.0.0.1:11434"
model = "all-minilm"
threshold = 0.60

[harnesses]
pi = true
codex = true
opencode = true
claude = true

[sources]
pi = "~/.pi/agent/sessions"
codex = "~/.codex/sessions"
claude = "~/.claude/projects"
# Leave empty to use `opencode db path`.
opencode = ""
```

Configuration precedence is:

1. command flags;
2. `RECALL_CONFIG`;
3. `~/.config/recall/config.toml`;
4. `AGENT_SESSIONS_CONFIG`;
5. `~/.config/agent-sessions/config.toml`;
6. defaults.

The embedding endpoint must use a loopback address. This prevents accidental uploads of private session or memory text. The default semantic threshold is `0.60`.

## Status and diagnostics

```sh
recall status
recall doctor
```

`status` reports session statistics, canonical records, active and archived memories, stored and pending embeddings, invalid records, conflicts, and storage paths.

`doctor` checks session adapters, both index schemas, canonical memory access, permissions, Ollama access, embedding dimensions, invalid records, and Syncthing conflicts. It reports conflicts without modifying them.

## Privacy and permissions

Application-created data directories use mode `0700`. Canonical JSON and SQLite files use mode `0600`. Existing user-selected Syncthing directories are not forcefully re-permissioned.

Session indexes contain derived copies of private session text. Memory indexes contain derived copies of explicit memory content. Ollama requests are restricted to loopback addresses. There is no cloud service, automatic prompt injection, automatic recall, or automatic memory write.

Recommended agent policy:

> Agents may search recall when prior project knowledge may matter.  
> Agents must not add, edit, archive, or restore memories unless the user explicitly requests it.

## Existing `agent-sessions` users

The compatibility command remains available:

```sh
agent-sessions index
agent-sessions search "empty state"
agent-sessions status
agent-sessions doctor
agent-sessions version
```

Its all-session search default, flags, text and JSON output, resume hints, rebuild behavior, and exit behavior are unchanged. Its configuration remains `~/.config/agent-sessions/config.toml`, or the path in `AGENT_SESSIONS_CONFIG`.

Migration requires no data move. Install `recall`, keep the existing session index path, and add the two optional memory settings to a new recall configuration. If no recall configuration exists, `recall` falls back to the existing agent-sessions configuration. The tool does not move or copy the session index.

## Session source data

The default adapters read:

| Harness | Default source |
|---|---|
| Pi | `~/.pi/agent/sessions/**/*.jsonl` |
| Codex | `~/.codex/sessions/**/*.jsonl` |
| OpenCode | The database returned by `opencode db path` |
| Claude Code | `~/.claude/projects/**/*.jsonl` |

The session index includes user messages, assistant prose, compaction summaries, and useful metadata. It excludes tool output, reasoning, patches, snapshots, base64 data, and duplicate protocol events.

## Scheduling

Run manual indexing first. The existing systemd user timer in [`contrib/systemd`](contrib/systemd) can run `recall index`; do not install a second timer for memory indexing.

```sh
mkdir -p ~/.config/systemd/user
cp contrib/systemd/agent-sessions-index.* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now agent-sessions-index.timer
```

The timer does not start Ollama. Lexical indexing still succeeds when Ollama is unavailable.

## Development

Run all checks and build both commands:

```sh
make check
make build
```

Tests use temporary directories and fake embedders. They do not inspect real sessions or require Ollama.

## License

[MIT](LICENSE)
