# FUNCTIONAL REQUIREMENTS DOCUMENT (FRD)

**Product Name:** Notulensi AI (working name), a web-based AI meeting minutes tool  
**Document Version:** 1.0  
**Status:** Draft  
**Date:** 21 November 2026  
**PRD Reference:** PRD v1.0 (`docs/PRD.md`)  
**Author:** Riezqi Dhermatria Rachmadi  

---

# 1. Introduction

## 1.1 Document Purpose
This FRD describes each function of Notulensi AI at the level developers and QA need to build and test it: actors, normal and failure flows, inputs, outputs, business rules, and system messages. It is derived from PRD v1.0 and uses the same requirement IDs.

## 1.2 Scope
The FRD covers everything listed under In Scope in PRD section 1.5: authentication, user management, upload and browser recording, the processing queue, transcription, minutes generation, templates, review and editing, regeneration, export, the meeting archive, participant access, deletion, audit log, and audio retention. Items under Out of Scope (live transcription, system audio capture, diarization, SSO, mobile apps, email distribution) are not specified here.

## 1.3 Definitions and Terms
| Term | Definition |
|---|---|
| Minutes (notulen) | The structured written record of a meeting produced from the transcript |
| Meeting | One recording with its transcript, minutes, and participants |
| Owner | The user who created the meeting by upload or recording |
| Participant | A user added to a meeting who may view it |
| Segment | One timestamped piece of transcript text returned by whisper.cpp |
| Template | Ordered list of sections with LLM instructions that defines the minutes structure |
| Chunk (audio) | A 5-second piece of browser recording sent to the server |
| Chunk (text) | A part of a long transcript summarized separately in map-reduce |
| Worker | The background goroutine that processes queued meetings |
| VAD | Voice activity detection, used by whisper.cpp to skip silence |
| Meetily | Open-source desktop meeting assistant used as the design reference |

## 1.4 References
- PRD v1.0, Notulensi AI
- Meetily repository, github.com/Zackriya-Solutions/meetily (MIT License): `frontend/src-tauri/src/audio/import.rs`, `summary/processor.rs`, `templates/*.json`, `migrations/*.sql`
- whisper.cpp server documentation
- OpenAI-compatible Chat Completions API (as served by Ollama)

---

# 2. System Overview

## 2.1 System Context
Notulensi AI sits between the meeting itself and the circulated minutes. Today the secretariat records the monthly manager and director meeting on a voice recorder and writes the minutes by hand. With this system, the recording goes into the web application, which produces a draft that the notetaker corrects and exports as DOCX. The system depends on two internal services: a whisper.cpp server for transcription and an LLM endpoint (Ollama by default) for minutes generation. It does not integrate with email, calendar, or HR systems in version 1.0.

## 2.2 Actors
| Actor | Description | Brief Access Rights |
|---|---|---|
| Admin | IT staff running the application | All functions, all meetings, users, templates, audit log |
| Notetaker | Secretariat staff who take minutes | Create meetings, manage own meetings, view participating meetings |
| Viewer | Managers and directors | Read and export meetings they participate in |
| Worker | System actor inside the Go process | Processes queued meetings, runs retention cleanup |

## 2.3 Module List
| Code | Module | Brief Description |
|---|---|---|
| MOD-01 | Authentication & Users | Login, logout, sessions, user accounts and roles |
| MOD-02 | Meeting Intake | File upload and browser recording |
| MOD-03 | Processing | Queue, audio conversion, transcription, minutes generation, audio retention |
| MOD-04 | Templates | Minutes template management |
| MOD-05 | Minutes Review | Review and edit, regenerate, export |
| MOD-06 | Meeting Archive & Access | Meeting list and search, participants, deletion |
| MOD-07 | Audit | Audit log recording and viewing |

---

# 3. Functional Requirements

## 3.1 MOD-01: Authentication & Users

### FR-001: Authentication

| Attribute | Details |
|---|---|
| Actor | Admin, Notetaker, Viewer |
| Priority | High |
| Precondition | The user account exists |
| Postcondition | A session row exists and the browser holds the session cookie |
| PRD Reference | FR-001, NFR-001 |

**Description:**  
Username and password login with server-side sessions. Sessions expire after 8 hours without activity.

**Normal Flow:**
1. The user opens `/login` and submits username and password.
2. The system loads the user by username and checks that it is active.
3. The system compares the password with the bcrypt hash.
4. The system generates a 32-byte random token, stores its SHA-256 hash in `sessions`, sets the cookie, and writes a `login_success` audit entry.
5. The system redirects to `/meetings`.

**Alternative Flow:**
- An already logged-in user opening `/login` is redirected to `/meetings`.
- Logout: the system deletes the session row, clears the cookie, and redirects to `/login`.

**Failure Flow:**
- Unknown username, wrong password, or inactive user: the system shows the generic error and writes a `login_failed` audit entry.
- 5 failures for one username within 15 minutes: further attempts for that username are rejected for 15 minutes.
- Session idle over 8 hours: the session is deleted and the user is redirected to `/login`.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| username | String | Yes | 3–50 characters |
| password | String | Yes | 8–128 characters |

**Output:**

| Field | Type | Notes |
|---|---|---|
| session cookie | String | `session`, HttpOnly, Secure, SameSite=Lax |
| redirect | URL | `/meetings` |

**Business Rules:**
- BR-012: Deactivated users cannot log in.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Success | Success | (redirect, no message) |
| Wrong credentials or inactive user | Error | Username atau password salah. |
| Too many attempts | Error | Terlalu banyak percobaan. Coba lagi dalam 15 menit. |
| Session expired | Info | Sesi berakhir. Silakan masuk kembali. |

### FR-002: User Management

| Attribute | Details |
|---|---|
| Actor | Admin |
| Priority | High |
| Precondition | Actor is logged in as Admin |
| Postcondition | User data is created or updated |
| PRD Reference | FR-002 |

**Description:**  
Admin creates users, edits names and roles, resets passwords, and deactivates or reactivates users.

**Normal Flow:**
1. Admin opens `/admin/users` and sees the user list.
2. Admin clicks "Tambah Pengguna", fills the form, and saves.
3. The system validates, hashes the password, inserts the user, and writes a `user_change` audit entry.
4. To edit, Admin opens a user, changes fields, and saves. The system updates the row and writes an audit entry.

**Alternative Flow:**
- Reset password: Admin enters a new password; the system updates the hash and deletes that user's sessions.
- Deactivate: the system sets `is_active = false` and deletes that user's sessions.

**Failure Flow:**
- Duplicate username: the form shows an error and nothing is saved.
- Admin edits their own role or active flag: rejected.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| username | String | Yes | 3–50 chars, `^[a-zA-Z0-9._]+$`, unique, not editable after creation |
| full_name | String | Yes | 1–255 characters |
| role | Enum | Yes | admin, notetaker, viewer |
| password | String | On create and reset | 8–128 characters |
| is_active | Boolean | No | Default true |

**Output:**

| Field | Type | Notes |
|---|---|---|
| id | UUID | User id |
| username, full_name, role, is_active | Mixed | Saved values, never the hash |

**Business Rules:**
- BR-001: One role per user.
- BR-012: Deactivated users keep ownership of their meetings.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Success | Success | Pengguna tersimpan. |
| Duplicate username | Error | Username sudah dipakai. |
| Editing own role or status | Error | Tidak bisa mengubah akun sendiri. |
| Invalid input | Error | Periksa kembali isian yang ditandai. |

## 3.2 MOD-02: Meeting Intake

### FR-003: Upload Recording

| Attribute | Details |
|---|---|
| Actor | Admin, Notetaker |
| Priority | High |
| Precondition | At least one template exists |
| Postcondition | Meeting exists with status `queued` and the audio file is stored |
| PRD Reference | FR-003 |

**Description:**  
Upload of a voice recorder file. Validation mirrors Meetily's `validate_audio_file` (extension list, size, readable duration), with limits set for this product.

**Normal Flow:**
1. The notetaker opens `/meetings/new`, tab "Unggah File".
2. The notetaker fills title, meeting date, template (default preselected), optional participants, and chooses a file.
3. The browser uploads with `XMLHttpRequest` and shows progress.
4. The system checks the extension, streams the file to `data/audio/<id>.<ext>` while counting bytes, and runs `ffprobe` to read the duration.
5. The system inserts the meeting (`source = upload`, `status = queued`, `queued_at = now`) and participants.
6. The system redirects to `/meetings/<id>`.

**Alternative Flow:**
- The user cancels during upload: the browser aborts, the server deletes the partial file, no meeting is created.

**Failure Flow:**
- Extension not allowed, size over 1 GB, duration over 4 hours, or ffprobe error: the file is deleted, no meeting is created, the form shows the error.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| title | String | Yes | 1–255 characters |
| meeting_date | Date | Yes | ≤ today |
| template_id | UUID | Yes | Existing template |
| participant_ids | UUID[] | No | Active users, not the owner |
| file | File | Yes | mp3, m4a, wav, aac, ogg, flac, webm, mp4; ≤ 1 GB; ≤ 4 hours |

**Output:**

| Field | Type | Notes |
|---|---|---|
| id | UUID | New meeting id |
| status | String | `queued` |

**Business Rules:**
- BR-006: Accepted audio formats and limits.
- BR-002: Owner is the uploader.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Success | Success | Rekaman berhasil diunggah dan masuk antrean. |
| Unsupported format | Error | Format file tidak didukung. Gunakan mp3, m4a, wav, aac, ogg, flac, webm, atau mp4. |
| Too large or too long | Error | File melebihi batas 1 GB atau 4 jam. |
| Unreadable | Error | File audio rusak atau tidak bisa dibaca. |

### FR-004: Browser Recording

| Attribute | Details |
|---|---|
| Actor | Admin, Notetaker |
| Priority | High |
| Precondition | HTTPS, microphone permission granted, at least one template exists |
| Postcondition | Meeting exists with status `queued` and the full recording is stored |
| PRD Reference | FR-004, NFR-005 |

**Description:**  
Microphone recording with `MediaRecorder` and a 5-second timeslice. Each chunk is appended to one file on the server in sequence order.

**Normal Flow:**
1. The notetaker opens `/meetings/new`, tab "Rekam Langsung", fills the form, and ticks the consent box.
2. The browser calls `getUserMedia({audio: true})` and shows a level meter.
3. On "Mulai Rekam", the browser calls `POST /api/recordings`. The system creates the meeting with `status = recording`, `source = browser`, and an empty audio file with extension `webm` or `mp4` based on the browser's mime type.
4. The browser starts `MediaRecorder` with `audioBitsPerSecond = 64000` and `start(5000)`, and requests a Screen Wake Lock.
5. Each `dataavailable` event is posted to `/api/recordings/{id}/chunks?seq=n`. The system checks `seq = last_chunk_seq + 1`, appends the bytes, and updates `last_chunk_seq`.
6. On "Berhenti" (with confirmation) or at 4 hours, the browser stops the recorder, uploads the final chunk, and calls `/finish`.
7. The system sets `status = queued`, `queued_at = now`, and the browser redirects to the meeting page.

**Alternative Flow:**
- Tab closed or browser crash: the meeting stays `recording`. On the meeting page, the owner clicks "Proses rekaman yang tersimpan", which calls `/finish`.
- Network drop: the browser holds pending chunks in an in-memory queue and retries the oldest every 5 seconds until it succeeds, then sends the rest in order.

**Failure Flow:**
- Permission denied or no device: recording does not start and no meeting is created.
- Chunk with wrong seq: 409 with `expected_seq`. The browser resends from that seq if it still has it.
- `/finish` on a meeting with zero bytes: 422, meeting stays `recording`.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| title | String | Yes | 1–255 characters |
| meeting_date | Date | Yes | ≤ today |
| template_id | UUID | Yes | Existing template |
| participant_ids | UUID[] | No | Active users |
| consent_confirmed | Boolean | Yes | true |
| mime_type | String | Yes | `audio/webm` or `audio/mp4` |
| seq | Integer | Yes per chunk | last_chunk_seq + 1 |
| chunk body | Binary | Yes per chunk | 1 byte – 5 MB |

**Output:**

| Field | Type | Notes |
|---|---|---|
| id | UUID | Meeting id (start) |
| last_chunk_seq | Integer | After each chunk |
| status | String | `queued` after finish |

**Business Rules:**
- BR-007: Recording stops at 4 hours.
- BR-011: Consent confirmation required.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Recording started | Info | Sedang merekam. Jangan tutup tab ini. |
| Stopped and queued | Success | Rekaman selesai dan masuk antrean. |
| Permission denied | Error | Izin mikrofon ditolak. Aktifkan izin mikrofon di browser. |
| No microphone | Error | Mikrofon tidak ditemukan. |
| Upload retrying | Warning | Koneksi terputus, mencoba lagi. |
| Consent not ticked | Error | Konfirmasi bahwa peserta sudah diberi tahu rapat direkam. |
| Empty recording on finish | Error | Belum ada audio yang tersimpan. |

## 3.3 MOD-03: Processing

### FR-005: Processing Queue

| Attribute | Details |
|---|---|
| Actor | Worker; Admin and owner for retry |
| Priority | High |
| Precondition | Meetings with status `queued` exist |
| Postcondition | Meeting status is `done` or `failed` |
| PRD Reference | FR-005, NFR-006 |

**Description:**  
One worker goroutine processes meetings sequentially, oldest `queued_at` first.

**Normal Flow:**
1. Every 5 seconds the worker selects the oldest `queued` meeting with `SELECT ... FOR UPDATE SKIP LOCKED` and sets it to `transcribing`, or to `summarizing` if `skip_transcription` is true.
2. The worker runs FR-006 (unless skipped), sets `summarizing`, runs FR-007, and sets `done`, clearing `skip_transcription` and `error_message`.
3. The meeting page polls `GET /api/meetings/{id}` every 5 seconds while status is not `done` or `failed`, and shows the label and queue position.

**Alternative Flow:**
- On startup, the system resets `transcribing` meetings to `queued` and `summarizing` meetings to `queued` with `skip_transcription = true` if segments exist.
- Retry: owner or Admin calls `/retry` on a `failed` meeting. If segments exist, `skip_transcription = true`. Status becomes `queued`.

**Failure Flow:**
- Any step error: status `failed`, `error_message` set to the user-facing message, full error written to the server log.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| meeting_id | UUID | Yes (retry) | Status `failed`, actor is owner or Admin |

**Output:**

| Field | Type | Notes |
|---|---|---|
| status | String | recording, queued, transcribing, summarizing, done, failed |
| queue_position | Integer | Only when queued; 1 = next |
| error_message | String | Only when failed |

**Business Rules:**
- BR-008: One job at a time, first in, first out.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Queued | Info | Dalam antrean (posisi {n}). |
| Transcribing | Info | Mentranskrip rekaman... |
| Summarizing | Info | Menyusun notulen... |
| Done | Success | Notulen siap ditinjau. |
| Failed | Error | {error_message} |
| Retry accepted | Success | Rapat dimasukkan kembali ke antrean. |

### FR-006: Transcription

| Attribute | Details |
|---|---|
| Actor | Worker |
| Priority | High |
| Precondition | Status `transcribing`, audio file exists |
| Postcondition | Transcript segments stored, `audio_duration_sec` set |
| PRD Reference | FR-006 |

**Description:**  
Converts audio to 16 kHz mono 16-bit PCM WAV (Meetily's transcription input format) and sends it to whisper.cpp with VAD on and language `id`.

**Normal Flow:**
1. The worker runs `ffmpeg -y -i <audio> -ar 16000 -ac 1 -c:a pcm_s16le <tmp>/<id>.wav`.
2. The worker posts the WAV as multipart to `WHISPER_URL/inference` with `language=id`, `response_format=verbose_json`, `temperature=0`.
3. The worker deletes existing segments for the meeting, then inserts each returned segment with `seq`, `start_sec`, `end_sec`, and trimmed `text`, skipping segments whose text is empty.
4. The worker sets `audio_duration_sec` from the WAV duration and deletes the WAV.

**Alternative Flow:**
- None.

**Failure Flow:**
- ffmpeg exit code ≠ 0: fail with "Konversi audio gagal."
- whisper HTTP error or timeout (timeout = 2 × audio duration, minimum 10 minutes): fail with "Layanan transkripsi tidak tersedia."
- Zero non-empty segments: fail with "Tidak ada suara yang terdeteksi di rekaman."

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| audio_path | String | Yes | File exists |

**Output:**

| Field | Type | Notes |
|---|---|---|
| segments[] | {seq, start_sec, end_sec, text} | Stored in `transcript_segments` |
| audio_duration_sec | Integer | Stored on meeting |

**Business Rules:**
- BR-004: Transcription language is fixed to Indonesian.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Conversion failed | Error | Konversi audio gagal. |
| Whisper unavailable | Error | Layanan transkripsi tidak tersedia. |
| No speech | Error | Tidak ada suara yang terdeteksi di rekaman. |

### FR-007: Minutes Generation

| Attribute | Details |
|---|---|
| Actor | Worker |
| Priority | High |
| Precondition | Status `summarizing`, at least one segment |
| Postcondition | `minutes_markdown` set, status `done` |
| PRD Reference | FR-007 |

**Description:**  
Fills the meeting's template from the transcript in Bahasa Indonesia. Uses Meetily's map-reduce design and token estimate (characters × 0.35), with the English output rule replaced by an Indonesian one.

**Normal Flow:**
1. The worker builds the transcript text, one line per segment: `[mm:ss] text` (`[hh:mm:ss]` past one hour).
2. The worker estimates tokens. If below `LLM_TOKEN_THRESHOLD` (default 6000), the full text is the source text.
3. Otherwise the worker splits the text into chunks of `LLM_TOKEN_THRESHOLD − 300` tokens with 100 tokens overlap, cutting at line breaks. For each chunk it calls the LLM with the chunk prompt. It then calls the LLM once with all chunk summaries and the combine prompt. The result is the source text.
4. The worker builds the final system prompt from the template: section titles as Markdown `##` headings, each section's instruction, format, and item format.
5. The worker calls the LLM with the final system prompt and the source text in `<transkrip>` tags.
6. The worker removes `<think>...</think>` blocks and surrounding code fences, checks the result is not empty, and saves it.

**Alternative Flow:**
- Regenerate (FR-010): same flow using the template chosen at regeneration.

**Failure Flow:**
- LLM error or 5-minute timeout: retry that call once after 10 seconds; on second failure, fail with "Penyusunan notulen gagal."
- Empty result after cleaning: fail with "AI tidak menghasilkan notulen."

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| segments | Array | Yes | ≥ 1 |
| template.sections | JSON | Yes | ≥ 1 section |

**Output:**

| Field | Type | Notes |
|---|---|---|
| minutes_markdown | Text | Template headings filled in Bahasa Indonesia |

**Business Rules:**
- BR-004: Output in Bahasa Indonesia only.
- BR-005: Only information from the transcript; empty sections show "Tidak ada catatan untuk bagian ini."

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| LLM failure | Error | Penyusunan notulen gagal. |
| Empty output | Error | AI tidak menghasilkan notulen. |

### FR-016: Audio Retention

| Attribute | Details |
|---|---|
| Actor | Worker |
| Priority | Medium |
| Precondition | `AUDIO_RETENTION_DAYS` configured |
| Postcondition | Expired audio files deleted |
| PRD Reference | FR-016 |

**Description:**  
Daily job at 02:00 server time that deletes old audio files and expired sessions and audit logs.

**Normal Flow:**
1. The job selects meetings with `status = done`, `audio_deleted_at IS NULL`, `created_at < now − AUDIO_RETENTION_DAYS`.
2. For each, it deletes the file and sets `audio_deleted_at = now`.
3. It deletes sessions idle over 8 hours and audit logs older than 2 years.

**Alternative Flow:**
- File already missing: set `audio_deleted_at` and continue.

**Failure Flow:**
- File deletion error other than not-found: log it and retry on the next run.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| AUDIO_RETENTION_DAYS | Integer | Yes | 1–3650 |

**Output:**

| Field | Type | Notes |
|---|---|---|
| audio_deleted_at | Timestamp | Set per processed meeting |

**Business Rules:**
- BR-010: Audio retention period.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Audio deleted (shown on meeting page) | Info | Audio sudah dihapus sesuai kebijakan retensi. |

## 3.4 MOD-04: Templates

### FR-008: Template Management

| Attribute | Details |
|---|---|
| Actor | Admin (manage); Notetaker (view in forms) |
| Priority | Medium |
| Precondition | Actor is Admin |
| Postcondition | Template saved |
| PRD Reference | FR-008 |

**Description:**  
CRUD for minutes templates in Meetily's section format. A seed template "Rapat Bulanan Manajemen" is created on first start and set as default.

**Normal Flow:**
1. Admin opens `/admin/templates`.
2. Admin creates or edits a template: name, description, and sections (add, remove, move up or down).
3. The system validates and saves `sections` as JSONB.
4. Admin may click "Jadikan Default"; the system clears the previous default in the same transaction.

**Alternative Flow:**
- Delete an unused template: confirmation, then delete.

**Failure Flow:**
- No sections, duplicate name, table section without item format, or deleting a template in use: rejected with a message.
- Deleting the default template: rejected.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| name | String | Yes | 1–100 characters, unique |
| description | String | No | ≤ 500 characters |
| sections[].title | String | Yes | 1–100 characters |
| sections[].instruction | String | Yes | 1–1000 characters |
| sections[].format | Enum | Yes | paragraph, list, table |
| sections[].item_format | String | When format = table | Markdown table header row, ≤ 500 characters |

**Output:**

| Field | Type | Notes |
|---|---|---|
| id | UUID | Template id |
| is_default | Boolean | Default flag |

**Business Rules:**
- A template in use cannot be deleted.
- Exactly one default template exists.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Success | Success | Template tersimpan. |
| No sections | Error | Template harus memiliki minimal satu bagian. |
| Duplicate name | Error | Nama template sudah dipakai. |
| Table without item format | Error | Bagian bertipe tabel wajib memiliki format kolom. |
| In use | Error | Template sedang dipakai oleh rapat dan tidak bisa dihapus. |
| Delete default | Error | Template default tidak bisa dihapus. |

## 3.5 MOD-05: Minutes Review

### FR-009: Review and Edit Minutes

| Attribute | Details |
|---|---|
| Actor | Admin, owner (edit); participants (view) |
| Priority | High |
| Precondition | Status `done`; actor can view the meeting |
| Postcondition | Edited minutes saved |
| PRD Reference | FR-009 |

**Description:**  
Two-column meeting page: minutes on the left, transcript and audio player on the right.

**Normal Flow:**
1. The user opens `/meetings/{id}`. The system checks access and writes a `meeting_view` audit entry.
2. The page shows rendered minutes, the transcript list, and the audio player (`/api/meetings/{id}/audio` with HTTP range support).
3. Clicking a transcript segment or a `[mm:ss]` timestamp in the minutes sets `audio.currentTime`.
4. The owner clicks "Edit", edits Markdown with a live preview, and clicks "Simpan".
5. The system checks `updated_at` matches, saves `minutes_markdown`, `minutes_edited_by`, `minutes_edited_at`, and writes a `minutes_edit` audit entry.

**Alternative Flow:**
- Audio deleted by retention: the player is hidden and an info message is shown.
- Leaving edit mode with unsaved changes: the browser asks for confirmation.

**Failure Flow:**
- `updated_at` mismatch: 409, nothing saved.
- Non-owner, non-Admin calls save: 403.
- No access: 404.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| minutes_markdown | Text | Yes | 1–200,000 characters |
| updated_at | Timestamp | Yes | Equals stored value |

**Output:**

| Field | Type | Notes |
|---|---|---|
| updated_at | Timestamp | New value for the next save |

**Business Rules:**
- BR-002: Visibility.
- BR-003: Edit rights.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Saved | Success | Notulen tersimpan. |
| Conflict | Error | Notulen sudah diubah oleh pengguna lain. Muat ulang halaman. |
| Empty | Error | Notulen tidak boleh kosong. |
| Unsaved changes | Warning | Ada perubahan yang belum disimpan. Tetap keluar? |

### FR-010: Regenerate Minutes

| Attribute | Details |
|---|---|
| Actor | Admin, owner |
| Priority | Medium |
| Precondition | Status `done` |
| Postcondition | New minutes saved, previous minutes in backup |
| PRD Reference | FR-010 |

**Description:**  
Runs FR-007 again on the stored transcript, keeping one backup like Meetily's `result_backup`.

**Normal Flow:**
1. The owner clicks "Buat Ulang", chooses a template, and confirms.
2. The system copies `minutes_markdown` to `minutes_backup`, sets `template_id`, `skip_transcription = true`, `status = queued`, `queued_at = now`, and writes a `regenerate` audit entry.
3. The worker runs FR-007 and saves the new minutes.

**Alternative Flow:**
- Restore: the owner clicks "Kembalikan versi sebelumnya". The system swaps `minutes_markdown` and `minutes_backup`.

**Failure Flow:**
- Generation fails: status `failed`; `minutes_markdown` is restored from `minutes_backup` so the meeting still shows minutes. Retry follows FR-005.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| template_id | UUID | Yes | Existing template |

**Output:**

| Field | Type | Notes |
|---|---|---|
| status | String | `queued` |

**Business Rules:**
- BR-009: One backup is kept.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Accepted | Success | Notulen sedang dibuat ulang. |
| Confirmation | Warning | Notulen saat ini akan disimpan sebagai versi sebelumnya. Lanjutkan? |
| Restored | Success | Versi sebelumnya dikembalikan. |

### FR-011: Export Minutes

| Attribute | Details |
|---|---|
| Actor | Admin, owner, participants |
| Priority | High |
| Precondition | Status `done` |
| Postcondition | File downloaded, audit entry written |
| PRD Reference | FR-011 |

**Description:**  
Exports the current minutes with a header block as DOCX (pandoc with `reference.docx`) or Markdown.

**Normal Flow:**
1. The user clicks "Unduh" and chooses DOCX or Markdown.
2. The system builds Markdown: `# {title}`, a line with meeting date (Indonesian date format, e.g. 21 November 2026), owner, and participants, then the minutes.
3. For DOCX the system runs `pandoc -f gfm -t docx --reference-doc=reference.docx` on a temp file.
4. The system streams the file with `Content-Disposition: attachment` and writes an `export` audit entry.

**Alternative Flow:**
- None.

**Failure Flow:**
- pandoc error: 500 with the export error message.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| format | Enum | Yes | docx, md |

**Output:**

| Field | Type | Notes |
|---|---|---|
| file | Binary | `Notulen-YYYY-MM-DD-<slug-title>.docx` or `.md` |

**Business Rules:**
- BR-002: Only users who can view the meeting can export it.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Export failed | Error | Ekspor gagal. Coba lagi atau unduh versi Markdown. |

## 3.6 MOD-06: Meeting Archive & Access

### FR-012: Meeting List and Search

| Attribute | Details |
|---|---|
| Actor | Admin, Notetaker, Viewer |
| Priority | High |
| Precondition | Logged in |
| Postcondition | None |
| PRD Reference | FR-012 |

**Description:**  
Paginated list of accessible meetings with filters.

**Normal Flow:**
1. The user opens `/meetings`.
2. The system queries meetings where the user is Admin, owner, or participant, applying `q` (case-insensitive match on title), `date_from`, and `date_to`.
3. Results are ordered by `meeting_date DESC, created_at DESC`, 20 per page.

**Alternative Flow:**
- Notetaker and Admin see a "Rapat Baru" button.

**Failure Flow:**
- Invalid date range: list not loaded, error shown.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| q | String | No | ≤ 100 characters |
| date_from | Date | No | ≤ date_to |
| date_to | Date | No | ≥ date_from |
| page | Integer | No | ≥ 1, default 1 |

**Output:**

| Field | Type | Notes |
|---|---|---|
| items[] | {id, title, meeting_date, owner_name, status, audio_duration_sec} | Current page |
| total | Integer | Total matches |

**Business Rules:**
- BR-002: Visibility.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| No results | Info | Belum ada rapat yang cocok. |
| Invalid range | Error | Rentang tanggal tidak valid. |

### FR-013: Meeting Access

| Attribute | Details |
|---|---|
| Actor | Admin, owner |
| Priority | High |
| Precondition | Meeting exists |
| Postcondition | Participant list replaced |
| PRD Reference | FR-013 |

**Description:**  
Owner sets who may view the meeting.

**Normal Flow:**
1. The owner opens the "Peserta" panel on the meeting page.
2. The owner searches active users by name, adds or removes them, and saves.
3. The system replaces rows in `meeting_participants` in one transaction and writes a `participants_change` audit entry.

**Alternative Flow:**
- Participants can also be set on the upload and recording forms (FR-003, FR-004).

**Failure Flow:**
- Inactive or unknown user id: rejected, nothing changed.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| participant_ids | UUID[] | Yes | Active users, owner excluded, ≤ 100 ids |

**Output:**

| Field | Type | Notes |
|---|---|---|
| participants[] | {id, full_name, role} | Saved list |

**Business Rules:**
- BR-002, BR-003.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Saved | Success | Daftar peserta tersimpan. |
| Inactive user | Error | Pengguna tidak aktif tidak bisa ditambahkan. |

### FR-014: Delete Meeting

| Attribute | Details |
|---|---|
| Actor | Admin, owner |
| Priority | Medium |
| Precondition | Status is not `transcribing` or `summarizing` |
| Postcondition | Meeting rows and audio file removed |
| PRD Reference | FR-014 |

**Description:**  
Permanent deletion confirmed by typing the meeting title.

**Normal Flow:**
1. The owner clicks "Hapus Rapat" and types the title.
2. The system deletes the meeting row (cascading participants and segments), deletes the audio file, and writes a `meeting_delete` audit entry with the title in `detail`.
3. The system redirects to `/meetings`.

**Alternative Flow:**
- Deleting a `recording` meeting is allowed and stops further chunk uploads (they return 404).

**Failure Flow:**
- Title mismatch or meeting processing: rejected.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| confirm_title | String | Yes | Equals meeting title |

**Output:**

| Field | Type | Notes |
|---|---|---|
| redirect | URL | `/meetings` |

**Business Rules:**
- BR-003: Delete rights.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| Deleted | Success | Rapat dihapus. |
| Title mismatch | Error | Judul yang diketik tidak sesuai. |
| Processing | Error | Rapat sedang diproses dan tidak bisa dihapus. |

## 3.7 MOD-07: Audit

### FR-015: Audit Log

| Attribute | Details |
|---|---|
| Actor | System (write); Admin (view) |
| Priority | Medium |
| Precondition | None (write); Admin (view) |
| Postcondition | Entry stored |
| PRD Reference | FR-015 |

**Description:**  
Append-only log of sensitive actions with a filterable Admin view.

**Normal Flow:**
1. The handlers for the listed actions call one `audit.Write` function after the action succeeds (login failures are written on failure).
2. Admin opens `/admin/audit`, filters, and pages through entries.

**Alternative Flow:**
- None.

**Failure Flow:**
- Audit insert fails: the request still returns its normal result and the error is written to the server log.

**Input:**

| Field | Type | Required | Validation |
|---|---|---|---|
| user_id | UUID | No | Existing user |
| action | Enum | No | login_success, login_failed, meeting_view, minutes_edit, regenerate, export, participants_change, meeting_delete, user_change |
| date_from, date_to | Date | No | Valid range |
| page | Integer | No | ≥ 1 |

**Output:**

| Field | Type | Notes |
|---|---|---|
| items[] | {created_at, user_full_name, action, meeting_title, ip_address} | 50 per page; meeting title shown if the meeting still exists |

**Business Rules:**
- Audit entries never contain transcript or minutes text.

**System Messages:**

| Condition | Type | Message |
|---|---|---|
| No entries | Info | Tidak ada aktivitas. |

---

# 4. Data Requirements

## 4.1 Data Entities
| Entity | Description | Key Attributes |
|---|---|---|
| User | Account with one role | id, username, full_name, role, is_active |
| Session | Login session | token_hash, user_id, last_seen_at |
| Template | Minutes structure | id, name, sections, is_default |
| Meeting | One recording and its outputs | id, title, meeting_date, owner_id, template_id, source, status, audio_path, minutes_markdown, minutes_backup |
| Meeting Participant | User allowed to view a meeting | meeting_id, user_id |
| Transcript Segment | Timestamped transcript text | meeting_id, seq, start_sec, end_sec, text |
| Audit Log | Record of a sensitive action | user_id, action, meeting_id, ip_address, created_at |

## 4.2 Data Validation Rules
| Field | Rule | Valid Example | Invalid Example |
|---|---|---|---|
| username | 3–50 chars, letters, digits, dot, underscore | `sekretariat.01` | `sek retariat` |
| password | 8–128 characters | `Rapat2026!` | `abc123` |
| meeting title | 1–255 characters | `Rapat Bulanan Manajemen November 2026` | (empty) |
| meeting_date | Not later than today | `2026-11-20` | `2026-12-31` (on 21 Nov 2026) |
| audio file | Allowed extension, ≤ 1 GB, ≤ 4 h, readable by ffprobe | `rapat-nov.m4a`, 180 MB, 2 h | `notulen.pdf` |
| chunk seq | last_chunk_seq + 1 | 13 after 12 | 15 after 12 |
| template section format | paragraph, list, or table | `table` | `grid` |
| export format | docx or md | `docx` | `pdf` |

## 4.3 Master and Reference Data
- Roles: `admin`, `notetaker`, `viewer`.
- Meeting statuses: `recording`, `queued`, `transcribing`, `summarizing`, `done`, `failed`, with Indonesian labels Merekam, Dalam antrean, Mentranskrip, Menyusun notulen, Selesai, Gagal.
- Meeting sources: `upload`, `browser`.
- Audit actions: as listed in FR-015.
- Initial Admin account created from `ADMIN_USERNAME` and `ADMIN_PASSWORD` environment variables on first start when no users exist.
- Seed template "Rapat Bulanan Manajemen", default:

| Section | Format | Instruction (summary) |
|---|---|---|
| Ringkasan | paragraph | One-paragraph summary of the whole meeting |
| Peserta yang Disebut | list | Names and positions mentioned in the conversation |
| Pokok Pembahasan | list | Each topic discussed with its key points |
| Keputusan | list | Decisions made |
| Tindak Lanjut | table | `\| PIC \| Tugas \| Tenggat \| Waktu di Rekaman \|` |

---

# 5. Interface Requirements

## 5.1 User Interface
| Page | Description | Actor | Related Functions |
|---|---|---|---|
| `/login` | Login form | All | FR-001 |
| `/meetings` | Meeting list with search and filters | All | FR-012 |
| `/meetings/new` | Tabs "Unggah File" and "Rekam Langsung" | Admin, Notetaker | FR-003, FR-004, FR-013 |
| `/meetings/{id}` | Status, minutes, transcript, audio, participants, actions | Admin, owner, participants | FR-005, FR-009, FR-010, FR-011, FR-013, FR-014 |
| `/admin/users` | User list and form | Admin | FR-002 |
| `/admin/templates` | Template list and editor | Admin | FR-008 |
| `/admin/audit` | Audit log with filters | Admin | FR-015 |

## 5.2 External Interfaces
| External System | Integration Type | Data Exchanged |
|---|---|---|
| whisper.cpp server | HTTP multipart (`POST /inference`) | Sends 16 kHz mono WAV and parameters; receives segments with start, end, text |
| LLM endpoint (Ollama by default) | REST, OpenAI-compatible `POST /v1/chat/completions` | Sends prompts with transcript text; receives Markdown |
| ffmpeg / ffprobe | Local process | Audio file in, WAV or duration out |
| pandoc | Local process | Markdown in, DOCX out |

## 5.3 Reports and Exports
| Report Name | Format | Filter | Actor |
|---|---|---|---|
| Meeting minutes | DOCX, Markdown | One meeting | Admin, owner, participants |
| Audit log | On-screen table | User, action, date range | Admin |

---

# 6. Traceability Matrix

| PRD ID | FRD ID | Module | Test Case |
|---|---|---|---|
| FR-001 | FR-001 | MOD-01 | TC-001 |
| FR-002 | FR-002 | MOD-01 | TC-002 |
| FR-003 | FR-003 | MOD-02 | TC-003 |
| FR-004 | FR-004 | MOD-02 | TC-004 |
| FR-005 | FR-005 | MOD-03 | TC-005 |
| FR-006 | FR-006 | MOD-03 | TC-006 |
| FR-007 | FR-007 | MOD-03 | TC-007 |
| FR-008 | FR-008 | MOD-04 | TC-008 |
| FR-009 | FR-009 | MOD-05 | TC-009 |
| FR-010 | FR-010 | MOD-05 | TC-010 |
| FR-011 | FR-011 | MOD-05 | TC-011 |
| FR-012 | FR-012 | MOD-06 | TC-012 |
| FR-013 | FR-013 | MOD-06 | TC-013 |
| FR-014 | FR-014 | MOD-06 | TC-014 |
| FR-015 | FR-015 | MOD-07 | TC-015 |
| FR-016 | FR-016 | MOD-03 | TC-016 |

---

# 7. Acceptance Criteria

| ID | Function | Criteria | Status |
|---|---|---|---|
| AC-001 | FR-001 | Given an active user, when they submit correct credentials, then they land on `/meetings`; given a session idle over 8 hours, when they open any page, then they are redirected to `/login`. | Pending |
| AC-002 | FR-002 | Given an Admin deactivates a logged-in user, when that user makes the next request, then they are redirected to `/login`. | Pending |
| AC-003 | FR-003 | Given a 90-minute m4a file, when a notetaker uploads it, then a meeting with status `queued` exists and a renamed .txt file is rejected as unreadable. | Pending |
| AC-004 | FR-004 | Given a recording closed at minute 30, when the owner processes the stored audio, then the queued audio is at least 29 min 55 s long. | Pending |
| AC-005 | FR-005 | Given two queued meetings, when the worker runs, then the older finishes before the newer starts, and a meeting that failed at the LLM step is retried without transcription. | Pending |
| AC-006 | FR-006 | Given a clear 5-minute Indonesian recording, when transcribed, then segments with increasing start times cover the recording; a silent file fails with the no-speech message. | Pending |
| AC-007 | FR-007 | Given a transcript where a director assigns a task with a deadline, when minutes are generated, then the Tindak Lanjut table lists PIC, task, and deadline, in Bahasa Indonesia. | Pending |
| AC-008 | FR-008 | Given a template in use, when Admin deletes it, then deletion is rejected; given a new 3-section template, then generated minutes have exactly those headings in order. | Pending |
| AC-009 | FR-009 | Given two owners' sessions editing the same minutes, when the second saves after the first, then the second save returns 409. | Pending |
| AC-010 | FR-010 | Given edited minutes, when the owner regenerates and then restores, then the edited text is back. | Pending |
| AC-011 | FR-011 | Given minutes with a table, when exported to DOCX, then Word shows a table, and an export audit entry exists. | Pending |
| AC-012 | FR-012 | Given a Viewer added to 2 of 10 meetings, when they open `/meetings`, then exactly 2 are listed. | Pending |
| AC-013 | FR-013 | Given a Viewer removed from a meeting, when they open its URL, then the response is 404. | Pending |
| AC-014 | FR-014 | Given a deleted meeting, when anyone opens its URL, then the response is 404 and its audio file no longer exists. | Pending |
| AC-015 | FR-015 | Given a Viewer exports minutes, when Admin opens the audit log, then an export entry with that user and meeting appears. | Pending |
| AC-016 | FR-016 | Given a done meeting created 91 days ago with retention 90, when the daily job runs, then its audio is deleted and its minutes still open. | Pending |

---

# Revision History

| Version | Date | Changes | Author |
|---|---|---|---|
| 1.0 | 21 November 2026 | Initial document | Riezqi Dhermatria Rachmadi |

# Approval

| Role | Name | Status |
|---|---|---|
| Product Owner | [To be decided] | Pending |
| Technical Lead | Riezqi Dhermatria Rachmadi | Pending |
| QA Lead | [To be decided] | Pending |
