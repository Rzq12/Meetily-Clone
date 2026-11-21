# CLAUDE.md

Instructions for Claude Code in this repository. This file is read automatically at the start of every session.

## About the Project

Notulensi AI is an internal web app that turns meeting recordings into draft minutes (notulen) in Bahasa Indonesia. Audio goes through ffmpeg and whisper.cpp, then an LLM fills a minutes template; notetakers review, edit, and export to DOCX.

- Stack: Go 1.23 (`net/http`, `html/template`), vanilla JavaScript, PostgreSQL 16 (`pgx/v5`), whisper.cpp server, Ollama, ffmpeg, pandoc, Docker Compose
- UI language: Indonesian
- Reference documents: @docs/PRD.md, @docs/FRD.md, @docs/SRS.md

## Commands

```bash
go mod download                                   # Install dependencies
docker compose up -d postgres whisper ollama      # Start backing services
go run ./cmd/server                               # Run development server on :8080
go test ./...                                     # Run tests
gofmt -l . && go vet ./...                        # Lint and format check
go build -o bin/server ./cmd/server               # Production build
```

## Short Architecture

- `cmd/server/` : Entry point, config from env, route registration, starts worker and daily job
- `internal/auth/` : Sessions, CSRF, role and meeting-access checks
- `internal/meeting/` : Upload, recording chunks, meeting pages and API, participants, export
- `internal/worker/` : Queue loop and pipeline (transcribe, then summarize), retention job
- `internal/transcribe/` : ffmpeg conversion and whisper.cpp client
- `internal/llm/` : Prompts, chunking, chat completions client, output cleaning
- `internal/template/`, `internal/user/`, `internal/audit/`, `internal/db/` : Their own domain
- `migrations/` : SQL migrations; `web/templates/` and `web/static/` : Pages and JS

Main data flow: page or JS fetch → handler in `internal/<domain>` → SQL via pgx → PostgreSQL; worker → ffmpeg → whisper.cpp → LLM → `meetings.minutes_markdown`.

## Code Rules

- Write simple code. Avoid abstractions that are not needed yet.
- Do not add fallback logic or backward compatibility unless I ask for it.
- Follow the patterns in nearby files before creating a new pattern.
- Do not add dependencies without asking.
- Validation and authorization happen in the backend, not only in the frontend.

## How to Work

- For tasks that touch more than three files, explain a short plan before changing code.
- Run tests and lint after changes and report the results as they are.
- If requirements are unclear or conflict with the reference documents, ask. Do not guess.
- Do not change files outside the scope of the task.

## Do Not

- Do not read or edit `.env` with real values.
- Do not commit or push unless asked.
- Do not run destructive commands (`rm -rf`, `DROP`, `git reset --hard`) without confirmation.
- Do not delete or weaken tests to make them pass.

## Special Notes

- One job at a time in the worker. Do not introduce Redis, a broker, or parallel jobs.
- Minutes are always in Bahasa Indonesia. Meetily (the design reference) forces English output; do not copy that rule.
- Recording chunks are appended only when `seq == last_chunk_seq + 1`; otherwise return 409 with the expected seq.
- Run external tools with `exec.Command` and argument lists, never a shell string.
- Inaccessible meetings return 404. Never put transcript or minutes text in logs or audit entries.
- No frontend build step: plain JS in `web/static/`, no npm.
