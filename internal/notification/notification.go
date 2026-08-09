package notification

import (
	"context"

	"vodio-telegram-bot/internal/taskstore"
)

// QueueStats describes video work remaining after a download completes.
type QueueStats struct {
	DownloadingTasks  int
	DownloadingVideos int
	WaitingTasks      int
	WaitingVideos     int
}

// DownloadComplete contains the persisted transfer metrics and current queue state.
type DownloadComplete struct {
	Task  taskstore.Task
	Queue QueueStats
}

// Sender delivers archive lifecycle notifications.
type Sender interface {
	NotifyDownloadComplete(context.Context, DownloadComplete) error
}
