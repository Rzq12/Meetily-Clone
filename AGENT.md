# AGENT.md

Guide for AI coding agents working in this repository. Read this whole file before changing code.

## Project Summary

Notulensi AI is an internal web app that turns meeting recordings (uploaded recorder files or browser recordings) into draft minutes in Bahasa Indonesia, using whisper.cpp for transcription and an LLM for the minutes. It is used by the company secretariat, managers, and directors.

- Name: Notulensi AI
- Stack: Go 1.23 (`net/http`, `html/template`), vanilla JavaScript, PostgreSQL 16 (`pgx/v5`), whisper.cpp server, Ollama (OpenAI-compatible API), ffmpeg, pandoc, Docker Compose, Caddy
- Reference documents: `docs/PRD.md`, `docs/FRD.md`, `docs/SRS.md`

## Important Commands

```bash
# Install dependencies
go mod download

# Start PostgreSQL, whisper.cpp, and Ollama for development
docker compose up -d postgres whisper ollama

# Run in development mode (reads .env)
go run ./cmd/server

# Run tests (integration tests need TEST_DATABASE_URL)
go test ./...

# Lint and format
gofmt -l . && go vet ./...

# Build
go build -o bin/server ./cmd/server
```

## Directory Structure

```
notulensi-ai/
├── cmd/server/          # main.go: config, routes, starts worker and daily job
├── internal/
│   ├── auth/            # Login, sessions, CSRF, role and access middleware
│   ├── user/            # User management handlers and queries
│   ├── meeting/         # Upload, browser recording chunks, meeting CRUD, participants, export
│   ├── template/        # Minutes template CRUD and seed template
│   ├── worker/          # Queue loop, pipeline steps, daily retention job
│   ├── transcribe/      # ffmpeg conversion and whisper.cpp client
│   ├── llm/             # Chat completions client, prompts, chunking, output cleaning
│   ├── audit/           # audit.Write and audit log handler
│   └── db/              # Connection pool and migration runner
├── migrations/          # Plain SQL files, applied in filename order
├── web/
│   ├── templates/       # html/template pages and layout
│   └── static/          # app.js, recorder.js, style.css
├── deploy/              # docker-compose.yml, Caddyfile, reference.docx
├── docs/                # PRD.md, FRD.md, SRS.md
└── THIRD_PARTY_NOTICES.md # Meetily MIT notice if its code or prompts are copied
```

## Code Style

- Write simple, readable code. Pick the most direct approach, not the cleverest one.
- Do not add fallback logic or backward compatibility unless explicitly asked.
- One function, one job. Function and variable names should be clear without comments.
- Follow the naming conventions already in the repository.
- Do not add new dependencies without a clear reason.

## Testing

- Every behavior change comes with a test.
- Run the tests before saying the work is done.
- Do not delete or weaken tests to make them pass.

## Boundaries

**Allowed without asking:**
- Reading files, running tests, lint, and build.
- Changing code within the scope of the requested task.

**Ask first:**
- Adding or replacing a dependency.
- Changing the database schema or existing migration files.
- Changing CI/CD, Docker, or deployment configuration.

**Never:**
- Store secrets, passwords, or tokens in code or commits.
- Edit `.env` files that hold real values.
- Force push or rewrite git history.
- Delete production data.

## Git and Commits

- Commit format: `type: short description` (example: `fix: validate chunk sequence`).
- One commit per logical change.
- Do not commit build output or temporary files.

## Project-Specific Notes

- Meetings are processed by one worker goroutine, one at a time (BR-008). Claim jobs with `SELECT ... FOR UPDATE SKIP LOCKED`. Do not add a second queue or a broker.
- Browser recording chunks must be appended strictly in `seq` order. Return 409 with the expected seq on a mismatch; never reorder on the server.
- Minutes must be in Bahasa Indonesia. Meetily's prompts force English (`ENGLISH_BASE_SUMMARY_INSTRUCTION`); our prompts in `internal/llm` replace that rule. Keep the rule "only use information present in the transcript".
- Token estimate is `len([]rune(text)) * 0.35`, chunk size `LLM_TOKEN_THRESHOLD - 300`, overlap 100, same as Meetily.
- Call ffmpeg, ffprobe, and pandoc with `exec.Command` and an argument list. Never build a shell string, and never use user input in file paths.
- Return 404, not 403, when a user cannot view a meeting.
- Never log or write transcript or minutes text to logs or the audit table.
- UI text is Bahasa Indonesia; code, comments, logs, and commits are English.
