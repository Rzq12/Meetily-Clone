package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/llm"
	"notulensi/internal/testutil"
	"notulensi/web"
)

type llmStub struct {
	mu    sync.Mutex
	fail  bool
	reply string
	calls int
}

func (s *llmStub) server(t *testing.T) *llm.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		fail := s.fail
		s.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": s.reply}}}})
	}))
	t.Cleanup(srv.Close)
	c := llm.NewClient(srv.URL, "m", "")
	c.RetryDelay = time.Millisecond
	return c
}

func newWorker(t *testing.T, pool *pgxpool.Pool, stub *llmStub) *Worker {
	return &Worker{
		Pool: pool, DataDir: t.TempDir(), LLM: stub.server(t), TokenThreshold: 6000,
		RetentionDays: 90, PollInterval: time.Second, Now: time.Now,
	}
}

func readyMeeting(t *testing.T, pool *pgxpool.Pool, ownerID, templateID, title string, queuedAt time.Time) string {
	id := testutil.Meeting(t, pool, ownerID, templateID, title, "queued")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE meetings SET skip_transcription = true, queued_at = $2 WHERE id = $1`, id, queuedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO transcript_segments (meeting_id, seq, start_sec, end_sec, text) VALUES ($1, 1, 5, 9, 'Direktur meminta laporan anggaran')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, pool *pgxpool.Pool, id string) (string, *string, *string) {
	var s string
	var minutes, errMsg *string
	if err := pool.QueryRow(context.Background(), `SELECT status, minutes_markdown, error_message FROM meetings WHERE id = $1`, id).Scan(&s, &minutes, &errMsg); err != nil {
		t.Fatal(err)
	}
	return s, minutes, errMsg
}

func TestProcessesOldestFirstOneAtATime(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	now := time.Now()
	newer := readyMeeting(t, pool, owner.ID, tmpl, "Baru", now)
	older := readyMeeting(t, pool, owner.ID, tmpl, "Lama", now.Add(-time.Hour))
	w := newWorker(t, pool, &llmStub{reply: "## Ringkasan\nRapat membahas anggaran."})

	found, err := w.ProcessNext(context.Background())
	if err != nil || !found {
		t.Fatalf("first run: found=%v err=%v", found, err)
	}
	if s, _, _ := status(t, pool, older); s != "done" {
		t.Fatalf("older meeting status %s, want done", s)
	}
	if s, _, _ := status(t, pool, newer); s != "queued" {
		t.Fatalf("newer meeting status %s, want still queued", s)
	}
	w.ProcessNext(context.Background())
	s, minutes, _ := status(t, pool, newer)
	if s != "done" || minutes == nil || *minutes != "## Ringkasan\nRapat membahas anggaran." {
		t.Fatalf("newer: status=%s minutes=%v", s, minutes)
	}
	if found, _ := w.ProcessNext(context.Background()); found {
		t.Fatal("queue should be empty")
	}
}

func TestLLMFailureMarksFailedAndKeepsTranscript(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	id := readyMeeting(t, pool, owner.ID, tmpl, "Rapat", time.Now())
	stub := &llmStub{fail: true}
	w := newWorker(t, pool, stub)

	w.ProcessNext(context.Background())
	s, _, msg := status(t, pool, id)
	if s != "failed" || msg == nil || *msg != "Penyusunan notulen gagal." {
		t.Fatalf("status=%s msg=%v", s, msg)
	}
	if stub.calls != 2 {
		t.Fatalf("LLM calls %d, want 2 (one retry)", stub.calls)
	}
	var segments int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM transcript_segments WHERE meeting_id = $1`, id).Scan(&segments)
	if segments != 1 {
		t.Fatalf("transcript must be kept, got %d segments", segments)
	}
}

func TestRegenerateFailureLeavesPreviousMinutes(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	id := readyMeeting(t, pool, owner.ID, tmpl, "Rapat", time.Now())
	pool.Exec(context.Background(), `UPDATE meetings SET minutes_markdown = 'Versi lama', minutes_backup = 'Versi lama' WHERE id = $1`, id)
	w := newWorker(t, pool, &llmStub{fail: true})

	w.ProcessNext(context.Background())
	s, minutes, _ := status(t, pool, id)
	if s != "failed" || minutes == nil || *minutes != "Versi lama" {
		t.Fatalf("status=%s minutes=%v, want failed with previous minutes", s, minutes)
	}
}

func TestEmptyLLMOutputHasOwnMessage(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	id := readyMeeting(t, pool, owner.ID, tmpl, "Rapat", time.Now())
	w := newWorker(t, pool, &llmStub{reply: "<think>...</think>"})

	w.ProcessNext(context.Background())
	if _, _, msg := status(t, pool, id); msg == nil || *msg != "AI tidak menghasilkan notulen." {
		t.Fatalf("msg=%v", msg)
	}
}

func TestRequeueInterrupted(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	transcribing := testutil.Meeting(t, pool, owner.ID, tmpl, "A", "transcribing")
	summarizing := testutil.Meeting(t, pool, owner.ID, tmpl, "B", "summarizing")
	pool.Exec(context.Background(), `INSERT INTO transcript_segments (meeting_id, seq, start_sec, end_sec, text) VALUES ($1, 1, 0, 1, 'x')`, summarizing)
	w := newWorker(t, pool, &llmStub{})

	if err := w.RequeueInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, wantSkip := range map[string]bool{transcribing: false, summarizing: true} {
		var s string
		var skip bool
		pool.QueryRow(context.Background(), `SELECT status, skip_transcription FROM meetings WHERE id = $1`, id).Scan(&s, &skip)
		if s != "queued" || skip != wantSkip {
			t.Errorf("meeting %s: status=%s skip=%v", id, s, skip)
		}
	}
}

func TestCleanupDeletesExpiredAudioOnly(t *testing.T) {
	pool := testutil.DB(t)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	tmpl := testutil.Template(t, pool, "T", true)
	w := newWorker(t, pool, &llmStub{})
	os.MkdirAll(filepath.Join(w.DataDir, "audio"), 0o755)
	ctx := context.Background()

	mk := func(title, st string, age time.Duration) (string, string) {
		id := testutil.Meeting(t, pool, owner.ID, tmpl, title, st)
		rel := filepath.Join("audio", id+".m4a")
		os.WriteFile(filepath.Join(w.DataDir, rel), []byte("x"), 0o644)
		pool.Exec(ctx, `UPDATE meetings SET audio_path = $2, created_at = $3, minutes_markdown = 'notulen' WHERE id = $1`, id, rel, time.Now().Add(-age))
		return id, filepath.Join(w.DataDir, rel)
	}
	oldDone, oldFile := mk("Lama", "done", 91*24*time.Hour)
	_, recentFile := mk("Baru", "done", 10*24*time.Hour)
	_, failedFile := mk("Gagal lama", "failed", 91*24*time.Hour)

	if err := w.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("expired audio should be deleted")
	}
	for _, f := range []string{recentFile, failedFile} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("audio %s should be kept: %v", f, err)
		}
	}
	var deletedAt *time.Time
	var minutes string
	pool.QueryRow(ctx, `SELECT audio_deleted_at, minutes_markdown FROM meetings WHERE id = $1`, oldDone).Scan(&deletedAt, &minutes)
	if deletedAt == nil || minutes != "notulen" {
		t.Fatalf("audio_deleted_at=%v minutes=%q", deletedAt, minutes)
	}
}

func TestNextRunIs0200WIB(t *testing.T) {
	cases := map[string]string{
		"2026-11-21T01:00:00+07:00": "2026-11-21T02:00:00+07:00",
		"2026-11-21T02:00:00+07:00": "2026-11-22T02:00:00+07:00",
		"2026-11-21T23:30:00+07:00": "2026-11-22T02:00:00+07:00",
	}
	for in, want := range cases {
		now, _ := time.Parse(time.RFC3339, in)
		if got := nextRun(now).In(web.Jakarta).Format(time.RFC3339); got != want {
			t.Errorf("nextRun(%s) = %s, want %s", in, got, want)
		}
	}
}
