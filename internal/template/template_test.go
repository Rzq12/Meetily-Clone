package template_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"notulensi/internal/auth"
	"notulensi/internal/template"
	"notulensi/internal/testutil"
)

func TestValidate(t *testing.T) {
	ok := template.Template{Name: "T", Sections: []template.Section{{Title: "A", Instruction: "B", Format: "list"}}}
	if msg := template.Validate(ok); msg != "" {
		t.Fatalf("valid template rejected: %s", msg)
	}
	cases := map[string]template.Template{
		"Template harus memiliki minimal satu bagian.":      {Name: "T"},
		"Bagian bertipe tabel wajib memiliki format kolom.": {Name: "T", Sections: []template.Section{{Title: "A", Instruction: "B", Format: "table"}}},
		"Format bagian harus paragraf, daftar, atau tabel.": {Name: "T", Sections: []template.Section{{Title: "A", Instruction: "B", Format: "grid"}}},
	}
	for want, tpl := range cases {
		if got := template.Validate(tpl); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func send(t *testing.T, h http.HandlerFunc, u *auth.User, method, id string, body any) (int, map[string]any) {
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

func TestDefaultAndDeleteRules(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	if err := template.SeedDefault(ctx, pool); err != nil {
		t.Fatal(err)
	}
	admin := testutil.User(t, pool, "admin", "admin")
	h := &template.Handler{Pool: pool}

	code, created := send(t, h.Create, admin, "POST", "", template.Template{Name: "Singkat", Sections: []template.Section{{Title: "Ringkasan", Instruction: "Ringkas", Format: "paragraph"}}})
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	if code, body := send(t, h.Create, admin, "POST", "", template.Template{Name: "Singkat", Sections: []template.Section{{Title: "A", Instruction: "B", Format: "list"}}}); code != 409 || body["error"] != "Nama template sudah dipakai." {
		t.Fatalf("duplicate name: %d %v", code, body)
	}
	newID := created["id"].(string)

	if code, _ := send(t, h.Update, admin, "PUT", newID, map[string]bool{"set_default": true}); code != 200 {
		t.Fatalf("set default: %d", code)
	}
	var defaults int
	var defaultID string
	pool.QueryRow(ctx, `SELECT count(*), max(id::text) FROM templates WHERE is_default`).Scan(&defaults, &defaultID)
	if defaults != 1 || defaultID != newID {
		t.Fatalf("defaults=%d id=%s, want exactly the new template", defaults, defaultID)
	}

	if code, body := send(t, h.Delete, admin, "DELETE", newID, nil); code != 422 || body["error"] != "Template default tidak bisa dihapus." {
		t.Fatalf("delete default: %d %v", code, body)
	}
	var seedID string
	pool.QueryRow(ctx, `SELECT id FROM templates WHERE NOT is_default`).Scan(&seedID)
	owner := testutil.User(t, pool, "notulis", "notetaker")
	testutil.Meeting(t, pool, owner.ID, seedID, "Rapat", "done")
	if code, body := send(t, h.Delete, admin, "DELETE", seedID, nil); code != 422 || body["error"] != "Template sedang dipakai oleh rapat dan tidak bisa dihapus." {
		t.Fatalf("delete in use: %d %v", code, body)
	}
	if code, _ := send(t, h.Delete, owner, "DELETE", seedID, nil); code != 403 {
		t.Fatalf("notetaker delete: %d, want 403", code)
	}
}
