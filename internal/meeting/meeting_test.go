package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notulensi/internal/auth"
	"notulensi/internal/testutil"
)

var fixedNow = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

const firstMinutes = "## Ringkasan\nIsi awal"

type fixture struct {
	pool        *pgxpool.Pool
	h           *Handler
	admin       *auth.User
	owner       *auth.User
	participant *auth.User
	outsider    *auth.User
	templateID  string
}

func setup(t *testing.T) *fixture {
	pool := testutil.DB(t)
	f := &fixture{
		pool:        pool,
		h:           &Handler{Pool: pool, DataDir: t.TempDir(), Now: func() time.Time { return fixedNow }},
		admin:       testutil.User(t, pool, "admin", "admin"),
		owner:       testutil.User(t, pool, "notulis", "notetaker"),
		participant: testutil.User(t, pool, "direktur", "viewer"),
		outsider:    testutil.User(t, pool, "manajer", "viewer"),
		templateID:  testutil.Template(t, pool, "Rapat Bulanan", true),
	}
	os.MkdirAll(filepath.Join(f.h.DataDir, "audio"), 0o755)
	return f
}

func (f *fixture) doneMeeting(t *testing.T, title string) string {
	id := testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, title, "done")
	mustExec(t, f.pool, `UPDATE meetings SET minutes_markdown = $2 WHERE id = $1`, id, firstMinutes)
	mustExec(t, f.pool, `INSERT INTO meeting_participants (meeting_id, user_id) VALUES ($1, $2)`, id, f.participant.ID)
	return id
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, handler http.HandlerFunc, u *auth.User, method, target, id string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	out := map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestMeetingAccessMatrix(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Oktober")

	cases := []struct {
		name    string
		user    *auth.User
		status  int
		canEdit bool
	}{
		{"admin", f.admin, 200, true},
		{"owner", f.owner, 200, true},
		{"participant", f.participant, 200, false},
		{"outsider", f.outsider, 404, false},
	}
	for _, c := range cases {
		rec, body := call(t, f.h.Detail, c.user, "GET", "/", id, nil)
		if rec.Code != c.status {
			t.Errorf("%s: detail status %d, want %d", c.name, rec.Code, c.status)
			continue
		}
		if c.status == 200 && body["can_edit"] != c.canEdit {
			t.Errorf("%s: can_edit %v, want %v", c.name, body["can_edit"], c.canEdit)
		}
	}

	if rec, _ := call(t, f.h.SaveMinutes, f.participant, "PUT", "/", id, map[string]any{"minutes_markdown": "x", "updated_at": fixedNow}); rec.Code != 403 {
		t.Errorf("participant save: %d, want 403", rec.Code)
	}
	if rec, _ := call(t, f.h.SaveMinutes, f.outsider, "PUT", "/", id, map[string]any{"minutes_markdown": "x", "updated_at": fixedNow}); rec.Code != 404 {
		t.Errorf("outsider save: %d, want 404", rec.Code)
	}
	if rec, _ := call(t, f.h.Detail, f.admin, "GET", "/", "bukan-uuid", nil); rec.Code != 404 {
		t.Errorf("malformed id: %d, want 404", rec.Code)
	}
}

func TestRemovedParticipantGets404(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Akses")
	rec, _ := call(t, f.h.SetParticipants, f.owner, "PUT", "/", id, map[string]any{"participant_ids": []string{}})
	if rec.Code != 200 {
		t.Fatalf("set participants: %d", rec.Code)
	}
	if rec, _ := call(t, f.h.Detail, f.participant, "GET", "/", id, nil); rec.Code != 404 {
		t.Fatalf("removed participant: %d, want 404", rec.Code)
	}
}

func TestSetParticipantsRejectsInactiveAndOwner(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Peserta")
	mustExec(t, f.pool, `UPDATE users SET is_active = false WHERE id = $1`, f.outsider.ID)
	rec, body := call(t, f.h.SetParticipants, f.owner, "PUT", "/", id, map[string]any{"participant_ids": []string{f.outsider.ID}})
	if rec.Code != 422 || body["error"] != "Pengguna tidak aktif tidak bisa ditambahkan." {
		t.Fatalf("inactive: %d %v", rec.Code, body)
	}
	if rec, _ := call(t, f.h.SetParticipants, f.owner, "PUT", "/", id, map[string]any{"participant_ids": []string{f.owner.ID}}); rec.Code != 422 {
		t.Fatalf("owner as participant: %d, want 422", rec.Code)
	}
}

func TestSaveMinutesRejectsStaleUpdatedAt(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Edit")
	_, d := call(t, f.h.Detail, f.owner, "GET", "/", id, nil)
	stale := d["updated_at"]

	rec, body := call(t, f.h.SaveMinutes, f.owner, "PUT", "/", id, map[string]any{"minutes_markdown": "## Ringkasan\nEdit pertama", "updated_at": stale})
	if rec.Code != 200 {
		t.Fatalf("first save: %d %v", rec.Code, body)
	}
	rec, body = call(t, f.h.SaveMinutes, f.admin, "PUT", "/", id, map[string]any{"minutes_markdown": "## Ringkasan\nEdit kedua", "updated_at": stale})
	if rec.Code != 409 || body["error"] != "Notulen sudah diubah oleh pengguna lain. Muat ulang halaman." {
		t.Fatalf("stale save: %d %v", rec.Code, body)
	}
	if rec, _ := call(t, f.h.SaveMinutes, f.owner, "PUT", "/", id, map[string]any{"minutes_markdown": "  ", "updated_at": stale}); rec.Code != 422 {
		t.Fatalf("empty minutes: %d, want 422", rec.Code)
	}
}

func TestListShowsOnlyVisibleMeetings(t *testing.T) {
	f := setup(t)
	f.doneMeeting(t, "Rapat Oktober")
	f.doneMeeting(t, "Rapat November")
	testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, "Rapat Oktober Khusus", "done")
	testutil.Meeting(t, f.pool, f.admin.ID, f.templateID, "Rapat Direksi", "done")

	total := func(u *auth.User, q string) float64 {
		rec, body := call(t, f.h.List, u, "GET", "/api/meetings?q="+q, "", nil)
		if rec.Code != 200 {
			t.Fatalf("list: %d", rec.Code)
		}
		return body["total"].(float64)
	}
	if got := total(f.participant, ""); got != 2 {
		t.Errorf("participant sees %v, want 2", got)
	}
	if got := total(f.outsider, ""); got != 0 {
		t.Errorf("outsider sees %v, want 0", got)
	}
	if got := total(f.owner, ""); got != 3 {
		t.Errorf("owner sees %v, want 3", got)
	}
	if got := total(f.admin, ""); got != 4 {
		t.Errorf("admin sees %v, want 4", got)
	}
	if got := total(f.admin, "oktober"); got != 2 {
		t.Errorf("search oktober: %v, want 2", got)
	}
	rec, _ := call(t, f.h.List, f.admin, "GET", "/api/meetings?date_from=2026-11-21&date_to=2026-11-01", "", nil)
	if rec.Code != 400 {
		t.Errorf("inverted range: %d, want 400", rec.Code)
	}
}

func startRecording(t *testing.T, f *fixture) string {
	t.Helper()
	rec, body := call(t, f.h.StartRecording, f.owner, "POST", "/", "", map[string]any{
		"title": "Rapat Rekam", "meeting_date": "2026-11-21", "template_id": f.templateID,
		"consent_confirmed": true, "mime_type": "audio/webm",
	})
	if rec.Code != 201 {
		t.Fatalf("start recording: %d %v", rec.Code, body)
	}
	return body["id"].(string)
}

func TestRecordingChunksMustArriveInOrder(t *testing.T) {
	f := setup(t)
	id := startRecording(t, f)
	chunk := func(seq string, data string) (*httptest.ResponseRecorder, map[string]any) {
		return call(t, f.h.AppendChunk, f.owner, "POST", "/?seq="+seq, id, []byte(data))
	}

	if rec, _ := chunk("1", "AAA"); rec.Code != 200 {
		t.Fatalf("seq 1: %d", rec.Code)
	}
	rec, body := chunk("3", "CCC")
	if rec.Code != 409 || body["expected_seq"] != float64(2) {
		t.Fatalf("seq 3 after 1: %d %v", rec.Code, body)
	}
	if rec, body := chunk("1", "AAA"); rec.Code != 409 || body["expected_seq"] != float64(2) {
		t.Fatalf("duplicate seq 1: %d %v", rec.Code, body)
	}
	if rec, _ := chunk("2", "BBB"); rec.Code != 200 {
		t.Fatalf("seq 2: %d", rec.Code)
	}
	if rec, _ := call(t, f.h.AppendChunk, f.participant, "POST", "/?seq=3", id, []byte("x")); rec.Code != 404 {
		t.Fatalf("non-participant chunk: %d, want 404", rec.Code)
	}

	data, err := os.ReadFile(filepath.Join(f.h.DataDir, "audio", id+".webm"))
	if err != nil || string(data) != "AAABBB" {
		t.Fatalf("stored audio %q, err %v", data, err)
	}

	if rec, _ := call(t, f.h.FinishRecording, f.owner, "POST", "/", id, nil); rec.Code != 200 {
		t.Fatalf("finish: %d", rec.Code)
	}
	if rec, _ := chunk("3", "CCC"); rec.Code != 409 {
		t.Fatalf("chunk after finish: %d, want 409", rec.Code)
	}
}

func TestFinishEmptyRecordingIsRejected(t *testing.T) {
	f := setup(t)
	id := startRecording(t, f)
	rec, body := call(t, f.h.FinishRecording, f.owner, "POST", "/", id, nil)
	if rec.Code != 422 || body["error"] != msgEmptyRecord {
		t.Fatalf("empty finish: %d %v", rec.Code, body)
	}
}

func TestStartRecordingRequiresConsentAndPastDate(t *testing.T) {
	f := setup(t)
	rec, body := call(t, f.h.StartRecording, f.owner, "POST", "/", "", map[string]any{
		"title": "Rapat", "meeting_date": "2026-11-21", "template_id": f.templateID, "mime_type": "audio/webm",
	})
	if rec.Code != 422 || body["error"] != msgNoConsent {
		t.Fatalf("no consent: %d %v", rec.Code, body)
	}
	rec, _ = call(t, f.h.StartRecording, f.owner, "POST", "/", "", map[string]any{
		"title": "Rapat", "meeting_date": "2026-12-31", "template_id": f.templateID, "consent_confirmed": true, "mime_type": "audio/webm",
	})
	if rec.Code != 422 {
		t.Fatalf("future date: %d, want 422", rec.Code)
	}
}

func multipartBody(fields map[string]string, filename string) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write([]byte("bukan audio"))
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestUploadChecksRoleAndFormat(t *testing.T) {
	f := setup(t)
	fields := map[string]string{"title": "Rapat", "meeting_date": "2026-11-20", "template_id": f.templateID}
	for _, c := range []struct {
		user   *auth.User
		file   string
		status int
	}{
		{f.participant, "rapat.m4a", 403},
		{f.owner, "notulen.pdf", 422},
	} {
		body, ct := multipartBody(fields, c.file)
		req := httptest.NewRequest("POST", "/api/meetings", body)
		req.Header.Set("Content-Type", ct)
		req = req.WithContext(auth.WithUser(req.Context(), c.user))
		rec := httptest.NewRecorder()
		f.h.Upload(rec, req)
		if rec.Code != c.status {
			t.Errorf("%s %s: %d, want %d", c.user.Role, c.file, rec.Code, c.status)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(f.h.DataDir, "audio"))
	if len(entries) != 0 {
		t.Fatalf("rejected uploads left %d files", len(entries))
	}
}

func TestDeleteMeeting(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Salah Unggah")
	audio := filepath.Join(f.h.DataDir, "audio", "none.m4a")
	os.WriteFile(audio, []byte("x"), 0o644)

	if rec, _ := call(t, f.h.Delete, f.participant, "DELETE", "/", id, map[string]string{"confirm_title": "Rapat Salah Unggah"}); rec.Code != 403 {
		t.Fatalf("participant delete: %d, want 403", rec.Code)
	}
	if rec, _ := call(t, f.h.Delete, f.owner, "DELETE", "/", id, map[string]string{"confirm_title": "salah"}); rec.Code != 422 {
		t.Fatalf("title mismatch: %d, want 422", rec.Code)
	}
	mustExec(t, f.pool, `UPDATE meetings SET status = 'transcribing' WHERE id = $1`, id)
	if rec, _ := call(t, f.h.Delete, f.owner, "DELETE", "/", id, map[string]string{"confirm_title": "Rapat Salah Unggah"}); rec.Code != 409 {
		t.Fatalf("processing delete: %d, want 409", rec.Code)
	}
	mustExec(t, f.pool, `UPDATE meetings SET status = 'done' WHERE id = $1`, id)
	if rec, _ := call(t, f.h.Delete, f.owner, "DELETE", "/", id, map[string]string{"confirm_title": "Rapat Salah Unggah"}); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec, _ := call(t, f.h.Detail, f.admin, "GET", "/", id, nil); rec.Code != 404 {
		t.Fatalf("deleted meeting: %d, want 404", rec.Code)
	}
	if _, err := os.Stat(audio); !os.IsNotExist(err) {
		t.Fatal("audio file still exists")
	}
	var audits int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = 'meeting_delete' AND meeting_id = $1`, id).Scan(&audits)
	if audits != 1 {
		t.Fatalf("meeting_delete audit entries: %d", audits)
	}
}

func TestRegenerateKeepsBackupAndRestoreSwaps(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Rapat Regenerasi")
	other := testutil.Template(t, f.pool, "Ringkas", false)

	rec, _ := call(t, f.h.Regenerate, f.owner, "POST", "/", id, map[string]string{"template_id": other})
	if rec.Code != 200 {
		t.Fatalf("regenerate: %d", rec.Code)
	}
	var status, backup, tmpl string
	var skip bool
	f.pool.QueryRow(context.Background(), `SELECT status, minutes_backup, template_id, skip_transcription FROM meetings WHERE id = $1`, id).Scan(&status, &backup, &tmpl, &skip)
	if status != "queued" || backup != firstMinutes || tmpl != other || !skip {
		t.Fatalf("after regenerate: status=%s backup=%q template=%s skip=%v", status, backup, tmpl, skip)
	}

	mustExec(t, f.pool, `UPDATE meetings SET status = 'done', minutes_markdown = 'Versi baru' WHERE id = $1`, id)
	if rec, _ := call(t, f.h.RestoreBackup, f.owner, "POST", "/", id, nil); rec.Code != 200 {
		t.Fatalf("restore: %d", rec.Code)
	}
	var minutes string
	f.pool.QueryRow(context.Background(), `SELECT minutes_markdown, minutes_backup FROM meetings WHERE id = $1`, id).Scan(&minutes, &backup)
	if minutes != firstMinutes || backup != "Versi baru" {
		t.Fatalf("after restore: minutes=%q backup=%q", minutes, backup)
	}
}

func TestRetrySkipsTranscriptionWhenSegmentsExist(t *testing.T) {
	f := setup(t)
	withSegments := testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, "Gagal di LLM", "failed")
	mustExec(t, f.pool, `INSERT INTO transcript_segments (meeting_id, seq, start_sec, end_sec, text) VALUES ($1, 1, 0, 2, 'halo')`, withSegments)
	noSegments := testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, "Gagal di whisper", "failed")

	for id, wantSkip := range map[string]bool{withSegments: true, noSegments: false} {
		if rec, _ := call(t, f.h.Retry, f.owner, "POST", "/", id, nil); rec.Code != 200 {
			t.Fatalf("retry: %d", rec.Code)
		}
		var status string
		var skip bool
		f.pool.QueryRow(context.Background(), `SELECT status, skip_transcription FROM meetings WHERE id = $1`, id).Scan(&status, &skip)
		if status != "queued" || skip != wantSkip {
			t.Errorf("meeting %s: status=%s skip=%v, want queued/%v", id, status, skip, wantSkip)
		}
	}
	if rec, _ := call(t, f.h.Retry, f.owner, "POST", "/", withSegments, nil); rec.Code != 409 {
		t.Fatalf("retry on queued meeting: %d, want 409", rec.Code)
	}
}

func TestSaveTitleAndContext(t *testing.T) {
	f := setup(t)
	id := f.doneMeeting(t, "Judul Lama")

	if rec, _ := call(t, f.h.SaveTitle, f.participant, "PUT", "/", id, map[string]string{"title": "X"}); rec.Code != 403 {
		t.Fatalf("participant title: %d, want 403", rec.Code)
	}
	if rec, _ := call(t, f.h.SaveTitle, f.owner, "PUT", "/", id, map[string]string{"title": "   "}); rec.Code != 422 {
		t.Fatalf("blank title: %d, want 422", rec.Code)
	}
	if rec, _ := call(t, f.h.SaveTitle, f.owner, "PUT", "/", id, map[string]string{"title": " Rapat Desember "}); rec.Code != 200 {
		t.Fatalf("save title: %d", rec.Code)
	}
	if rec, _ := call(t, f.h.SaveContext, f.owner, "PUT", "/", id, map[string]string{"summary_context": strings.Repeat("a", 2001)}); rec.Code != 422 {
		t.Fatalf("long context: %d, want 422", rec.Code)
	}
	if rec, _ := call(t, f.h.SaveContext, f.owner, "PUT", "/", id, map[string]string{"summary_context": "Pak Budi = Direktur Keuangan"}); rec.Code != 200 {
		t.Fatalf("save context: %d", rec.Code)
	}
	_, d := call(t, f.h.Detail, f.owner, "GET", "/", id, nil)
	if d["title"] != "Rapat Desember" || d["summary_context"] != "Pak Budi = Direktur Keuangan" {
		t.Fatalf("detail after save: title=%v context=%v", d["title"], d["summary_context"])
	}
}

func TestCancelQueuedMeeting(t *testing.T) {
	f := setup(t)
	fresh := testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, "Baru", "queued")
	regen := f.doneMeeting(t, "Regenerasi")
	mustExec(t, f.pool, `UPDATE meetings SET status = 'queued', skip_transcription = true WHERE id = $1`, regen)

	for id, want := range map[string]string{fresh: "failed", regen: "done"} {
		if rec, _ := call(t, f.h.Cancel, f.owner, "POST", "/", id, nil); rec.Code != 200 {
			t.Fatalf("cancel: %d", rec.Code)
		}
		_, d := call(t, f.h.Detail, f.owner, "GET", "/", id, nil)
		if d["status"] != want {
			t.Errorf("status after cancel %v, want %s", d["status"], want)
		}
	}
	_, d := call(t, f.h.Detail, f.owner, "GET", "/", fresh, nil)
	if d["error_message"] != "Pemrosesan dibatalkan." {
		t.Errorf("error_message %v", d["error_message"])
	}
	if rec, _ := call(t, f.h.Cancel, f.owner, "POST", "/", regen, nil); rec.Code != 409 {
		t.Fatalf("cancel done meeting: %d, want 409", rec.Code)
	}
}

func TestCancelRunningMeetingAsksWorker(t *testing.T) {
	f := setup(t)
	id := testutil.Meeting(t, f.pool, f.owner.ID, f.templateID, "Berjalan", "transcribing")
	var asked string
	f.h.CancelJob = func(m string) bool { asked = m; return true }
	if rec, _ := call(t, f.h.Cancel, f.owner, "POST", "/", id, nil); rec.Code != 202 || asked != id {
		t.Fatalf("running cancel: %d asked=%q", rec.Code, asked)
	}
	if rec, _ := call(t, f.h.Cancel, f.participant, "POST", "/", id, nil); rec.Code != 404 {
		t.Fatalf("outsider cancel: %d, want 404", rec.Code)
	}
}

func TestRecordingFormStoresContext(t *testing.T) {
	f := setup(t)
	rec, body := call(t, f.h.StartRecording, f.owner, "POST", "/", "", map[string]any{
		"title": "Rapat", "meeting_date": "2026-11-21", "template_id": f.templateID,
		"consent_confirmed": true, "mime_type": "audio/webm", "summary_context": "Agenda: anggaran Q4",
	})
	if rec.Code != 201 {
		t.Fatalf("start: %d %v", rec.Code, body)
	}
	_, d := call(t, f.h.Detail, f.owner, "GET", "/", body["id"].(string), nil)
	if d["summary_context"] != "Agenda: anggaran Q4" {
		t.Fatalf("context %v", d["summary_context"])
	}
}

func TestRenderMinutesEscapesHTMLAndLinksTimestamps(t *testing.T) {
	out := string(RenderMinutes("## Keputusan\n- Setuju [12:34] dan [01:02:03]\n\n<script>alert(1)</script>"))
	if strings.Contains(out, "<script>") {
		t.Fatal("raw HTML must not be rendered")
	}
	if !strings.Contains(out, `data-seek="754"`) || !strings.Contains(out, `data-seek="3723"`) {
		t.Fatalf("timestamps not linked: %s", out)
	}
	if !strings.Contains(out, "<h2>Keputusan</h2>") {
		t.Fatalf("heading missing: %s", out)
	}
}

func TestSlugAndExportHeader(t *testing.T) {
	if got := slug("Rapat Bulanan: November 2026!"); got != "rapat-bulanan-november-2026" {
		t.Fatalf("slug %q", got)
	}
	minutes := "## Ringkasan\nIsi"
	doc := exportMarkdown(&detail{
		Title: "Rapat Bulanan", MeetingDate: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), OwnerName: "Sekretariat",
		Participants: []participant{{FullName: "Direktur Utama"}}, MinutesMarkdown: &minutes,
	})
	for _, want := range []string{"# Rapat Bulanan", "21 November 2026", "Sekretariat", "Direktur Utama", "## Ringkasan"} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q", want)
		}
	}
}
