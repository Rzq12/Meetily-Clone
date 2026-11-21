package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"notulensi/internal/auth"
	"notulensi/internal/testutil"
)

func send(t *testing.T, h http.HandlerFunc, u *auth.User, method, id string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/", bytes.NewReader(raw))
	req = req.WithContext(auth.WithUser(req.Context(), u))
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	out := map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestUserManagement(t *testing.T) {
	pool := testutil.DB(t)
	admin := testutil.User(t, pool, "admin", "admin")
	notetaker := testutil.User(t, pool, "notulis", "notetaker")
	h := &Handler{Pool: pool, Auth: auth.NewService(pool)}
	ctx := context.Background()

	newUser := map[string]string{"username": "direktur.utama", "full_name": "Direktur Utama", "role": "viewer", "password": "Rapat2026!"}
	if code, _ := send(t, h.Create, notetaker, "POST", "", newUser); code != 403 {
		t.Fatalf("notetaker create: %d, want 403", code)
	}
	code, created := send(t, h.Create, admin, "POST", "", newUser)
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	if code, body := send(t, h.Create, admin, "POST", "", newUser); code != 409 || body["error"] != "Username sudah dipakai." {
		t.Fatalf("duplicate: %d %v", code, body)
	}
	if code, body := send(t, h.Create, admin, "POST", "", map[string]string{"username": "sek retariat", "full_name": "X", "role": "viewer", "password": "abc123"}); code != 422 || body["fields"] == nil {
		t.Fatalf("invalid input: %d %v", code, body)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id) VALUES ('t1', $1)`, created["id"]); err != nil {
		t.Fatal(err)
	}
	code, _ = send(t, h.Update, admin, "PUT", created["id"].(string), map[string]any{"full_name": "Direktur Utama", "role": "viewer", "is_active": false})
	if code != 200 {
		t.Fatalf("deactivate: %d", code)
	}
	var sessions int
	pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, created["id"]).Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("deactivated user still has %d sessions", sessions)
	}

	code, body := send(t, h.Update, admin, "PUT", admin.ID, map[string]any{"full_name": "Admin", "role": "viewer", "is_active": true})
	if code != 422 || body["error"] != "Tidak bisa mengubah akun sendiri." {
		t.Fatalf("self demotion: %d %v", code, body)
	}
	if code, _ := send(t, h.Update, admin, "PUT", admin.ID, map[string]any{"full_name": "Admin Baru", "role": "admin", "is_active": true}); code != 200 {
		t.Fatalf("self name change: %d", code)
	}
}

func TestSeedAdminOnlyWhenEmpty(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	if err := SeedAdmin(ctx, pool, "admin", "pendek"); err == nil {
		t.Fatal("short password must be rejected")
	}
	if err := SeedAdmin(ctx, pool, "admin", "Rahasia2026!"); err != nil {
		t.Fatal(err)
	}
	if err := SeedAdmin(ctx, pool, "admin2", "Rahasia2026!"); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("users after seeding twice: %d, want 1", n)
	}
}
