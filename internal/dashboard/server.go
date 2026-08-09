package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vodio-telegram-bot/internal/taskstore"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	address     string
	storageRoot string
	store       *taskstore.Store
	logger      *slog.Logger
}

func New(address, storageRoot string, store *taskstore.Store, logger *slog.Logger) *Server {
	absRoot, err := filepath.Abs(storageRoot)
	if err != nil {
		absRoot = filepath.Clean(storageRoot)
	}
	return &Server{address: address, storageRoot: absRoot, store: store, logger: logger}
}

type previewMedia struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
	path string
}

type taskView struct {
	taskstore.Task
	Preview []previewMedia `json:"preview"`
}

func (s *Server) Run(ctx context.Context) error {
	handler, err := s.handler()
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              s.address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("dashboard shutdown failed", "error", err)
		}
	}()

	s.logger.Info("dashboard started", "url", "http://"+s.address)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return ctx.Err()
	}
	return err
}

func (s *Server) handler() (http.Handler, error) {
	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded dashboard files: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.retryTask)
	mux.HandleFunc("POST /api/tasks/{id}/resume", s.resumeTask)
	mux.HandleFunc("GET /api/tasks/{id}/media/{index}", s.serveMedia)
	mux.Handle("/", http.FileServer(http.FS(staticRoot)))
	return securityHeaders(mux), nil
}

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	tasks := s.store.List()
	views := make([]taskView, 0, len(tasks))
	now := time.Now()
	summary := map[taskstore.Status]int{
		taskstore.StatusQueued:      0,
		taskstore.StatusClassifying: 0,
		taskstore.StatusDownloading: 0,
		taskstore.StatusSucceeded:   0,
		taskstore.StatusFailed:      0,
	}
	var activeSpeedBPS float64
	var downloadedBytes int64
	var totalBytes int64
	for _, task := range tasks {
		views = append(views, taskView{Task: task, Preview: s.previewForTask(task)})
		summary[task.Status]++
		downloadedBytes += task.DownloadedBytes
		totalBytes += task.TotalBytes
		if task.Status == taskstore.StatusDownloading && now.Sub(task.UpdatedAt) <= 3*time.Second {
			activeSpeedBPS += task.CurrentSpeedBPS
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":   views,
		"summary": summary,
		"transfer": map[string]any{
			"active_speed_bps": activeSpeedBPS,
			"downloaded_bytes": downloadedBytes,
			"total_bytes":      totalBytes,
		},
		"generated_at": now,
	})
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	task, ok := s.store.Get(strings.TrimSpace(r.PathValue("id")))
	if !ok {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	media := s.previewForTask(task)
	if err != nil || index < 0 || index >= len(media) {
		http.NotFound(w, r)
		return
	}
	item := media[index]
	file, err := os.Open(item.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", item.MIME)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": item.Name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, item.Name, info.ModTime(), file)
}

func (s *Server) previewForTask(task taskstore.Task) []previewMedia {
	archiveRoot, ok := securePath(s.storageRoot, task.ArchivePath)
	if !ok {
		return []previewMedia{}
	}
	items := make([]previewMedia, 0, len(task.SavedFiles))
	for _, name := range task.SavedFiles {
		if name == "" || filepath.Base(name) != name {
			continue
		}
		kind, mimeType := previewType(name)
		if kind == "" {
			continue
		}
		path, ok := securePath(archiveRoot, filepath.Join(archiveRoot, name))
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		index := len(items)
		items = append(items, previewMedia{
			Name: name,
			Kind: kind,
			MIME: mimeType,
			Size: info.Size(),
			URL:  fmt.Sprintf("/api/tasks/%s/media/%d", url.PathEscape(task.ID), index),
			path: path,
		})
	}
	return items
}

func securePath(root, target string) (string, bool) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil || !isWithin(absRoot, absTarget) {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", false
	}
	current := absRoot
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return "", false
			}
		}
	}
	return absTarget, true
}

func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func previewType(name string) (string, string) {
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if strings.HasPrefix(mimeType, "image/") {
		return "image", mimeType
	}
	if strings.HasPrefix(mimeType, "video/") {
		return "video", mimeType
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image", "image/jpeg"
	case ".png":
		return "image", "image/png"
	case ".webp":
		return "image", "image/webp"
	case ".gif":
		return "image", "image/gif"
	case ".mp4", ".m4v":
		return "video", "video/mp4"
	case ".mov":
		return "video", "video/quicktime"
	case ".webm":
		return "video", "video/webm"
	case ".mkv":
		return "video", "video/x-matroska"
	default:
		return "", ""
	}
}

func (s *Server) retryTask(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, false)
}

func (s *Server) resumeTask(w http.ResponseWriter, r *http.Request) {
	s.taskAction(w, r, true)
}

func (s *Server) taskAction(w http.ResponseWriter, r *http.Request, resume bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少任务 ID"})
		return
	}
	var err error
	if resume {
		err = s.store.Resume(r.Context(), id)
	} else {
		err = s.store.Retry(r.Context(), id)
	}
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
