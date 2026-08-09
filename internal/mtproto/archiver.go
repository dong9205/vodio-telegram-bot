package mtproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	gotdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"vodio-telegram-bot/internal/ai"
	"vodio-telegram-bot/internal/config"
	"vodio-telegram-bot/internal/model"
	"vodio-telegram-bot/internal/notification"
	"vodio-telegram-bot/internal/storage"
	"vodio-telegram-bot/internal/taskstore"
)

type Archiver struct {
	cfg        config.MTProtoConfig
	classifier ai.Classifier
	storage    *storage.Manager
	tasks      *taskstore.Store
	notifier   notification.Sender
	logger     *slog.Logger
	jobs       chan archiveJob
	groupsMu   sync.Mutex
	groups     map[int64]*mediaGroup
}

const archiveWorkerCount = 2
const archiveQueueSize = 32
const downloadProgressInterval = time.Second
const mtprotoDownloadPartSize = 512 * 1024

const (
	mtprotoDataCenterLabel = "DC 2（主连接，媒体自动路由）"
	mtprotoConnectionMode  = "MTProto 直连"
	mtprotoDownloadThreads = 1
)

type mediaGroup struct {
	id    int64
	items []archiveItem
	timer *time.Timer
}

type archiveItem struct {
	messageID int
	groupedID int64
	peerID    int64
	peerType  string
	kind      string
	caption   string
	fileName  string
	mimeType  string
	fileSize  int64
	location  tg.InputFileLocationClass
}

type archiveJob struct {
	taskID string
	items  []archiveItem
	mode   taskstore.RetryMode
}

type downloadTracker struct {
	archiver               *Archiver
	taskID                 string
	totalBytes             int64
	downloadedBytes        int64
	sessionBytes           int64
	currentFileBytes       int64
	currentFileTotal       int64
	startedAt              time.Time
	lastReportAt           time.Time
	lastReportSessionBytes int64
}

type progressWriter struct {
	writer  io.Writer
	tracker *downloadTracker
}

func newDownloadTracker(archiver *Archiver, taskID string, totalBytes int64) *downloadTracker {
	now := time.Now()
	tracker := &downloadTracker{
		archiver:     archiver,
		taskID:       taskID,
		totalBytes:   totalBytes,
		startedAt:    now,
		lastReportAt: now,
	}
	zeroBytes := int64(0)
	zeroSpeed := float64(0)
	dataCenter := mtprotoDataCenterLabel
	connectionMode := mtprotoConnectionMode
	proxyEnabled := false
	threads := mtprotoDownloadThreads
	archiver.updateTask(taskID, taskstore.Update{
		TotalBytes:              &totalBytes,
		DownloadedBytes:         &zeroBytes,
		CurrentFileBytes:        &zeroBytes,
		CurrentFileTotalBytes:   &zeroBytes,
		CurrentSpeedBPS:         &zeroSpeed,
		AverageSpeedBPS:         &zeroSpeed,
		ETASeconds:              &zeroBytes,
		DownloadElapsedSeconds:  &zeroBytes,
		DownloadStartedAt:       &now,
		ClearDownloadFinishedAt: true,
		DataCenter:              &dataCenter,
		ConnectionMode:          &connectionMode,
		MTProtoProxyEnabled:     &proxyEnabled,
		DownloadThreads:         &threads,
	})
	return tracker
}

func (t *downloadTracker) startFile(totalBytes, resumeOffset int64) {
	t.downloadedBytes += resumeOffset
	t.currentFileBytes = resumeOffset
	t.currentFileTotal = totalBytes
	t.report(time.Now(), true)
}

func (t *downloadTracker) wrap(writer io.Writer) io.Writer {
	return &progressWriter{writer: writer, tracker: t}
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.tracker.downloadedBytes += int64(n)
		w.tracker.sessionBytes += int64(n)
		w.tracker.currentFileBytes += int64(n)
	}
	w.tracker.report(time.Now(), err != nil)
	return n, err
}

func (t *downloadTracker) endFile() {
	t.report(time.Now(), true)
}

func (t *downloadTracker) finish() {
	now := time.Now()
	t.report(now, true)
	zeroSpeed := float64(0)
	zeroETA := int64(0)
	elapsed := int64(now.Sub(t.startedAt).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	t.archiver.updateTask(t.taskID, taskstore.Update{
		CurrentSpeedBPS:        &zeroSpeed,
		ETASeconds:             &zeroETA,
		DownloadElapsedSeconds: &elapsed,
		DownloadFinishedAt:     &now,
	})
}

func (t *downloadTracker) report(now time.Time, force bool) {
	interval := now.Sub(t.lastReportAt)
	if !force && interval < downloadProgressInterval {
		return
	}
	elapsed := now.Sub(t.startedAt)
	currentSpeed := float64(0)
	if interval > 0 {
		currentSpeed = float64(t.sessionBytes-t.lastReportSessionBytes) / interval.Seconds()
	}
	averageSpeed := float64(0)
	if elapsed > 0 {
		averageSpeed = float64(t.sessionBytes) / elapsed.Seconds()
	}
	etaSeconds := int64(0)
	if t.totalBytes > t.downloadedBytes && currentSpeed > 0 {
		etaSeconds = int64(float64(t.totalBytes-t.downloadedBytes) / currentSpeed)
	}
	elapsedSeconds := int64(elapsed.Seconds())
	downloadedBytes := t.downloadedBytes
	currentFileBytes := t.currentFileBytes
	currentFileTotal := t.currentFileTotal
	t.archiver.updateTask(t.taskID, taskstore.Update{
		DownloadedBytes:        &downloadedBytes,
		CurrentFileBytes:       &currentFileBytes,
		CurrentFileTotalBytes:  &currentFileTotal,
		CurrentSpeedBPS:        &currentSpeed,
		AverageSpeedBPS:        &averageSpeed,
		ETASeconds:             &etaSeconds,
		DownloadElapsedSeconds: &elapsedSeconds,
	})
	t.lastReportAt = now
	t.lastReportSessionBytes = t.sessionBytes
}

type persistedArchiveJob struct {
	Items []persistedArchiveItem `json:"items"`
}

type persistedArchiveItem struct {
	MessageID int                `json:"message_id"`
	GroupedID int64              `json:"grouped_id"`
	PeerID    int64              `json:"peer_id"`
	PeerType  string             `json:"peer_type"`
	Kind      string             `json:"kind"`
	Caption   string             `json:"caption,omitempty"`
	FileName  string             `json:"file_name,omitempty"`
	MIMEType  string             `json:"mime_type,omitempty"`
	FileSize  int64              `json:"file_size"`
	Location  *persistedLocation `json:"location,omitempty"`
}

type persistedLocation struct {
	Type          string `json:"type"`
	ID            int64  `json:"id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference []byte `json:"file_reference"`
	ThumbSize     string `json:"thumb_size,omitempty"`
}

func New(cfg config.MTProtoConfig, classifier ai.Classifier, store *storage.Manager, tasks *taskstore.Store, notifier notification.Sender, logger *slog.Logger) *Archiver {
	return &Archiver{
		cfg:        cfg,
		classifier: classifier,
		storage:    store,
		tasks:      tasks,
		notifier:   notifier,
		logger:     logger,
		jobs:       make(chan archiveJob, archiveQueueSize),
		groups:     make(map[int64]*mediaGroup),
	}
}

func (a *Archiver) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.SessionFile), 0o700); err != nil {
		return fmt.Errorf("create MTProto session directory: %w", err)
	}

	dispatcher := tg.NewUpdateDispatcher()
	opts := gotdtelegram.Options{
		SessionStorage: &session.FileStorage{Path: a.cfg.SessionFile},
		UpdateHandler:  dispatcher,
	}
	client := gotdtelegram.NewClient(a.cfg.AppID, a.cfg.AppHash, opts)
	raw := tg.NewClient(client)

	handle := func(ctx context.Context, msgClass tg.MessageClass) error {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			return nil
		}
		return a.handleMessage(ctx, msg)
	}
	dispatcher.OnNewMessage(func(ctx context.Context, _ tg.Entities, update *tg.UpdateNewMessage) error {
		return handle(ctx, update.Message)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, _ tg.Entities, update *tg.UpdateNewChannelMessage) error {
		return handle(ctx, update.Message)
	})

	return client.Run(ctx, func(ctx context.Context) error {
		if err := a.authenticate(ctx, client); err != nil {
			return err
		}

		workerCtx, cancelWorkers := context.WithCancel(ctx)
		var workers sync.WaitGroup
		for i := 0; i < archiveWorkerCount; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				a.runArchiveWorker(workerCtx, raw)
			}()
		}
		a.tasks.SetRetryHandler(a.retryArchive)
		defer func() {
			a.tasks.SetRetryHandler(nil)
			cancelWorkers()
			workers.Wait()
		}()

		a.logger.Info("MTProto archiver started", "inbox_chat_id", a.cfg.InboxChatID)
		<-ctx.Done()
		return ctx.Err()
	})
}

func (a *Archiver) authenticate(ctx context.Context, client *gotdtelegram.Client) error {
	codePrompt := func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
		fmt.Print("Telegram login code: ")
		code, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(code), nil
	}

	var authenticator auth.UserAuthenticator
	if a.cfg.Password == "" {
		authenticator = auth.CodeOnly(a.cfg.Phone, auth.CodeAuthenticatorFunc(codePrompt))
	} else {
		authenticator = auth.Constant(a.cfg.Phone, a.cfg.Password, auth.CodeAuthenticatorFunc(codePrompt))
	}

	if err := client.Auth().IfNecessary(ctx, auth.NewFlow(authenticator, auth.SendCodeOptions{})); err != nil {
		return fmt.Errorf("MTProto auth failed: %w", err)
	}
	return nil
}

func (a *Archiver) handleMessage(ctx context.Context, msg *tg.Message) error {
	peerID, peerType := peerInfo(msg.PeerID)
	if a.cfg.InboxChatID == 0 {
		a.logger.Info("MTProto discovery message received",
			"message_id", msg.ID,
			"peer_id", peerID,
			"peer_type", peerType,
		)
		return nil
	}
	if !matchesInboxChatID(a.cfg.InboxChatID, peerID, peerType) {
		return nil
	}
	a.logger.Info("MTProto target message received",
		"message_id", msg.ID,
		"peer_id", peerID,
		"peer_type", peerType,
		"media_type", fmt.Sprintf("%T", msg.Media),
	)

	item, ok := a.extractItem(msg)
	if !ok {
		a.logger.Warn("MTProto target message has no supported text or media",
			"message_id", msg.ID,
			"media_type", fmt.Sprintf("%T", msg.Media),
		)
		return nil
	}
	if item.groupedID != 0 {
		a.enqueueGroup(ctx, item)
		return nil
	}
	return a.createAndEnqueueArchive(ctx, []archiveItem{item})
}

func (a *Archiver) createAndEnqueueArchive(ctx context.Context, items []archiveItem) error {
	job, payload, err := newArchiveJob(items)
	if err != nil {
		return err
	}
	meta := metadataFromItems(items)
	if err := a.tasks.Create(taskstore.Task{
		ID:         job.taskID,
		Source:     "mtproto",
		Status:     taskstore.StatusQueued,
		Title:      fallbackArchiveName(meta, items),
		Caption:    meta.Caption,
		MediaCount: len(items),
		VideoCount: countArchiveVideos(items),
		TotalBytes: meta.FileSize,
	}, payload); err != nil {
		return fmt.Errorf("record archive task: %w", err)
	}
	return a.enqueueArchive(ctx, job)
}

func (a *Archiver) enqueueArchive(ctx context.Context, job archiveJob) error {
	select {
	case a.jobs <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Archiver) runArchiveWorker(ctx context.Context, raw *tg.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.jobs:
			a.processArchive(ctx, raw, job)
		}
	}
}

func (a *Archiver) retryArchive(ctx context.Context, taskID string, payload json.RawMessage, mode taskstore.RetryMode) error {
	items, err := decodeArchivePayload(payload)
	if err != nil {
		return fmt.Errorf("decode retry payload: %w", err)
	}
	if mode == taskstore.RetryModeRestart {
		if task, ok := a.tasks.Get(taskID); ok && task.ArchivePath != "" {
			for _, item := range items {
				if item.kind == "text" {
					continue
				}
				if err := a.storage.DiscardPartial(task.ArchivePath, resumeKey(taskID, item)); err != nil {
					return fmt.Errorf("discard partial download: %w", err)
				}
			}
		}
	}
	return a.enqueueArchive(ctx, archiveJob{taskID: taskID, items: items, mode: mode})
}

func matchesInboxChatID(configuredID, peerID int64, peerType string) bool {
	if configuredID == peerID {
		return true
	}
	if configuredID >= 0 {
		return false
	}

	switch peerType {
	case "chat":
		return -configuredID == peerID
	case "channel":
		const botAPIChannelOffset int64 = 1_000_000_000_000
		return -configuredID-botAPIChannelOffset == peerID
	default:
		return false
	}
}

func (a *Archiver) enqueueGroup(ctx context.Context, item archiveItem) {
	a.groupsMu.Lock()
	group := a.groups[item.groupedID]
	if group == nil {
		group = &mediaGroup{id: item.groupedID}
		a.groups[item.groupedID] = group
	}
	group.items = append(group.items, item)
	if group.timer != nil {
		group.timer.Stop()
	}
	group.timer = time.AfterFunc(2*time.Second, func() {
		a.flushGroup(ctx, item.groupedID)
	})
	a.groupsMu.Unlock()
}

func (a *Archiver) flushGroup(ctx context.Context, groupedID int64) {
	a.groupsMu.Lock()
	group := a.groups[groupedID]
	if group != nil {
		delete(a.groups, groupedID)
	}
	a.groupsMu.Unlock()
	if group == nil {
		return
	}

	sort.SliceStable(group.items, func(i, j int) bool {
		return group.items[i].messageID < group.items[j].messageID
	})
	if err := a.createAndEnqueueArchive(ctx, group.items); err != nil && !errors.Is(err, context.Canceled) {
		a.logger.Error("failed to enqueue MTProto media group", "error", err, "grouped_id", groupedID)
	}
}

func (a *Archiver) extractItem(msg *tg.Message) (archiveItem, bool) {
	elem := messages.Elem{Msg: msg}
	file, ok := elem.File()
	if !ok {
		if strings.TrimSpace(msg.Message) == "" {
			return archiveItem{}, false
		}
		peerID, peerType := peerInfo(msg.PeerID)
		groupedID, _ := msg.GetGroupedID()
		return archiveItem{
			messageID: msg.ID,
			groupedID: groupedID,
			peerID:    peerID,
			peerType:  peerType,
			kind:      "text",
			caption:   msg.Message,
			fileName:  "message.txt",
			mimeType:  "text/plain",
		}, true
	}

	kind := mediaKind(file.Name, file.MIMEType)
	if kind == "" {
		return archiveItem{}, false
	}

	peerID, peerType := peerInfo(msg.PeerID)
	groupedID, _ := msg.GetGroupedID()
	return archiveItem{
		messageID: msg.ID,
		groupedID: groupedID,
		peerID:    peerID,
		peerType:  peerType,
		kind:      kind,
		caption:   msg.Message,
		fileName:  file.Name,
		mimeType:  file.MIMEType,
		fileSize:  mediaSize(msg),
		location:  file.Location,
	}, true
}

func newArchiveJob(items []archiveItem) (archiveJob, json.RawMessage, error) {
	if len(items) == 0 {
		return archiveJob{}, nil, errors.New("archive job has no items")
	}
	first := items[0]
	var taskID string
	if first.groupedID != 0 {
		taskID = fmt.Sprintf("mt-%s-%d-group-%d", first.peerType, first.peerID, first.groupedID)
	} else {
		taskID = fmt.Sprintf("mt-%s-%d-message-%d", first.peerType, first.peerID, first.messageID)
	}
	payload, err := encodeArchivePayload(items)
	if err != nil {
		return archiveJob{}, nil, err
	}
	return archiveJob{taskID: taskID, items: items}, payload, nil
}

func encodeArchivePayload(items []archiveItem) (json.RawMessage, error) {
	persisted := persistedArchiveJob{Items: make([]persistedArchiveItem, 0, len(items))}
	for _, item := range items {
		location, err := persistLocation(item.location)
		if err != nil {
			return nil, err
		}
		persisted.Items = append(persisted.Items, persistedArchiveItem{
			MessageID: item.messageID,
			GroupedID: item.groupedID,
			PeerID:    item.peerID,
			PeerType:  item.peerType,
			Kind:      item.kind,
			Caption:   item.caption,
			FileName:  item.fileName,
			MIMEType:  item.mimeType,
			FileSize:  item.fileSize,
			Location:  location,
		})
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, fmt.Errorf("encode archive payload: %w", err)
	}
	return payload, nil
}

func decodeArchivePayload(payload json.RawMessage) ([]archiveItem, error) {
	var persisted persistedArchiveJob
	if err := json.Unmarshal(payload, &persisted); err != nil {
		return nil, err
	}
	if len(persisted.Items) == 0 {
		return nil, errors.New("archive payload has no items")
	}
	items := make([]archiveItem, 0, len(persisted.Items))
	for _, item := range persisted.Items {
		location, err := restoreLocation(item.Location)
		if err != nil {
			return nil, err
		}
		items = append(items, archiveItem{
			messageID: item.MessageID,
			groupedID: item.GroupedID,
			peerID:    item.PeerID,
			peerType:  item.PeerType,
			kind:      item.Kind,
			caption:   item.Caption,
			fileName:  item.FileName,
			mimeType:  item.MIMEType,
			fileSize:  item.FileSize,
			location:  location,
		})
	}
	return items, nil
}

func persistLocation(location tg.InputFileLocationClass) (*persistedLocation, error) {
	switch value := location.(type) {
	case nil:
		return nil, nil
	case *tg.InputDocumentFileLocation:
		return &persistedLocation{
			Type:          "document",
			ID:            value.ID,
			AccessHash:    value.AccessHash,
			FileReference: append([]byte(nil), value.FileReference...),
			ThumbSize:     value.ThumbSize,
		}, nil
	case *tg.InputPhotoFileLocation:
		return &persistedLocation{
			Type:          "photo",
			ID:            value.ID,
			AccessHash:    value.AccessHash,
			FileReference: append([]byte(nil), value.FileReference...),
			ThumbSize:     value.ThumbSize,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported retry file location %T", location)
	}
}

func restoreLocation(location *persistedLocation) (tg.InputFileLocationClass, error) {
	if location == nil {
		return nil, nil
	}
	switch location.Type {
	case "document":
		return &tg.InputDocumentFileLocation{
			ID:            location.ID,
			AccessHash:    location.AccessHash,
			FileReference: append([]byte(nil), location.FileReference...),
			ThumbSize:     location.ThumbSize,
		}, nil
	case "photo":
		return &tg.InputPhotoFileLocation{
			ID:            location.ID,
			AccessHash:    location.AccessHash,
			FileReference: append([]byte(nil), location.FileReference...),
			ThumbSize:     location.ThumbSize,
		}, nil
	default:
		return nil, fmt.Errorf("unknown retry file location type %q", location.Type)
	}
}

func (a *Archiver) processArchive(ctx context.Context, raw *tg.Client, job archiveJob) {
	items := job.items
	empty := ""
	videoCount := countArchiveVideos(items)
	meta := metadataFromItems(items)
	resuming := job.mode == taskstore.RetryModeResume
	var classification model.Classification
	var archiveDir storage.ArchiveDir
	var savedFiles []string
	if resuming {
		task, ok := a.tasks.Get(job.taskID)
		if !ok || task.ArchivePath == "" {
			err := errors.New("找不到原归档目录，请使用重新下载")
			a.failTask(job.taskID, err, items, nil, nil, false)
			return
		}
		info, err := os.Stat(task.ArchivePath)
		if err != nil {
			err = fmt.Errorf("原归档目录不可用，请使用重新下载: %w", err)
			a.failTask(job.taskID, err, items, task.SavedFiles, nil, false)
			return
		}
		if !info.IsDir() {
			err = errors.New("原归档路径不是目录，请使用重新下载")
			a.failTask(job.taskID, err, items, task.SavedFiles, nil, false)
			return
		}
		classification = model.Classification{
			Directory: task.Directory,
			Title:     task.Title,
			Reason:    "断点续传沿用原分类",
		}
		archiveDir = storage.ArchiveDir{Directory: task.Directory, Name: task.ArchiveName, Path: task.ArchivePath}
		savedFiles = append([]string(nil), task.SavedFiles...)
	} else {
		classifying := taskstore.StatusClassifying
		a.updateTask(job.taskID, taskstore.Update{Status: &classifying, VideoCount: &videoCount, CurrentFile: &empty, Error: &empty})
		var err error
		classification, err = a.classifier.Classify(ctx, meta)
		if err != nil {
			a.logger.Warn("AI classification failed, using fallback", "error", err)
			classification = model.DefaultClassification()
		}
		a.updateTask(job.taskID, taskstore.Update{
			Title:     &classification.Title,
			Directory: &classification.Directory,
		})
		archiveDir, err = a.storage.CreateArchiveDir(classification, fallbackArchiveName(meta, items))
		if err != nil {
			a.logger.Error("failed to create archive directory", "error", err)
			a.failTask(job.taskID, err, items, nil, nil, false)
			return
		}
	}

	downloading := taskstore.StatusDownloading
	notResumable := false
	a.updateTask(job.taskID, taskstore.Update{
		Status:      &downloading,
		VideoCount:  &videoCount,
		Directory:   &archiveDir.Directory,
		ArchiveName: &archiveDir.Name,
		ArchivePath: &archiveDir.Path,
		Resumable:   &notResumable,
	})

	downloadCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	tracker := newDownloadTracker(a, job.taskID, meta.FileSize)

	var failedFiles []string
	var failedItems []archiveItem
	var lastErr error
	resumeAvailable := false
	for i, item := range items {
		if item.kind == "text" {
			continue
		}
		desiredName := item.desiredName(i + 1)
		a.updateTask(job.taskID, taskstore.Update{CurrentFile: &desiredName})
		progressStarted := false
		saved, err := a.storage.SaveResumableStreamToDir(
			downloadCtx,
			archiveDir.Path,
			desiredName,
			item.mimeType,
			resumeKey(job.taskID, item),
			resuming,
			item.fileSize,
			mtprotoDownloadPartSize,
			func(ctx context.Context, w io.Writer, offset int64) error {
				tracker.startFile(item.fileSize, offset)
				progressStarted = true
				if item.fileSize > 0 && offset >= item.fileSize {
					return nil
				}
				return downloadMTProtoFile(ctx, raw, item.location, offset, tracker.wrap(w))
			},
		)
		if !progressStarted {
			tracker.startFile(item.fileSize, 0)
		}
		tracker.endFile()
		if err != nil {
			a.logger.Error("failed to save MTProto media", "error", err, "file_name", desiredName)
			failedFiles = append(failedFiles, desiredName)
			failedItems = append(failedItems, item)
			if progressStarted && tracker.currentFileBytes > 0 {
				resumeAvailable = true
			}
			lastErr = err
			continue
		}
		if !contains(savedFiles, saved.FileName) {
			savedFiles = append(savedFiles, saved.FileName)
		}
	}
	tracker.finish()

	if !resuming || !hasDescription(savedFiles) {
		description := buildDescription(meta, classification, items, savedFiles, failedFiles)
		if saved, err := a.storage.SaveTextFile(archiveDir.Path, "description.txt", description); err != nil {
			a.logger.Warn("failed to save description", "error", err, "archive_dir", archiveDir.Path)
			if lastErr == nil {
				lastErr = err
				failedItems = append(failedItems, items...)
			}
		} else if !contains(savedFiles, saved.FileName) {
			savedFiles = append([]string{saved.FileName}, savedFiles...)
		}
	}

	a.logger.Info("MTProto archive saved",
		"path", archiveDir.Path,
		"directory", archiveDir.Directory,
		"archive_name", archiveDir.Name,
		"saved_count", len(savedFiles),
		"failed_count", len(failedFiles),
	)

	if lastErr != nil {
		a.failTask(job.taskID, lastErr, failedItems, savedFiles, failedFiles, resumeAvailable)
		return
	}
	succeeded := taskstore.StatusSucceeded
	retryable := false
	resumable := false
	a.updateTask(job.taskID, taskstore.Update{
		Status:      &succeeded,
		CurrentFile: &empty,
		SavedFiles:  savedFiles,
		FailedFiles: []string{},
		Error:       &empty,
		Retryable:   &retryable,
		Resumable:   &resumable,
		Payload:     json.RawMessage{},
	})
	a.notifyDownloadComplete(ctx, job.taskID)
}

func downloadMTProtoFile(ctx context.Context, raw *tg.Client, location tg.InputFileLocationClass, offset int64, output io.Writer) error {
	for {
		request := &tg.UploadGetFileRequest{Offset: offset, Limit: mtprotoDownloadPartSize, Location: location}
		request.SetPrecise(true)
		result, err := raw.UploadGetFile(ctx, request)
		if err != nil {
			if flood, waitErr := tgerr.FloodWait(ctx, err); waitErr != nil {
				if flood || tgerr.Is(err, tg.ErrTimeout) {
					continue
				}
				return fmt.Errorf("get file chunk at offset %d: %w", offset, waitErr)
			}
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			return fmt.Errorf("unexpected upload.getFile result %T", result)
		}
		if len(file.Bytes) == 0 {
			return nil
		}
		n, err := output.Write(file.Bytes)
		if err != nil {
			return fmt.Errorf("write file chunk at offset %d: %w", offset, err)
		}
		if n != len(file.Bytes) {
			return io.ErrShortWrite
		}
		offset += int64(n)
		if len(file.Bytes) < mtprotoDownloadPartSize {
			return nil
		}
	}
}

func resumeKey(taskID string, item archiveItem) string {
	return fmt.Sprintf("%s-message-%d", taskID, item.messageID)
}

func hasDescription(files []string) bool {
	for _, file := range files {
		if strings.EqualFold(filepath.Base(file), "description.txt") {
			return true
		}
	}
	return false
}

func (a *Archiver) notifyDownloadComplete(ctx context.Context, taskID string) {
	if a.notifier == nil {
		return
	}

	tasks := a.tasks.List()
	var completed taskstore.Task
	var found bool
	queue := notification.QueueStats{}
	for _, task := range tasks {
		if task.ID == taskID {
			completed = task
			found = true
		}
		if task.VideoCount == 0 {
			continue
		}
		switch task.Status {
		case taskstore.StatusDownloading:
			queue.DownloadingTasks++
			queue.DownloadingVideos += task.VideoCount
		case taskstore.StatusQueued, taskstore.StatusClassifying:
			queue.WaitingTasks++
			queue.WaitingVideos += task.VideoCount
		}
	}
	if !found {
		a.logger.Warn("completed task missing before notification", "task_id", taskID)
		return
	}
	if completed.VideoCount == 0 {
		return
	}

	if err := a.notifier.NotifyDownloadComplete(ctx, notification.DownloadComplete{Task: completed, Queue: queue}); err != nil {
		a.logger.Warn("failed to send download completion notification", "error", err, "task_id", taskID)
	}
}

func countArchiveVideos(items []archiveItem) int {
	count := 0
	for _, item := range items {
		if item.kind == "video" {
			count++
		}
	}
	return count
}

func (a *Archiver) failTask(taskID string, taskErr error, retryItems []archiveItem, savedFiles, failedFiles []string, resumable bool) {
	failed := taskstore.StatusFailed
	retryable := len(retryItems) > 0
	errorText := taskErr.Error()
	empty := ""
	update := taskstore.Update{
		Status:      &failed,
		CurrentFile: &empty,
		SavedFiles:  savedFiles,
		FailedFiles: failedFiles,
		Error:       &errorText,
		Retryable:   &retryable,
		Resumable:   &resumable,
	}
	if retryable {
		if payload, err := encodeArchivePayload(retryItems); err == nil {
			update.Payload = payload
		} else {
			retryable = false
			update.Retryable = &retryable
			a.logger.Error("failed to encode retry payload", "error", err, "task_id", taskID)
		}
	}
	a.updateTask(taskID, update)
}

func (a *Archiver) updateTask(taskID string, update taskstore.Update) {
	if err := a.tasks.Update(taskID, update); err != nil {
		a.logger.Warn("failed to update dashboard task", "error", err, "task_id", taskID)
	}
}

func peerInfo(peer tg.PeerClass) (int64, string) {
	switch p := peer.(type) {
	case *tg.PeerChat:
		return p.ChatID, "chat"
	case *tg.PeerChannel:
		return p.ChannelID, "channel"
	case *tg.PeerUser:
		return p.UserID, "user"
	default:
		return 0, "unknown"
	}
}

func mediaKind(name, mimeType string) string {
	mimeType = strings.ToLower(mimeType)
	if strings.HasPrefix(mimeType, "image/") {
		return "image"
	}
	if strings.HasPrefix(mimeType, "video/") {
		return "video"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return "image"
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".flv", ".wmv":
		return "video"
	default:
		return ""
	}
}

func mediaSize(msg *tg.Message) int64 {
	switch media := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		if doc, ok := media.Document.AsNotEmpty(); ok {
			return doc.Size
		}
	}
	return 0
}

func metadataFromItems(items []archiveItem) model.VideoMetadata {
	var captions []string
	var names []string
	var mimeTypes []string
	var totalSize int64
	var sourceType string
	for _, item := range items {
		if item.caption != "" && !contains(captions, item.caption) {
			captions = append(captions, item.caption)
		}
		if item.fileName != "" {
			names = append(names, item.fileName)
		}
		if item.mimeType != "" && !contains(mimeTypes, item.mimeType) {
			mimeTypes = append(mimeTypes, item.mimeType)
		}
		totalSize += item.fileSize
		if sourceType == "" {
			sourceType = item.kind
		} else if sourceType != item.kind {
			sourceType = "media_group"
		}
	}
	if len(items) > 1 {
		sourceType = "media_group"
	}
	return model.VideoMetadata{
		Caption:    strings.Join(captions, "\n\n"),
		FileName:   strings.Join(names, ", "),
		MIMEType:   strings.Join(mimeTypes, ", "),
		FileSize:   totalSize,
		SourceType: sourceType,
		MediaCount: len(items),
	}
}

func fallbackArchiveName(meta model.VideoMetadata, items []archiveItem) string {
	if meta.Caption != "" {
		return strings.Split(meta.Caption, "\n")[0]
	}
	if len(items) > 0 {
		return strings.TrimSuffix(items[0].fileName, filepath.Ext(items[0].fileName))
	}
	return "telegram-archive"
}

func buildDescription(meta model.VideoMetadata, classification model.Classification, items []archiveItem, savedFiles, failedFiles []string) string {
	var b strings.Builder
	b.WriteString("# Telegram Archive\n\n")
	if meta.Caption != "" {
		b.WriteString("## Description\n")
		b.WriteString(meta.Caption)
		b.WriteString("\n\n")
	}
	b.WriteString("## Classification\n")
	b.WriteString("- Directory: " + classification.Directory + "\n")
	b.WriteString("- Title: " + classification.Title + "\n")
	b.WriteString("- Reason: " + classification.Reason + "\n\n")

	b.WriteString("## Media\n")
	for _, item := range items {
		b.WriteString(fmt.Sprintf("- %s: %s, %s, %d bytes\n", item.kind, item.displayName(), item.mimeType, item.fileSize))
	}
	if len(savedFiles) > 0 {
		b.WriteString("\n## Saved Files\n")
		for _, file := range savedFiles {
			b.WriteString("- " + file + "\n")
		}
	}
	if len(failedFiles) > 0 {
		b.WriteString("\n## Failed Files\n")
		for _, file := range failedFiles {
			b.WriteString("- " + file + "\n")
		}
	}
	return b.String()
}

func (item archiveItem) desiredName(index int) string {
	if item.fileName != "" {
		return item.fileName
	}
	return fmt.Sprintf("%s-%02d", item.kind, index)
}

func (item archiveItem) displayName() string {
	if item.fileName != "" {
		return item.fileName
	}
	return item.kind
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
