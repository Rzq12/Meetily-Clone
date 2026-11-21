package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
	_ "time/tzdata"

	"notulensi/internal/auth"
)

//go:embed templates/*.html static
var assets embed.FS

func Static() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

var Jakarta = mustLoad("Asia/Jakarta")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

var months = [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}

// Tanggal formats a date the SRS way: 21 November 2026.
func Tanggal(t time.Time) string {
	t = t.In(Jakarta)
	return fmt.Sprintf("%d %s %d", t.Day(), months[t.Month()-1], t.Year())
}

func TanggalJam(t time.Time) string {
	t = t.In(Jakarta)
	return fmt.Sprintf("%s %02d:%02d WIB", Tanggal(t), t.Hour(), t.Minute())
}

// Durasi renders seconds as h:mm:ss, or m:ss under an hour.
func Durasi(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec%3600/60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

var statusLabels = map[string]string{
	"recording":    "Merekam",
	"queued":       "Dalam antrean",
	"transcribing": "Mentranskrip",
	"summarizing":  "Menyusun notulen",
	"done":         "Selesai",
	"failed":       "Gagal",
}

func StatusLabel(s string) string { return statusLabels[s] }

var roleLabels = map[string]string{"admin": "Admin", "notetaker": "Notulis", "viewer": "Pembaca"}

func RoleLabel(r string) string { return roleLabels[r] }

var funcs = template.FuncMap{
	"icon":        Icon,
	"tanggal":     Tanggal,
	"tanggalJam":  TanggalJam,
	"durasi":      Durasi,
	"statusLabel": StatusLabel,
	"roleLabel":   RoleLabel,
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		return template.JS(b), err
	},
}

type Page struct {
	Title  string
	Active string
	User   *auth.User
	CSRF   string
	Data   any
}

type Renderer struct {
	pages map[string]*template.Template
}

// NewRenderer parses each page together with the shared layout. login.html is standalone.
func NewRenderer() *Renderer {
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		panic(err)
	}
	r := &Renderer{pages: map[string]*template.Template{}}
	for _, path := range names {
		name := path[len("templates/"):]
		if name == "layout.html" {
			continue
		}
		files := []string{path}
		if name != "login.html" {
			files = append([]string{"templates/layout.html"}, path)
		}
		r.pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(assets, files...))
	}
	return r
}

func (rd *Renderer) Render(w http.ResponseWriter, req *http.Request, name string, p Page) {
	t, ok := rd.pages[name]
	if !ok {
		panic("unknown page " + name)
	}
	if p.User == nil {
		p.User = auth.UserFrom(req.Context())
	}
	if p.User != nil {
		p.CSRF = auth.CSRFFrom(req.Context())
	}
	root := "layout"
	if name == "login.html" {
		root = "login"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, root, p); err != nil {
		slog.Error("render page", "page", name, "err", err)
	}
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "err", err)
	}
}

func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// Internal logs the real error and returns a generic 500.
func Internal(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	Error(w, http.StatusInternalServerError, "Terjadi kesalahan pada server.")
}

func Decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20)).Decode(v)
}

func NotFound(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "Tidak ditemukan.")
}

// ParseDate reads YYYY-MM-DD as midnight WIB.
func ParseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, Jakarta)
}

// ParseDateRange accepts empty bounds; ok is false for a bad date or from > to.
func ParseDateRange(fromStr, toStr string) (from, to time.Time, ok bool) {
	var err error
	if fromStr != "" {
		if from, err = ParseDate(fromStr); err != nil {
			return from, to, false
		}
	}
	if toStr != "" {
		if to, err = ParseDate(toStr); err != nil {
			return from, to, false
		}
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return from, to, false
	}
	return from, to, true
}
