// Package config loads config.yaml and secrets from the environment / .env.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelCfg describes one model of a provider and its free-tier limits.
type ModelCfg struct {
	Model string `yaml:"model"`
	// RPM / RPD — requests per minute / per day. 0 means "no limit" (e.g. local model).
	RPM         int            `yaml:"rpm"`
	RPD         int            `yaml:"rpd"`
	Temperature *float64       `yaml:"temperature"`
	MaxTokens   int            `yaml:"max_tokens"` // 0 = do not send
	Extra       map[string]any `yaml:"extra"`      // merged into the request body as-is
	// StructuredOutputs: send the JSON schema of the answer (response_format) where the bot has one
	// — evaluation and command parsing. Only for models and hosters that support it (SPEC 7).
	StructuredOutputs bool `yaml:"structured_outputs"`
}

// Provider is one OpenAI-compatible endpoint.
type Provider struct {
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"`
	// Time zone in which the provider resets daily quotas (Gemini: America/Los_Angeles).
	DailyResetTZ string    `yaml:"daily_reset_tz"`
	Filter       *ModelCfg `yaml:"filter"`
	Reply        *ModelCfg `yaml:"reply"`
}

type Behavior struct {
	BatchIntervalSeconds int     `yaml:"batch_interval_seconds"`
	ContextMessages      int     `yaml:"context_messages"`
	CooldownMinutes      float64 `yaml:"cooldown_minutes"`
	ReplyDelayMinSeconds float64 `yaml:"reply_delay_min_seconds"`
	ReplyDelayMaxSeconds float64 `yaml:"reply_delay_max_seconds"`
	MaxConsecutiveOwn    int     `yaml:"max_consecutive_own"`
	StaleAfterMinutes    float64 `yaml:"stale_after_minutes"`
	OfflineReaction      string  `yaml:"offline_reaction"`
	RetentionDays        int     `yaml:"retention_days"`
	// A filter reply to someone who talked with the bot this recently (same topic) is part of
	// a dialog: it neither starts nor respects the cooldown.
	DialogMinutes float64 `yaml:"dialog_minutes"`
	// Someone in a dialog with the bot who writes on without its name is answered once they have
	// been silent this long (Telegram does not tell bots that someone is typing); their new message
	// meanwhile starts the wait over and cancels the evaluation under way.
	DialogWaitSeconds float64 `yaml:"dialog_wait_seconds"`
	// Reaction habits (SPEC 4.6): the bot adopts a reaction once people put it on at least
	// HabitMinMessages messages of a kind in a topic, and on at least HabitMinShare of them.
	HabitMinMessages int     `yaml:"habit_min_messages"`
	HabitMinShare    float64 `yaml:"habit_min_share"`
	// A reply in a thread (SPEC 7a) sees the thread's first ThreadHeadMessages and last
	// ContextMessages messages, and the topic's last AroundMessages messages outside the thread.
	ThreadHeadMessages int `yaml:"thread_head_messages"`
	AroundMessages     int `yaml:"around_messages"`
	// A message without a reply may continue, by its meaning, one of the topic's last
	// ThreadCandidates threads active within ThreadActiveHours.
	ThreadActiveHours float64 `yaml:"thread_active_hours"`
	ThreadCandidates  int     `yaml:"thread_candidates"`
	// The running summary of a topic's older messages (SPEC 4.5): updated once SummaryChunkMessages
	// messages older than the topic's last ContextMessages pile up; at most SummaryMaxChars long.
	SummaryChunkMessages int `yaml:"summary_chunk_messages"`
	SummaryMaxChars      int `yaml:"summary_max_chars"`
	// Notes about participants (SPEC 4.5) are updated with each summary chunk; each of a note's two
	// parts (how the chat treats the person, the bot's own impression) is at most NoteMaxChars long.
	NoteMaxChars int `yaml:"note_max_chars"`
	// Lore the bot picks out of the conversation itself (SPEC 4.5), with each summary chunk: at most
	// LoreMaxEntries such entries per chat (the leadership's do not count), each at most LoreMaxChars long.
	LoreMaxEntries int `yaml:"lore_max_entries"`
	LoreMaxChars   int `yaml:"lore_max_chars"`
}

type Config struct {
	Telegram struct {
		// The bot's creators: they turn groups on and off and appoint their admins (SPEC 3.1, 3.2).
		AdminIDs []int64  `yaml:"admin_ids"`
		Aliases  []string `yaml:"aliases"`
		// Removed: kept only to warn on old configs. Groups are turned on by the creator (SPEC 3.1),
		// the bot no longer leaves groups, so there is no farewell either.
		LegacyAllowedChatIDs  []int64 `yaml:"allowed_chat_ids"`
		LegacyStrangerMessage string  `yaml:"stranger_message"`
	} `yaml:"telegram"`
	Behavior Behavior `yaml:"behavior"`
	Quota    struct {
		ReserveForMentions float64 `yaml:"reserve_for_mentions"`
	} `yaml:"quota"`
	LLM struct {
		TimeoutSeconds int `yaml:"timeout_seconds"`
		// Retries of a request to the same provider after a transient failure (5xx, connection
		// refused/reset) before falling back to the next one. nil → default; 0 disables.
		Retries           *int       `yaml:"retries"`
		RetryDelaySeconds float64    `yaml:"retry_delay_seconds"`
		Providers         []Provider `yaml:"providers"`
	} `yaml:"llm"`
	Storage struct {
		Path string `yaml:"path"`
	} `yaml:"storage"`
	PersonaPath string `yaml:"persona_path"`
	LogLevel    string `yaml:"log_level"`

	TelegramToken string `yaml:"-"`
}

// Load reads the YAML config, loads .env (without overriding real env vars),
// applies defaults and resolves secrets.
func Load(path, envPath string) (*Config, error) {
	if err := loadDotEnv(envPath); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()

	c.TelegramToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	if c.TelegramToken == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is not set")
	}
	if len(c.Telegram.AdminIDs) == 0 {
		return nil, errors.New("telegram.admin_ids is empty: list the Telegram user ids of the bot's creators, " +
			"who turn groups on (your id: send any message to @userinfobot)")
	}
	if len(c.Telegram.LegacyAllowedChatIDs) > 0 {
		slog.Warn("telegram.allowed_chat_ids is no longer used: a creator from telegram.admin_ids turns groups on " +
			"in a private chat with the bot")
	}
	if c.Telegram.LegacyStrangerMessage != "" {
		slog.Warn("telegram.stranger_message is no longer used: the bot stays in any group, silent until a creator turns it on")
	}

	var providers []Provider
	for _, p := range c.LLM.Providers {
		if p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
			if p.APIKey == "" {
				slog.Warn("provider skipped: API key env var is empty", "provider", p.Name, "env", p.APIKeyEnv)
				continue
			}
		}
		if p.BaseURL == "" || (p.Filter == nil && p.Reply == nil) {
			return nil, fmt.Errorf("provider %q: base_url and at least one of filter/reply are required", p.Name)
		}
		if p.DailyResetTZ == "" {
			p.DailyResetTZ = "UTC"
		}
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		return nil, errors.New("no usable LLM providers (check API keys)")
	}
	c.LLM.Providers = providers
	return &c, nil
}

func (c *Config) applyDefaults() {
	b := &c.Behavior
	setInt(&b.BatchIntervalSeconds, 3600)
	setInt(&b.ContextMessages, 30)
	setFloat(&b.CooldownMinutes, 15)
	setFloat(&b.ReplyDelayMinSeconds, 3)
	setFloat(&b.ReplyDelayMaxSeconds, 12)
	setInt(&b.MaxConsecutiveOwn, 2)
	setFloat(&b.StaleAfterMinutes, 30)
	setInt(&b.RetentionDays, 30)
	setFloat(&b.DialogMinutes, 10)
	setFloat(&b.DialogWaitSeconds, 15)
	setInt(&b.HabitMinMessages, 3)
	setFloat(&b.HabitMinShare, 0.5)
	setInt(&b.ThreadHeadMessages, 3)
	setInt(&b.AroundMessages, 10)
	setFloat(&b.ThreadActiveHours, 6)
	setInt(&b.ThreadCandidates, 10)
	setInt(&b.SummaryChunkMessages, 40)
	setInt(&b.SummaryMaxChars, 1500)
	setInt(&b.NoteMaxChars, 300)
	setInt(&b.LoreMaxEntries, 30)
	setInt(&b.LoreMaxChars, 200)
	if b.ReplyDelayMaxSeconds < b.ReplyDelayMinSeconds {
		b.ReplyDelayMaxSeconds = b.ReplyDelayMinSeconds
	}
	if c.Quota.ReserveForMentions < 0 || c.Quota.ReserveForMentions >= 1 {
		c.Quota.ReserveForMentions = 0.2
	}
	setInt(&c.LLM.TimeoutSeconds, 90)
	if c.LLM.Retries == nil || *c.LLM.Retries < 0 {
		def := 2
		c.LLM.Retries = &def
	}
	setFloat(&c.LLM.RetryDelaySeconds, 3)
	if c.Storage.Path == "" {
		c.Storage.Path = "data/bot.db"
	}
	if c.PersonaPath == "" {
		c.PersonaPath = "persona.md"
	}
}

func setInt(v *int, def int) {
	if *v <= 0 {
		*v = def
	}
}

func setFloat(v *float64, def float64) {
	if *v <= 0 {
		*v = def
	}
}

// loadDotEnv sets KEY=VALUE pairs from the file unless already present in the environment.
// A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
