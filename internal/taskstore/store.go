package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusClassifying Status = "classifying"
	StatusDownloading Status = "downloading"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
)

type Task struct {
	ID                     string          `json:"id"`
	Source                 string          `json:"source"`
	Status                 Status          `json:"status"`
	Title                  string          `json:"title,omitempty"`
	Caption                string          `json:"caption,omitempty"`
	MediaCount             int             `json:"media_count"`
	VideoCount             int             `json:"video_count"`
	TotalBytes             int64           `json:"total_bytes"`
	DownloadedBytes        int64           `json:"downloaded_bytes"`
	CurrentFileBytes       int64           `json:"current_file_bytes"`
	CurrentFileTotalBytes  int64           `json:"current_file_total_bytes"`
	CurrentSpeedBPS        float64         `json:"current_speed_bps"`
	AverageSpeedBPS        float64         `json:"average_speed_bps"`
	ETASeconds             int64           `json:"eta_seconds"`
	DownloadElapsedSeconds int64           `json:"download_elapsed_seconds"`
	DownloadStartedAt      *time.Time      `json:"download_started_at,omitempty"`
	DownloadFinishedAt     *time.Time      `json:"download_finished_at,omitempty"`
	CurrentFile            string          `json:"current_file,omitempty"`
	Directory              string          `json:"directory,omitempty"`
	ArchiveName            string          `json:"archive_name,omitempty"`
	ArchivePath            string          `json:"archive_path,omitempty"`
	SavedFiles             []string        `json:"saved_files,omitempty"`
	FailedFiles            []string        `json:"failed_files,omitempty"`
	Error                  string          `json:"error,omitempty"`
	Attempts               int             `json:"attempts"`
	RetryCount             int             `json:"retry_count"`
	DataCenter             string          `json:"data_center,omitempty"`
	ConnectionMode         string          `json:"connection_mode,omitempty"`
	MTProtoProxyEnabled    bool            `json:"mtproto_proxy_enabled"`
	DownloadThreads        int             `json:"download_threads"`
	Retryable              bool            `json:"retryable"`
	Resumable              bool            `json:"resumable"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
	Payload                json.RawMessage `json:"payload,omitempty"`
}

type Update struct {
	Status                  *Status
	Title                   *string
	VideoCount              *int
	TotalBytes              *int64
	DownloadedBytes         *int64
	CurrentFileBytes        *int64
	CurrentFileTotalBytes   *int64
	CurrentSpeedBPS         *float64
	AverageSpeedBPS         *float64
	ETASeconds              *int64
	DownloadElapsedSeconds  *int64
	DownloadStartedAt       *time.Time
	DownloadFinishedAt      *time.Time
	ClearDownloadStartedAt  bool
	ClearDownloadFinishedAt bool
	CurrentFile             *string
	Directory               *string
	ArchiveName             *string
	ArchivePath             *string
	SavedFiles              []string
	FailedFiles             []string
	Error                   *string
	Retryable               *bool
	Resumable               *bool
	DataCenter              *string
	ConnectionMode          *string
	MTProtoProxyEnabled     *bool
	DownloadThreads         *int
	Payload                 json.RawMessage
}

type RetryMode string

const (
	RetryModeRestart RetryMode = "restart"
	RetryModeResume  RetryMode = "resume"
)

type RetryHandler func(context.Context, string, json.RawMessage, RetryMode) error

type Store struct {
	mu           sync.RWMutex
	path         string
	tasks        map[string]Task
	retryHandler RetryHandler
}

func New(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("task state path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve task state path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, fmt.Errorf("create task state directory: %w", err)
	}

	s := &Store{path: absPath, tasks: make(map[string]Task)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) SetRetryHandler(handler RetryHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryHandler = handler
}

func (s *Store) Create(task Task, payload json.RawMessage) error {
	now := time.Now()
	if task.ID == "" {
		return errors.New("task id is required")
	}
	if task.Status == "" {
		task.Status = StatusQueued
	}
	if task.Attempts == 0 {
		task.Attempts = 1
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	task.Payload = append(json.RawMessage(nil), payload...)
	task.Retryable = len(payload) > 0

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = cloneTask(task, false)
	return s.persistLocked()
}

func (s *Store) Update(id string, update Update) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %q not found", id)
	}
	applyUpdate(&task, update)
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	return s.persistLocked()
}

func (s *Store) GetPayload(id string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok || len(task.Payload) == 0 {
		return nil, false
	}
	return append(json.RawMessage(nil), task.Payload...), true
}

// Get returns a task without the private retry payload.
func (s *Store) Get(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return Task{}, false
	}
	return cloneTask(task, true), true
}

func (s *Store) List() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tasks := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, cloneTask(task, true))
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})
	return tasks
}

func (s *Store) Retry(ctx context.Context, id string) error {
	return s.requeue(ctx, id, RetryModeRestart)
}

func (s *Store) Resume(ctx context.Context, id string) error {
	return s.requeue(ctx, id, RetryModeResume)
}

func (s *Store) requeue(ctx context.Context, id string, mode RetryMode) error {
	s.mu.Lock()
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("task %q not found", id)
	}
	if task.Status != StatusFailed {
		s.mu.Unlock()
		return fmt.Errorf("task %q is not failed", id)
	}
	if !task.Retryable || len(task.Payload) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("task %q is not retryable", id)
	}
	if mode == RetryModeResume && !task.Resumable {
		s.mu.Unlock()
		return fmt.Errorf("task %q has no partial download to resume", id)
	}
	handler := s.retryHandler
	if handler == nil {
		s.mu.Unlock()
		return errors.New("retry worker is not ready")
	}

	task.Status = StatusQueued
	task.Error = ""
	if mode == RetryModeRestart {
		task.CurrentFile = ""
		task.DownloadedBytes = 0
		task.CurrentFileBytes = 0
		task.CurrentFileTotalBytes = 0
	}
	task.CurrentSpeedBPS = 0
	task.AverageSpeedBPS = 0
	task.ETASeconds = 0
	task.DownloadElapsedSeconds = 0
	task.DownloadStartedAt = nil
	task.DownloadFinishedAt = nil
	task.FailedFiles = nil
	task.Resumable = mode == RetryModeResume
	task.Attempts++
	task.RetryCount++
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	payload := append(json.RawMessage(nil), task.Payload...)
	s.mu.Unlock()

	if err := handler(ctx, id, payload, mode); err != nil {
		status := StatusFailed
		retryable := true
		resumable := mode == RetryModeResume
		errorText := err.Error()
		_ = s.Update(id, Update{Status: &status, Error: &errorText, Retryable: &retryable, Resumable: &resumable})
		return err
	}
	return nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read task state: %w", err)
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return fmt.Errorf("decode task state: %w", err)
	}

	changed := false
	for _, task := range tasks {
		switch task.Status {
		case StatusQueued, StatusClassifying, StatusDownloading:
			wasDownloading := task.Status == StatusDownloading
			now := time.Now()
			task.Status = StatusFailed
			task.Retryable = len(task.Payload) > 0
			if wasDownloading && task.DownloadedBytes > 0 {
				task.Resumable = true
				task.Error = "程序在任务完成前退出，可继续下载或重新下载"
			} else {
				task.Error = "程序在任务完成前退出，可点击重新下载"
			}
			task.CurrentSpeedBPS = 0
			task.ETASeconds = 0
			task.DownloadFinishedAt = &now
			task.UpdatedAt = now
			changed = true
		}
		s.tasks[task.ID] = task
	}
	if changed {
		return s.persistLocked()
	}
	return nil
}

func (s *Store) persistLocked() error {
	tasks := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tasks-*.tmp")
	if err != nil {
		return fmt.Errorf("create task state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure task state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write task state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close task state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace task state: %w", err)
	}
	cleanup = false
	return nil
}

func applyUpdate(task *Task, update Update) {
	if update.Status != nil {
		task.Status = *update.Status
	}
	if update.Title != nil {
		task.Title = *update.Title
	}
	if update.VideoCount != nil {
		task.VideoCount = *update.VideoCount
	}
	if update.TotalBytes != nil {
		task.TotalBytes = *update.TotalBytes
	}
	if update.DownloadedBytes != nil {
		task.DownloadedBytes = *update.DownloadedBytes
	}
	if update.CurrentFileBytes != nil {
		task.CurrentFileBytes = *update.CurrentFileBytes
	}
	if update.CurrentFileTotalBytes != nil {
		task.CurrentFileTotalBytes = *update.CurrentFileTotalBytes
	}
	if update.CurrentSpeedBPS != nil {
		task.CurrentSpeedBPS = *update.CurrentSpeedBPS
	}
	if update.AverageSpeedBPS != nil {
		task.AverageSpeedBPS = *update.AverageSpeedBPS
	}
	if update.ETASeconds != nil {
		task.ETASeconds = *update.ETASeconds
	}
	if update.DownloadElapsedSeconds != nil {
		task.DownloadElapsedSeconds = *update.DownloadElapsedSeconds
	}
	if update.ClearDownloadStartedAt {
		task.DownloadStartedAt = nil
	} else if update.DownloadStartedAt != nil {
		startedAt := *update.DownloadStartedAt
		task.DownloadStartedAt = &startedAt
	}
	if update.ClearDownloadFinishedAt {
		task.DownloadFinishedAt = nil
	} else if update.DownloadFinishedAt != nil {
		finishedAt := *update.DownloadFinishedAt
		task.DownloadFinishedAt = &finishedAt
	}
	if update.CurrentFile != nil {
		task.CurrentFile = *update.CurrentFile
	}
	if update.Directory != nil {
		task.Directory = *update.Directory
	}
	if update.ArchiveName != nil {
		task.ArchiveName = *update.ArchiveName
	}
	if update.ArchivePath != nil {
		task.ArchivePath = *update.ArchivePath
	}
	if update.SavedFiles != nil {
		task.SavedFiles = append([]string(nil), update.SavedFiles...)
	}
	if update.FailedFiles != nil {
		task.FailedFiles = append([]string(nil), update.FailedFiles...)
	}
	if update.Error != nil {
		task.Error = *update.Error
	}
	if update.Retryable != nil {
		task.Retryable = *update.Retryable
	}
	if update.Resumable != nil {
		task.Resumable = *update.Resumable
	}
	if update.DataCenter != nil {
		task.DataCenter = *update.DataCenter
	}
	if update.ConnectionMode != nil {
		task.ConnectionMode = *update.ConnectionMode
	}
	if update.MTProtoProxyEnabled != nil {
		task.MTProtoProxyEnabled = *update.MTProtoProxyEnabled
	}
	if update.DownloadThreads != nil {
		task.DownloadThreads = *update.DownloadThreads
	}
	if update.Payload != nil {
		task.Payload = append(json.RawMessage(nil), update.Payload...)
	}
}

func cloneTask(task Task, hidePayload bool) Task {
	task.SavedFiles = append([]string(nil), task.SavedFiles...)
	task.FailedFiles = append([]string(nil), task.FailedFiles...)
	if hidePayload {
		task.Payload = nil
	} else {
		task.Payload = append(json.RawMessage(nil), task.Payload...)
	}
	return task
}
