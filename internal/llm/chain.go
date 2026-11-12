package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
)

// Role selects which model of a provider to use.
type Role int

const (
	RoleFilter     Role = iota // cheap "should I speak?" decision
	RoleReply                  // writing the actual message
	RoleCommand                // parsing an instruction to the bot; served by the filter models
	RoleThread                 // which thread a message without a reply continues; served by the filter models
	RoleSummary                // the running summary of a topic's older messages; served by the filter models
	RoleNotes                  // notes about participants, updated with the summary; served by the filter models
	RoleLore                   // lore the bot picks out of the conversation, with the summary; served by the filter models
	RoleLoreSelect             // semantic retrieval of lore; served by the filter models
	RoleTone                   // how people talk in a topic, with the summary; served by the filter models
)

func (r Role) String() string {
	switch r {
	case RoleFilter:
		return "filter"
	case RoleCommand:
		return "command"
	case RoleThread:
		return "thread"
	case RoleSummary:
		return "summary"
	case RoleNotes:
		return "notes"
	case RoleLore:
		return "lore"
	case RoleLoreSelect:
		return "lore_select"
	case RoleTone:
		return "tone"
	}
	return "reply"
}

// Priority decides how much of the daily quota a request may use.
type Priority int

const (
	Initiative Priority = iota // unsolicited participation; cannot touch the reserve
	Direct                     // bot was addressed directly; may use the whole quota
)

// ErrUnavailable means every provider for the role is exhausted or failing: the bot is "offline".
var ErrUnavailable = errors.New("llm: all providers unavailable")

// Usage persists daily request counters (implemented by storage.Store).
type Usage interface {
	QuotaGet(ctx context.Context, provider, model, day string) (int, error)
	QuotaInc(ctx context.Context, provider, model, day string) error
}

type endpoint struct {
	provider string
	cfg      config.ModelCfg
	client   *Client
	loc      *time.Location

	mu           sync.Mutex
	window       []time.Time // request times within the last minute
	blockedUntil time.Time
}

// Chain tries providers in configured order, respecting local quota accounting.
type Chain struct {
	filters []*endpoint
	replies []*endpoint
	usage   Usage
	reserve float64
	now     func() time.Time

	retries    int           // extra attempts on the same endpoint after a transient error
	retryDelay time.Duration // pause between those attempts
	sleep      func(ctx context.Context, d time.Duration) error
}

func NewChain(cfg *config.Config, usage Usage) (*Chain, error) {
	httpc := &http.Client{Timeout: time.Duration(cfg.LLM.TimeoutSeconds) * time.Second}
	c := &Chain{usage: usage, reserve: cfg.Quota.ReserveForMentions, now: time.Now, sleep: sleepCtx,
		retryDelay: time.Duration(cfg.LLM.RetryDelaySeconds * float64(time.Second))}
	if cfg.LLM.Retries != nil && *cfg.LLM.Retries > 0 {
		c.retries = *cfg.LLM.Retries
	}
	for _, p := range cfg.LLM.Providers {
		loc, err := time.LoadLocation(p.DailyResetTZ)
		if err != nil {
			return nil, fmt.Errorf("provider %s: bad daily_reset_tz: %w", p.Name, err)
		}
		client := &Client{BaseURL: p.BaseURL, APIKey: p.APIKey, HTTP: httpc}
		if p.Filter != nil {
			c.filters = append(c.filters, &endpoint{provider: p.Name, cfg: *p.Filter, client: client, loc: loc})
		}
		if p.Reply != nil {
			c.replies = append(c.replies, &endpoint{provider: p.Name, cfg: *p.Reply, client: client, loc: loc})
		}
	}
	return c, nil
}

func (c *Chain) endpoints(role Role) []*endpoint {
	if role != RoleReply {
		// A provider without a dedicated filter model may use its reply model.
		if len(c.filters) > 0 {
			return c.filters
		}
		return c.replies
	}
	return c.replies
}

// Available reports whether at least one endpoint could serve the request right now.
func (c *Chain) Available(ctx context.Context, role Role, prio Priority) bool {
	now := c.now()
	for _, e := range c.endpoints(role) {
		if ok, _ := c.allow(ctx, e, prio, now); ok {
			return true
		}
	}
	return false
}

// Complete runs the request on the first endpoint that has quota and succeeds.
// A transient failure (5xx, connection refused/reset) is retried on the same endpoint up to
// c.retries times before moving on. Returns ErrUnavailable if none could serve it.
func (c *Chain) Complete(ctx context.Context, role Role, prio Priority, msgs []Message) (string, string, error) {
	for _, e := range c.endpoints(role) {
		ok, reason := c.allow(ctx, e, prio, c.now())
		if !ok {
			slog.Debug("llm endpoint skipped", "provider", e.provider, "model", e.cfg.Model, "reason", reason)
			continue
		}
		var err error
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				if ok, reason := c.allow(ctx, e, prio, c.now()); !ok {
					slog.Debug("llm retry skipped", "provider", e.provider, "model", e.cfg.Model, "reason", reason)
					break
				}
			}
			now := c.now()
			c.recordAttempt(e, now)
			start := time.Now()
			var res Result
			req := Request{
				Model: e.cfg.Model, Messages: msgs, Temperature: e.cfg.Temperature,
				MaxTokens: e.cfg.MaxTokens, Extra: e.cfg.Extra, Session: SessionOf(ctx),
			}
			if e.cfg.StructuredOutputs {
				req.Schema = SchemaOf(ctx)
			}
			res, err = e.client.Do(ctx, req)
			if err == nil {
				c.countDaily(ctx, e, now)
				u := res.Usage
				slog.Debug("llm ok", "provider", e.provider, "model", e.cfg.Model, "role", role,
					"attempt", attempt+1, "took", time.Since(start).Round(time.Millisecond),
					"prompt_tokens", u.PromptTokens, "cached_tokens", u.CachedTokens,
					"completion_tokens", u.CompletionTokens, "cost", u.Cost, "upstream", u.Upstream, "session", SessionOf(ctx))
				return res.Text, e.provider, nil
			}
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			if attempt >= c.retries || !transient(err) {
				break
			}
			slog.Info("llm transient error, retrying", "provider", e.provider, "model", e.cfg.Model,
				"role", role, "attempt", attempt+1, "delay", c.retryDelay, "err", err)
			if err := c.sleep(ctx, c.retryDelay); err != nil {
				return "", "", err
			}
		}
		until := c.block(e, err, c.now())
		slog.Warn("llm request failed, trying next provider",
			"provider", e.provider, "model", e.cfg.Model, "role", role, "blocked_until", until.Format(time.RFC3339), "err", err)
	}
	return "", "", ErrUnavailable
}

// transient reports whether a failed request is worth repeating right away on the same endpoint:
// server-side 5xx errors and connection failures. Timeouts are not retried — the request has
// already taken the whole timeout — nor are 4xx/429 and malformed responses.
func transient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) && !urlErr.Timeout()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *endpoint) day(now time.Time) string { return now.In(e.loc).Format("2006-01-02") }

func (c *Chain) dailyLimit(e *endpoint, prio Priority) int {
	if prio == Direct {
		return e.cfg.RPD
	}
	return int(math.Floor(float64(e.cfg.RPD) * (1 - c.reserve)))
}

func (c *Chain) allow(ctx context.Context, e *endpoint, prio Priority, now time.Time) (bool, string) {
	e.mu.Lock()
	if now.Before(e.blockedUntil) {
		e.mu.Unlock()
		return false, "blocked"
	}
	e.pruneWindow(now)
	if e.cfg.RPM > 0 && len(e.window) >= e.cfg.RPM {
		e.mu.Unlock()
		return false, "rpm"
	}
	e.mu.Unlock()

	if e.cfg.RPD > 0 {
		used, err := c.usage.QuotaGet(ctx, e.provider, e.cfg.Model, e.day(now))
		if err != nil {
			slog.Error("quota read failed", "err", err)
			return false, "quota-read-error"
		}
		if used >= c.dailyLimit(e, prio) {
			return false, "rpd"
		}
	}
	return true, ""
}

// recordAttempt counts a request against the per-minute window: providers count every request.
func (c *Chain) recordAttempt(e *endpoint, now time.Time) {
	e.mu.Lock()
	e.window = append(e.window, now)
	e.mu.Unlock()
}

// countDaily counts a successful request against the daily quota. Failed requests are not
// counted: providers do not charge them, and counting would push the bot offline early.
func (c *Chain) countDaily(ctx context.Context, e *endpoint, now time.Time) {
	if e.cfg.RPD > 0 {
		if err := c.usage.QuotaInc(ctx, e.provider, e.cfg.Model, e.day(now)); err != nil {
			slog.Error("quota write failed", "err", err)
		}
	}
}

// block takes the endpoint out of rotation after a failure and returns until when.
func (c *Chain) block(e *endpoint, err error, now time.Time) time.Time {
	var d time.Duration
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.RateLimited() && apiErr.Daily:
		d = nextMidnight(now, e.loc).Sub(now)
	case errors.As(err, &apiErr) && apiErr.RateLimited():
		d = apiErr.RetryAfter
		if d <= 0 {
			d = time.Minute
		}
	case errors.As(err, &apiErr) && apiErr.LimitSource == LimitKey:
		// The key's spending limit (set in OpenRouter) is spent; a daily limit resets at 00:00 UTC.
		// For weekly/monthly limits this means one probe a day.
		d = nextMidnight(now, time.UTC).Sub(now)
	case errors.As(err, &apiErr) && apiErr.LimitSource == LimitInFlight:
		d = apiErr.RetryAfter
		if d <= 0 {
			d = time.Minute
		}
	case errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500:
		d = 10 * time.Minute // bad request / auth / unknown model / no credits — back off hard
	default:
		d = 30 * time.Second // timeouts, 5xx, network
	}
	until := now.Add(d)
	e.mu.Lock()
	if until.After(e.blockedUntil) {
		e.blockedUntil = until
	}
	e.mu.Unlock()
	return until
}

func (e *endpoint) pruneWindow(now time.Time) {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(e.window) && !e.window[i].After(cut) {
		i++
	}
	e.window = e.window[i:]
}

func nextMidnight(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}
