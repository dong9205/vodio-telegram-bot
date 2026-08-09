package taskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestStorePersistsAndRetriesFailedTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	payload := json.RawMessage(`{"items":[{"message_id":42}]}`)
	if err := store.Create(Task{ID: "task-1", Source: "mtproto", Status: StatusQueued}, payload); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	failed := StatusFailed
	errorText := "context canceled"
	downloaded := int64(2048)
	speed := float64(1024)
	if err := store.Update("task-1", Update{Status: &failed, Error: &errorText, DownloadedBytes: &downloaded, CurrentSpeedBPS: &speed}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatalf("reload returned error: %v", err)
	}
	listed := reloaded.List()
	if len(listed) != 1 || listed[0].Status != StatusFailed {
		t.Fatalf("reloaded tasks = %#v", listed)
	}
	if listed[0].Payload != nil {
		t.Fatal("List exposed private retry payload")
	}

	called := false
	reloaded.SetRetryHandler(func(_ context.Context, id string, got json.RawMessage, mode RetryMode) error {
		called = true
		if mode != RetryModeRestart {
			t.Fatalf("retry mode = %q", mode)
		}
		var compactGot bytes.Buffer
		var compactWant bytes.Buffer
		if err := json.Compact(&compactGot, got); err != nil {
			t.Fatalf("compact retry payload: %v", err)
		}
		if err := json.Compact(&compactWant, payload); err != nil {
			t.Fatalf("compact expected payload: %v", err)
		}
		if id != "task-1" || compactGot.String() != compactWant.String() {
			t.Fatalf("retry handler received id=%q payload=%s", id, got)
		}
		return nil
	})
	if err := reloaded.Retry(context.Background(), "task-1"); err != nil {
		t.Fatalf("Retry returned error: %v", err)
	}
	if !called {
		t.Fatal("retry handler was not called")
	}
	listed = reloaded.List()
	if listed[0].Status != StatusQueued || listed[0].Attempts != 2 || listed[0].RetryCount != 1 {
		t.Fatalf("task after retry = %#v", listed[0])
	}
	if listed[0].DownloadedBytes != 0 || listed[0].CurrentSpeedBPS != 0 {
		t.Fatalf("retry did not reset transfer metrics: %#v", listed[0])
	}
}

func TestStoreMarksInterruptedTaskFailedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if err := store.Create(Task{ID: "task-2", Status: StatusDownloading, DownloadedBytes: 2048, CurrentSpeedBPS: 4096, ETASeconds: 30}, json.RawMessage(`{"items":[{}]}`)); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatalf("reload returned error: %v", err)
	}
	task := reloaded.List()[0]
	if task.Status != StatusFailed || !task.Retryable {
		t.Fatalf("interrupted task = %#v", task)
	}
	if task.CurrentSpeedBPS != 0 || task.ETASeconds != 0 || task.DownloadFinishedAt == nil {
		t.Fatalf("interrupted task retained live transfer metrics = %#v", task)
	}
	if !task.Resumable {
		t.Fatalf("interrupted task was not marked resumable: %#v", task)
	}
}

func TestStoreResumesFailedTaskWithoutClearingProgress(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	payload := json.RawMessage(`{"items":[{"message_id":42}]}`)
	if err := store.Create(Task{
		ID:               "task-resume",
		Status:           StatusFailed,
		DownloadedBytes:  4096,
		CurrentFileBytes: 4096,
		Resumable:        true,
	}, payload); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	var gotMode RetryMode
	store.SetRetryHandler(func(_ context.Context, _ string, _ json.RawMessage, mode RetryMode) error {
		gotMode = mode
		return nil
	})
	if err := store.Resume(context.Background(), "task-resume"); err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	task := store.List()[0]
	if gotMode != RetryModeResume || task.Status != StatusQueued {
		t.Fatalf("resume mode=%q task=%#v", gotMode, task)
	}
	if task.DownloadedBytes != 4096 || task.CurrentFileBytes != 4096 {
		t.Fatalf("resume cleared partial progress: %#v", task)
	}
}
