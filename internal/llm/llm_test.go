package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"notulensi/internal/template"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens("abcdefghij"); got != 4 {
		t.Fatalf("10 chars: got %d, want 4 (ceil 3.5)", got)
	}
	if got := EstimateTokens("ééé"); got != 2 {
		t.Fatalf("counts runes, not bytes: got %d", got)
	}
}

func TestChunkTextCoversTextWithOverlapAndCutsAtLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "[00:%02d] baris transkrip nomor %d yang cukup panjang\n", i%60, i)
	}
	text := b.String()
	chunks := ChunkText(text, 500, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks[:len(chunks)-1] {
		if !strings.HasSuffix(c, "\n") {
			t.Errorf("chunk %d does not end at a line break", i)
		}
		if EstimateTokens(c) > 500 {
			t.Errorf("chunk %d has %d tokens, above 500", i, EstimateTokens(c))
		}
	}
	if !strings.HasPrefix(text, chunks[0]) || !strings.HasSuffix(text, chunks[len(chunks)-1]) {
		t.Fatal("first and last chunks must cover the start and end of the transcript")
	}
	lines := strings.Split(strings.TrimSuffix(chunks[0], "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(chunks[1], last) {
		t.Fatalf("chunk 1 should repeat the end of chunk 0 (%q)", last)
	}
}

func TestChunkTextShortTextIsOneChunk(t *testing.T) {
	if got := ChunkText("pendek", 100, 10); len(got) != 1 || got[0] != "pendek" {
		t.Fatalf("got %q", got)
	}
}

func TestClean(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"think block":  {"<think>rencana</think>\n## Ringkasan\nIsi", "## Ringkasan\nIsi", true},
		"thinking tag": {"<thinking a=\"1\">x</thinking>## A", "## A", true},
		"fence":        {"```markdown\n## A\nisi\n```", "## A\nisi", true},
		"empty":        {"<think>semua</think>   ", "", false},
		"unterminated": {"<think>tidak selesai\n## A", "", false},
		"plain":        {"## A", "## A", true},
	}
	for name, c := range cases {
		got, ok := Clean(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", name, got, ok, c.want, c.ok)
		}
	}
}

func TestFinalSystemPromptKeepsSectionsInOrderAndIndonesianRules(t *testing.T) {
	p := FinalSystemPrompt([]template.Section{
		{Title: "Ringkasan", Instruction: "Ringkas rapat", Format: "paragraph"},
		{Title: "Keputusan", Instruction: "Daftar keputusan", Format: "list"},
		{Title: "Tindak Lanjut", Instruction: "Tugas", Format: "table", ItemFormat: "| PIC | Tugas |"},
	})
	a, b, c := strings.Index(p, "## Ringkasan"), strings.Index(p, "## Keputusan"), strings.Index(p, "## Tindak Lanjut")
	if a < 0 || !(a < b && b < c) {
		t.Fatal("template headings missing or out of order")
	}
	for _, want := range []string{"Bahasa Indonesia", "hanya informasi yang ada di teks sumber", EmptySection, "| PIC | Tugas |"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "English") {
		t.Error("prompt must not carry Meetily's English-only rule")
	}
}

type stubLLM struct {
	mu       sync.Mutex
	calls    []string
	failures int
	reply    string
}

func (s *stubLLM) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []message `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.calls = append(s.calls, req.Messages[1].Content)
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if fail {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": s.reply}}}})
}

func newStub(t *testing.T, s *stubLLM) *Client {
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "m", "")
	c.RetryDelay = time.Millisecond
	return c
}

var oneSection = []template.Section{{Title: "Ringkasan", Instruction: "Ringkas", Format: "paragraph"}}

func TestGenerateShortTranscriptUsesOneCall(t *testing.T) {
	s := &stubLLM{reply: "## Ringkasan\nRapat membahas anggaran."}
	got, err := newStub(t, s).Generate(context.Background(), "[00:01] halo\n", "", oneSection, 6000)
	if err != nil || got != s.reply {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if len(s.calls) != 1 || !strings.Contains(s.calls[0], "<transkrip>") {
		t.Fatalf("expected one final call with the transcript, got %d", len(s.calls))
	}
}

func TestGenerateLongTranscriptMapsThenCombines(t *testing.T) {
	s := &stubLLM{reply: "ringkasan potongan"}
	long := strings.Repeat("[00:01] kalimat panjang dalam rapat bulanan\n", 300)
	if _, err := newStub(t, s).Generate(context.Background(), long, "", oneSection, 1000); err != nil {
		t.Fatal(err)
	}
	n := len(s.calls)
	if n < 4 {
		t.Fatalf("expected chunk calls + combine + final, got %d calls", n)
	}
	if !strings.Contains(s.calls[0], "<potongan_transkrip>") || !strings.Contains(s.calls[n-2], "<ringkasan>") || !strings.Contains(s.calls[n-1], "<transkrip>") {
		t.Fatal("calls are not in chunk, combine, final order")
	}
}

func TestGenerateRetriesOnceThenFails(t *testing.T) {
	s := &stubLLM{reply: "## Ringkasan\nok", failures: 1}
	if _, err := newStub(t, s).Generate(context.Background(), "x\n", "", oneSection, 6000); err != nil {
		t.Fatalf("one failure should be retried: %v", err)
	}
	s2 := &stubLLM{reply: "ok", failures: 2}
	_, err := newStub(t, s2).Generate(context.Background(), "x\n", "", oneSection, 6000)
	if err == nil || len(s2.calls) != 2 {
		t.Fatalf("second failure must fail the step after exactly 2 calls; err=%v calls=%d", err, len(s2.calls))
	}
}

func TestGenerateEmptyOutputIsErrEmpty(t *testing.T) {
	s := &stubLLM{reply: "<think>hanya berpikir</think>"}
	_, err := newStub(t, s).Generate(context.Background(), "x\n", "", oneSection, 6000)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
}

func TestGeneratePassesNotetakerContextToFinalCall(t *testing.T) {
	s := &stubLLM{reply: "## Ringkasan\nok"}
	if _, err := newStub(t, s).Generate(context.Background(), "[00:01] halo\n", "Pak Budi = Direktur Keuangan", oneSection, 6000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.calls[0], "<konteks>\nPak Budi = Direktur Keuangan\n</konteks>") {
		t.Fatalf("final prompt lacks the context block: %q", s.calls[0])
	}
	if strings.Contains(FinalUserPrompt("x", "   "), "<konteks>") {
		t.Fatal("blank context must not add a block")
	}
}

func TestFormatTranscript(t *testing.T) {
	got := FormatTranscript([]Line{{Start: 65, Text: "pembukaan"}, {Start: 3725, Text: "penutup"}})
	want := "[01:05] pembukaan\n[01:02:05] penutup\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
