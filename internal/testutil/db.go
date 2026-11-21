package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/auth"
	"notulensi/internal/db"
)

// DB returns a pool bound to a fresh schema in TEST_DATABASE_URL with all migrations applied.
// The schema is dropped when the test ends. Tests are skipped when the variable is unset.
func DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	raw := make([]byte, 6)
	rand.Read(raw)
	schema := "test_" + hex.EncodeToString(raw)

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool
}

// User inserts an active user with a placeholder password hash and returns it.
func User(t *testing.T, pool *pgxpool.Pool, username, role string) *auth.User {
	t.Helper()
	u := &auth.User{Username: username, FullName: "Nama " + username, Role: role}
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (username, full_name, password_hash, role) VALUES ($1, $2, 'x', $3) RETURNING id`,
		u.Username, u.FullName, role).Scan(&u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Template inserts a one-section template and returns its id.
func Template(t *testing.T, pool *pgxpool.Pool, name string, isDefault bool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO templates (name, sections, is_default) VALUES ($1, '[{"title":"Ringkasan","instruction":"Ringkas","format":"paragraph"}]', $2) RETURNING id`,
		name, isDefault).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Meeting inserts a meeting with the given status and returns its id.
func Meeting(t *testing.T, pool *pgxpool.Pool, ownerID, templateID, title, status string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO meetings (title, meeting_date, owner_id, template_id, source, status, audio_path, queued_at)
		VALUES ($1, '2026-11-20', $2, $3, 'upload', $4, 'audio/none.m4a', now()) RETURNING id`,
		title, ownerID, templateID, status).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
