package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"notulensi/internal/auth"
)

func TestTanggalAndDurasi(t *testing.T) {
	if got := Tanggal(time.Date(2026, 11, 20, 18, 30, 0, 0, time.UTC)); got != "21 November 2026" {
		t.Fatalf("Tanggal uses WIB: got %q", got)
	}
	if got := TanggalJam(time.Date(2026, 1, 5, 2, 7, 0, 0, time.UTC)); got != "5 Januari 2026 09:07 WIB" {
		t.Fatalf("TanggalJam: %q", got)
	}
	for sec, want := range map[int]string{65: "1:05", 3725: "1:02:05"} {
		if got := Durasi(sec); got != want {
			t.Errorf("Durasi(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestParseDateRange(t *testing.T) {
	if _, _, ok := ParseDateRange("2026-11-01", "2026-11-30"); !ok {
		t.Error("valid range rejected")
	}
	if _, _, ok := ParseDateRange("", ""); !ok {
		t.Error("empty range rejected")
	}
	if _, _, ok := ParseDateRange("2026-11-30", "2026-11-01"); ok {
		t.Error("inverted range accepted")
	}
	if _, _, ok := ParseDateRange("30/11/2026", ""); ok {
		t.Error("bad format accepted")
	}
}

// Every page must parse and execute with representative data, so template errors fail here, not at runtime.
func TestEveryPageRenders(t *testing.T) {
	r := NewRenderer()
	admin := &auth.User{ID: "u1", Username: "admin", FullName: "Administrator", Role: "admin"}
	pages := map[string]any{
		"meetings.html":        nil,
		"meeting_new.html":     map[string]any{"Mode": "record", "Today": "2026-11-21"},
		"meeting.html":         map[string]any{"id": "m1", "title": "Rapat <b>"},
		"admin_users.html":     nil,
		"admin_templates.html": nil,
		"admin_audit.html":     []string{"login_success", "export"},
		"login.html":           "Sesi berakhir. Silakan masuk kembali.",
	}
	for name, data := range pages {
		req := httptest.NewRequest("GET", "/", nil)
		if name != "login.html" {
			req = req.WithContext(auth.WithUser(req.Context(), admin))
		}
		rec := httptest.NewRecorder()
		r.Render(rec, req, name, Page{Title: "Judul", Active: "users", Data: data})
		body := rec.Body.String()
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s did not render a full document", name)
		}
		if name != "login.html" && !strings.Contains(body, "Notulensi AI") {
			t.Errorf("%s is missing the sidebar", name)
		}
	}
}

func TestIconRendersKnownLucideIcon(t *testing.T) {
	out := string(Icon("mic", "w-4 h-4"))
	if !strings.Contains(out, `class="lucide lucide-mic w-4 h-4"`) || !strings.Contains(out, `<path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/>`) {
		t.Fatalf("unexpected icon markup: %s", out)
	}
}
