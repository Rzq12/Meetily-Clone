package audit

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/auth"
	"notulensi/web"
)

var Actions = []string{
	"login_success", "login_failed", "meeting_view", "minutes_edit", "regenerate",
	"export", "participants_change", "meeting_delete", "user_change",
}

// Write stores one entry. A failed insert is logged and never fails the request (FR-015).
// detail must never contain transcript or minutes text.
func Write(ctx context.Context, pool *pgxpool.Pool, userID, action, meetingID, ip string, detail map[string]any) {
	_, err := pool.Exec(ctx,
		`INSERT INTO audit_logs (user_id, action, meeting_id, ip_address, detail) VALUES ($1, $2, $3, $4, $5)`,
		nullable(userID), action, nullable(meetingID), ip, detail)
	if err != nil {
		slog.Error("audit write", "action", action, "meeting_id", meetingID, "err", err)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ClientIP trusts X-Forwarded-For because the app is only reachable through Caddy.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
	h.Render.Render(w, r, "admin_audit.html", web.Page{Title: "Audit", Active: "audit", Data: Actions})
}

type entry struct {
	CreatedAt    time.Time `json:"created_at"`
	UserFullName *string   `json:"user_full_name"`
	Action       string    `json:"action"`
	MeetingTitle *string   `json:"meeting_title"`
	IPAddress    string    `json:"ip_address"`
}

const auditPageSize = 50

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).IsAdmin() {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return
	}
	q := r.URL.Query()
	from, to, ok := web.ParseDateRange(q.Get("date_from"), q.Get("date_to"))
	if !ok {
		web.Error(w, http.StatusBadRequest, "Rentang tanggal tidak valid.")
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	where := []string{"true"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if v := q.Get("user_id"); v != "" {
		if !auth.ValidUUID(v) {
			web.Error(w, http.StatusBadRequest, "Pengguna tidak valid.")
			return
		}
		add("a.user_id = ?", v)
	}
	if v := q.Get("action"); v != "" {
		add("a.action = ?", v)
	}
	if !from.IsZero() {
		add("a.created_at >= ?", from)
	}
	if !to.IsZero() {
		add("a.created_at < ?", to.AddDate(0, 0, 1))
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := h.Pool.QueryRow(r.Context(), `SELECT count(*) FROM audit_logs a WHERE `+cond, args...).Scan(&total); err != nil {
		web.Internal(w, err)
		return
	}

	args = append(args, auditPageSize, (page-1)*auditPageSize)
	rows, err := h.Pool.Query(r.Context(), `
		SELECT a.created_at, u.full_name, a.action, m.title, a.ip_address
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.user_id
		LEFT JOIN meetings m ON m.id = a.meeting_id
		WHERE `+cond+`
		ORDER BY a.created_at DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		web.Internal(w, err)
		return
	}
	defer rows.Close()
	items := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.CreatedAt, &e.UserFullName, &e.Action, &e.MeetingTitle, &e.IPAddress); err != nil {
			web.Internal(w, err)
			return
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page_size": auditPageSize})
}
