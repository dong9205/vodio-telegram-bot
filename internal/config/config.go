package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultAIProvider         = "openai"
	defaultAIBaseURL          = "https://api.openai.com/v1"
	defaultAIModel            = "gpt-4o-mini"
	defaultTelegramAPIBaseURL = "https://api.telegram.org"
)

type Config struct {
	BotEnabled           bool
	TelegramBotToken     string
	TelegramAPIEndpoint  string
	TelegramFileEndpoint string
	AllowedUserIDs       map[int64]struct{}
	BotRelayChatID       int64
	StorageRoot          string
	HTTPProxyURL         string
	AI                   AIConfig
	MTProto              MTProtoConfig
	Dashboard            DashboardConfig
}

type AIConfig struct {
	Provider      string
	OpenAIAPIKey  string
	OpenAIBaseURL string
	Model         string
}

type MTProtoConfig struct {
	Enabled     bool
	AppID       int
	AppHash     string
	Phone       string
	Password    string
	SessionFile string
	InboxChatID int64
}

type DashboardConfig struct {
	Enabled   bool
	Address   string
	StateFile string
}

func Load() (Config, error) {
	var cfg Config

	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}

	var err error
	cfg.MTProto, err = parseMTProtoConfig()
	if err != nil {
		return Config{}, err
	}

	cfg.BotEnabled = boolEnv("BOT_ENABLED", !cfg.MTProto.Enabled || strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")) != "")
	if cfg.BotEnabled {
		cfg.TelegramBotToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
		if cfg.TelegramBotToken == "" {
			return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN is required when BOT_ENABLED=true")
		}
		apiEndpoint, fileEndpoint, err := telegramEndpoints()
		if err != nil {
			return Config{}, err
		}
		cfg.TelegramAPIEndpoint = apiEndpoint
		cfg.TelegramFileEndpoint = fileEndpoint

		allowed, err := parseAllowedUserIDs(os.Getenv("ALLOWED_USER_IDS"))
		if err != nil {
			return Config{}, err
		}
		cfg.AllowedUserIDs = allowed

		relayChatID, err := parseOptionalInt64Env("BOT_RELAY_CHAT_ID")
		if err != nil {
			return Config{}, err
		}
		cfg.BotRelayChatID = relayChatID
	}

	if !cfg.BotEnabled && !cfg.MTProto.Enabled {
		return Config{}, fmt.Errorf("at least one ingestion mode must be enabled: BOT_ENABLED or MT_ENABLED")
	}

	cfg.StorageRoot = strings.TrimSpace(os.Getenv("STORAGE_ROOT"))
	if cfg.StorageRoot == "" {
		return Config{}, fmt.Errorf("STORAGE_ROOT is required")
	}

	cfg.Dashboard.Enabled = boolEnv("DASHBOARD_ENABLED", true)
	cfg.Dashboard.Address = strings.TrimSpace(os.Getenv("DASHBOARD_ADDR"))
	if cfg.Dashboard.Address == "" {
		cfg.Dashboard.Address = "127.0.0.1:9090"
	}
	if _, _, err := net.SplitHostPort(cfg.Dashboard.Address); err != nil {
		return Config{}, fmt.Errorf("invalid DASHBOARD_ADDR: %w", err)
	}
	cfg.Dashboard.StateFile = strings.TrimSpace(os.Getenv("DASHBOARD_STATE_FILE"))
	if cfg.Dashboard.StateFile == "" {
		cfg.Dashboard.StateFile = filepath.Join(cfg.StorageRoot, ".dashboard", "tasks.json")
	}

	proxyURL, err := parseHTTPProxyURL()
	if err != nil {
		return Config{}, err
	}
	cfg.HTTPProxyURL = proxyURL

	provider := strings.ToLower(strings.TrimSpace(os.Getenv("AI_PROVIDER")))
	if provider == "" {
		provider = defaultAIProvider
	}
	if provider != "openai" {
		return Config{}, fmt.Errorf("unsupported AI_PROVIDER %q", provider)
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultAIBaseURL
	}
	model := strings.TrimSpace(os.Getenv("AI_MODEL"))
	if model == "" {
		model = defaultAIModel
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return Config{}, fmt.Errorf("OPENAI_API_KEY is required when AI_PROVIDER=openai")
	}

	cfg.AI = AIConfig{
		Provider:      provider,
		OpenAIAPIKey:  apiKey,
		OpenAIBaseURL: strings.TrimRight(baseURL, "/"),
		Model:         model,
	}

	return cfg, nil
}

func parseOptionalInt64Env(key string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return value, nil
}

func parseAllowedUserIDs(raw string) (map[int64]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("ALLOWED_USER_IDS is required")
	}

	ids := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid ALLOWED_USER_IDS value %q: %w", part, err)
		}
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("ALLOWED_USER_IDS must contain at least one user id")
	}
	return ids, nil
}

func parseMTProtoConfig() (MTProtoConfig, error) {
	enabled := boolEnv("MT_ENABLED", false)
	if !enabled {
		return MTProtoConfig{}, nil
	}

	appIDRaw := strings.TrimSpace(os.Getenv("MT_API_ID"))
	if appIDRaw == "" {
		return MTProtoConfig{}, fmt.Errorf("MT_API_ID is required when MT_ENABLED=true")
	}
	appID, err := strconv.Atoi(appIDRaw)
	if err != nil {
		return MTProtoConfig{}, fmt.Errorf("invalid MT_API_ID: %w", err)
	}

	appHash := strings.TrimSpace(os.Getenv("MT_API_HASH"))
	if appHash == "" {
		return MTProtoConfig{}, fmt.Errorf("MT_API_HASH is required when MT_ENABLED=true")
	}

	phone := strings.TrimSpace(os.Getenv("MT_PHONE"))
	if phone == "" {
		return MTProtoConfig{}, fmt.Errorf("MT_PHONE is required when MT_ENABLED=true")
	}

	sessionFile := strings.TrimSpace(os.Getenv("MT_SESSION_FILE"))
	if sessionFile == "" {
		sessionFile = filepath.Join("data", "telegram-session", "session.json")
	}

	chatIDRaw := strings.TrimSpace(os.Getenv("MT_INBOX_CHAT_ID"))
	var chatID int64
	if chatIDRaw != "" {
		chatID, err = strconv.ParseInt(chatIDRaw, 10, 64)
		if err != nil {
			return MTProtoConfig{}, fmt.Errorf("invalid MT_INBOX_CHAT_ID: %w", err)
		}
	}

	return MTProtoConfig{
		Enabled:     true,
		AppID:       appID,
		AppHash:     appHash,
		Phone:       phone,
		Password:    strings.TrimSpace(os.Getenv("MT_PASSWORD")),
		SessionFile: sessionFile,
		InboxChatID: chatID,
	}, nil
}

func boolEnv(key string, defaultValue bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return defaultValue
	}
	switch raw {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return defaultValue
	}
}

func parseHTTPProxyURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv("HTTP_PROXY_URL"))
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid HTTP proxy URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("HTTP proxy URL must use http or https scheme")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("HTTP proxy URL must include host")
	}
	return raw, nil
}

func telegramEndpoints() (string, string, error) {
	baseURL := strings.TrimSpace(os.Getenv("TELEGRAM_API_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultTelegramAPIBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid TELEGRAM_API_BASE_URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("TELEGRAM_API_BASE_URL must use http or https scheme")
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("TELEGRAM_API_BASE_URL must include host")
	}

	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL + "/bot%s/%s", baseURL + "/file/bot%s/%s", nil
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid %s line %d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("invalid %s line %d: empty key", path, lineNumber)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		value = trimInlineComment(value)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set env %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func trimInlineComment(value string) string {
	inSingleQuote := false
	inDoubleQuote := false
	for i, r := range value {
		switch r {
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '#':
			if !inSingleQuote && !inDoubleQuote && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return value[:i]
			}
		}
	}
	return value
}
