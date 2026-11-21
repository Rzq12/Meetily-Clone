package meeting

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/audit"
	"notulensi/internal/auth"
	"notulensi/internal/llm"
	"notulensi/web"
)

type Handler struct {
	Pool         *pgxpool.Pool
	Render       *web.Renderer
	DataDir      string
	ReferenceDoc string
	Now          func() time.Time
	// CancelJob stops the worker's running job for a meeting; false when that meeting is not running.
	CancelJob func(meetingID string) bool
}

const listPageSize = 20

func isProcessing(status string) bool { return status == "transcribing" || status == "summarizing" }

// access resolves the path id; it writes 404 for no access and returns ok=false.
func (h *Handler) access(w http.ResponseWriter, r *http.Request) (id string, a auth.Access, ok bool) {
	id = r.PathValue("id")
	a, err := auth.MeetingAccess(r.Context(), h.Pool, auth.UserFrom(r.Context()), id)
	if err != nil {
		web.Internal(w, err)
		return id, a, false
	}
	if !a.CanView {
		web.NotFound(w)
		return id, a, false
	}
	return id, a, true
}

// editAccess is access plus BR-003: viewers who can see the meeting get 403.
func (h *Handler) editAccess(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, a, ok := h.access(w, r)
	if !ok {
		return id, false
	}
	if !a.CanEdit {
		web.Error(w, http.StatusForbidden, "Akses ditolak.")
		return id, false
	}
	return id, true
}

func (h *Handler) today() time.Time {
	now := h.Now().In(web.Jakarta)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, web.Jakarta)
}

// validateParticipants enforces FR-013: unique active users, not the owner, at most 100.
func validateParticipants(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ids []string, ownerID string) (string, error) {
	if len(ids) > 100 {
		return "Maksimal 100 peserta.", nil
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !auth.ValidUUID(id) || id == ownerID || seen[id] {
			return "Daftar peserta tidak valid.", nil
		}
		seen[id] = true
	}
	if len(ids) == 0 {
		return "", nil
	}
	var active int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = ANY($1) AND is_active`, ids).Scan(&active); err != nil {
		return "", err
	}
	if active != len(ids) {
		return "Pengguna tidak aktif tidak bisa ditambahkan.", nil
	}
	return "", nil
}

func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/meetings", http.StatusSeeOther)
}

func (h *Handler) ListPage(w http.ResponseWriter, r *http.Request) {
	h.Render.Render(w, r, "meetings.html", web.Page{Title: "Rapat", Active: "home"})
}

func (h *Handler) NewPage(w http.ResponseWriter, r *http.Request) {
	if !auth.UserFrom(r.Context()).CanCreateMeetings() {
		http.NotFound(w, r)
		return
	}
	mode := "upload"
	if r.URL.Query().Get("mode") == "record" {
		mode = "record"
	}
	h.Render.Render(w, r, "meeting_new.html", web.Page{
		Title: "Rapat Baru", Active: "new",
		Data: map[string]any{"Mode": mode, "Today": h.today().Format("2006-01-02")},
	})
}

type listItem struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	MeetingDate      time.Time `json:"meeting_date"`
	MeetingDateLabel string    `json:"meeting_date_label"`
	OwnerName        string    `json:"owner_name"`
	Status           string    `json:"status"`
	StatusLabel      string    `json:"status_label"`
	AudioDurationSec *int      `json:"audio_duration_sec"`
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))
	if len([]rune(text)) > 100 {
		web.Error(w, http.StatusBadRequest, "Kata kunci maksimal 100 karakter.")
		return
	}
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
	add := func(cond string, vals ...any) {
		for _, v := range vals {
			args = append(args, v)
			cond = strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1)
		}
		where = append(where, cond)
	}
	if !u.IsAdmin() {
		add("(m.owner_id = ? OR EXISTS (SELECT 1 FROM meeting_participants p WHERE p.meeting_id = m.id AND p.user_id = ?))", u.ID, u.ID)
	}
	if text != "" {
		add("m.title ILIKE ?", "%"+escapeLike(text)+"%")
	}
	if !from.IsZero() {
		add("m.meeting_date >= ?", from.Format("2006-01-02"))
	}
	if !to.IsZero() {
		add("m.meeting_date <= ?", to.Format("2006-01-02"))
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := h.Pool.QueryRow(r.Context(), `SELECT count(*) FROM meetings m WHERE `+cond, args...).Scan(&total); err != nil {
		web.Internal(w, err)
		return
	}
	args = append(args, listPageSize, (page-1)*listPageSize)
	rows, err := h.Pool.Query(r.Context(), `
		SELECT m.id, m.title, m.meeting_date, u.full_name, m.status, m.audio_duration_sec
		FROM meetings m JOIN users u ON u.id = m.owner_id
		WHERE `+cond+`
		ORDER BY m.meeting_date DESC, m.created_at DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		web.Internal(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (listItem, error) {
		var it listItem
		err := row.Scan(&it.ID, &it.Title, &it.MeetingDate, &it.OwnerName, &it.Status, &it.AudioDurationSec)
		it.MeetingDateLabel = web.Tanggal(it.MeetingDate)
		it.StatusLabel = web.StatusLabel(it.Status)
		return it, err
	})
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page_size": listPageSize})
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

type participant struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

type detail struct {
	ID               string        `json:"id"`
	Title            string        `json:"title"`
	MeetingDate      time.Time     `json:"meeting_date"`
	MeetingDateLabel string        `json:"meeting_date_label"`
	OwnerID          string        `json:"owner_id"`
	OwnerName        string        `json:"owner_name"`
	TemplateID       string        `json:"template_id"`
	TemplateName     string        `json:"template_name"`
	Source           string        `json:"source"`
	Status           string        `json:"status"`
	StatusLabel      string        `json:"status_label"`
	QueuePosition    int           `json:"queue_position,omitempty"`
	ErrorMessage     *string       `json:"error_message"`
	AudioDurationSec *int          `json:"audio_duration_sec"`
	AudioDeleted     bool          `json:"audio_deleted"`
	MinutesMarkdown  *string       `json:"minutes_markdown"`
	SummaryContext   string        `json:"summary_context"`
	MinutesHTML      template.HTML `json:"minutes_html"`
	HasBackup        bool          `json:"has_backup"`
	MinutesEditedBy  *string       `json:"minutes_edited_by"`
	MinutesEditedAt  *time.Time    `json:"minutes_edited_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	CanEdit          bool          `json:"can_edit"`
	Participants     []participant `json:"participants"`
}

func (h *Handler) load(ctx context.Context, id string) (*detail, error) {
	d := &detail{}
	var audioDeletedAt *time.Time
	var backup *string
	err := h.Pool.QueryRow(ctx, `
		SELECT m.id, m.title, m.meeting_date, m.owner_id, o.full_name, m.template_id, t.name,
		       m.source, m.status, m.error_message, m.audio_duration_sec, m.audio_deleted_at,
		       m.minutes_markdown, m.minutes_backup, e.full_name, m.minutes_edited_at, m.updated_at,
		       coalesce(m.summary_context, '')
		FROM meetings m
		JOIN users o ON o.id = m.owner_id
		JOIN templates t ON t.id = m.template_id
		LEFT JOIN users e ON e.id = m.minutes_edited_by
		WHERE m.id = $1`, id).Scan(
		&d.ID, &d.Title, &d.MeetingDate, &d.OwnerID, &d.OwnerName, &d.TemplateID, &d.TemplateName,
		&d.Source, &d.Status, &d.ErrorMessage, &d.AudioDurationSec, &audioDeletedAt,
		&d.MinutesMarkdown, &backup, &d.MinutesEditedBy, &d.MinutesEditedAt, &d.UpdatedAt,
		&d.SummaryContext)
	if err != nil {
		return nil, err
	}
	d.MeetingDateLabel = web.Tanggal(d.MeetingDate)
	d.StatusLabel = web.StatusLabel(d.Status)
	d.AudioDeleted = audioDeletedAt != nil
	d.HasBackup = backup != nil
	if d.MinutesMarkdown != nil {
		d.MinutesHTML = RenderMinutes(*d.MinutesMarkdown)
	}
	if d.Status == "queued" {
		if err := h.Pool.QueryRow(ctx, `
			SELECT count(*) + 1 FROM meetings
			WHERE status = 'queued' AND queued_at < (SELECT queued_at FROM meetings WHERE id = $1)`, id).
			Scan(&d.QueuePosition); err != nil {
			return nil, err
		}
	}
	rows, err := h.Pool.Query(ctx, `
		SELECT u.id, u.full_name, u.role FROM meeting_participants p
		JOIN users u ON u.id = p.user_id WHERE p.meeting_id = $1 ORDER BY u.full_name`, id)
	if err != nil {
		return nil, err
	}
	d.Participants, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (participant, error) {
		var p participant
		err := row.Scan(&p.ID, &p.FullName, &p.Role)
		return p, err
	})
	return d, err
}

func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	id, a, ok := h.access(w, r)
	if !ok {
		return
	}
	d, err := h.load(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		web.NotFound(w)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	d.CanEdit = a.CanEdit
	web.JSON(w, http.StatusOK, d)
}

func (h *Handler) DetailPage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	a, err := auth.MeetingAccess(r.Context(), h.Pool, u, id)
	if err != nil {
		web.Internal(w, err)
		return
	}
	if !a.CanView {
		http.NotFound(w, r)
		return
	}
	d, err := h.load(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		web.Internal(w, err)
		return
	}
	d.CanEdit = a.CanEdit
	audit.Write(r.Context(), h.Pool, u.ID, "meeting_view", id, audit.ClientIP(r), nil)
	h.Render.Render(w, r, "meeting.html", web.Page{Title: d.Title, Active: "meeting:" + id, Data: d})
}

type segment struct {
	Seq      int     `json:"seq"`
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
	Text     string  `json:"text"`
	Label    string  `json:"label"`
}

func (h *Handler) Transcript(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.access(w, r)
	if !ok {
		return
	}
	rows, err := h.Pool.Query(r.Context(),
		`SELECT seq, start_sec, end_sec, text FROM transcript_segments WHERE meeting_id = $1 ORDER BY seq`, id)
	if err != nil {
		web.Internal(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (segment, error) {
		var s segment
		var start, end float32
		err := row.Scan(&s.Seq, &start, &end, &s.Text)
		s.StartSec, s.EndSec = float64(start), float64(end)
		s.Label = llm.Timestamp(s.StartSec)
		return s, err
	})
	if err != nil {
		web.Internal(w, err)
		return
	}
	web.JSON(w, http.StatusOK, map[string]any{"items": items})
}
