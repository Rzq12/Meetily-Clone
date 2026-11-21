package meeting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"notulensi/internal/audit"
	"notulensi/internal/auth"
	"notulensi/web"
)

type minutesInput struct {
	MinutesMarkdown string    `json:"minutes_markdown"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// SaveMinutes uses updated_at as an optimistic lock: a stale value gets 409 (FR-009).
func (h *Handler) SaveMinutes(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	var in minutesInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Isian tidak valid.")
		return
	}
	if strings.TrimSpace(in.MinutesMarkdown) == "" {
		web.Error(w, http.StatusUnprocessableEntity, "Notulen tidak boleh kosong.")
		return
	}
	if utf8.RuneCountInString(in.MinutesMarkdown) > 200000 {
		web.Error(w, http.StatusUnprocessableEntity, "Notulen maksimal 200.000 karakter.")
		return
	}
	u := auth.UserFrom(r.Context())
	var updatedAt time.Time
	err := h.Pool.QueryRow(r.Context(), `
		UPDATE meetings SET minutes_markdown = $2, minutes_edited_by = $3, minutes_edited_at = $4, updated_at = $4
		WHERE id = $1 AND status = 'done' AND updated_at = $5
		RETURNING updated_at`, id, in.MinutesMarkdown, u.ID, h.Now(), in.UpdatedAt).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		web.Error(w, http.StatusConflict, "Notulen sudah diubah oleh pengguna lain. Muat ulang halaman.")
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	audit.Write(r.Context(), h.Pool, u.ID, "minutes_edit", id, audit.ClientIP(r), nil)
	web.JSON(w, http.StatusOK, map[string]any{"updated_at": updatedAt, "minutes_html": RenderMinutes(in.MinutesMarkdown)})
}

type regenerateInput struct {
	TemplateID string `json:"template_id"`
}

func (h *Handler) Regenerate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	var in regenerateInput
	if err := web.Decode(r, &in); err != nil || !auth.ValidUUID(in.TemplateID) {
		web.Error(w, http.StatusUnprocessableEntity, "Template tidak ditemukan.")
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE meetings SET minutes_backup = minutes_markdown, template_id = t.id,
		       skip_transcription = true, status = 'queued', queued_at = $3, error_message = NULL, updated_at = $3
		FROM templates t
		WHERE meetings.id = $1 AND t.id = $2 AND meetings.status = 'done'`, id, in.TemplateID, h.Now())
	if err != nil {
		web.Internal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		web.Error(w, http.StatusConflict, "Notulen hanya bisa dibuat ulang setelah selesai diproses.")
		return
	}
	audit.Write(r.Context(), h.Pool, auth.UserFrom(r.Context()).ID, "regenerate", id, audit.ClientIP(r),
		map[string]any{"template_id": in.TemplateID})
	web.JSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func (h *Handler) RestoreBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE meetings SET minutes_markdown = minutes_backup, minutes_backup = minutes_markdown, updated_at = $2
		WHERE id = $1 AND status = 'done' AND minutes_backup IS NOT NULL`, id, h.Now())
	if err != nil {
		web.Internal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		web.Error(w, http.StatusConflict, "Tidak ada versi sebelumnya.")
		return
	}
	web.JSON(w, http.StatusOK, map[string]string{"status": "done"})
}

// Retry skips transcription when segments already exist, so an LLM failure does not re-run whisper.
func (h *Handler) Retry(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE meetings SET status = 'queued', queued_at = $2, updated_at = $2,
		       skip_transcription = EXISTS (SELECT 1 FROM transcript_segments s WHERE s.meeting_id = $1)
		WHERE id = $1 AND status = 'failed'`, id, h.Now())
	if err != nil {
		web.Internal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		web.Error(w, http.StatusConflict, "Hanya rapat yang gagal yang bisa diproses ulang.")
		return
	}
	web.JSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

type participantsInput struct {
	ParticipantIDs []string `json:"participant_ids"`
}

func (h *Handler) SetParticipants(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	var in participantsInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Isian tidak valid.")
		return
	}
	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM meetings WHERE id = $1 FOR UPDATE`, id).Scan(&ownerID); err != nil {
		web.Internal(w, err)
		return
	}
	msg, err := validateParticipants(ctx, tx, in.ParticipantIDs, ownerID)
	if err != nil {
		web.Internal(w, err)
		return
	}
	if msg != "" {
		web.Error(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM meeting_participants WHERE meeting_id = $1`, id); err != nil {
		web.Internal(w, err)
		return
	}
	for _, pid := range in.ParticipantIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO meeting_participants (meeting_id, user_id) VALUES ($1, $2)`, id, pid); err != nil {
			web.Internal(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		web.Internal(w, err)
		return
	}
	audit.Write(ctx, h.Pool, auth.UserFrom(ctx).ID, "participants_change", id, audit.ClientIP(r),
		map[string]any{"participant_count": len(in.ParticipantIDs)})

	d, err := h.load(ctx, id)
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"participants": d.Participants})
}

type deleteInput struct {
	ConfirmTitle string `json:"confirm_title"`
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.editAccess(w, r)
	if !ok {
		return
	}
	var in deleteInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Isian tidak valid.")
		return
	}
	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var title, status, rel string
	if err := tx.QueryRow(ctx, `SELECT title, status, audio_path FROM meetings WHERE id = $1 FOR UPDATE`, id).Scan(&title, &status, &rel); err != nil {
		web.Internal(w, err)
		return
	}
	if isProcessing(status) {
		web.Error(w, http.StatusConflict, "Rapat sedang diproses dan tidak bisa dihapus.")
		return
	}
	if in.ConfirmTitle != title {
		web.Error(w, http.StatusUnprocessableEntity, "Judul yang diketik tidak sesuai.")
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM meetings WHERE id = $1`, id); err != nil {
		web.Internal(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		web.Internal(w, err)
		return
	}
	if err := os.Remove(h.audioPath(rel)); err != nil && !os.IsNotExist(err) {
		slog.Error("remove audio of deleted meeting", "meeting_id", id, "err", err)
	}
	audit.Write(ctx, h.Pool, auth.UserFrom(ctx).ID, "meeting_delete", id, audit.ClientIP(r), map[string]any{"title": title})
	web.JSON(w, http.StatusOK, map[string]string{"redirect": "/meetings"})
}

var audioTypes = map[string]string{
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".aac": "audio/aac",
	".ogg": "audio/ogg", ".flac": "audio/flac", ".webm": "audio/webm", ".mp4": "audio/mp4",
}

// Audio streams the file with range support so the player can seek.
func (h *Handler) Audio(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.access(w, r)
	if !ok {
		return
	}
	var rel string
	var deletedAt *time.Time
	var updatedAt time.Time
	if err := h.Pool.QueryRow(r.Context(), `SELECT audio_path, audio_deleted_at, updated_at FROM meetings WHERE id = $1`, id).
		Scan(&rel, &deletedAt, &updatedAt); err != nil {
		web.Internal(w, err)
		return
	}
	if deletedAt != nil {
		web.NotFound(w)
		return
	}
	f, err := os.Open(h.audioPath(rel))
	if os.IsNotExist(err) {
		web.NotFound(w)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", audioTypes[filepath.Ext(rel)])
	http.ServeContent(w, r, "", updatedAt, f)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.access(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "docx" && format != "md" {
		web.Error(w, http.StatusBadRequest, "Format ekspor harus docx atau md.")
		return
	}
	d, err := h.load(r.Context(), id)
	if err != nil {
		web.Internal(w, err)
		return
	}
	if d.MinutesMarkdown == nil {
		web.Error(w, http.StatusConflict, "Notulen belum tersedia.")
		return
	}
	doc := exportMarkdown(d)
	name := fmt.Sprintf("Notulen-%s-%s.%s", d.MeetingDate.Format("2006-01-02"), slug(d.Title), format)

	var body []byte
	contentType := "text/markdown; charset=utf-8"
	if format == "docx" {
		body, err = h.pandocDocx(r.Context(), doc)
		if err != nil {
			slog.Error("pandoc export", "meeting_id", id, "err", err)
			web.Error(w, http.StatusInternalServerError, "Ekspor gagal. Coba lagi atau unduh versi Markdown.")
			return
		}
		contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	} else {
		body = []byte(doc)
	}
	audit.Write(r.Context(), h.Pool, auth.UserFrom(r.Context()).ID, "export", id, audit.ClientIP(r), map[string]any{"format": format})
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(body)
}

// exportMarkdown adds the FR-011 header block above the minutes.
func exportMarkdown(d *detail) string {
	names := make([]string, len(d.Participants))
	for i, p := range d.Participants {
		names[i] = p.FullName
	}
	peserta := "-"
	if len(names) > 0 {
		peserta = strings.Join(names, ", ")
	}
	return fmt.Sprintf("# %s\n\n**Tanggal:** %s  \n**Notulis:** %s  \n**Peserta:** %s\n\n%s\n",
		d.Title, web.Tanggal(d.MeetingDate), d.OwnerName, peserta, *d.MinutesMarkdown)
}

func (h *Handler) pandocDocx(ctx context.Context, doc string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "export-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.md")
	out := filepath.Join(dir, "out.docx")
	if err := os.WriteFile(in, []byte(doc), 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pandoc", "-f", "gfm", "-t", "docx", "--reference-doc="+h.ReferenceDoc, "-o", out, in)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return os.ReadFile(out)
}
