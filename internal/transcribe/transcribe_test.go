package transcribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWhisperSendsIndonesianAndParsesSegments(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" {
			http.NotFound(w, r)
			return
		}
		r.ParseMultipartForm(1 << 20)
		got = map[string]string{
			"language":        r.FormValue("language"),
			"response_format": r.FormValue("response_format"),
			"temperature":     r.FormValue("temperature"),
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "no file", 400)
			return
		}
		w.Write([]byte(`{"text":"x","segments":[{"start":0.0,"end":2.5,"text":" Selamat pagi."},{"start":2.5,"end":4,"text":"  "}]}`))
	}))
	defer srv.Close()

	wav := filepath.Join(t.TempDir(), "a.wav")
	os.WriteFile(wav, []byte("RIFF"), 0o644)
	segs, err := (&Client{URL: srv.URL + "/"}).Whisper(context.Background(), wav, 60)
	if err != nil {
		t.Fatal(err)
	}
	if got["language"] != "id" || got["response_format"] != "verbose_json" || got["temperature"] != "0" {
		t.Fatalf("request fields: %v", got)
	}
	if len(segs) != 2 || segs[0].End != 2.5 || segs[0].Text != " Selamat pagi." {
		t.Fatalf("segments: %+v", segs)
	}
}

func TestWhisperHTTPErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	wav := filepath.Join(t.TempDir(), "a.wav")
	os.WriteFile(wav, []byte("RIFF"), 0o644)
	if _, err := (&Client{URL: srv.URL}).Whisper(context.Background(), wav, 1); err == nil {
		t.Fatal("expected an error for HTTP 503")
	}
}
