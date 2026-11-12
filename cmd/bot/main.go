package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	_ "time/tzdata" // daily quota reset time zones work in minimal containers

	"github.com/vvchaschin/communitybot/internal/brain"
	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
	"github.com/vvchaschin/communitybot/internal/tg"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env (optional)")
	flag.Parse()

	if err := run(*cfgPath, *envPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath, envPath string) error {
	cfg, err := config.Load(cfgPath, envPath)
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	persona, err := os.ReadFile(cfg.PersonaPath)
	if err != nil {
		return err
	}

	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()

	chain, err := llm.NewChain(cfg, store)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var br *brain.Brain
	adapter, err := tg.New(cfg.TelegramToken, tg.Handlers{
		Message:    func(ctx context.Context, in brain.Incoming) { br.OnMessage(ctx, in) },
		Membership: func(ctx context.Context, ev brain.Membership) { br.OnMembership(ctx, ev) },
		Migrate:    func(ctx context.Context, from, to int64) { br.OnMigrate(ctx, from, to) },
		Reaction:   func(ctx context.Context, r brain.Reaction) { br.OnReaction(ctx, r) },
		Callback:   func(ctx context.Context, cb brain.Callback) { br.OnCallback(ctx, cb) },
		Topic:      func(ctx context.Context, t brain.Topic) { br.OnTopic(ctx, t) },
	})
	if err != nil {
		return err
	}
	me, err := adapter.Me(ctx)
	if err != nil {
		return err
	}
	if !me.CanReadAllGroupMessages {
		slog.Warn("privacy mode is ON: the bot sees only commands, mentions and replies. " +
			"Disable it in @BotFather (/setprivacy → Disable) and re-add the bot to the group, or make it an admin.")
	}
	self := brain.Identity{ID: me.ID, Username: me.Username, Name: me.FirstName}
	br = brain.New(cfg, string(persona), store, chain, adapter, self)

	providers := make([]string, 0, len(cfg.LLM.Providers))
	for _, p := range cfg.LLM.Providers {
		providers = append(providers, p.Name)
	}
	slog.Info("bot started", "username", me.Username, "admins", cfg.Telegram.AdminIDs,
		"providers", strings.Join(providers, " → "))

	go br.Run(ctx)
	adapter.Start(ctx)
	slog.Info("bot stopped")
	return nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
