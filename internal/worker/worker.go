package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/llm"
	"notulensi/internal/template"
	"notulensi/internal/transcribe"
)

type Worker struct {
	Pool           *pgxpool.Pool
	DataDir        string
	Whisper        *transcribe.Client
	LLM            *llm.Client
	TokenThreshold int
	RetentionDays  int
	PollInterval   time.Duration
	Now            func() time.Time

	mu        sync.Mutex
	runningID string
	cancelRun context.CancelFunc
}

// Cancel stops the job currently processing meetingID; false means it is not running.
func (w *Worker) Cancel(meetingID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runningID != meetingID || w.cancelRun == nil {
		return false
	}
	w.cancelRun()
	return true
}

// stepError carries the user-facing message stored in error_message; err goes to the log only.
type stepError struct {
	step string
	msg  string
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }

// RequeueInterrupted runs at startup so jobs cut off by a restart resume (FR-005).
func (w *Worker) RequeueInterrupted(ctx context.Context) error {
	if _, err := w.Pool.Exec(ctx, `UPDATE meetings SET status = 'queued' WHERE status = 'transcribing'`); err != nil {
		return err
	}
	_, err := w.Pool.Exec(ctx, `
		UPDATE meetings SET status = 'queued',
		       skip_transcription = EXISTS (SELECT 1 FROM transcript_segments s WHERE s.meeting_id = meetings.id)
		WHERE status = 'summarizing'`)
	return err
}

// Run drains the queue one meeting at a time, then waits PollInterval (BR-008).
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()
	for {
		for {
			found, err := w.ProcessNext(ctx)
			if err != nil {
				slog.Error("worker claim", "err", err)
				break
			}
			if !found {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type job struct {
	id         string
	templateID string
	audioPath  string
	context    string
	skip       bool
}

// ProcessNext claims the oldest queued meeting and runs the pipeline. found is false when the queue is empty.
func (w *Worker) ProcessNext(ctx context.Context) (found bool, err error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var j job
	err = tx.QueryRow(ctx, `
		SELECT id, template_id, audio_path, coalesce(summary_context, ''), skip_transcription FROM meetings
		WHERE status = 'queued' ORDER BY queued_at LIMIT 1 FOR UPDATE SKIP LOCKED`).
		Scan(&j.id, &j.templateID, &j.audioPath, &j.context, &j.skip)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next := "transcribing"
	if j.skip {
		next = "summarizing"
	}
	if _, err := tx.Exec(ctx, `UPDATE meetings SET status = $2 WHERE id = $1`, j.id, next); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	jobCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.runningID, w.cancelRun = j.id, cancel
	w.mu.Unlock()
	err = w.process(jobCtx, j)
	w.mu.Lock()
	w.runningID, w.cancelRun = "", nil
	w.mu.Unlock()
	cancelled := jobCtx.Err() != nil && ctx.Err() == nil
	cancel()

	if cancelled {
		// A cancelled regenerate still has its previous minutes, so it returns to done.
		slog.Info("meeting processing cancelled", "meeting_id", j.id)
		if _, dbErr := w.Pool.Exec(ctx, `
			UPDATE meetings SET
			  status = CASE WHEN minutes_markdown IS NULL THEN 'failed' ELSE 'done' END,
			  error_message = CASE WHEN minutes_markdown IS NULL THEN 'Pemrosesan dibatalkan.' END,
			  skip_transcription = false, updated_at = $2
			WHERE id = $1`, j.id, w.Now()); dbErr != nil {
			return true, dbErr
		}
		return true, nil
	}
	if err != nil {
		var se *stepError
		msg := "Pemrosesan gagal."
		if errors.As(err, &se) {
			msg = se.msg
		}
		slog.Error("meeting processing failed", "meeting_id", j.id, "err", err)
		if _, dbErr := w.Pool.Exec(ctx,
			`UPDATE meetings SET status = 'failed', error_message = $2, updated_at = $3 WHERE id = $1`,
			j.id, msg, w.Now()); dbErr != nil {
			return true, dbErr
		}
	}
	return true, nil
}

func (w *Worker) process(ctx context.Context, j job) error {
	if !j.skip {
		start := time.Now()
		if err := w.transcribe(ctx, j); err != nil {
			return err
		}
		slog.Info("transcription done", "meeting_id", j.id, "elapsed", time.Since(start).Round(time.Second))
	}
	start := time.Now()
	if err := w.summarize(ctx, j); err != nil {
		return err
	}
	slog.Info("minutes done", "meeting_id", j.id, "elapsed", time.Since(start).Round(time.Second))
	return nil
}

func (w *Worker) transcribe(ctx context.Context, j job) error {
	wav := filepath.Join(os.TempDir(), j.id+".wav")
	defer os.Remove(wav)

	if err := transcribe.ToWAV(ctx, filepath.Join(w.DataDir, j.audioPath), wav); err != nil {
		return &stepError{"convert", "Konversi audio gagal.", err}
	}
	duration, err := transcribe.Probe(ctx, wav)
	if err != nil {
		return &stepError{"convert", "Konversi audio gagal.", err}
	}
	segments, err := w.Whisper.Whisper(ctx, wav, duration)
	if err != nil {
		return &stepError{"transcribe", "Layanan transkripsi tidak tersedia.", err}
	}

	rows := [][]any{}
	for _, s := range segments {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		rows = append(rows, []any{j.id, len(rows) + 1, float32(s.Start), float32(s.End), text})
	}
	if len(rows) == 0 {
		return &stepError{"transcribe", "Tidak ada suara yang terdeteksi di rekaman.", errors.New("no non-empty segments")}
	}

	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return &stepError{"save segments", "Pemrosesan gagal.", err}
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM transcript_segments WHERE meeting_id = $1`, j.id); err != nil {
		return &stepError{"save segments", "Pemrosesan gagal.", err}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"transcript_segments"},
		[]string{"meeting_id", "seq", "start_sec", "end_sec", "text"}, pgx.CopyFromRows(rows)); err != nil {
		return &stepError{"save segments", "Pemrosesan gagal.", err}
	}
	if _, err := tx.Exec(ctx, `UPDATE meetings SET audio_duration_sec = $2, status = 'summarizing' WHERE id = $1`,
		j.id, int(duration)); err != nil {
		return &stepError{"save segments", "Pemrosesan gagal.", err}
	}
	if err := tx.Commit(ctx); err != nil {
		return &stepError{"save segments", "Pemrosesan gagal.", err}
	}
	return nil
}

func (w *Worker) summarize(ctx context.Context, j job) error {
	sections, err := template.Load(ctx, w.Pool, j.templateID)
	if err != nil {
		return &stepError{"load template", "Penyusunan notulen gagal.", err}
	}
	rows, err := w.Pool.Query(ctx, `SELECT start_sec, text FROM transcript_segments WHERE meeting_id = $1 ORDER BY seq`, j.id)
	if err != nil {
		return &stepError{"load transcript", "Penyusunan notulen gagal.", err}
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (llm.Line, error) {
		var start float32
		var l llm.Line
		err := row.Scan(&start, &l.Text)
		l.Start = float64(start)
		return l, err
	})
	if err != nil {
		return &stepError{"load transcript", "Penyusunan notulen gagal.", err}
	}

	minutes, err := w.LLM.Generate(ctx, llm.FormatTranscript(lines), j.context, sections, w.TokenThreshold)
	if errors.Is(err, llm.ErrEmpty) {
		return &stepError{"summarize", "AI tidak menghasilkan notulen.", err}
	}
	if err != nil {
		return &stepError{"summarize", "Penyusunan notulen gagal.", err}
	}

	// On a regenerate failure minutes_markdown was never overwritten, so the
	// previous minutes stay visible without an explicit restore (FR-010).
	if _, err := w.Pool.Exec(ctx, `
		UPDATE meetings SET minutes_markdown = $2, status = 'done', skip_transcription = false,
		       error_message = NULL, updated_at = $3
		WHERE id = $1`, j.id, minutes, w.Now()); err != nil {
		return &stepError{"save minutes", "Penyusunan notulen gagal.", err}
	}
	return nil
}
