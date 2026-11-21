package user

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"notulensi/internal/audit"
	"notulensi/internal/auth"
	"notulensi/web"
)

const msgBadCredentials = "Username atau password salah."

func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if u := h.Auth.Current(r); u != nil {
		http.Redirect(w, r, "/meetings", http.StatusSeeOther)
		return
	}
	notice := ""
	if r.URL.Query().Get("expired") == "1" {
		notice = "Sesi berakhir. Silakan masuk kembali."
	}
	h.Render.Render(w, r, "login.html", web.Page{Title: "Masuk", Data: notice})
}

type loginInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in loginInput
	if err := web.Decode(r, &in); err != nil {
		web.Error(w, http.StatusBadRequest, msgBadCredentials)
		return
	}
	ip := audit.ClientIP(r)
	ulen, plen := utf8.RuneCountInString(in.Username), utf8.RuneCountInString(in.Password)
	if ulen < 3 || ulen > 50 || plen < 8 || plen > 128 {
		audit.Write(r.Context(), h.Pool, "", "login_failed", "", ip, map[string]any{"username": in.Username})
		web.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	}

	token, u, knownID, err := h.Auth.Login(r.Context(), in.Username, in.Password)
	switch {
	case errors.Is(err, auth.ErrLocked):
		web.Error(w, http.StatusTooManyRequests, "Terlalu banyak percobaan. Coba lagi dalam 15 menit.")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		audit.Write(r.Context(), h.Pool, knownID, "login_failed", "", ip, map[string]any{"username": in.Username})
		web.Error(w, http.StatusUnauthorized, msgBadCredentials)
		return
	case err != nil:
		web.Internal(w, err)
		return
	}

	auth.SetSessionCookie(w, token)
	audit.Write(r.Context(), h.Pool, u.ID, "login_success", "", ip, nil)
	web.JSON(w, http.StatusOK, map[string]string{"redirect": "/meetings"})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.Auth.Logout(r.Context(), auth.SessionToken(r)); err != nil {
		web.Internal(w, err)
		return
	}
	auth.ClearSessionCookie(w)
	web.JSON(w, http.StatusOK, map[string]string{"redirect": "/login"})
}
