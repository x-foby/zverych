package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFailsFastOnBadSetup(t *testing.T) {
	dir := t.TempDir()
	noEnv := filepath.Join(dir, "missing.env")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:abc")

	if err := run(filepath.Join(dir, "nope.yaml"), noEnv); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("missing config: %v", err)
	}

	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte(`
telegram: {admin_ids: [7]}
persona_path: `+filepath.Join(dir, "no-persona.md")+`
storage: {path: `+filepath.Join(dir, "bot.db")+`}
llm: {providers: [{name: local, base_url: http://localhost/v1, reply: {model: m}}]}
`), 0o600)
	if err := run(cfg, noEnv); err == nil || !strings.Contains(err.Error(), "no-persona.md") {
		t.Fatalf("missing persona: %v", err)
	}
}

func TestSetupLogging(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := context.Background()
	for level, want := range map[string]slog.Level{"debug": slog.LevelDebug, "WARN": slog.LevelWarn, "": slog.LevelInfo, "bogus": slog.LevelInfo} {
		setupLogging(level)
		h := slog.Default().Handler()
		if !h.Enabled(ctx, want) || h.Enabled(ctx, want-1) {
			t.Errorf("setupLogging(%q): expected minimum level %v", level, want)
		}
	}
}
