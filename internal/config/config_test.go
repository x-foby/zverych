package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unsetEnv removes a variable for the duration of the test (restored on cleanup).
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
telegram: {admin_ids: [7]}
llm:
  providers:
    - name: local
      base_url: http://localhost:11434/v1
      reply: {model: qwen}
`

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.yaml", minimal), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatal(err)
	}
	want := Behavior{BatchIntervalSeconds: 3600, ContextMessages: 30, CooldownMinutes: 15,
		ReplyDelayMinSeconds: 3, ReplyDelayMaxSeconds: 12, MaxConsecutiveOwn: 2, StaleAfterMinutes: 30, RetentionDays: 30, DialogMinutes: 10, DialogWaitSeconds: 15,
		HabitMinMessages: 3, HabitMinShare: 0.5, ThreadHeadMessages: 3, AroundMessages: 10,
		ThreadActiveHours: 6, ThreadCandidates: 10, SummaryChunkMessages: 40, SummaryMaxChars: 1500, NoteMaxChars: 300,
		LoreMaxEntries: 30, LoreMaxChars: 200}
	if c.Behavior != want {
		t.Errorf("behavior defaults:\n got %+v\nwant %+v", c.Behavior, want)
	}
	if c.Quota.ReserveForMentions != 0 || c.LLM.TimeoutSeconds != 90 || c.Storage.Path != "data/bot.db" ||
		c.PersonaPath != "persona.md" || c.TelegramToken != "tok" {
		t.Errorf("defaults: %+v", c)
	}
	if p := c.LLM.Providers; len(p) != 1 || p[0].DailyResetTZ != "UTC" || p[0].APIKey != "" {
		t.Errorf("provider: %+v", p)
	}
	if c.LLM.Retries == nil || *c.LLM.Retries != 2 || c.LLM.RetryDelaySeconds != 3 {
		t.Errorf("retry defaults: retries=%v delay=%v", c.LLM.Retries, c.LLM.RetryDelaySeconds)
	}
}

func TestLoadKeepsExplicitValues(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.yaml", `
telegram: {admin_ids: [7, 8], aliases: [бот]}
behavior:
  batch_interval_seconds: 1
  context_messages: 1
  cooldown_minutes: 0.5
  reply_delay_min_seconds: 0.5
  reply_delay_max_seconds: 0.5
  max_consecutive_own: 1
  stale_after_minutes: 0.5
  offline_reaction: "🤔"
  retention_days: 1
  dialog_minutes: 0.5
  dialog_wait_seconds: 0.5
  habit_min_messages: 1
  habit_min_share: 2
  thread_head_messages: 1
  around_messages: 1
  thread_active_hours: 0.5
  thread_candidates: 1
  summary_chunk_messages: 2
  summary_max_chars: 3
  note_max_chars: 4
  lore_max_entries: 5
  lore_max_chars: 6
quota: {reserve_for_mentions: 0.999}
llm:
  timeout_seconds: 1
  retries: 0
  retry_delay_seconds: 0.5
  providers: [{name: local, base_url: http://localhost/v1, reply: {model: qwen}}]
storage: {path: /tmp/x.db}
persona_path: p.md
log_level: debug
`), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatal(err)
	}
	want := Behavior{1, 1, 0.5, 0.5, 0.5, 1, 0.5, "🤔", 1, 0.5, 0.5, 1, 2, 1, 1, 0.5, 1, 2, 3, 4, 5, 6}
	if c.Behavior != want {
		t.Errorf("explicit values must win:\n got %+v\nwant %+v", c.Behavior, want)
	}
	if c.Quota.ReserveForMentions != 0.999 || c.LLM.TimeoutSeconds != 1 || c.Storage.Path != "/tmp/x.db" ||
		c.PersonaPath != "p.md" || c.LogLevel != "debug" ||
		!reflect.DeepEqual(c.Telegram.AdminIDs, []int64{7, 8}) || !reflect.DeepEqual(c.Telegram.Aliases, []string{"бот"}) {
		t.Errorf("explicit values: %+v", c)
	}
	if c.LLM.Retries == nil || *c.LLM.Retries != 0 || c.LLM.RetryDelaySeconds != 0.5 {
		t.Errorf("explicit retries: 0 must disable retries: retries=%v delay=%v", c.LLM.Retries, c.LLM.RetryDelaySeconds)
	}
}

func TestApplyDefaults(t *testing.T) {
	for _, c := range []struct {
		reserve, want float64
	}{{-0.1, 0.2}, {0, 0}, {0.5, 0.5}, {1, 0.2}, {1.5, 0.2}} {
		var cfg Config
		cfg.Quota.ReserveForMentions = c.reserve
		cfg.applyDefaults()
		if cfg.Quota.ReserveForMentions != c.want {
			t.Errorf("reserve %v → %v want %v", c.reserve, cfg.Quota.ReserveForMentions, c.want)
		}
	}

	var cfg Config
	cfg.Behavior.ReplyDelayMinSeconds, cfg.Behavior.ReplyDelayMaxSeconds = 20, 5
	cfg.Behavior.ContextMessages, cfg.Behavior.CooldownMinutes = -1, -1
	cfg.applyDefaults()
	if b := cfg.Behavior; b.ReplyDelayMaxSeconds != 20 || b.ContextMessages != 30 || b.CooldownMinutes != 15 {
		t.Errorf("max delay must be raised to min, negatives replaced: %+v", b)
	}
	neg := -1
	cfg = Config{}
	cfg.LLM.Retries = &neg
	cfg.applyDefaults()
	if *cfg.LLM.Retries != 2 {
		t.Errorf("negative retries must fall back to the default, got %d", *cfg.LLM.Retries)
	}
}

func TestLoadSecrets(t *testing.T) {
	unsetEnv(t, "TELEGRAM_BOT_TOKEN", "CB_TEST_GEMINI", "# CB_TEST_COMMENTED", "CB_TEST_EMPTY", "CB_TEST_QUOTED", "CB_TEST_EXPORTED", "not a pair")
	t.Setenv("CB_TEST_PRESET", "from-env")
	dir := t.TempDir()
	env := write(t, dir, ".env", `
# comment
# CB_TEST_COMMENTED=1
TELEGRAM_BOT_TOKEN = tok-from-file
not a pair
CB_TEST_GEMINI=gem

CB_TEST_QUOTED="with spaces"
export CB_TEST_EXPORTED='x'
CB_TEST_PRESET=from-file
`)
	cfg := write(t, dir, "c.yaml", `
telegram: {admin_ids: [7]}
llm:
  providers:
    - {name: openrouter, base_url: http://o, api_key_env: CB_TEST_EMPTY, reply: {model: free}}
    - {name: gemini, base_url: http://g, api_key_env: CB_TEST_GEMINI, daily_reset_tz: America/Los_Angeles, filter: {model: lite}}
`)
	c, err := Load(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.TelegramToken != "tok-from-file" {
		t.Errorf("token %q", c.TelegramToken)
	}
	if len(c.LLM.Providers) != 1 || c.LLM.Providers[0].Name != "gemini" || c.LLM.Providers[0].APIKey != "gem" ||
		c.LLM.Providers[0].DailyResetTZ != "America/Los_Angeles" {
		t.Errorf("providers without a key must be skipped: %+v", c.LLM.Providers)
	}
	if _, set := os.LookupEnv("not a pair"); set {
		t.Error("lines without '=' must be skipped")
	}
	if _, set := os.LookupEnv("# CB_TEST_COMMENTED"); set {
		t.Error("commented-out lines must be ignored")
	}
	for k, want := range map[string]string{"CB_TEST_QUOTED": "with spaces", "CB_TEST_EXPORTED": "x", "CB_TEST_PRESET": "from-env"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s=%q want %q", k, got, want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	const adm = "telegram: {admin_ids: [7]}\n"
	dir := t.TempDir()
	noEnv := filepath.Join(dir, "missing.env")
	cases := []struct {
		name, yaml, token, wantErr string
	}{
		{"no token", minimal, "", "TELEGRAM_BOT_TOKEN"},
		{"bad yaml", "llm: [", "tok", "parse config"},
		{"no admins", "llm: {providers: [{name: g, base_url: http://g, reply: {model: m}}]}", "tok", "telegram.admin_ids is empty"},
		{"no providers", adm + "llm: {providers: []}", "tok", "no usable LLM providers"},
		{"all keys missing", adm + "llm: {providers: [{name: g, base_url: http://g, api_key_env: CB_TEST_NOPE, reply: {model: m}}]}", "tok", "no usable"},
		{"no base_url", adm + "llm: {providers: [{name: g, reply: {model: m}}]}", "tok", `provider "g"`},
		{"no models", adm + "llm: {providers: [{name: g, base_url: http://g}]}", "tok", `provider "g"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unsetEnv(t, "CB_TEST_NOPE")
			t.Setenv("TELEGRAM_BOT_TOKEN", c.token)
			_, err := Load(write(t, dir, "c.yaml", c.yaml), noEnv)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}

	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	if _, err := Load(filepath.Join(dir, "nope.yaml"), noEnv); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("missing config: %v", err)
	}
	locked := write(t, dir, "locked.env", "A=1\n")
	os.Chmod(locked, 0)
	if _, err := Load(write(t, dir, "c.yaml", minimal), locked); err == nil || !strings.Contains(err.Error(), "open "+locked) {
		t.Fatalf("an unreadable .env must be reported: %v", err)
	}
	if _, err := Load(write(t, dir, "c.yaml", minimal), dir); err == nil {
		t.Fatal("an unreadable .env (a directory) must be an error")
	}
}

func TestLegacyTelegramSettingsOnlyWarn(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.yaml", `
telegram: {admin_ids: [7], stranger_message: "Кто ты?", allowed_chat_ids: [-100]}
llm: {providers: [{name: l, base_url: http://l, reply: {model: m}}]}
`), filepath.Join(dir, "missing.env"))
	if err != nil || len(c.Telegram.AdminIDs) != 1 {
		t.Fatalf("legacy allowed_chat_ids and stranger_message must only warn: %v", err)
	}
}

// The template must stay loadable: OpenRouter first, both roles without data collection.
func TestExampleConfig(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := strings.Replace(string(raw), "admin_ids: []", "admin_ids: [1]", 1)
	t.Setenv("TELEGRAM_BOT_TOKEN", "t")
	t.Setenv("OPENROUTER_API_KEY", "k")
	c, err := Load(write(t, dir, "config.yaml", cfg), filepath.Join(dir, "none.env"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LLM.Providers) != 1 {
		t.Fatalf("providers: %+v", c.LLM.Providers)
	}
	p := c.LLM.Providers[0]
	reply := map[string]any{
		"reasoning": map[string]any{"enabled": false},
		"provider":  map[string]any{"data_collection": "deny"},
	}
	// The filter takes the cheapest host of those that honour the answer's schema (SPEC 7); replies
	// keep the host pinned by session_id (SPEC 7a) and have no schema.
	filter := map[string]any{
		"reasoning": map[string]any{"enabled": false},
		"provider":  map[string]any{"data_collection": "deny", "sort": "price", "require_parameters": true},
	}
	if p.Name != "openrouter" || p.APIKey != "k" || p.Filter == nil || p.Reply == nil ||
		!reflect.DeepEqual(p.Filter.Extra, filter) || !reflect.DeepEqual(p.Reply.Extra, reply) ||
		!p.Filter.StructuredOutputs || p.Reply.StructuredOutputs {
		t.Errorf("openrouter provider: %+v filter %+v reply %+v", p, p.Filter, p.Reply)
	}
}
