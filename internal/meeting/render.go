package meeting

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"notulensi/web"
)

// Raw HTML stays disabled (goldmark's default) so minutes cannot inject markup.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

var timestampPattern = regexp.MustCompile(`\[(\d{1,2}):(\d{2})(?::(\d{2}))?\]`)

// RenderMinutes converts Markdown to HTML and turns [mm:ss] / [hh:mm:ss] into seek buttons.
func RenderMinutes(markdown string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(markdown), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(markdown))
	}
	out := timestampPattern.ReplaceAllStringFunc(buf.String(), func(m string) string {
		p := timestampPattern.FindStringSubmatch(m)
		a, _ := strconv.Atoi(p[1])
		b, _ := strconv.Atoi(p[2])
		sec := a*60 + b
		if p[3] != "" {
			c, _ := strconv.Atoi(p[3])
			sec = a*3600 + b*60 + c
		}
		return fmt.Sprintf(`<button type="button" class="ts-link" data-seek="%d">%s</button>`, sec, m)
	})
	return template.HTML(out)
}

type previewInput struct {
	Markdown string `json:"markdown"`
}

// Preview renders the editor's live preview with the same pipeline as saved minutes.
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	var in previewInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, "Isi tidak valid.")
		return
	}
	web.JSON(w, http.StatusOK, map[string]string{"html": string(RenderMinutes(in.Markdown))})
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slug(title string) string {
	s := strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		s = "rapat"
	}
	return s
}
