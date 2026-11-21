package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	CookieName   = "session"
	IdleTimeout  = 8 * time.Hour
	lockWindow   = 15 * time.Minute
	lockAttempts = 5
	BcryptCost   = 12
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrLocked             = errors.New("too many attempts")
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

// CanCreateMeetings covers upload, browser recording, and the participant picker.
func (u *User) CanCreateMeetings() bool { return u.Role == "admin" || u.Role == "notetaker" }

type ctxKey int

const (
	userKey ctxKey = iota
	tokenKey
)

func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userKey).(*User)
	return u
}

// WithUser is for tests that call handlers directly.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

func CSRFFrom(ctx context.Context) string {
	token, _ := ctx.Value(tokenKey).(string)
	return CSRFToken(token)
}

type Service struct {
	Pool *pgxpool.Pool
	Now  func() time.Time

	mu           sync.Mutex
	failures     map[string][]time.Time
	blockedUntil map[string]time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		Pool:         pool,
		Now:          time.Now,
		failures:     map[string][]time.Time{},
		blockedUntil: map[string]time.Time{},
	}
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CSRFToken is derived from the session token, so it needs no storage and dies with the session.
func CSRFToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("csrf:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	return string(b), err
}

func (s *Service) isBlocked(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Now().Before(s.blockedUntil[username])
}

func (s *Service) recordFailure(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	recent := []time.Time{}
	for _, t := range s.failures[username] {
		if now.Sub(t) < lockWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	if len(recent) >= lockAttempts {
		s.blockedUntil[username] = now.Add(lockWindow)
		recent = nil
	}
	s.failures[username] = recent
}

func (s *Service) clearFailures(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, username)
}

// Login checks credentials and creates a session. knownUserID is set for an
// existing username even on failure, so the caller can audit it.
func (s *Service) Login(ctx context.Context, username, password string) (token string, user *User, knownUserID string, err error) {
	if s.isBlocked(username) {
		return "", nil, "", ErrLocked
	}

	var hash string
	var active bool
	u := &User{}
	err = s.Pool.QueryRow(ctx,
		`SELECT id, username, full_name, role, password_hash, is_active FROM users WHERE username = $1`,
		username).Scan(&u.ID, &u.Username, &u.FullName, &u.Role, &hash, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		s.recordFailure(username)
		return "", nil, "", ErrInvalidCredentials
	}
	if err != nil {
		return "", nil, "", err
	}
	if !active || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.recordFailure(username)
		return "", nil, u.ID, ErrInvalidCredentials
	}
	s.clearFailures(username)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, u.ID, err
	}
	token = hex.EncodeToString(raw)
	if _, err := s.Pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, last_seen_at, created_at) VALUES ($1, $2, $3, $3)`,
		HashToken(token), u.ID, s.Now()); err != nil {
		return "", nil, u.ID, err
	}
	return token, u, u.ID, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, HashToken(token))
	return err
}

// lookup returns the session's user and refreshes last_seen_at, or nil when the
// session is missing, idle too long, or belongs to a deactivated user.
func (s *Service) lookup(ctx context.Context, token string) (*User, error) {
	now := s.Now()
	u := &User{}
	err := s.Pool.QueryRow(ctx, `
		UPDATE sessions s SET last_seen_at = $2
		FROM users u
		WHERE s.token_hash = $1 AND s.user_id = u.id AND u.is_active AND s.last_seen_at > $3
		RETURNING u.id, u.username, u.full_name, u.role`,
		HashToken(token), now, now.Add(-IdleTimeout)).Scan(&u.ID, &u.Username, &u.FullName, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, HashToken(token)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Current returns the user for a request outside Require, such as /login.
func (s *Service) Current(r *http.Request) *User {
	token := SessionToken(r)
	if token == "" {
		return nil
	}
	u, _ := s.lookup(r.Context(), token)
	return u
}

func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func SessionToken(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// Require rejects requests without a valid session and checks the CSRF token
// on state-changing methods.
func (s *Service) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := SessionToken(r)
		var user *User
		if token != "" {
			var err error
			user, err = s.lookup(r.Context(), token)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if user == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"Sesi berakhir. Silakan masuk kembali."}`))
				return
			}
			target := "/login"
			if token != "" {
				ClearSessionCookie(w)
				target = "/login?expired=1"
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			sent := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(sent), []byte(CSRFToken(token))) != 1 {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, tokenKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
