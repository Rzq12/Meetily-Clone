package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"notulensi/internal/auth"
	"notulensi/internal/testutil"
)

func setPassword(t *testing.T, s *auth.Service, userID, password string) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
		t.Fatal(err)
	}
}

func TestLoginLockoutAfterFiveFailures(t *testing.T) {
	pool := testutil.DB(t)
	u := testutil.User(t, pool, "sekretariat", "notetaker")
	s := auth.NewService(pool)
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	setPassword(t, s, u.ID, "Rapat2026!")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, _, _, err := s.Login(ctx, "sekretariat", "salah-sekali"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, _, _, err := s.Login(ctx, "sekretariat", "Rapat2026!"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("correct password while locked: %v, want ErrLocked", err)
	}
	now = now.Add(16 * time.Minute)
	if _, _, _, err := s.Login(ctx, "sekretariat", "Rapat2026!"); err != nil {
		t.Fatalf("after 15 minutes: %v", err)
	}
}

func TestDeactivatedUserCannotLogIn(t *testing.T) {
	pool := testutil.DB(t)
	u := testutil.User(t, pool, "mantan", "viewer")
	s := auth.NewService(pool)
	setPassword(t, s, u.ID, "Rapat2026!")
	pool.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.ID)
	_, _, known, err := s.Login(context.Background(), "mantan", "Rapat2026!")
	if !errors.Is(err, auth.ErrInvalidCredentials) || known != u.ID {
		t.Fatalf("err=%v known=%q", err, known)
	}
}

func protected(s *auth.Service) http.Handler {
	return s.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(auth.UserFrom(r.Context()).Username))
	}))
}

func TestRequireSessionIdleTimeoutAndCSRF(t *testing.T) {
	pool := testutil.DB(t)
	u := testutil.User(t, pool, "direktur", "viewer")
	s := auth.NewService(pool)
	now := time.Now()
	s.Now = func() time.Time { return now }
	setPassword(t, s, u.ID, "Rapat2026!")
	token, _, _, err := s.Login(context.Background(), "direktur", "Rapat2026!")
	if err != nil {
		t.Fatal(err)
	}
	h := protected(s)
	send := func(method, path, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := send("GET", "/meetings", ""); rec.Code != 200 || rec.Body.String() != "direktur" {
		t.Fatalf("valid session: %d %q", rec.Code, rec.Body.String())
	}
	if rec := send("POST", "/api/x", ""); rec.Code != 403 {
		t.Fatalf("POST without CSRF: %d, want 403", rec.Code)
	}
	if rec := send("POST", "/api/x", auth.CSRFToken(token)); rec.Code != 200 {
		t.Fatalf("POST with CSRF: %d", rec.Code)
	}

	now = now.Add(7 * time.Hour)
	if rec := send("GET", "/meetings", ""); rec.Code != 200 {
		t.Fatalf("activity within 8 hours keeps the session: %d", rec.Code)
	}
	now = now.Add(8*time.Hour + time.Minute)
	rec := send("GET", "/meetings", "")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?expired=1" {
		t.Fatalf("idle session: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := send("GET", "/api/meetings", ""); rec.Code != 401 {
		t.Fatalf("API with expired session: %d, want 401", rec.Code)
	}
}

func TestDeactivationEndsSession(t *testing.T) {
	pool := testutil.DB(t)
	u := testutil.User(t, pool, "manajer", "viewer")
	s := auth.NewService(pool)
	setPassword(t, s, u.ID, "Rapat2026!")
	token, _, _, _ := s.Login(context.Background(), "manajer", "Rapat2026!")
	pool.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.ID)

	req := httptest.NewRequest("GET", "/meetings", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	rec := httptest.NewRecorder()
	protected(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deactivated user's next request: %d, want redirect", rec.Code)
	}
}

func TestMeetingAccess(t *testing.T) {
	pool := testutil.DB(t)
	admin := testutil.User(t, pool, "admin", "admin")
	owner := testutil.User(t, pool, "notulis", "notetaker")
	viewer := testutil.User(t, pool, "direktur", "viewer")
	tmpl := testutil.Template(t, pool, "T", true)
	id := testutil.Meeting(t, pool, owner.ID, tmpl, "Rapat", "done")
	ctx := context.Background()

	check := func(u *auth.User, view, edit bool) {
		a, err := auth.MeetingAccess(ctx, pool, u, id)
		if err != nil || a.CanView != view || a.CanEdit != edit {
			t.Errorf("%s: %+v err=%v, want view=%v edit=%v", u.Username, a, err, view, edit)
		}
	}
	check(admin, true, true)
	check(owner, true, true)
	check(viewer, false, false)
	pool.Exec(ctx, `INSERT INTO meeting_participants (meeting_id, user_id) VALUES ($1, $2)`, id, viewer.ID)
	check(viewer, true, false)

	if a, err := auth.MeetingAccess(ctx, pool, admin, "00000000-0000-0000-0000-000000000000"); err != nil || a.CanView {
		t.Fatalf("missing meeting: %+v %v", a, err)
	}
}
