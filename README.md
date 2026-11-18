# ask

Ask a Claude Code agent on another host a question and get its answer back, with follow-ups
and file attachments in both directions. A proof of concept.

The answering agent is a headless Claude Code that runs in a directory you prepare. What it
knows and what it may do is decided by that directory, not by this tool.

One binary, two roles:

- `ask serve` runs on the answering host. For each question it starts `claude -p` in a prepared
  workspace directory and streams progress and the answer back.
- `ask "<question>"` runs on a person's computer, usually started by that person's own Claude
  through its Bash tool.

## Run

```bash
go build -o ask .

# answering host
./ask serve --workspace /path/to/workspace

# anywhere that reaches the host
export ASK_SERVER=http://127.0.0.1:8700
./ask "How does the importer decide which records to skip?"
./ask --thread <id printed with the answer> "And when a record has no date?"
./ask --attach screenshot.png --attach output.log "What went wrong here?"
```

Progress goes to stderr; the answer and the line `thread: <id>` go to stdout.

## How it works

- The workspace decides what the agent knows and may do: its files, `CLAUDE.md`, skills and
  `.claude/settings.json` permission rules. `ask serve` adds no permission flags of its own;
  pass extra Claude flags with `--claude-arg` (repeatable).
- A thread id is the Claude session id. The server keeps no conversation state; a follow-up
  runs `claude -p --resume <id>`. Use the id printed with the latest answer.
- `--max-parallel` (default 3) limits concurrent questions; follow-ups in one thread run one
  at a time. `--timeout` (default 15m) limits one answer.
- Progress events carry tool names only, never tool input.
- `--journal` (default `ask-journal.jsonl`) records who asked, the question, the answer, the
  duration and the cost.
- Attachments: every run gets an empty directory, named in `ASK_OUTBOX` and in the system
  prompt. Regular files Claude leaves there (at most 10, 50 MB each) are listed with the
  answer. `ask` saves them under `ASK_ATTACHMENTS` (default the user cache directory) in a
  directory per thread and prints only their paths, so the content stays out of the calling
  agent's context until it opens the file. The workspace's `CLAUDE.md` should say when to
  write an attachment, and its permission rules must let Claude write to the outbox
  (`--outbox`, kept for `--attachment-ttl`).
- Files sent with a question (`--attach`, same limits) land in a directory per run under
  `--inbox`. Claude gets their paths at the end of the question, and the directory is added to
  its working directories.
- `--tokens <file>` holds `{"<token>": "<name>"}`; callers send the token in `ASK_TOKEN`. The
  flag is required unless `--listen` is a loopback address.
- `--claude <path>` chooses the binary, for hosts where Claude Code starts through a launcher.

## Protocol

`POST /ask` with `{"question": "...", "thread": "..."}`, or as `multipart/form-data` with that
JSON in the field `request` and one `attachment` part per file. The response is newline-delimited JSON:
`progress` events, then one `answer` or `error` event with `text`, `thread`, `cost_usd` and
`duration_ms`.

## Test

```bash
go test -race ./...
```

The tests use a shell script in place of Claude Code.
