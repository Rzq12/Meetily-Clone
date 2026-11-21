# Notulensi AI

Internal web app that turns meeting recordings into draft minutes in Bahasa Indonesia: ffmpeg → whisper.cpp → LLM (Ollama) → editable Markdown → DOCX. The UI follows Meetily's design. See `PRD.md`, `FRD.md`, `SRS.md`, and `THIRD_PARTY_NOTICES.md`.

## Requirements

- Docker 27 with the NVIDIA Container Toolkit (GPU for whisper.cpp and Ollama)
- Go 1.23 for local development

## Setup

1. `cp .env.example .env` and fill in real values.
2. Put the models in `./models`:
   - `ggml-large-v3-turbo.bin` (whisper.cpp)
   - `ggml-silero-v5.1.2.bin` (VAD)
3. Start everything: `docker compose up -d --build`
4. Pull the LLM once: `docker compose exec ollama ollama pull "$LLM_MODEL"`
5. Install Caddy's root certificate (`docker compose exec caddy cat /data/caddy/pki/authorities/local/root.crt`) on company browsers. The microphone only works over trusted HTTPS.
6. Log in with `ADMIN_USERNAME` / `ADMIN_PASSWORD` and create the other users.

## Development

```bash
docker compose up -d postgres whisper ollama
go run ./cmd/server          # reads .env, serves http://localhost:8080
go test ./...                # integration tests need TEST_DATABASE_URL
gofmt -l . && go vet ./...
```

`TEST_DATABASE_URL` points to any PostgreSQL 16 database; each test package works in its own temporary schema.

## Operations

- Logs: `docker compose logs -f app` (JSON, never contains transcript or minutes text).
- One meeting is processed at a time. Interrupted jobs resume after a restart.
- At 02:00 WIB a daily job deletes audio older than `AUDIO_RETENTION_DAYS`, idle sessions, and audit logs older than 2 years.
- Data: database in volume `pgdata`, audio in volume `appdata` (`/data/audio`). Back up both. `pg_dump` is scheduled outside the app.
- Minutes styling in DOCX comes from `deploy/reference.docx` (currently pandoc's default; replace it with the company template).
