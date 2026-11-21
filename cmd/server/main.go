package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"notulensi/internal/audit"
	"notulensi/internal/auth"
	"notulensi/internal/db"
	"notulensi/internal/llm"
	"notulensi/internal/meeting"
	"notulensi/internal/template"
	"notulensi/internal/transcribe"
	"notulensi/internal/user"
	"notulensi/internal/worker"
	"notulensi/web"
)

const referenceDoc = "deploy/reference.docx"

type config struct {
	DatabaseURL    string
	DataDir        string
	WhisperURL     string
	LLMBaseURL     string
	LLMModel       string
	LLMAPIKey      string
	TokenThreshold int
	RetentionDays  int
	AdminUsername  string
	AdminPassword  string
}

func loadConfig() (config, error) {
	c := config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		DataDir:       os.Getenv("DATA_DIR"),
		WhisperURL:    os.Getenv("WHISPER_URL"),
		LLMBaseURL:    os.Getenv("LLM_BASE_URL"),
		LLMModel:      os.Getenv("LLM_MODEL"),
		LLMAPIKey:     os.Getenv("LLM_API_KEY"),
		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
	}
	for name, v := range map[string]string{
		"DATABASE_URL": c.DatabaseURL, "DATA_DIR": c.DataDir, "WHISPER_URL": c.WhisperURL,
		"LLM_BASE_URL": c.LLMBaseURL, "LLM_MODEL": c.LLMModel,
	} {
		if v == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	var err error
	if c.TokenThreshold, err = intEnv("LLM_TOKEN_THRESHOLD", 6000, 1000, 1000000); err != nil {
		return c, err
	}
	if c.RetentionDays, err = intEnv("AUDIO_RETENTION_DAYS", 90, 1, 3650); err != nil {
		return c, err
	}
	return c, nil
}

func intEnv(name string, def, lo, hi int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi)
	}
	return n, nil
}

// loadDotEnv reads KEY=VALUE lines from .env for local development; variables already set win.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(strings.TrimSpace(key)); !set {
			os.Setenv(strings.TrimSpace(key), strings.TrimSpace(value))
		}
	}
	return nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := loadDotEnv(".env"); err != nil {
		slog.Error("read .env", "err", err)
		os.Exit(1)
	}
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := user.SeedAdmin(ctx, pool, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}
	if err := template.SeedDefault(ctx, pool); err != nil {
		return fmt.Errorf("seed template: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "audio"), 0o750); err != nil {
		return err
	}

	authSvc := auth.NewService(pool)
	render := web.NewRenderer()
	users := &user.Handler{Pool: pool, Auth: authSvc, Render: render}
	templates := &template.Handler{Pool: pool, Render: render}
	audits := &audit.Handler{Pool: pool, Render: render}
	meetings := &meeting.Handler{Pool: pool, Render: render, DataDir: cfg.DataDir, ReferenceDoc: referenceDoc, Now: time.Now}

	w := &worker.Worker{
		Pool:           pool,
		DataDir:        cfg.DataDir,
		Whisper:        &transcribe.Client{URL: cfg.WhisperURL},
		LLM:            llm.NewClient(cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey),
		TokenThreshold: cfg.TokenThreshold,
		RetentionDays:  cfg.RetentionDays,
		PollInterval:   5 * time.Second,
		Now:            time.Now,
	}
	if err := w.RequeueInterrupted(ctx); err != nil {
		return fmt.Errorf("requeue: %w", err)
	}
	go w.Run(ctx)
	go w.RunDaily(ctx)

	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", meetings.Home)
	app.HandleFunc("GET /meetings", meetings.ListPage)
	app.HandleFunc("GET /meetings/new", meetings.NewPage)
	app.HandleFunc("GET /meetings/{id}", meetings.DetailPage)
	app.HandleFunc("GET /admin/users", users.Page)
	app.HandleFunc("GET /admin/templates", templates.Page)
	app.HandleFunc("GET /admin/audit", audits.Page)

	app.HandleFunc("POST /api/auth/logout", users.Logout)
	app.HandleFunc("GET /api/me", users.Me)
	app.HandleFunc("GET /api/users", users.List)
	app.HandleFunc("POST /api/users", users.Create)
	app.HandleFunc("GET /api/users/options", users.Options)
	app.HandleFunc("PUT /api/users/{id}", users.Update)

	app.HandleFunc("GET /api/templates", templates.List)
	app.HandleFunc("POST /api/templates", templates.Create)
	app.HandleFunc("PUT /api/templates/{id}", templates.Update)
	app.HandleFunc("DELETE /api/templates/{id}", templates.Delete)

	app.HandleFunc("GET /api/meetings", meetings.List)
	app.HandleFunc("POST /api/meetings", meetings.Upload)
	app.HandleFunc("POST /api/recordings", meetings.StartRecording)
	app.HandleFunc("POST /api/recordings/{id}/chunks", meetings.AppendChunk)
	app.HandleFunc("POST /api/recordings/{id}/finish", meetings.FinishRecording)
	app.HandleFunc("GET /api/meetings/{id}", meetings.Detail)
	app.HandleFunc("GET /api/meetings/{id}/transcript", meetings.Transcript)
	app.HandleFunc("GET /api/meetings/{id}/audio", meetings.Audio)
	app.HandleFunc("PUT /api/meetings/{id}/minutes", meetings.SaveMinutes)
	app.HandleFunc("POST /api/meetings/{id}/regenerate", meetings.Regenerate)
	app.HandleFunc("POST /api/meetings/{id}/restore-backup", meetings.RestoreBackup)
	app.HandleFunc("POST /api/meetings/{id}/retry", meetings.Retry)
	app.HandleFunc("PUT /api/meetings/{id}/participants", meetings.SetParticipants)
	app.HandleFunc("GET /api/meetings/{id}/export", meetings.Export)
	app.HandleFunc("DELETE /api/meetings/{id}", meetings.Delete)
	app.HandleFunc("POST /api/markdown/preview", meetings.Preview)
	app.HandleFunc("GET /api/audit-logs", audits.List)

	root := http.NewServeMux()
	root.Handle("GET /static/", web.Static())
	root.HandleFunc("GET /login", users.LoginPage)
	root.HandleFunc("POST /api/auth/login", users.Login)
	root.Handle("/", authSvc.Require(app))

	srv := &http.Server{Addr: ":8080", Handler: root, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
