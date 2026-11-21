package user

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/audit"
	"notulensi/internal/auth"
	"notulensi/web"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._]{3,50}$`)

var roles = map[string]bool{"admin": true, "notetaker": true, "viewer": true}

const msgInvalid = "Periksa kembali isian yang ditandai."

type Handler struct {
	Pool   *pgxpool.Pool
	Auth   *auth.Service
	Render *web.Renderer
}

type userRow struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
	IsActive bool   `json:"is_active"`
}

// SeedAdmin creates the first Admin from env vars when the users table is empty.
func SeedAdmin(ctx context.Context, pool *pgxpool.Pool, username, password string) error {
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if !usernamePattern.MatchString(username) || !validPassword(password) {
		return errors.New("ADMIN_USERNAME or ADMIN_PASSWORD is missing or invalid")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO users (username, full_name, password_hash, role) VALUES ($1, 'Administrator', $2, 'admin')`,
		username, hash)
	return err
}

func validPassword(p string) bool {
	n := utf8.RuneCountInString(p)
	return n >= 8 && n <= 128
}

func validFullName(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= 255
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		http.NotFound(w, r)
		return
	}
	h.Render.Render(w, r, "admin_users.html", web.Page{Title: "Pengguna", Active: "users"})
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	web.JSON(w, http.StatusOK, auth.UserFrom(r.Context()))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	rows, err := h.Pool.Query(r.Context(),
		`SELECT id, username, full_name, role, is_active FROM users ORDER BY full_name`)
	if err != nil {
		web.Internal(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (userRow, error) {
		var u userRow
		err := row.Scan(&u.ID, &u.Username, &u.FullName, &u.Role, &u.IsActive)
		return u, err
	})
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items})
}

type option struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

// Options lists active users for the participant picker.
func (h *Handler) Options(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).CanCreateMeetings() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	rows, err := h.Pool.Query(r.Context(),
		`SELECT id, full_name, role FROM users WHERE is_active ORDER BY full_name`)
	if err != nil {
		web.Internal(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (option, error) {
		var o option
		err := row.Scan(&o.ID, &o.FullName, &o.Role)
		return o, err
	})
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items})
}

type createInput struct {
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	actor := auth.UserFrom(r.Context())
	if !actor.IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	var in createInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, msgInvalid)
		return
	}
	fields := map[string]string{}
	if !usernamePattern.MatchString(in.Username) {
		fields["username"] = "3–50 karakter: huruf, angka, titik, atau garis bawah."
	}
	if !validFullName(in.FullName) {
		fields["full_name"] = "Wajib diisi, maksimal 255 karakter."
	}
	if !roles[in.Role] {
		fields["role"] = "Pilih peran."
	}
	if !validPassword(in.Password) {
		fields["password"] = "8–128 karakter."
	}
	if len(fields) > 0 {
		web.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": msgInvalid, "fields": fields})
		return
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		web.Internal(w, err)
		return
	}
	var id string
	err = h.Pool.QueryRow(r.Context(),
		`INSERT INTO users (username, full_name, password_hash, role) VALUES ($1, $2, $3, $4) RETURNING id`,
		in.Username, in.FullName, hash, in.Role).Scan(&id)
	if isUniqueViolation(err) {
		web.JSON(w, http.StatusConflict, map[string]any{"error": "Username sudah dipakai.", "fields": map[string]string{"username": "Username sudah dipakai."}})
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	audit.Write(r.Context(), h.Pool, actor.ID, "user_change", "", audit.ClientIP(r),
		map[string]any{"target_user_id": id, "change": "create", "role": in.Role})
	web.JSON(w, http.StatusCreated, userRow{ID: id, Username: in.Username, FullName: in.FullName, Role: in.Role, IsActive: true})
}

type updateInput struct {
	FullName string `json:"full_name"`
	Role     string `json:"role"`
	IsActive bool   `json:"is_active"`
	Password string `json:"password"`
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	actor := auth.UserFrom(r.Context())
	if !actor.IsAdmin() {
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
		web.Error(w, http.StatusBadRequest, msgInvalid)
		return
	}
	fields := map[string]string{}
	if !validFullName(in.FullName) {
		fields["full_name"] = "Wajib diisi, maksimal 255 karakter."
	}
	if !roles[in.Role] {
		fields["role"] = "Pilih peran."
	}
	if in.Password != "" && !validPassword(in.Password) {
		fields["password"] = "8–128 karakter."
	}
	if len(fields) > 0 {
		web.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": msgInvalid, "fields": fields})
		return
	}

	tx, err := h.Pool.Begin(r.Context())
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	var cur userRow
	err = tx.QueryRow(r.Context(),
		`SELECT id, username, full_name, role, is_active FROM users WHERE id = $1 FOR UPDATE`, id).
		Scan(&cur.ID, &cur.Username, &cur.FullName, &cur.Role, &cur.IsActive)
	if errors.Is(err, pgx.ErrNoRows) {
		web.NotFound(w)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	if id == actor.ID && (in.Role != cur.Role || !in.IsActive) {
		web.Error(w, http.StatusUnprocessableEntity, "Tidak bisa mengubah akun sendiri.")
		return
	}

	if _, err := tx.Exec(r.Context(),
		`UPDATE users SET full_name = $2, role = $3, is_active = $4, updated_at = now() WHERE id = $1`,
		id, in.FullName, in.Role, in.IsActive); err != nil {
		web.Internal(w, err)
		return
	}
	if in.Password != "" {
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			web.Internal(w, err)
			return
		}
		if _, err := tx.Exec(r.Context(), `UPDATE users SET password_hash = $2 WHERE id = $1`, id, hash); err != nil {
			web.Internal(w, err)
			return
		}
	}
	if in.Password != "" || !in.IsActive {
		if _, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
			web.Internal(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		web.Internal(w, err)
		return
	}

	audit.Write(r.Context(), h.Pool, actor.ID, "user_change", "", audit.ClientIP(r), map[string]any{
		"target_user_id": id, "change": "update", "role": in.Role, "is_active": in.IsActive, "password_reset": in.Password != "",
	})
	web.JSON(w, http.StatusOK, userRow{ID: id, Username: cur.Username, FullName: in.FullName, Role: in.Role, IsActive: in.IsActive})
}
