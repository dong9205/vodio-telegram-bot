package mtproto

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"vodio-telegram-bot/internal/config"
	"vodio-telegram-bot/internal/notification"
	"vodio-telegram-bot/internal/taskstore"
)

type recordingNotifier struct {
	event notification.DownloadComplete
	calls int
}

func (n *recordingNotifier) NotifyDownloadComplete(_ context.Context, event notification.DownloadComplete) error {
	n.event = event
	n.calls++
	return nil
}

func TestExtractItemAcceptsTextMessage(t *testing.T) {
	archiver := &Archiver{}
	item, ok := archiver.extractItem(&tg.Message{
		ID:      42,
		PeerID:  &tg.PeerChat{ChatID: 123},
		Message: "pinned message content",
	})

	if !ok {
		t.Fatal("extractItem rejected a text message")
	}
	if item.kind != "text" {
		t.Fatalf("item.kind = %q, want text", item.kind)
	}
	if item.caption != "pinned message content" {
		t.Fatalf("item.caption = %q", item.caption)
	}
	if item.peerID != 123 {
		t.Fatalf("item.peerID = %d, want 123", item.peerID)
	}
}

func TestHandleMessageEnqueuesMatchingTarget(t *testing.T) {
	tasks, err := taskstore.New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("create task store: %v", err)
	}
	archiver := &Archiver{
		cfg:    config.MTProtoConfig{InboxChatID: -5267891219},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		tasks:  tasks,
		jobs:   make(chan archiveJob, 1),
		groups: make(map[int64]*mediaGroup),
	}
	msg := &tg.Message{
		ID:      43,
		PeerID:  &tg.PeerChat{ChatID: 5267891219},
		Message: "queued content",
	}

	if err := archiver.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	select {
	case job := <-archiver.jobs:
		if len(job.items) != 1 || job.items[0].messageID != 43 {
			t.Fatalf("queued items = %#v", job.items)
		}
	default:
		t.Fatal("matching target message was not queued")
	}
}

func TestHandleMessageIgnoresOtherPeer(t *testing.T) {
	tasks, err := taskstore.New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("create task store: %v", err)
	}
	archiver := &Archiver{
		cfg:    config.MTProtoConfig{InboxChatID: -5267891219},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		tasks:  tasks,
		jobs:   make(chan archiveJob, 1),
		groups: make(map[int64]*mediaGroup),
	}
	msg := &tg.Message{
		ID:      44,
		PeerID:  &tg.PeerChat{ChatID: 123},
		Message: "unrelated content",
	}

	if err := archiver.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage returned error: %v", err)
	}
	select {
	case job := <-archiver.jobs:
		t.Fatalf("unrelated message was queued: %#v", job.items)
	default:
	}
}

func TestExtractItemRejectsEmptyMessage(t *testing.T) {
	archiver := &Archiver{}
	if _, ok := archiver.extractItem(&tg.Message{
		ID:     42,
		PeerID: &tg.PeerChat{ChatID: 123},
	}); ok {
		t.Fatal("extractItem accepted an empty message")
	}
}

func TestMatchesInboxChatID(t *testing.T) {
	tests := []struct {
		name         string
		configuredID int64
		peerID       int64
		peerType     string
		want         bool
	}{
		{name: "raw MTProto chat id", configuredID: 5267891219, peerID: 5267891219, peerType: "chat", want: true},
		{name: "Bot API basic group id", configuredID: -5267891219, peerID: 5267891219, peerType: "chat", want: true},
		{name: "Bot API supergroup id", configuredID: -1001234567890, peerID: 1234567890, peerType: "channel", want: true},
		{name: "different group", configuredID: -5267891219, peerID: 42, peerType: "chat", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesInboxChatID(tt.configuredID, tt.peerID, tt.peerType); got != tt.want {
				t.Fatalf("matchesInboxChatID(%d, %d, %q) = %t, want %t", tt.configuredID, tt.peerID, tt.peerType, got, tt.want)
			}
		})
	}
}

func TestArchivePayloadRoundTrip(t *testing.T) {
	items := []archiveItem{
		{
			messageID:      45,
			peerID:         5267891219,
			peerType:       "channel",
			peerAccessHash: 123456,
			kind:           "video",
			fileName:       "example.mp4",
			mimeType:       "video/mp4",
			fileSize:       1024,
			location: &tg.InputDocumentFileLocation{
				ID:            99,
				AccessHash:    100,
				FileReference: []byte{1, 2, 3},
			},
		},
	}

	payload, err := encodeArchivePayload(items)
	if err != nil {
		t.Fatalf("encodeArchivePayload returned error: %v", err)
	}
	got, err := decodeArchivePayload(payload)
	if err != nil {
		t.Fatalf("decodeArchivePayload returned error: %v", err)
	}
	if len(got) != 1 || got[0].fileName != "example.mp4" || got[0].fileSize != 1024 || got[0].peerAccessHash != 123456 {
		t.Fatalf("round-tripped items = %#v", got)
	}
	location, ok := got[0].location.(*tg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("location type = %T", got[0].location)
	}
	if location.ID != 99 || location.AccessHash != 100 || !bytes.Equal(location.FileReference, []byte{1, 2, 3}) {
		t.Fatalf("round-tripped location = %#v", location)
	}
}

func TestDownloadMTProtoFileRefreshesExpiredReferenceAtSameOffset(t *testing.T) {
	const resumeOffset = int64(484442112)
	oldLocation := &tg.InputDocumentFileLocation{
		ID:            99,
		AccessHash:    100,
		FileReference: []byte("expired"),
	}
	newLocation := &tg.InputDocumentFileLocation{
		ID:            99,
		AccessHash:    100,
		FileReference: []byte("fresh"),
	}

	getCalls := 0
	getFile := func(_ context.Context, request *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
		getCalls++
		if request.Offset != resumeOffset {
			t.Fatalf("request offset = %d, want %d", request.Offset, resumeOffset)
		}
		switch getCalls {
		case 1:
			if request.Location != oldLocation {
				t.Fatalf("first request location = %#v", request.Location)
			}
			return nil, tgerr.New(400, tg.ErrFileReferenceExpired)
		case 2:
			if request.Location != newLocation {
				t.Fatalf("second request did not use refreshed location: %#v", request.Location)
			}
			return &tg.UploadFile{Bytes: []byte("remaining bytes")}, nil
		default:
			t.Fatalf("unexpected getFile call %d", getCalls)
			return nil, nil
		}
	}

	refreshCalls := 0
	refresh := func(context.Context) (tg.InputFileLocationClass, error) {
		refreshCalls++
		return newLocation, nil
	}
	var output bytes.Buffer
	if err := downloadMTProtoFileWithGetter(context.Background(), getFile, oldLocation, resumeOffset, &output, refresh); err != nil {
		t.Fatalf("downloadMTProtoFileWithGetter returned error: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if got := output.String(); got != "remaining bytes" {
		t.Fatalf("downloaded bytes = %q", got)
	}
}

func TestDownloadMTProtoFileLimitsConsecutiveReferenceRefreshes(t *testing.T) {
	location := &tg.InputDocumentFileLocation{ID: 99, AccessHash: 100, FileReference: []byte("expired")}
	getCalls := 0
	getFile := func(context.Context, *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
		getCalls++
		return nil, tgerr.New(400, tg.ErrFileReferenceExpired)
	}
	refreshCalls := 0
	refresh := func(context.Context) (tg.InputFileLocationClass, error) {
		refreshCalls++
		return location, nil
	}

	err := downloadMTProtoFileWithGetter(context.Background(), getFile, location, 0, io.Discard, refresh)
	if err == nil || !tgerr.Is(err, tg.ErrFileReferenceExpired) {
		t.Fatalf("error = %v, want FILE_REFERENCE_EXPIRED", err)
	}
	if refreshCalls != maxFileReferenceRefreshAttempts || getCalls != maxFileReferenceRefreshAttempts+1 {
		t.Fatalf("refresh calls = %d, get calls = %d", refreshCalls, getCalls)
	}
}

func TestDownloadTrackerRecordsTransferMetrics(t *testing.T) {
	tasks, err := taskstore.New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("create task store: %v", err)
	}
	if err := tasks.Create(taskstore.Task{ID: "task-progress", Status: taskstore.StatusDownloading}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}
	archiver := &Archiver{
		tasks:  tasks,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tracker := newDownloadTracker(archiver, "task-progress", 1024)
	tracker.startFile(1024, 0)
	var output bytes.Buffer
	if _, err := tracker.wrap(&output).Write(make([]byte, 512)); err != nil {
		t.Fatalf("write progress data: %v", err)
	}
	tracker.endFile()
	tracker.finish()

	task := tasks.List()[0]
	if task.DownloadedBytes != 512 || task.CurrentFileBytes != 512 || task.CurrentFileTotalBytes != 1024 {
		t.Fatalf("transfer byte metrics = %#v", task)
	}
	if task.AverageSpeedBPS <= 0 || task.CurrentSpeedBPS != 0 {
		t.Fatalf("transfer speed metrics = %#v", task)
	}
	if task.DownloadStartedAt == nil || task.DownloadFinishedAt == nil {
		t.Fatalf("transfer timestamps = %#v", task)
	}
	if task.DataCenter == "" || task.ConnectionMode != mtprotoConnectionMode || task.DownloadThreads != 1 || task.MTProtoProxyEnabled {
		t.Fatalf("connection metrics = %#v", task)
	}
}

func TestDownloadTrackerRecordsProxyConnectionMetrics(t *testing.T) {
	tasks, err := taskstore.New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("create task store: %v", err)
	}
	if err := tasks.Create(taskstore.Task{ID: "task-proxy", Status: taskstore.StatusDownloading}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}
	archiver := &Archiver{
		cfg:    config.MTProtoConfig{ProxyURL: "socks5://proxy.internal:7890"},
		tasks:  tasks,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	newDownloadTracker(archiver, "task-proxy", 1024)

	task := tasks.List()[0]
	if !task.MTProtoProxyEnabled || task.ConnectionMode != mtprotoProxyMode {
		t.Fatalf("proxy connection metrics = %#v", task)
	}
}

func TestNotifyDownloadCompleteIncludesRemainingVideoQueue(t *testing.T) {
	tasks, err := taskstore.New(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("create task store: %v", err)
	}
	for _, task := range []taskstore.Task{
		{ID: "completed", Status: taskstore.StatusSucceeded, VideoCount: 1},
		{ID: "downloading", Status: taskstore.StatusDownloading, VideoCount: 2},
		{ID: "queued", Status: taskstore.StatusQueued, VideoCount: 3},
		{ID: "classifying", Status: taskstore.StatusClassifying, VideoCount: 1},
		{ID: "image-only", Status: taskstore.StatusQueued, VideoCount: 0},
	} {
		if err := tasks.Create(task, nil); err != nil {
			t.Fatalf("create task %q: %v", task.ID, err)
		}
	}
	notifier := &recordingNotifier{}
	archiver := &Archiver{
		tasks:    tasks,
		notifier: notifier,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	archiver.notifyDownloadComplete(context.Background(), "completed")

	if notifier.calls != 1 || notifier.event.Task.ID != "completed" {
		t.Fatalf("notification = %#v calls=%d", notifier.event, notifier.calls)
	}
	queue := notifier.event.Queue
	if queue.DownloadingTasks != 1 || queue.DownloadingVideos != 2 || queue.WaitingTasks != 2 || queue.WaitingVideos != 4 {
		t.Fatalf("queue stats = %#v", queue)
	}
}
