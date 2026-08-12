// Command bot runs dr1wbot: a Telegram guest-mode bot that answers when
// mentioned, using an OpenAI-compatible LLM backend.
//
// Copyright (C) 2026 faustyu1.
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU Affero General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option) any
// later version. It is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; see the GNU Affero General Public License for details.
// You should have received a copy of the license along with this program; if
// not, see <https://www.gnu.org/licenses/>.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"dr1wbot/internal/access"
	"dr1wbot/internal/admin"
	"dr1wbot/internal/alert"
	"dr1wbot/internal/answers"
	"dr1wbot/internal/config"
	"dr1wbot/internal/imagegen"
	"dr1wbot/internal/llm"
	"dr1wbot/internal/memory"
	"dr1wbot/internal/menu"
	"dr1wbot/internal/quota"
	"dr1wbot/internal/reply"
	"dr1wbot/internal/search"
	"dr1wbot/internal/session"
	"dr1wbot/internal/settings"
	"dr1wbot/internal/tgchannel"
	"dr1wbot/internal/tgfile"
	"dr1wbot/internal/tools"
	"dr1wbot/internal/transcribe"
)

// shutdownGrace is how long in-flight answers get to finish after a signal.
const shutdownGrace = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when config fails, so use stderr directly.
		fmt.Fprintln(os.Stderr, "fatal: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	botOpts := []telego.BotOption{telego.WithDiscardLogger()}
	if cfg.Debug {
		botOpts = []telego.BotOption{telego.WithDefaultDebugLogger()}
	}

	bot, err := telego.NewBot(cfg.BotToken, botOpts...)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	me, err := bot.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}

	allow, err := access.New(access.Options{
		Admins:    cfg.AdminUsers,
		EnvUsers:  cfg.AllowedUsers,
		EnvChats:  cfg.AllowedChats,
		StateFile: cfg.StateFile,
	})
	if err != nil {
		return fmt.Errorf("load access list: %w", err)
	}
	if allow.Open() {
		log.Warn("no whitelist configured: anyone on Telegram can summon this bot and spend your LLM credits; " +
			"set ADMIN_USER_IDS and/or ALLOWED_USER_IDS")
	}
	if !allow.HasAdmins() {
		log.Warn("no ADMIN_USER_IDS configured: /add, /del and /list will refuse everyone")
	}
	if !me.SupportsGuestQueries {
		log.Warn("guest mode is off for this bot: it will never receive guest_message updates; " +
			"enable it in @BotFather -> your bot -> settings -> Guest Mode")
	}

	// The environment seeds the panel's settings on first run only; after that
	// the saved file wins, so a value changed from the panel is not undone by
	// the next restart.
	tuner, err := settings.Load(cfg.SettingsFile, settings.Values{
		PublicDailyLimit:    cfg.PublicDailyLimit,
		PublicOpenLimit:     cfg.PublicDailyLimit,
		GlobalDailyLimit:    cfg.GlobalDailyLimit,
		NewAccountThreshold: cfg.NewAccountThreshold,
		Burst:               cfg.PublicBurst,
		BurstWindow:         cfg.PublicBurstWindow,
		BanFor:              cfg.PublicBanFor,
		PublicMaxTokens:     cfg.PublicMaxTokens,
		PublicMaxRunes:      cfg.PublicMaxRunes,
		RawFlagEnabled:      cfg.RawSystemPrompt != "",
		CacheTTL:            cfg.CacheTTL,
	})
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	live := tuner.Get()

	rations, err := quota.New(quota.Options{
		Limit:               live.PublicDailyLimit,
		GlobalLimit:         live.GlobalDailyLimit,
		NewAccountThreshold: live.NewAccountThreshold,
		Burst:               live.Burst,
		Window:              live.BurstWindow,
		BanFor:              live.BanFor,
		WarnTTL:             cfg.WarnTTL,
		MaxWarns:            cfg.MaxWarns,
		File:                cfg.QuotaFile,
	})
	if err != nil {
		return fmt.Errorf("load public quota: %w", err)
	}
	if rations.Enabled() {
		log.Info("public access is on",
			"daily_limit", live.PublicDailyLimit,
			"burst", live.Burst,
			"burst_window", live.BurstWindow,
			"ban_for", live.BanFor)
	}

	alerts := alert.New(alert.Options{
		Sender:   bot,
		Admins:   adminIDs(cfg.AdminUsers),
		Logger:   log,
		Cooldown: cfg.AlertCooldown,
	})

	cache := answers.New(answers.Options{TTL: live.CacheTTL})

	// A nil generator disables the image path; reply.Handler answers such
	// requests with an explanation instead of failing.
	var images imagegen.Generator
	if cfg.ImagesEnabled() {
		images = imagegen.New(imagegen.Options{
			BaseURL:     cfg.ImageBaseURL,
			APIKeys:     cfg.ImageAPIKeys,
			KeyCooldown: cfg.KeyCooldown,
			Model:       cfg.ImageModel,
			Size:        cfg.ImageSize,
			Timeout:     cfg.ImageTimeout,
		})
	} else {
		log.Warn("picture generation is off: set IMAGE_MODEL and IMAGE_STORAGE_CHAT_ID to enable it; " +
			"note that image generation is billed separately, so the key needs billing enabled")
	}

	// One client serves both the answers and the admin menu's numbers, so what
	// the menu shows is what actually happened.
	model := llm.New(llm.Options{
		BaseURL:         cfg.BaseURL,
		APIKeys:         cfg.APIKeys,
		KeyCooldown:     cfg.KeyCooldown,
		Models:          cfg.Models,
		SystemPrompt:    cfg.SystemPrompt,
		MaxTokens:       cfg.MaxTokens,
		ReasoningEffort: cfg.ReasoningEffort,
		Timeout:         cfg.Timeout,
	})

	// The executor wraps the model with a tool-calling loop. When no tools are
	// configured it is a pure pass-through, so existing deployments keep working.
	var completer llm.Completer = model
	if cfg.SearchEnabled || cfg.TGChannelEnabled {
		var searcher *search.Searcher
		if cfg.SearchEnabled {
			searcher = search.New(search.Options{
				PaxsenixKey: cfg.PaxsenixKey,
				PaxsenixURL: cfg.PaxsenixURL,
				SearxngURL:  cfg.SearxngURL,
				Timeout:     cfg.Timeout,
				Log:         log,
			})
			log.Info("web search enabled",
				"backend", searchBackend(cfg),
				"paxsenix", cfg.PaxsenixKey != "",
				"searxng_fallback", cfg.SearxngURL != "")
		}

		var tgreader *tgchannel.Reader
		if cfg.TGChannelEnabled {
			tgreader = tgchannel.New(tgchannel.Options{
				Timeout: cfg.Timeout,
				Log:     log,
			})
			log.Info("telegram channel reading enabled")
		}

		completer = tools.New(tools.Options{
			Model:    model,
			Search:   searcher,
			TGReader: tgreader,
			Log:      log,
		})
	}

	// A persistent, compression-aware session store for private chats. Every
	// user gets their own per-user context file (sessions/{userID}.json) with
	// git auto-commit so the data is versioned and can be pushed to a private
	// remote for backup. The summariser uses the raw LLM client (not the
	// tool-wrapped executor) so compression calls are quick and never trigger
	// tool loops.
	var dmStore *session.Store
	{
		sessionDir := filepath.Join(filepath.Dir(cfg.StateFile), "sessions")
		summariser := session.SummariserFunc(func(ctx context.Context, turns []session.Turn) (string, error) {
			var b strings.Builder
			for _, t := range turns {
				b.WriteString(t.Role)
				b.WriteString(": ")
				b.WriteString(t.Content)
				b.WriteString("\n")
			}
			resp, err := model.Complete(ctx, llm.Request{
				System: "Сожми диалог в краткое саммари на русском. Сохраняй ключевые факты, имена, числа, решения и контекст. Пиши плотно — без воды, без повторов. Максимум 3-4 предложения.",
				Prompt: b.String(),
			})
			if err != nil {
				return "", err
			}
			return "Краткое содержание предыдущих сообщений:\n" + resp, nil
		})
		dmStore, err = session.New(session.Options{
			Dir:        sessionDir,
			GitSync:    true,
			Summariser: summariser,
			MaxTurns:   10,
			KeepTurns:  4,
			Logger:     log,
		})
		if err != nil {
			return fmt.Errorf("load sessions: %w", err)
		}
	}

	handler := reply.New(reply.Options{
		Sender:            bot,
		Model:             completer,
		Quota:             rations,
		Cache:             cache,
		Alerts:            alerts,
		Images:            images,
		Files:             tgfile.New(bot, nil),
		Voice:             transcribe.New(cfg.GroqAPIKey, "whisper-large-v3"),
		Memory:            memory.New(memory.Options{TTL: cfg.MemoryTTL}),
		DMRecall:          dmStore,
		Sessions:          dmStore,
		Access:            allow,
		Commands:          admin.New(allow).WithPardoner(rations),
		Logger:            log,
		BotID:             me.ID,
		BotUsername:       me.Username,
		StorageChatID:     cfg.ImageStorageChatID,
		MaxConcurrent:     cfg.MaxConcurrent,
		MaxReplyRunes:     cfg.MaxReplyRunes,
		CommandReplyTTL:   cfg.CommandReplyTTL,
		RawSystemPrompt:   cfg.RawSystemPrompt,
		DivkaSystemPrompt: config.DivkaSystemPrompt,
		Presets:           tuner,
		PublicMaxTokens:   live.PublicMaxTokens,
		PublicMaxRunes:    live.PublicMaxRunes,
		MaxQueue:          cfg.MaxQueue,
		Timeout:           cfg.Timeout,
		ImageTimeout:      cfg.ImageTimeout,
	})
	handler.SetRawFlagEnabled(live.RawFlagEnabled)

	panel := menu.New(menu.Options{
		Sender:   bot,
		Models:   model,
		Quota:    rations,
		Access:   allow,
		Settings: tuner,
		Prober:   model,
		Picker:   model,
		Switcher: model,
		// One place decides what a changed setting means, so the panel does not
		// have to know which component enforces which knob.
		Apply: func(v settings.Values) {
			rations.SetPolicy(v.PublicDailyLimit, v.GlobalDailyLimit, v.Burst,
				v.BurstWindow, v.BanFor, v.NewAccountThreshold)
			handler.SetPublicPolicy(v.PublicMaxTokens, v.PublicMaxRunes)
			handler.SetRawFlagEnabled(v.RawFlagEnabled)
			cache.SetTTL(v.CacheTTL)
		},
		Logger:      log,
		BotUsername: me.Username,
		ImagesOn:    cfg.ImagesEnabled(),
		StartedAt:   time.Now(),
		Restorer:    dmStore,
	})

	// guest_message is how the bot is summoned in other chats; message and
	// callback_query are the private-chat panel. Nothing else has a handler, so
	// nothing else is requested.
	updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{"guest_message", "message", "callback_query"},
	})
	if err != nil {
		return fmt.Errorf("start long polling: %w", err)
	}

	botHandler, err := th.NewBotHandler(bot, updates)
	if err != nil {
		return fmt.Errorf("create bot handler: %w", err)
	}
	botHandler.HandleGuestMessage(func(hctx *th.Context, msg telego.Message) error {
		return handler.HandleGuestMessage(hctx, msg)
	})
	botHandler.HandleMessage(func(hctx *th.Context, msg telego.Message) error {
		// The panel only claims its own commands; anything else in a private
		// chat is an ordinary question and is answered like one.
		handled, err := panel.HandleMessage(hctx, msg)
		if err != nil || handled {
			return err
		}
		if msg.Chat.Type != telego.ChatTypePrivate {
			return nil
		}
		return handler.HandleDirectMessage(hctx, msg)
	})
	botHandler.HandleCallbackQuery(func(hctx *th.Context, query telego.CallbackQuery) error {
		return panel.HandleCallback(hctx, query)
	})

	go func() {
		<-ctx.Done()
		log.Info("shutting down", "grace", shutdownGrace)

		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := botHandler.StopWithContext(stopCtx); err != nil {
			log.Error("stop bot handler", "err", err)
		}
	}()

	log.Info("started",
		"bot", me.Username,
		"models", cfg.Models,
		"keys", len(cfg.APIKeys),
		"guest_mode", me.SupportsGuestQueries,
		"images", cfg.ImagesEnabled(),
		"web_search", cfg.SearchEnabled,
		"tg_channel_read", cfg.TGChannelEnabled,
		"tools", cfg.SearchEnabled || cfg.TGChannelEnabled,
		"voice", cfg.GroqAPIKey != "",
		"admins", len(cfg.AdminUsers),
		"allowed_entries", len(allow.List()),
		"state_file", cfg.StateFile,
	)

	if err := botHandler.Start(); err != nil {
		return fmt.Errorf("bot handler: %w", err)
	}
	log.Info("stopped")
	return nil
}

// adminIDs flattens the admin set for the alert notifier.
func adminIDs(set map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out) // a stable order keeps the logs readable
	return out
}

// searchBackend reports which search path is active, for the startup log.
func searchBackend(cfg *config.Config) string {
	if cfg.PaxsenixKey != "" {
		return "paxsenix"
	}
	return "searxng"
}
