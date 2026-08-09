package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vodio-telegram-bot/internal/taskstore"
)

func newTestServer(t *testing.T, root string, store *taskstore.Store) *Server {
	t.Helper()
	return New("127.0.0.1:0", root, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestListTasksHidesRetryPayload(t *testing.T) {
	root := t.TempDir()
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{ID: "task-1", Status: taskstore.StatusFailed}, json.RawMessage(`{"private":true}`)); err != nil {
		t.Fatalf("create task: %v", err)
	}
	recorder := httptest.NewRecorder()
	newTestServer(t, root, store).listTasks(recorder, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("response is not JSON: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "private") || strings.Contains(recorder.Body.String(), "payload") {
		t.Fatalf("response exposed retry payload: %s", recorder.Body.String())
	}
}

func TestListTasksIncludesAggregateBandwidth(t *testing.T) {
	root := t.TempDir()
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{
		ID: "task-bandwidth", Status: taskstore.StatusDownloading,
		TotalBytes: 4096, DownloadedBytes: 1024, CurrentSpeedBPS: 512,
	}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}
	recorder := httptest.NewRecorder()
	newTestServer(t, root, store).listTasks(recorder, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))

	var response struct {
		Transfer struct {
			ActiveSpeedBPS  float64 `json:"active_speed_bps"`
			DownloadedBytes int64   `json:"downloaded_bytes"`
		} `json:"transfer"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Transfer.ActiveSpeedBPS != 512 || response.Transfer.DownloadedBytes != 1024 {
		t.Fatalf("transfer summary = %#v", response.Transfer)
	}
}

func TestRetryTaskQueuesFailedTask(t *testing.T) {
	root := t.TempDir()
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{ID: "task-1", Status: taskstore.StatusFailed}, json.RawMessage(`{"items":[{}]}`)); err != nil {
		t.Fatalf("create task: %v", err)
	}
	store.SetRetryHandler(func(context.Context, string, json.RawMessage, taskstore.RetryMode) error { return nil })
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/retry", nil)
	request.SetPathValue("id", "task-1")
	recorder := httptest.NewRecorder()
	newTestServer(t, root, store).retryTask(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := store.List()[0].Status; got != taskstore.StatusQueued {
		t.Fatalf("task status = %q", got)
	}
}

func TestResumeTaskQueuesPartialDownload(t *testing.T) {
	root := t.TempDir()
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{ID: "task-resume", Status: taskstore.StatusFailed, Resumable: true}, json.RawMessage(`{"items":[{}]}`)); err != nil {
		t.Fatalf("create task: %v", err)
	}
	var mode taskstore.RetryMode
	store.SetRetryHandler(func(_ context.Context, _ string, _ json.RawMessage, got taskstore.RetryMode) error {
		mode = got
		return nil
	})
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/task-resume/resume", nil)
	request.SetPathValue("id", "task-resume")
	recorder := httptest.NewRecorder()
	newTestServer(t, root, store).resumeTask(recorder, request)

	if recorder.Code != http.StatusAccepted || mode != taskstore.RetryModeResume {
		t.Fatalf("status=%d mode=%q body=%s", recorder.Code, mode, recorder.Body.String())
	}
}

func TestEmbeddedDashboardIsServedWithSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler, err := newTestServer(t, root, store).handler()
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Telegram 归档预览") {
		t.Fatalf("unexpected dashboard body: %s", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("dashboard response is missing Content-Security-Policy")
	}
}

func TestPreviewPreservesSavedMediaOrder(t *testing.T) {
	root := t.TempDir()
	archiveDir := filepath.Join(root, "Category", "Archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("create archive: %v", err)
	}
	for name, contents := range map[string]string{
		"description.txt": "description",
		"first.jpg":       "jpeg-data",
		"second.mp4":      "video-data",
	} {
		if err := os.WriteFile(filepath.Join(archiveDir, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	store, err := taskstore.New(filepath.Join(root, ".dashboard", "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{
		ID: "task-preview", Status: taskstore.StatusSucceeded, ArchivePath: archiveDir,
		SavedFiles: []string{"description.txt", "first.jpg", "second.mp4"},
	}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}

	items := newTestServer(t, root, store).previewForTask(store.List()[0])
	if len(items) != 2 {
		t.Fatalf("preview count = %d, want 2", len(items))
	}
	if items[0].Name != "first.jpg" || items[0].Kind != "image" || items[1].Name != "second.mp4" || items[1].Kind != "video" {
		t.Fatalf("preview order = %#v", items)
	}
	if items[0].URL != "/api/tasks/task-preview/media/0" || items[1].URL != "/api/tasks/task-preview/media/1" {
		t.Fatalf("preview URLs = %#v", items)
	}
}

func TestServeMediaSupportsRangeRequests(t *testing.T) {
	root := t.TempDir()
	archiveDir := filepath.Join(root, "Archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("create archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "clip.mp4"), []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	store, err := taskstore.New(filepath.Join(root, ".dashboard", "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Create(taskstore.Task{ID: "range", ArchivePath: archiveDir, SavedFiles: []string{"clip.mp4"}}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}
	handler, err := newTestServer(t, root, store).handler()
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/range/media/0", nil)
	request.Header.Set("Range", "bytes=2-5")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "2345" {
		t.Fatalf("range response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestPreviewRejectsPathsOutsideStorageRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.jpg"), []byte("private"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	store, err := taskstore.New(filepath.Join(root, "tasks.json"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	items := newTestServer(t, root, store).previewForTask(taskstore.Task{
		ArchivePath: outside, SavedFiles: []string{"private.jpg"},
	})
	if len(items) != 0 {
		t.Fatalf("outside file was exposed: %#v", items)
	}
}
