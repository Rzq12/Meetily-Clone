package meeting

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"notulensi/internal/auth"
	"notulensi/internal/transcribe"
	"notulensi/web"
)

const (
	maxUploadBytes  = 1 << 30
	maxDurationSec  = 4 * 3600
	maxChunkBytes   = 5 << 20
	msgBadFormat    = "Format file tidak didukung. Gunakan mp3, m4a, wav, aac, ogg, flac, webm, atau mp4."
	msgTooLarge     = "File melebihi batas 1 GB atau 4 jam."
	msgUnreadable   = "File audio rusak atau tidak bisa dibaca."
	msgEmptyRecord  = "Belum ada audio yang tersimpan."
	msgNoConsent    = "Konfirmasi bahwa peserta sudah diberi tahu rapat direkam."
	msgRecordingEnd = "Rekaman sudah selesai."
)

// Meetily's import.rs validates the same extension list; limits are ours (BR-006).
var allowedExt = map[string]bool{"mp3": true, "m4a": true, "wav": true, "aac": true, "ogg": true, "flac": true, "webm": true, "mp4": true}

type meetingInput struct {
	Title          string   `json:"title"`
	MeetingDate    string   `json:"meeting_date"`
	TemplateID     string   `json:"template_id"`
	ParticipantIDs []string `json:"participant_ids"`
}

func (h *Handler) validateInput(ctx context.Context, in *meetingInput, ownerID string) (string, error) {
	in.Title = strings.TrimSpace(in.Title)
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > 255 {
		return "Judul wajib diisi, maksimal 255 karakter.", nil
	}
	date, err := web.ParseDate(in.MeetingDate)
	if err != nil {
		return "Tanggal rapat wajib diisi.", nil
	}
	if date.After(h.today()) {
		return "Tanggal rapat tidak boleh melebihi hari ini.", nil
	}
	if !auth.ValidUUID(in.TemplateID) {
		return "Template tidak ditemukan.", nil
	}
	var exists bool
	if err := h.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM templates WHERE id = $1)`, in.TemplateID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "Template tidak ditemukan.", nil
	}
	return validateParticipants(ctx, h.Pool, in.ParticipantIDs, ownerID)
}

func (h *Handler) newID(ctx context.Context) (string, error) {
	var id string
	err := h.Pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
	return id, err
}

func (h *Handler) audioPath(rel string) string { return filepath.Join(h.DataDir, rel) }

func (h *Handler) insertMeeting(ctx context.Context, id, ownerID, source, status, rel string, in meetingInput, durationSec *int) error {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var queuedAt any
	if status == "queued" {
		queuedAt = h.Now()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meetings (id, title, meeting_date, owner_id, template_id, source, status, audio_path, audio_duration_sec, queued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, in.Title, in.MeetingDate, ownerID, in.TemplateID, source, status, rel, durationSec, queuedAt); err != nil {
		return err
	}
	for _, pid := range in.ParticipantIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO meeting_participants (meeting_id, user_id) VALUES ($1, $2)`, id, pid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Upload streams the multipart file to disk; the client sends text fields before the file part.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.CanCreateMeetings() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		web.Error(w, http.StatusBadRequest, "Unggahan tidak valid.")
		return
	}

	var in meetingInput
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			web.Error(w, http.StatusBadRequest, "Pilih file rekaman.")
			return
		}
		if err != nil {
			web.Error(w, http.StatusBadRequest, "Unggahan tidak valid.")
			return
		}
		if part.FormName() == "file" {
			h.storeUpload(w, r, u, in, part)
			return
		}
		value, err := io.ReadAll(io.LimitReader(part, 4096))
		if err != nil {
			web.Error(w, http.StatusBadRequest, "Unggahan tidak valid.")
			return
		}
		switch part.FormName() {
		case "title":
			in.Title = string(value)
		case "meeting_date":
			in.MeetingDate = string(value)
		case "template_id":
			in.TemplateID = string(value)
		case "participant_ids":
			in.ParticipantIDs = append(in.ParticipantIDs, string(value))
		}
	}
}

func (h *Handler) storeUpload(w http.ResponseWriter, r *http.Request, u *auth.User, in meetingInput, file *multipart.Part) {
	ctx := r.Context()
	if msg, err := h.validateInput(ctx, &in, u.ID); err != nil {
		web.Internal(w, err)
		return
	} else if msg != "" {
		web.Error(w, http.StatusUnprocessableEntity, msg)
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(file.FileName()), "."))
	if !allowedExt[ext] {
		web.Error(w, http.StatusUnprocessableEntity, msgBadFormat)
		return
	}

	id, err := h.newID(ctx)
	if err != nil {
		web.Internal(w, err)
		return
	}
	rel := filepath.Join("audio", id+"."+ext)
	full := h.audioPath(rel)
	f, err := os.Create(full)
	if err != nil {
		web.Internal(w, err)
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(file, maxUploadBytes+1))
	closeErr := f.Close()
	fail := func(status int, msg string) {
		if err := os.Remove(full); err != nil {
			slog.Error("remove rejected upload", "meeting_id", id, "err", err)
		}
		web.Error(w, status, msg)
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(copyErr, &tooBig) || n > maxUploadBytes:
		fail(http.StatusRequestEntityTooLarge, msgTooLarge)
		return
	case copyErr != nil:
		fail(http.StatusBadRequest, "Unggahan terputus.")
		return
	case closeErr != nil:
		fail(http.StatusInternalServerError, "Terjadi kesalahan pada server.")
		return
	}

	duration, err := transcribe.Probe(ctx, full)
	if err != nil {
		fail(http.StatusUnprocessableEntity, msgUnreadable)
		return
	}
	if duration > maxDurationSec {
		fail(http.StatusUnprocessableEntity, msgTooLarge)
		return
	}
	sec := int(duration)
	if err := h.insertMeeting(ctx, id, u.ID, "upload", "queued", rel, in, &sec); err != nil {
		fail(http.StatusInternalServerError, "Terjadi kesalahan pada server.")
		slog.Error("insert uploaded meeting", "meeting_id", id, "err", err)
		return
	}
	web.JSON(w, http.StatusCreated, map[string]string{"id": id, "status": "queued"})
}

type recordingInput struct {
	meetingInput
	ConsentConfirmed bool   `json:"consent_confirmed"`
	MimeType         string `json:"mime_type"`
}

func (h *Handler) StartRecording(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.CanCreateMeetings() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	var in recordingInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Isian tidak valid.")
		return
	}
	if !in.ConsentConfirmed {
		web.Error(w, http.StatusUnprocessableEntity, msgNoConsent)
		return
	}
	ext := map[string]string{"audio/webm": "webm", "audio/mp4": "mp4"}[in.MimeType]
	if ext == "" {
		web.Error(w, http.StatusUnprocessableEntity, "Browser tidak mendukung format rekaman.")
		return
	}
	if msg, err := h.validateInput(r.Context(), &in.meetingInput, u.ID); err != nil {
		web.Internal(w, err)
		return
	} else if msg != "" {
		web.Error(w, http.StatusUnprocessableEntity, msg)
		return
	}

	id, err := h.newID(r.Context())
	if err != nil {
		web.Internal(w, err)
		return
	}
	rel := filepath.Join("audio", id+"."+ext)
	f, err := os.Create(h.audioPath(rel))
	if err != nil {
		web.Internal(w, err)
		return
	}
	f.Close()
	if err := h.insertMeeting(r.Context(), id, u.ID, "browser", "recording", rel, in.meetingInput, nil); err != nil {
		os.Remove(h.audioPath(rel))
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusCreated, map[string]string{"id": id})
}

// AppendChunk enforces strict ordering: only seq == last_chunk_seq + 1 is written.
func (h *Handler) AppendChunk(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil || seq < 1 {
		web.Error(w, http.StatusBadRequest, "Nomor urut tidak valid.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunkBytes))
	if err != nil {
		web.Error(w, http.StatusRequestEntityTooLarge, "Potongan audio melebihi 5 MB.")
		return
	}
	if len(body) == 0 {
		web.Error(w, http.StatusBadRequest, "Potongan audio kosong.")
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var status, rel string
	var last int
	err = tx.QueryRow(ctx, `SELECT status, last_chunk_seq, audio_path FROM meetings WHERE id = $1 FOR UPDATE`, id).Scan(&status, &last, &rel)
	if errors.Is(err, pgx.ErrNoRows) {
		web.NotFound(w)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	if status != "recording" {
		web.Error(w, http.StatusConflict, msgRecordingEnd)
		return
	}
	if seq != last+1 {
		web.JSON(w, http.StatusConflict, map[string]any{"error": "Urutan potongan tidak sesuai.", "expected_seq": last + 1})
		return
	}

	f, err := os.OpenFile(h.audioPath(rel), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		web.Internal(w, err)
		return
	}
	undo := func() {
		if err := f.Truncate(info.Size()); err != nil {
			slog.Error("truncate after failed chunk", "meeting_id", id, "seq", seq, "err", err)
		}
	}
	if _, err := f.Write(body); err != nil {
		undo()
		web.Internal(w, err)
		return
	}
	if err := f.Sync(); err != nil {
		undo()
		web.Internal(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE meetings SET last_chunk_seq = $2 WHERE id = $1`, id, seq); err != nil {
		undo()
		web.Internal(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		undo()
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]int{"last_chunk_seq": seq})
}

func (h *Handler) FinishRecording(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var status, rel string
	if err := tx.QueryRow(ctx, `SELECT status, audio_path FROM meetings WHERE id = $1 FOR UPDATE`, id).Scan(&status, &rel); err != nil {
		web.Internal(w, err)
		return
	}
	if status != "recording" {
		web.Error(w, http.StatusConflict, msgRecordingEnd)
		return
	}
	info, err := os.Stat(h.audioPath(rel))
	if err != nil {
		web.Internal(w, err)
		return
	}
	if info.Size() == 0 {
		web.Error(w, http.StatusUnprocessableEntity, msgEmptyRecord)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE meetings SET status = 'queued', queued_at = $2, updated_at = $2 WHERE id = $1`, id, h.Now()); err != nil {
		web.Internal(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]string{"status": "queued"})
}
