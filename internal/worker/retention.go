package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"notulensi/web"
)

// nextRun returns the next 02:00 WIB strictly after now.
func nextRun(now time.Time) time.Time {
	local := now.In(web.Jakarta)
	run := time.Date(local.Year(), local.Month(), local.Day(), 2, 0, 0, 0, web.Jakarta)
	if !run.After(local) {
		run = run.AddDate(0, 0, 1)
	}
	return run
}

func (w *Worker) RunDaily(ctx context.Context) {
	for {
		wait := time.Until(nextRun(w.Now()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := w.Cleanup(ctx); err != nil {
			slog.Error("daily cleanup", "err", err)
		}
	}
}

// Cleanup deletes expired audio (FR-016), idle sessions, and audit logs older than 2 years.
func (w *Worker) Cleanup(ctx context.Context) error {
	now := w.Now()
	rows, err := w.Pool.Query(ctx, `
		SELECT id, audio_path FROM meetings
		WHERE status = 'done' AND audio_deleted_at IS NULL AND created_at < $1`,
		now.AddDate(0, 0, -w.RetentionDays))
	if err != nil {
		return err
	}
	type expired struct{ id, path string }
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (expired, error) {
		var e expired
		err := row.Scan(&e.id, &e.path)
		return e, err
	})
	if err != nil {
		return err
	}
	for _, e := range list {
		if err := os.Remove(filepath.Join(w.DataDir, e.path)); err != nil && !os.IsNotExist(err) {
			slog.Error("retention delete audio", "meeting_id", e.id, "err", err)
			continue
		}
		if _, err := w.Pool.Exec(ctx, `UPDATE meetings SET audio_deleted_at = $2 WHERE id = $1`, e.id, now); err != nil {
			return err
		}
	}
	if _, err := w.Pool.Exec(ctx, `DELETE FROM sessions WHERE last_seen_at < $1`, now.Add(-8*time.Hour)); err != nil {
		return err
	}
	_, err = w.Pool.Exec(ctx, `DELETE FROM audit_logs WHERE created_at < $1`, now.AddDate(-2, 0, 0))
	return err
}
