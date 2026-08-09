package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"vodio-telegram-bot/internal/model"
)

const fallbackDirectory = "Unsorted"

var (
	drivePrefixPattern = regexp.MustCompile(`^[A-Za-z]:`)
	unsafeFileChars    = regexp.MustCompile(`[<>:"/\\|?*]+`)
	whitespacePattern  = regexp.MustCompile(`\s+`)
)

type Manager struct {
	root   string
	client *http.Client
}

type SavedFile struct {
	Directory string
	FileName  string
	Path      string
}

type ArchiveDir struct {
	Directory string
	Name      string
	Path      string
}

func New(root string) (*Manager, error) {
	return NewWithClient(root, &http.Client{
		// The per-download context controls cancellation; avoid a fixed client
		// timeout so slow NAS or large Telegram downloads can still complete.
		Timeout: 0,
	})
}

func NewWithClient(root string, client *http.Client) (*Manager, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &Manager{
		root:   absRoot,
		client: client,
	}, nil
}

func (m *Manager) SaveFromURL(ctx context.Context, downloadURL string, classification model.Classification, meta model.VideoMetadata) (SavedFile, error) {
	directory := CleanDirectory(classification.Directory)
	targetDir := filepath.Join(m.root, filepath.FromSlash(directory))
	if err := ensureWithinRoot(m.root, targetDir); err != nil {
		return SavedFile{}, err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return SavedFile{}, fmt.Errorf("create target directory: %w", err)
	}

	ext := extensionFor(meta.FileName, meta.MIMEType)
	stem := CleanFileStem(classification.Title)
	if stem == "" {
		stem = CleanFileStem(strings.TrimSuffix(meta.FileName, filepath.Ext(meta.FileName)))
	}
	if stem == "" {
		stem = "telegram-video"
	}

	finalPath, finalName, err := m.uniquePath(targetDir, stem, ext)
	if err != nil {
		return SavedFile{}, err
	}
	if err := ensureWithinRoot(m.root, finalPath); err != nil {
		return SavedFile{}, err
	}

	tmpFile, err := os.CreateTemp(targetDir, "."+stem+"-*.tmp")
	if err != nil {
		return SavedFile{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("create download request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("telegram file download returned status %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return SavedFile{}, fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return SavedFile{}, fmt.Errorf("move temp file into place: %w", err)
	}
	cleanup = false

	return SavedFile{
		Directory: directory,
		FileName:  finalName,
		Path:      finalPath,
	}, nil
}

func (m *Manager) CreateArchiveDir(classification model.Classification, fallbackName string) (ArchiveDir, error) {
	directory := CleanDirectory(classification.Directory)
	parentDir := filepath.Join(m.root, filepath.FromSlash(directory))
	if err := ensureWithinRoot(m.root, parentDir); err != nil {
		return ArchiveDir{}, err
	}
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return ArchiveDir{}, fmt.Errorf("create archive parent directory: %w", err)
	}

	stem := CleanFileStem(classification.Title)
	if stem == "" {
		stem = CleanFileStem(fallbackName)
	}
	if stem == "" {
		stem = "telegram-archive"
	}

	dirPath, dirName, err := m.uniqueDirPath(parentDir, stem)
	if err != nil {
		return ArchiveDir{}, err
	}
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return ArchiveDir{}, err
	}
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		return ArchiveDir{}, fmt.Errorf("create archive directory: %w", err)
	}
	return ArchiveDir{
		Directory: directory,
		Name:      dirName,
		Path:      dirPath,
	}, nil
}

func (m *Manager) SaveTextFile(dirPath, fileName, content string) (SavedFile, error) {
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return SavedFile{}, err
	}
	stem := CleanFileStem(strings.TrimSuffix(fileName, filepath.Ext(fileName)))
	if stem == "" {
		stem = "description"
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		ext = ".txt"
	}
	finalPath, finalName, err := m.uniquePath(dirPath, stem, ext)
	if err != nil {
		return SavedFile{}, err
	}
	if err := os.WriteFile(finalPath, []byte(content), 0o644); err != nil {
		return SavedFile{}, fmt.Errorf("write text file: %w", err)
	}
	return SavedFile{
		FileName: finalName,
		Path:     finalPath,
	}, nil
}

func (m *Manager) SaveURLToDir(ctx context.Context, dirPath, downloadURL, desiredName, mimeType string) (SavedFile, error) {
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return SavedFile{}, err
	}
	stem := CleanFileStem(strings.TrimSuffix(desiredName, filepath.Ext(desiredName)))
	if stem == "" {
		stem = "telegram-media"
	}
	ext := extensionFor(desiredName, mimeType)
	finalPath, finalName, err := m.uniquePath(dirPath, stem, ext)
	if err != nil {
		return SavedFile{}, err
	}
	if err := ensureWithinRoot(m.root, finalPath); err != nil {
		return SavedFile{}, err
	}

	tmpFile, err := os.CreateTemp(dirPath, "."+stem+"-*.tmp")
	if err != nil {
		return SavedFile{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("create download request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("telegram file download returned status %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return SavedFile{}, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return SavedFile{}, fmt.Errorf("move temp file into place: %w", err)
	}
	cleanup = false

	return SavedFile{
		FileName: finalName,
		Path:     finalPath,
	}, nil
}

func (m *Manager) SaveStreamToDir(ctx context.Context, dirPath, desiredName, mimeType string, write func(context.Context, io.Writer) error) (SavedFile, error) {
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return SavedFile{}, err
	}
	stem := CleanFileStem(strings.TrimSuffix(desiredName, filepath.Ext(desiredName)))
	if stem == "" {
		stem = "telegram-media"
	}
	ext := extensionFor(desiredName, mimeType)
	finalPath, finalName, err := m.uniquePath(dirPath, stem, ext)
	if err != nil {
		return SavedFile{}, err
	}
	if err := ensureWithinRoot(m.root, finalPath); err != nil {
		return SavedFile{}, err
	}

	tmpFile, err := os.CreateTemp(dirPath, "."+stem+"-*.tmp")
	if err != nil {
		return SavedFile{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := write(ctx, tmpFile); err != nil {
		_ = tmpFile.Close()
		return SavedFile{}, fmt.Errorf("write stream: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return SavedFile{}, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return SavedFile{}, fmt.Errorf("move temp file into place: %w", err)
	}
	cleanup = false

	return SavedFile{
		FileName: finalName,
		Path:     finalPath,
	}, nil
}

// SaveResumableStreamToDir keeps a deterministic .part file when writing fails.
// A later call with resume=true truncates it to an aligned offset and appends
// the remaining bytes before atomically moving it to the final file name.
func (m *Manager) SaveResumableStreamToDir(
	ctx context.Context,
	dirPath, desiredName, mimeType, resumeKey string,
	resume bool,
	expectedSize, alignment int64,
	write func(context.Context, io.Writer, int64) error,
) (SavedFile, error) {
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return SavedFile{}, err
	}
	stem := CleanFileStem(strings.TrimSuffix(desiredName, filepath.Ext(desiredName)))
	if stem == "" {
		stem = "telegram-media"
	}
	ext := extensionFor(desiredName, mimeType)
	finalPath, finalName, err := m.uniquePath(dirPath, stem, ext)
	if err != nil {
		return SavedFile{}, err
	}
	if err := ensureWithinRoot(m.root, finalPath); err != nil {
		return SavedFile{}, err
	}
	partPath, err := m.partialPath(dirPath, resumeKey)
	if err != nil {
		return SavedFile{}, err
	}

	partFile, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return SavedFile{}, fmt.Errorf("open partial file: %w", err)
	}
	closeWithError := func(prefix string, cause error) (SavedFile, error) {
		_ = partFile.Sync()
		_ = partFile.Close()
		return SavedFile{}, fmt.Errorf("%s: %w", prefix, cause)
	}

	var offset int64
	if resume {
		info, err := partFile.Stat()
		if err != nil {
			return closeWithError("inspect partial file", err)
		}
		offset = info.Size()
		if expectedSize > 0 && offset > expectedSize {
			offset = 0
		}
		if expectedSize <= 0 || offset != expectedSize {
			if alignment <= 0 {
				alignment = 1
			}
			offset -= offset % alignment
		}
	} else {
		offset = 0
	}
	if err := partFile.Truncate(offset); err != nil {
		return closeWithError("truncate partial file", err)
	}
	if _, err := partFile.Seek(offset, io.SeekStart); err != nil {
		return closeWithError("seek partial file", err)
	}

	if err := write(ctx, partFile, offset); err != nil {
		return closeWithError("write resumable stream", err)
	}
	if err := partFile.Sync(); err != nil {
		return closeWithError("sync partial file", err)
	}
	if err := partFile.Close(); err != nil {
		return SavedFile{}, fmt.Errorf("close partial file: %w", err)
	}
	if err := os.Rename(partPath, finalPath); err != nil {
		return SavedFile{}, fmt.Errorf("move partial file into place: %w", err)
	}
	return SavedFile{FileName: finalName, Path: finalPath}, nil
}

func (m *Manager) DiscardPartial(dirPath, resumeKey string) error {
	partPath, err := m.partialPath(dirPath, resumeKey)
	if err != nil {
		return err
	}
	if err := os.Remove(partPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove partial file: %w", err)
	}
	return nil
}

func (m *Manager) partialPath(dirPath, resumeKey string) (string, error) {
	if err := ensureWithinRoot(m.root, dirPath); err != nil {
		return "", err
	}
	key := CleanFileStem(resumeKey)
	if key == "" {
		return "", errors.New("resume key is required")
	}
	path := filepath.Join(dirPath, "."+key+".part")
	if err := ensureWithinRoot(m.root, path); err != nil {
		return "", err
	}
	return path, nil
}

func CleanDirectory(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) || filepath.IsAbs(raw) || drivePrefixPattern.MatchString(raw) || hasControl(raw) {
		return fallbackDirectory
	}

	raw = strings.ReplaceAll(raw, `\`, "/")
	cleaned := filepath.ToSlash(filepath.Clean(raw))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.Contains(cleaned, "/../") {
		return fallbackDirectory
	}

	parts := strings.Split(strings.Trim(cleaned, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 {
		return fallbackDirectory
	}

	safeParts := make([]string, 0, len(parts))
	for _, part := range parts {
		part = cleanPathPart(part)
		if part == "" || part == "." || part == ".." {
			return fallbackDirectory
		}
		safeParts = append(safeParts, part)
	}
	return strings.Join(safeParts, "/")
}

func CleanFileStem(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || hasControl(raw) {
		return ""
	}
	raw = unsafeFileChars.ReplaceAllString(raw, " ")
	raw = strings.Trim(raw, " .\t\r\n")
	raw = whitespacePattern.ReplaceAllString(raw, " ")
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "." || raw == ".." {
		return ""
	}
	return trimRunes(raw, 120)
}

func cleanPathPart(raw string) string {
	raw = unsafeFileChars.ReplaceAllString(raw, " ")
	raw = strings.Trim(raw, " .\t\r\n")
	raw = whitespacePattern.ReplaceAllString(raw, " ")
	raw = trimRunes(raw, 80)
	return raw
}

func extensionFor(fileName, mimeType string) string {
	if ext := strings.ToLower(filepath.Ext(fileName)); ext != "" && len(ext) <= 10 {
		return ext
	}
	if mimeType != "" {
		if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
			return strings.ToLower(exts[0])
		}
		switch strings.ToLower(mimeType) {
		case "video/mp4":
			return ".mp4"
		case "video/quicktime":
			return ".mov"
		case "video/x-matroska":
			return ".mkv"
		case "video/webm":
			return ".webm"
		}
	}
	return ".mp4"
}

func (m *Manager) uniquePath(dir, stem, ext string) (string, string, error) {
	candidates := []string{
		stem + ext,
		fmt.Sprintf("%s-%s%s", stem, time.Now().Format("20060102-150405"), ext),
	}
	for i := 1; i <= 999; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%03d%s", stem, i, ext))
	}

	for _, name := range candidates {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if os.IsNotExist(err) {
			return path, name, nil
		} else {
			return "", "", fmt.Errorf("check target path: %w", err)
		}
	}
	return "", "", fmt.Errorf("could not find available file name")
}

func (m *Manager) uniqueDirPath(parentDir, stem string) (string, string, error) {
	candidates := []string{
		stem,
		fmt.Sprintf("%s-%s", stem, time.Now().Format("20060102-150405")),
	}
	for i := 1; i <= 999; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%03d", stem, i))
	}

	for _, name := range candidates {
		path := filepath.Join(parentDir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if os.IsNotExist(err) {
			return path, name, nil
		} else {
			return "", "", fmt.Errorf("check target directory: %w", err)
		}
	}
	return "", "", fmt.Errorf("could not find available directory name")
}

func ensureWithinRoot(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve relative path: %w", err)
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("target path escapes storage root")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func trimRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max]))
}
