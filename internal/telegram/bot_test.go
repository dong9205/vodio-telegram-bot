package telegram

import (
	"errors"
	"strings"
	"testing"
	"time"

	"vodio-telegram-bot/internal/notification"
	"vodio-telegram-bot/internal/taskstore"
)

func TestSafeErrorRedactsBotToken(t *testing.T) {
	const token = "123456:secret-token"
	bot := &Bot{token: token}

	got := bot.safeError(errors.New("request https://api.telegram.org/bot" + token + "/sendMessage failed"))
	if strings.Contains(got, token) {
		t.Fatalf("safeError leaked bot token: %s", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("safeError did not mark redacted content: %s", got)
	}
}

func TestFormatDownloadComplete(t *testing.T) {
	started := time.Date(2026, 8, 9, 10, 0, 0, 0, time.Local)
	finished := started.Add(90 * time.Second)
	message := formatDownloadComplete(notification.DownloadComplete{
		Task: taskstore.Task{
			Title:                  "测试视频",
			VideoCount:             1,
			DownloadedBytes:        128 * 1024 * 1024,
			AverageSpeedBPS:        2 * 1024 * 1024,
			DownloadStartedAt:      &started,
			DownloadFinishedAt:     &finished,
			DownloadElapsedSeconds: 90,
			ArchivePath:            `D:\archive\test`,
			SavedFiles:             []string{"description.txt", "video.mp4"},
		},
		Queue: notification.QueueStats{
			DownloadingTasks:  1,
			DownloadingVideos: 2,
			WaitingTasks:      3,
			WaitingVideos:     4,
		},
	})

	for _, expected := range []string{
		"✅ 视频下载完成",
		"标题：测试视频",
		"传输大小：128 MB",
		"平均速度：2.0 MB/s",
		"开始时间：2026-08-09 10:00:00",
		"结束时间：2026-08-09 10:01:30",
		"下载耗时：1 分 30 秒",
		`归档位置：D:\archive\test`,
		"正在下载：2 个视频（1 个任务）",
		"等待处理：4 个视频（3 个任务）",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("completion message missing %q:\n%s", expected, message)
		}
	}
}
