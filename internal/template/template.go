package template

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/auth"
	"notulensi/web"
)

// Section follows Meetily's templates/*.json section shape.
type Section struct {
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
	Format      string `json:"format"`
	ItemFormat  string `json:"item_format,omitempty"`
}

type Template struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Sections    []Section `json:"sections"`
	IsDefault   bool      `json:"is_default"`
	InUse       bool      `json:"in_use"`
}

var seed = Template{
	Name:        "Rapat Bulanan Manajemen",
	Description: "Notulen rapat bulanan manajer dan direksi.",
	Sections: []Section{
		{Title: "Ringkasan", Format: "paragraph",
			Instruction: "Tulis satu paragraf ringkasan seluruh rapat: tujuan, topik utama, dan hasil pentingnya."},
		{Title: "Peserta yang Disebut", Format: "list",
			Instruction: "Daftar nama dan jabatan orang yang disebut dalam percakapan. Jangan menebak nama yang tidak disebut."},
		{Title: "Pokok Pembahasan", Format: "list",
			Instruction: "Setiap topik yang dibahas beserta poin-poin pentingnya. Sertakan waktu di rekaman dalam format [mm:ss] bila ada."},
		{Title: "Keputusan", Format: "list",
			Instruction: "Setiap keputusan yang diambil dalam rapat, ditulis jelas dan singkat."},
		{Title: "Tindak Lanjut", Format: "table",
			Instruction: "Setiap tugas yang diberikan: siapa penanggung jawabnya, tugasnya, tenggat waktunya, dan waktu di rekaman.",
			ItemFormat:  "| PIC | Tugas | Tenggat | Waktu di Rekaman |"},
	},
}

// SeedDefault inserts the default template on first start.
func SeedDefault(ctx context.Context, pool *pgxpool.Pool) error {
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM templates`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO templates (name, description, sections, is_default) VALUES ($1, $2, $3, true)`,
		seed.Name, seed.Description, seed.Sections)
	return err
}

// Load returns one template's sections, used by the worker.
func Load(ctx context.Context, pool *pgxpool.Pool, id string) ([]Section, error) {
	var sections []Section
	err := pool.QueryRow(ctx, `SELECT sections FROM templates WHERE id = $1`, id).Scan(&sections)
	return sections, err
}

var formats = map[string]bool{"paragraph": true, "list": true, "table": true}

func runes(s string) int { return utf8.RuneCountInString(s) }

// Validate returns the first FRD FR-008 message that applies, or "".
func Validate(t Template) string {
	if runes(t.Name) < 1 || runes(t.Name) > 100 {
		return "Nama template wajib diisi, maksimal 100 karakter."
	}
	if runes(t.Description) > 500 {
		return "Deskripsi maksimal 500 karakter."
	}
	if len(t.Sections) == 0 {
		return "Template harus memiliki minimal satu bagian."
	}
	for _, s := range t.Sections {
		if runes(s.Title) < 1 || runes(s.Title) > 100 {
			return "Judul bagian wajib diisi, maksimal 100 karakter."
		}
		if runes(s.Instruction) < 1 || runes(s.Instruction) > 1000 {
			return "Instruksi bagian wajib diisi, maksimal 1000 karakter."
		}
		if !formats[s.Format] {
			return "Format bagian harus paragraf, daftar, atau tabel."
		}
		if s.Format == "table" && s.ItemFormat == "" {
			return "Bagian bertipe tabel wajib memiliki format kolom."
		}
		if runes(s.ItemFormat) > 500 {
			return "Format kolom maksimal 500 karakter."
		}
	}
	return ""
}

type Handler struct {
	Pool   *pgxpool.Pool
	Render *web.Renderer
}

func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		http.NotFound(w, r)
		return
	}
	h.Render.Render(w, r, "admin_templates.html", web.Page{Title: "Template", Active: "templates"})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).CanCreateMeetings() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	rows, err := h.Pool.Query(r.Context(), `
		SELECT t.id, t.name, coalesce(t.description, ''), t.sections, t.is_default,
		       EXISTS (SELECT 1 FROM meetings m WHERE m.template_id = t.id)
		FROM templates t ORDER BY t.is_default DESC, t.name`)
	if err != nil {
		web.Internal(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Template, error) {
		var t Template
		err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Sections, &t.IsDefault, &t.InUse)
		return t, err
	})
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	var t Template
	if err := web.Decode(r, &t); err != nil {
		web.Error(w, http.StatusBadRequest, "Periksa kembali isian yang ditandai.")
		return
	}
	if msg := Validate(t); msg != "" {
		web.Error(w, http.StatusUnprocessableEntity, msg)
		return
	}
	err := h.Pool.QueryRow(r.Context(),
		`INSERT INTO templates (name, description, sections) VALUES ($1, $2, $3) RETURNING id`,
		t.Name, t.Description, t.Sections).Scan(&t.ID)
	if isUniqueViolation(err) {
		web.Error(w, http.StatusConflict, "Nama template sudah dipakai.")
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusCreated, t)
}

type updateInput struct {
	Template
	SetDefault bool `json:"set_default"`
}

// Update saves name, description, and sections, or with set_default only moves the default flag.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	id := r.PathValue("id")
	if !auth.ValidUUID(id) {
		web.NotFound(w)
		return
	}
	var in updateInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Periksa kembali isian yang ditandai.")
		return
	}

	if in.SetDefault {
		tx, err := h.Pool.Begin(r.Context())
		if err != nil {
			web.Internal(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err := tx.Exec(r.Context(), `UPDATE templates SET is_default = false WHERE is_default AND id <> $1`, id); err != nil {
			web.Internal(w, err)
			return
		}
		tag, err := tx.Exec(r.Context(), `UPDATE templates SET is_default = true, updated_at = now() WHERE id = $1`, id)
		if err != nil {
			web.Internal(w, err)
			return
		}
		if tag.RowsAffected() == 0 {
			web.NotFound(w)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			web.Internal(w, err)
			return
		}
		web.JSON(w, http.StatusOK, map[string]any{"id": id, "is_default": true})
		return
	}

	if msg := Validate(in.Template); msg != "" {
		web.Error(w, http.StatusUnprocessableEntity, msg)
		return
	}
	tag, err := h.Pool.Exec(r.Context(),
		`UPDATE templates SET name = $2, description = $3, sections = $4, updated_at = now() WHERE id = $1`,
		id, in.Name, in.Description, in.Sections)
	if isUniqueViolation(err) {
		web.Error(w, http.StatusConflict, "Nama template sudah dipakai.")
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		web.NotFound(w)
		return
	}
	in.ID = id
	web.JSON(w, http.StatusOK, in.Template)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	id := r.PathValue("id")
	if !auth.ValidUUID(id) {
		web.NotFound(w)
		return
	}
	var isDefault, inUse bool
	err := h.Pool.QueryRow(r.Context(), `
		SELECT is_default, EXISTS (SELECT 1 FROM meetings WHERE template_id = $1)
		FROM templates WHERE id = $1`, id).Scan(&isDefault, &inUse)
	if errors.Is(err, pgx.ErrNoRows) {
		web.NotFound(w)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	if isDefault {
		web.Error(w, http.StatusUnprocessableEntity, "Template default tidak bisa dihapus.")
		return
	}
	if inUse {
		web.Error(w, http.StatusUnprocessableEntity, "Template sedang dipakai oleh rapat dan tidak bisa dihapus.")
		return
	}
	if _, err := h.Pool.Exec(r.Context(), `DELETE FROM templates WHERE id = $1`, id); err != nil {
		web.Internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
