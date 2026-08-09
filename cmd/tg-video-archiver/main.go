package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"vodio-telegram-bot/internal/ai"
	"vodio-telegram-bot/internal/config"
	"vodio-telegram-bot/internal/dashboard"
	"vodio-telegram-bot/internal/httpclient"
	"vodio-telegram-bot/internal/mtproto"
	"vodio-telegram-bot/internal/notification"
	"vodio-telegram-bot/internal/storage"
	"vodio-telegram-bot/internal/taskstore"
	"vodio-telegram-bot/internal/telegram"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config failed", "error", err)
		os.Exit(1)
	}

	aiClient, err := httpclient.New(cfg.HTTPProxyURL, 35*time.Second)
	if err != nil {
		logger.Error("initialize AI HTTP client failed", "error", err)
		os.Exit(1)
	}
	downloadClient, err := httpclient.New(cfg.HTTPProxyURL, 0)
	if err != nil {
		logger.Error("initialize download HTTP client failed", "error", err)
		os.Exit(1)
	}

	if cfg.HTTPProxyURL != "" {
		logger.Info("HTTP proxy enabled")
	}

	store, err := storage.NewWithClient(cfg.StorageRoot, downloadClient)
	if err != nil {
		logger.Error("initialize storage failed", "error", err)
		os.Exit(1)
	}
	tasks, err := taskstore.New(cfg.Dashboard.StateFile)
	if err != nil {
		logger.Error("initialize task history failed", "error", err)
		os.Exit(1)
	}

	classifier := ai.NewOpenAIClassifierWithClient(cfg.AI, aiClient)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 3)
	started := 0
	var completionNotifier notification.Sender

	if cfg.Dashboard.Enabled {
		dashboardServer := dashboard.New(cfg.Dashboard.Address, cfg.StorageRoot, tasks, logger)
		started++
		go func() {
			errCh <- dashboardServer.Run(ctx)
		}()
	}

	if cfg.BotEnabled {
		// Long polling waits up to 60 seconds; keep a slightly larger client
		// timeout so notification sends cannot block an archive worker forever.
		telegramClient, err := httpclient.New(cfg.HTTPProxyURL, 75*time.Second)
		if err != nil {
			logger.Error("initialize telegram HTTP client failed", "error", err)
			os.Exit(1)
		}
		botAPI, err := tgbotapi.NewBotAPIWithClient(cfg.TelegramBotToken, cfg.TelegramAPIEndpoint, telegramClient)
		if err != nil {
			logger.Error("initialize telegram bot failed", "error", err)
			os.Exit(1)
		}
		bot := telegram.NewBot(botAPI, cfg.TelegramBotToken, cfg.TelegramFileEndpoint, cfg.AllowedUserIDs, cfg.BotRelayChatID, classifier, store, logger)
		completionNotifier = bot
		started++
		go func() {
			errCh <- bot.Run(ctx)
		}()
	}

	if cfg.MTProto.Enabled {
		mtArchiver := mtproto.New(cfg.MTProto, classifier, store, tasks, completionNotifier, logger)
		started++
		go func() {
			errCh <- mtArchiver.Run(ctx)
		}()
	}

	for i := 0; i < started; i++ {
		if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("ingestion worker stopped with error", "error", err)
			stop()
			os.Exit(1)
		}
	}
	logger.Info("telegram archiver stopped")
}
