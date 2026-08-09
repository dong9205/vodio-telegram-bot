package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveResumableStreamToDirContinuesFromAlignedPartial(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatalf("create storage manager: %v", err)
	}
	dir := filepath.Join(root, "archive")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create archive dir: %v", err)
	}

	const alignment = int64(512 * 1024)
	const expectedSize = int64(1024 * 1024)
	firstChunk := bytes.Repeat([]byte{'a'}, 600*1024)
	_, err = manager.SaveResumableStreamToDir(
		context.Background(), dir, "video.mp4", "video/mp4", "task-video", false, expectedSize, alignment,
		func(_ context.Context, writer io.Writer, offset int64) error {
			if offset != 0 {
				t.Fatalf("initial offset = %d, want 0", offset)
			}
			if _, err := writer.Write(firstChunk); err != nil {
				return err
			}
			return errors.New("network disconnected")
		},
	)
	if err == nil {
		t.Fatal("first download unexpectedly succeeded")
	}

	secondChunk := bytes.Repeat([]byte{'b'}, int(expectedSize-alignment))
	saved, err := manager.SaveResumableStreamToDir(
		context.Background(), dir, "video.mp4", "video/mp4", "task-video", true, expectedSize, alignment,
		func(_ context.Context, writer io.Writer, offset int64) error {
			if offset != alignment {
				t.Fatalf("resume offset = %d, want %d", offset, alignment)
			}
			_, err := writer.Write(secondChunk)
			return err
		},
	)
	if err != nil {
		t.Fatalf("resume download: %v", err)
	}
	data, err := os.ReadFile(saved.Path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if len(data) != int(expectedSize) {
		t.Fatalf("saved size = %d, want %d", len(data), expectedSize)
	}
	if !bytes.Equal(data[:alignment], firstChunk[:alignment]) || !bytes.Equal(data[alignment:], secondChunk) {
		t.Fatal("saved file does not contain the expected resumed content")
	}
}

func TestDiscardPartialRemovesSavedProgress(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatalf("create storage manager: %v", err)
	}
	dir := filepath.Join(root, "archive")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create archive dir: %v", err)
	}
	_, _ = manager.SaveResumableStreamToDir(
		context.Background(), dir, "video.mp4", "video/mp4", "task-video", false, 1024, 512,
		func(_ context.Context, writer io.Writer, _ int64) error {
			_, _ = writer.Write([]byte("partial"))
			return errors.New("stop")
		},
	)
	if err := manager.DiscardPartial(dir, "task-video"); err != nil {
		t.Fatalf("discard partial: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".task-video.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file still exists: %v", err)
	}
}
