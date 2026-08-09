package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"vodio-telegram-bot/internal/ai"
	"vodio-telegram-bot/internal/model"
	"vodio-telegram-bot/internal/notification"
	"vodio-telegram-bot/internal/storage"
)

type Bot struct {
	api            *tgbotapi.BotAPI
	token          string
	fileEndpoint   string
	allowedUserIDs map[int64]struct{}
	relayChatID    int64
	classifier     ai.Classifier
	storage        *storage.Manager
	logger         *slog.Logger
	groupsMu       sync.Mutex
	groups         map[string]*mediaGroup
}

func NewBot(api *tgbotapi.BotAPI, token, fileEndpoint string, allowedUserIDs map[int64]struct{}, relayChatID int64, classifier ai.Classifier, store *storage.Manager, logger *slog.Logger) *Bot {
	return &Bot{
		api:            api,
		token:          token,
		fileEndpoint:   fileEndpoint,
		allowedUserIDs: allowedUserIDs,
		relayChatID:    relayChatID,
		classifier:     classifier,
		storage:        store,
		logger:         logger,
		groups:         make(map[string]*mediaGroup),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60

	updates := b.api.GetUpdatesChan(updateConfig)
	defer b.api.StopReceivingUpdates()

	b.logger.Info("telegram bot started", "username", b.api.Self.UserName)
	if b.relayChatID != 0 {
		b.logger.Info("telegram bot relay enabled", "relay_chat_id", b.relayChatID)
	}

	sem := make(chan struct{}, 2)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			go func(msg *tgbotapi.Message) {
				defer func() { <-sem }()
				b.handleMessage(ctx, msg)
			}(update.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	fromID := int64(0)
	if msg.From != nil {
		fromID = msg.From.ID
	}

	b.logger.Info("received message",
		"chat_id", msg.Chat.ID,
		"message_id", msg.MessageID,
		"from_user_id", fromID,
	)
	if b.relayChatID != 0 && msg.Chat.ID == b.relayChatID {
		// The MTProto worker consumes this chat. Never relay messages that are
		// already in the inbox back into the same chat.
		return
	}

	if !b.isAllowed(fromID) {
		b.logger.Warn("rejected message from non-whitelisted user", "from_user_id", fromID)
		b.reply(msg, "无权限使用此 Bot。")
		return
	}

	if b.relayChatID != 0 {
		if msg.MediaGroupID != "" {
			b.enqueueRelayMediaGroup(ctx, msg)
			return
		}
		b.relayMessages([]*tgbotapi.Message{msg}, msg)
		return
	}

	if msg.MediaGroupID != "" {
		if item, ok := extractArchiveItem(msg); ok {
			b.enqueueMediaGroup(ctx, msg, item)
			return
		}
	}

	item, ok := extractArchiveItem(msg)
	if !ok {
		b.reply(msg, "请发送或转发 video、图片，或 MIME type 为 video/* 的 document。")
		return
	}
	b.processArchive(ctx, []*tgbotapi.Message{msg}, []archiveItem{item}, msg)
}

func (b *Bot) enqueueRelayMediaGroup(ctx context.Context, msg *tgbotapi.Message) {
	key := fmt.Sprintf("%d:%s", msg.Chat.ID, msg.MediaGroupID)

	b.groupsMu.Lock()
	group := b.groups[key]
	if group == nil {
		group = &mediaGroup{key: key, replyTo: msg}
		b.groups[key] = group
	}
	group.messages = append(group.messages, msg)
	if group.timer != nil {
		group.timer.Stop()
	}
	group.timer = time.AfterFunc(2*time.Second, func() {
		b.flushRelayMediaGroup(ctx, key)
	})
	b.groupsMu.Unlock()
}

func (b *Bot) flushRelayMediaGroup(ctx context.Context, key string) {
	b.groupsMu.Lock()
	group := b.groups[key]
	if group != nil {
		delete(b.groups, key)
	}
	b.groupsMu.Unlock()
	if group == nil {
		return
	}

	sort.SliceStable(group.messages, func(i, j int) bool {
		return group.messages[i].MessageID < group.messages[j].MessageID
	})
	b.relayMessages(group.messages, group.replyTo)
}

func (b *Bot) relayMessages(messages []*tgbotapi.Message, replyTo *tgbotapi.Message) {
	if len(messages) == 0 {
		return
	}

	forwarded, err := b.forwardMessages(messages)
	if err != nil {
		b.logger.Error("failed to relay messages to MTProto inbox",
			"error", b.safeError(err),
			"source_chat_id", messages[0].Chat.ID,
			"message_count", len(messages),
			"relay_chat_id", b.relayChatID,
		)
	}

	if forwarded == 0 {
		b.reply(replyTo, "转发到归档群失败，请确认 Bot 已加入归档群并拥有发消息权限。")
		return
	}
	if forwarded < len(messages) {
		b.reply(replyTo, fmt.Sprintf("已转发 %d/%d 条消息到归档队列，部分消息失败。", forwarded, len(messages)))
		return
	}
	b.reply(replyTo, fmt.Sprintf("已加入归档队列，共 %d 条消息。", forwarded))
}

func (b *Bot) forwardMessages(messages []*tgbotapi.Message) (int, error) {
	if len(messages) == 1 {
		forward := tgbotapi.NewForward(b.relayChatID, messages[0].Chat.ID, messages[0].MessageID)
		if _, err := b.api.Send(forward); err != nil {
			return 0, err
		}
		return 1, nil
	}

	messageIDs := make([]int, 0, len(messages))
	for _, msg := range messages {
		messageIDs = append(messageIDs, msg.MessageID)
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", b.relayChatID)
	params.AddNonZero64("from_chat_id", messages[0].Chat.ID)
	if err := params.AddInterface("message_ids", messageIDs); err != nil {
		return 0, fmt.Errorf("encode relay message ids: %w", err)
	}
	response, err := b.api.MakeRequest("forwardMessages", params)
	if err != nil {
		return 0, err
	}
	var forwarded []struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(response.Result, &forwarded); err != nil {
		return 0, fmt.Errorf("decode forwardMessages response: %w", err)
	}
	return len(forwarded), nil
}

func (b *Bot) safeError(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), b.token, "<redacted>")
}

func (b *Bot) processArchive(ctx context.Context, messages []*tgbotapi.Message, items []archiveItem, replyTo *tgbotapi.Message) {
	meta := metadataFromItems(messages, items)
	classification, err := b.classifier.Classify(ctx, meta)
	if err != nil {
		b.logger.Warn("AI classification failed, using fallback",
			"error", b.safeError(err),
			"file_name", meta.FileName,
		)
		classification = model.DefaultClassification()
	}
	b.logger.Info("AI classification result",
		"directory", classification.Directory,
		"title", classification.Title,
		"reason", classification.Reason,
	)

	archiveDir, err := b.storage.CreateArchiveDir(classification, fallbackArchiveName(meta, items))
	if err != nil {
		b.logger.Error("failed to create archive directory", "error", b.safeError(err))
		b.reply(replyTo, "创建归档目录失败。")
		return
	}

	downloadCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()

	var savedFiles []string
	var failedFiles []string
	for i, item := range items {
		fileURL, err := b.fileURL(item.fileID)
		if err != nil {
			b.logger.Error("failed to get telegram file url", "error", b.safeError(err), "file_id", item.fileID, "kind", item.kind)
			failedFiles = append(failedFiles, item.displayName())
			continue
		}

		desiredName := item.desiredName(i + 1)
		saved, err := b.storage.SaveURLToDir(downloadCtx, archiveDir.Path, fileURL, desiredName, item.mimeType)
		if err != nil {
			b.logger.Error("failed to save media", "error", b.safeError(err), "file_name", desiredName)
			failedFiles = append(failedFiles, desiredName)
			continue
		}
		savedFiles = append(savedFiles, saved.FileName)
	}

	description := buildDescription(meta, classification, items, savedFiles, failedFiles)
	if saved, err := b.storage.SaveTextFile(archiveDir.Path, "description.txt", description); err != nil {
		b.logger.Warn("failed to save description", "error", b.safeError(err), "archive_dir", archiveDir.Path)
	} else {
		savedFiles = append([]string{saved.FileName}, savedFiles...)
	}

	b.logger.Info("archive saved",
		"path", archiveDir.Path,
		"directory", archiveDir.Directory,
		"archive_name", archiveDir.Name,
		"saved_count", len(savedFiles),
		"failed_count", len(failedFiles),
	)

	if len(savedFiles) == 0 {
		b.reply(replyTo, "创建了归档目录，但媒体文件下载失败。若日志显示 file is too big，这是 Telegram Bot API 的下载限制。")
		return
	}

	reply := fmt.Sprintf("保存成功\n分类目录：%s\n归档目录：%s\n文件数：%d", archiveDir.Directory, archiveDir.Name, len(savedFiles))
	if len(failedFiles) > 0 {
		reply += fmt.Sprintf("\n部分文件下载失败：%d 个。若原因是 file is too big，需要本地 Telegram Bot API Server 才能下载超大文件。", len(failedFiles))
	}
	b.reply(replyTo, reply)
}

func (b *Bot) fileURL(fileID string) (string, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(b.fileEndpoint, b.token, file.FilePath), nil
}

func (b *Bot) isAllowed(userID int64) bool {
	if userID == 0 {
		return false
	}
	_, ok := b.allowedUserIDs[userID]
	return ok
}

func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ReplyToMessageID = msg.MessageID
	if _, err := b.api.Send(reply); err != nil {
		b.logger.Warn("failed to send telegram reply", "error", b.safeError(err), "chat_id", msg.Chat.ID)
	}
}

// NotifyDownloadComplete sends MTProto completion details to every whitelisted user.
// This also covers files posted directly in the monitored group, where there is no
// originating Bot API chat to reply to.
func (b *Bot) NotifyDownloadComplete(ctx context.Context, event notification.DownloadComplete) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	messageText := formatDownloadComplete(event)
	userIDs := make([]int64, 0, len(b.allowedUserIDs))
	for userID := range b.allowedUserIDs {
		userIDs = append(userIDs, userID)
	}
	sort.Slice(userIDs, func(i, j int) bool { return userIDs[i] < userIDs[j] })

	var failures []string
	for _, userID := range userIDs {
		message := tgbotapi.NewMessage(userID, messageText)
		message.DisableWebPagePreview = true
		if _, err := b.api.Send(message); err != nil {
			failures = append(failures, fmt.Sprintf("user %d: %s", userID, b.safeError(err)))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("send completion notification: %s", strings.Join(failures, "; "))
	}
	return nil
}

func formatDownloadComplete(event notification.DownloadComplete) string {
	task := event.Task
	lines := []string{
		"✅ 视频下载完成",
		"",
		"标题：" + fallbackNotificationValue(task.Title, "未命名归档"),
		"视频数量：" + fmt.Sprintf("%d", task.VideoCount),
		"传输大小：" + formatNotificationBytes(task.DownloadedBytes),
		"平均速度：" + formatNotificationBytes(int64(task.AverageSpeedBPS)) + "/s",
		"开始时间：" + formatNotificationTime(task.DownloadStartedAt),
		"结束时间：" + formatNotificationTime(task.DownloadFinishedAt),
		"下载耗时：" + formatNotificationDuration(task.DownloadElapsedSeconds),
		"归档位置：" + fallbackNotificationValue(task.ArchivePath, task.ArchiveName),
	}
	if len(task.SavedFiles) > 0 {
		const maxFiles = 6
		shown := task.SavedFiles
		if len(shown) > maxFiles {
			shown = shown[:maxFiles]
		}
		filesLine := "已保存文件：" + strings.Join(shown, "、")
		if remaining := len(task.SavedFiles) - len(shown); remaining > 0 {
			filesLine += fmt.Sprintf("，另有 %d 个", remaining)
		}
		lines = append(lines, filesLine)
	}
	lines = append(lines,
		"",
		"队列状态：",
		fmt.Sprintf("正在下载：%d 个视频（%d 个任务）", event.Queue.DownloadingVideos, event.Queue.DownloadingTasks),
		fmt.Sprintf("等待处理：%d 个视频（%d 个任务）", event.Queue.WaitingVideos, event.Queue.WaitingTasks),
	)
	return strings.Join(lines, "\n")
}

func fallbackNotificationValue(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return fallback
	}
	return "未知"
}

func formatNotificationTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "未知"
	}
	return value.In(time.Local).Format("2006-01-02 15:04:05")
}

func formatNotificationDuration(seconds int64) string {
	if seconds <= 0 {
		return "少于 1 秒"
	}
	duration := time.Duration(seconds) * time.Second
	hours := int64(duration / time.Hour)
	minutes := int64(duration%time.Hour) / int64(time.Minute)
	secs := int64(duration%time.Minute) / int64(time.Second)
	if hours > 0 {
		return fmt.Sprintf("%d 小时 %d 分 %d 秒", hours, minutes, secs)
	}
	if minutes > 0 {
		return fmt.Sprintf("%d 分 %d 秒", minutes, secs)
	}
	return fmt.Sprintf("%d 秒", secs)
}

func formatNotificationBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 || value >= 10 {
		return fmt.Sprintf("%.0f %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func (b *Bot) enqueueMediaGroup(ctx context.Context, msg *tgbotapi.Message, item archiveItem) {
	key := fmt.Sprintf("%d:%s", msg.Chat.ID, msg.MediaGroupID)

	b.groupsMu.Lock()
	group := b.groups[key]
	if group == nil {
		group = &mediaGroup{
			key:     key,
			replyTo: msg,
		}
		b.groups[key] = group
	}
	group.messages = append(group.messages, msg)
	group.items = append(group.items, item)
	if group.timer != nil {
		group.timer.Stop()
	}
	group.timer = time.AfterFunc(2*time.Second, func() {
		b.flushMediaGroup(ctx, key)
	})
	b.groupsMu.Unlock()
}

func (b *Bot) flushMediaGroup(ctx context.Context, key string) {
	b.groupsMu.Lock()
	group := b.groups[key]
	if group != nil {
		delete(b.groups, key)
	}
	b.groupsMu.Unlock()
	if group == nil {
		return
	}

	sort.SliceStable(group.messages, func(i, j int) bool {
		return group.messages[i].MessageID < group.messages[j].MessageID
	})
	sort.SliceStable(group.items, func(i, j int) bool {
		return group.items[i].messageID < group.items[j].messageID
	})

	b.processArchive(ctx, group.messages, group.items, group.replyTo)
}

type mediaGroup struct {
	key      string
	replyTo  *tgbotapi.Message
	messages []*tgbotapi.Message
	items    []archiveItem
	timer    *time.Timer
}

type archiveItem struct {
	messageID int
	kind      string
	fileID    string
	fileName  string
	mimeType  string
	fileSize  int64
	caption   string
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

func extractArchiveItem(msg *tgbotapi.Message) (archiveItem, bool) {
	item := archiveItem{
		messageID: msg.MessageID,
		caption:   msg.Caption,
	}

	if msg.Video != nil {
		item.kind = "video"
		item.fileID = msg.Video.FileID
		item.fileName = msg.Video.FileName
		item.mimeType = msg.Video.MimeType
		item.fileSize = int64(msg.Video.FileSize)
		return item, true
	}
	if msg.Document != nil && isVideoDocument(msg.Document) {
		item.kind = "video"
		item.fileID = msg.Document.FileID
		item.fileName = msg.Document.FileName
		item.mimeType = msg.Document.MimeType
		item.fileSize = int64(msg.Document.FileSize)
		return item, true
	}
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		item.kind = "image"
		item.fileID = photo.FileID
		item.fileName = fmt.Sprintf("image-%d.jpg", msg.MessageID)
		item.mimeType = "image/jpeg"
		item.fileSize = int64(photo.FileSize)
		return item, true
	}
	return archiveItem{}, false
}

func extractVideoMetadata(msg *tgbotapi.Message) (model.VideoMetadata, bool) {
	meta := model.VideoMetadata{
		Caption: msg.Caption,
	}

	if msg.Video != nil {
		meta.FileID = msg.Video.FileID
		meta.FileName = msg.Video.FileName
		meta.MIMEType = msg.Video.MimeType
		meta.FileSize = int64(msg.Video.FileSize)
		meta.SourceType = "video"
	} else if msg.Document != nil && isVideoDocument(msg.Document) {
		meta.FileID = msg.Document.FileID
		meta.FileName = msg.Document.FileName
		meta.MIMEType = msg.Document.MimeType
		meta.FileSize = int64(msg.Document.FileSize)
		meta.SourceType = "document"
	} else {
		return model.VideoMetadata{}, false
	}

	meta.ForwardFromName, meta.ForwardFromUsername = forwardSource(msg)
	return meta, true
}

func metadataFromItems(messages []*tgbotapi.Message, items []archiveItem) model.VideoMetadata {
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

	meta := model.VideoMetadata{
		Caption:    strings.Join(captions, "\n\n"),
		FileName:   strings.Join(names, ", "),
		MIMEType:   strings.Join(mimeTypes, ", "),
		FileSize:   totalSize,
		SourceType: sourceType,
		MediaCount: len(items),
	}
	if len(items) == 1 {
		meta.FileID = items[0].fileID
	}
	if len(messages) > 0 {
		meta.ForwardFromName, meta.ForwardFromUsername = forwardSource(messages[0])
	}
	return meta
}

func fallbackArchiveName(meta model.VideoMetadata, items []archiveItem) string {
	if meta.Caption != "" {
		lines := strings.Split(meta.Caption, "\n")
		return lines[0]
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
	if meta.ForwardFromName != "" || meta.ForwardFromUsername != "" {
		b.WriteString("## Forward Source\n")
		if meta.ForwardFromName != "" {
			b.WriteString("- Name: " + meta.ForwardFromName + "\n")
		}
		if meta.ForwardFromUsername != "" {
			b.WriteString("- Username: @" + meta.ForwardFromUsername + "\n")
		}
		b.WriteString("\n")
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

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func isVideoDocument(doc *tgbotapi.Document) bool {
	if strings.HasPrefix(strings.ToLower(doc.MimeType), "video/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(doc.FileName)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".flv", ".wmv":
		return true
	default:
		return false
	}
}

func forwardSource(msg *tgbotapi.Message) (string, string) {
	if msg.ForwardFrom != nil {
		return strings.TrimSpace(msg.ForwardFrom.FirstName + " " + msg.ForwardFrom.LastName), msg.ForwardFrom.UserName
	}
	if msg.ForwardFromChat != nil {
		name := msg.ForwardFromChat.Title
		if name == "" {
			name = strings.TrimSpace(msg.ForwardFromChat.FirstName + " " + msg.ForwardFromChat.LastName)
		}
		return name, msg.ForwardFromChat.UserName
	}
	if msg.ForwardSenderName != "" {
		return msg.ForwardSenderName, ""
	}
	return "", ""
}
