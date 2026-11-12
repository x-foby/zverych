package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
)

type memUsage struct {
	mu      sync.Mutex
	m       map[string]int
	gets    int
	failGet error
}

func newUsage() *memUsage { return &memUsage{m: map[string]int{}} }

func (u *memUsage) key(p, m, d string) string { return p + "|" + m + "|" + d }
func (u *memUsage) QuotaGet(_ context.Context, p, m, d string) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gets++
	if u.failGet != nil {
		return 0, u.failGet
	}
	return u.m[u.key(p, m, d)], nil
}
func (u *memUsage) QuotaInc(_ context.Context, p, m, d string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.m[u.key(p, m, d)]++
	return nil
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

const geminiDaily429 = `[{"error":{"code":429,"message":"You exceeded your current quota","status":"RESOURCE_EXHAUSTED",
"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]},
{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}]`

const okBody = `{"choices":[{"message":{"content":"привет"}}]}`

func server(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func chain(t *testing.T, usage Usage, reserve float64, providers ...config.Provider) (*Chain, *clock) {
	t.Helper()
	cfg := &config.Config{}
	cfg.LLM.TimeoutSeconds = 5
	cfg.Quota.ReserveForMentions = reserve
	cfg.LLM.Providers = providers
	ch, err := NewChain(cfg, usage)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	ch.now = clk.Now
	return ch, clk
}

// ---- client ----

func TestClientRequest(t *testing.T) {
	type seen struct {
		auth, ctype string
		body        map[string]any
	}
	var got []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Errorf("bad request body %s", raw)
		}
		got = append(got, seen{r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		w.Write([]byte(okBody))
	}))
	defer s.Close()

	temp := 0.3
	c := &Client{BaseURL: s.URL + "/", APIKey: "secret", HTTP: s.Client()}
	text, err := c.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp, MaxTokens: 1, Extra: map[string]any{"reasoning_effort": "none"}, Session: "-100:7",
		Schema: &Schema{Name: "answer", Schema: map[string]any{"type": "object"}}})
	if err != nil || text != "привет" {
		t.Fatalf("got %q %v", text, err)
	}
	c.APIKey = ""
	if _, err := c.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}

	full := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": 0.3, "max_tokens": 1.0, "reasoning_effort": "none", "session_id": "-100:7",
		"response_format": map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "answer", "strict": true, "schema": map[string]any{"type": "object"}}}}
	if got[0].auth != "Bearer secret" || got[0].ctype != "application/json" || !reflect.DeepEqual(got[0].body, full) {
		t.Errorf("full request: %+v", got[0])
	}
	if got[1].auth != "" || !reflect.DeepEqual(got[1].body, map[string]any{"model": "m", "messages": nil}) {
		t.Errorf("minimal request must omit auth, temperature, max_tokens, session and schema: %+v", got[1])
	}
}

func TestClientUsage(t *testing.T) {
	// OpenRouter's shape: usage with cost and cached tokens, and the provider it routed to.
	s := server(t, 200, `{"provider":"NextBit","choices":[{"message":{"content":"ок"}}],
		"usage":{"prompt_tokens":1700,"completion_tokens":50,"cost":0.00013,
		"prompt_tokens_details":{"cached_tokens":1200}}}`)
	res, err := (&Client{BaseURL: s.URL, HTTP: s.Client()}).Do(context.Background(), Request{Model: "m"})
	want := Result{Text: "ок", Usage: TokenUsage{PromptTokens: 1700, CompletionTokens: 50, CachedTokens: 1200,
		Cost: 0.00013, Upstream: "NextBit"}}
	if err != nil || res != want {
		t.Fatalf("got %+v %v, want %+v", res, err, want)
	}
	// llama-server's shape: no cost, no provider.
	s = server(t, 200, `{"choices":[{"message":{"content":"ок"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	res, err = (&Client{BaseURL: s.URL, HTTP: s.Client()}).Do(context.Background(), Request{Model: "m"})
	if err != nil || res.Usage != (TokenUsage{PromptTokens: 10, CompletionTokens: 2}) {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestClientBadResponses(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{
		{200, `{"choices":[]}`},
		{200, `{"choices":[{"message":{"content":null}}]}`},
		{200, `{"choices":[{"message":{"content":"  \n"}}]}`},
		{200, `not json`},
		{500, `oops`},
	} {
		s := server(t, c.status, c.body)
		_, err := (&Client{BaseURL: s.URL, HTTP: s.Client()}).Complete(context.Background(), Request{Model: "m"})
		if err == nil {
			t.Errorf("%d %s: want error", c.status, c.body)
		}
		var ae *APIError
		if isAPI := errors.As(err, &ae); isAPI != (c.status != 200) || (isAPI && (ae.RateLimited() || ae.Status != c.status)) {
			t.Errorf("%d %s: got %#v", c.status, c.body, err)
		}
	}
}

func TestParse429(t *testing.T) {
	s := server(t, 429, geminiDaily429)
	c := &Client{BaseURL: s.URL + "/", HTTP: s.Client()}
	_, err := c.Complete(context.Background(), Request{Model: "m"})
	var ae *APIError
	if !errors.As(err, &ae) || !ae.RateLimited() || !ae.Daily || ae.RetryAfter != 37*time.Second {
		t.Fatalf("got %#v", err)
	}
}

func TestNewAPIError(t *testing.T) {
	cases := []struct {
		status int
		header string
		body   string
		retry  time.Duration
		daily  bool
	}{
		{429, "12", "", 12 * time.Second, false},
		{429, "1", "", time.Second, false},
		{429, " 7 ", `"retryDelay": "3s"`, 7 * time.Second, false},
		{429, "0", `"retryDelay": "1.5s"`, 1500 * time.Millisecond, false},
		{429, "-5", "", 0, false},
		{429, "Wed, 21 Oct 2026 07:28:00 GMT", `"retryDelay":"3s"`, 3 * time.Second, false},
		{429, "", `{"error":"Rate limit exceeded: free-models-per-day"}`, 0, true},
		{429, "", `limit per day reached`, 0, true},
		{429, "", `Rate limit exceeded: requests per minute`, 0, false},
		{500, "", `PerDay`, 0, false},
	}
	for _, c := range cases {
		e := newAPIError(c.status, c.header, []byte(c.body))
		if e.Status != c.status || e.RetryAfter != c.retry || e.Daily != c.daily || e.Body != c.body {
			t.Errorf("newAPIError(%d, %q, %q) = %+v", c.status, c.header, c.body, e)
		}
	}
}

func TestNewAPIErrorLimitSource(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{402, `{"error":{"code":402,"message":"Key limit exceeded","metadata":{"limit_source":"openrouter_key_limit"}}}`, LimitKey},
		{403, `{"error":{"code":403,"metadata":{"limit_source":"openrouter_key_limit"}}}`, LimitKey},
		{402, `{"error":{"code":402,"metadata":{"limit_source":"openrouter_credits"}}}`, "openrouter_credits"},
		{402, `{"error":{"code":402,"message":"Insufficient credits"}}`, ""},
		{402, `not json`, ""},
		{429, `{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`, ""},
		{400, `{"error":{"metadata":{"limit_source":"openrouter_key_limit"}}}`, ""},
	}
	for _, c := range cases {
		if got := newAPIError(c.status, "", []byte(c.body)).LimitSource; got != c.want {
			t.Errorf("newAPIError(%d, %q).LimitSource = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	exact := strings.Repeat("a", 300)
	if got := (&APIError{Status: 400, Body: exact}).Error(); got != "llm http 400: "+exact {
		t.Errorf("300-byte body must not be truncated: %q", got)
	}
	if got := (&APIError{Status: 400, Body: exact + "b"}).Error(); got != "llm http 400: "+exact+"…" {
		t.Errorf("long body must be truncated: %q", got)
	}
	if got := (&APIError{Status: 502, Body: "bad gateway"}).Error(); got != "llm http 502: bad gateway" {
		t.Errorf("short body: %q", got)
	}
}

// ---- chain ----

func TestChainFallbackAndBlock(t *testing.T) {
	bad := server(t, 429, geminiDaily429)
	good := server(t, 200, okBody)
	ch, _ := chain(t, newUsage(), 0.5,
		config.Provider{Name: "a", BaseURL: bad.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPM: 10, RPD: 100}},
		config.Provider{Name: "b", BaseURL: good.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "y", RPM: 10, RPD: 4}},
	)
	ctx := context.Background()
	text, prov, err := ch.Complete(ctx, RoleReply, Direct, nil)
	if err != nil || prov != "b" || text != "привет" {
		t.Fatalf("got %q %q %v", text, prov, err)
	}
	// "a" is blocked until midnight; "b" has RPD 4 with 50% reserve: 2 initiative requests total.
	if _, prov, _ := ch.Complete(ctx, RoleReply, Initiative, nil); prov != "b" {
		t.Fatalf("second initiative call should use b, got %q", prov)
	}
	if ch.Available(ctx, RoleReply, Initiative) {
		t.Fatal("initiative must not dip into the reserve")
	}
	if !ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("direct requests may use the reserve")
	}
	ch.Complete(ctx, RoleReply, Direct, nil)
	ch.Complete(ctx, RoleReply, Direct, nil)
	if _, _, err := ch.Complete(ctx, RoleReply, Direct, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	// Filter role falls back to reply models when no filter model is configured.
	if ch.Available(ctx, RoleFilter, Direct) {
		t.Fatal("filter should share exhausted reply quota")
	}
}

func TestChainFallsBackOnNetworkError(t *testing.T) {
	dead := server(t, 200, okBody)
	dead.Close()
	good := server(t, 200, okBody)
	ch, clk := chain(t, newUsage(), 0,
		config.Provider{Name: "dead", BaseURL: dead.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x"}},
		config.Provider{Name: "good", BaseURL: good.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "y"}},
	)
	if _, prov, err := ch.Complete(context.Background(), RoleReply, Direct, nil); err != nil || prov != "good" {
		t.Fatalf("got %q %v", prov, err)
	}
	if want := clk.Now().Add(30 * time.Second); !ch.replies[0].blockedUntil.Equal(want) {
		t.Fatalf("network error should block for 30s, blocked until %v", ch.replies[0].blockedUntil)
	}
}

func TestChainCancelledContext(t *testing.T) {
	s := server(t, 200, okBody)
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ch.Complete(ctx, RoleReply, Direct, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !ch.Available(context.Background(), RoleReply, Direct) {
		t.Fatal("a cancelled request must not block the provider")
	}
}

func TestChainUsesDedicatedFilterModel(t *testing.T) {
	var mu sync.Mutex
	var models []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		models = append(models, b.Model)
		mu.Unlock()
		w.Write([]byte(okBody))
	}))
	defer s.Close()
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC",
			Filter: &config.ModelCfg{Model: "lite"}, Reply: &config.ModelCfg{Model: "flash"}},
		config.Provider{Name: "b", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "other"}},
	)
	ctx := context.Background()
	ch.Complete(ctx, RoleFilter, Initiative, nil)
	ch.Complete(ctx, RoleReply, Initiative, nil)
	ch.Complete(ctx, RoleCommand, Direct, nil)
	ch.Complete(ctx, RoleThread, Direct, nil)
	ch.Complete(ctx, RoleSummary, Initiative, nil)
	ch.Complete(ctx, RoleNotes, Initiative, nil)
	ch.Complete(ctx, RoleLore, Initiative, nil)
	ch.Complete(ctx, RoleTone, Initiative, nil)
	ch.Complete(ctx, RoleLoreSelect, Initiative, nil)
	if !reflect.DeepEqual(models, []string{"lite", "flash", "lite", "lite", "lite", "lite", "lite", "lite", "lite"}) {
		t.Fatalf("models used: %v", models)
	}
	if len(ch.filters) != 1 || len(ch.replies) != 2 {
		t.Fatalf("filters %d replies %d", len(ch.filters), len(ch.replies))
	}
}

func TestFilterFallsBackToReplyModels(t *testing.T) {
	s := server(t, 200, okBody)
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "flash"}})
	ctx := context.Background()
	if !ch.Available(ctx, RoleFilter, Initiative) {
		t.Fatal("without a filter model the reply model serves the filter")
	}
	if _, prov, err := ch.Complete(ctx, RoleFilter, Initiative, nil); err != nil || prov != "a" {
		t.Fatalf("got %q %v", prov, err)
	}
}

func TestRPMWindow(t *testing.T) {
	s := server(t, 200, okBody)
	usage := newUsage()
	ch, clk := chain(t, usage, 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPM: 2}})
	ctx := context.Background()
	t0 := clk.Now()
	ch.Complete(ctx, RoleReply, Direct, nil)
	clk.Advance(10 * time.Second)
	ch.Complete(ctx, RoleReply, Direct, nil)
	if ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("RPM 2 is used up")
	}
	clk.t = t0.Add(59 * time.Second)
	if ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("first request is still inside the minute window")
	}
	clk.t = t0.Add(time.Minute)
	if !ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("first request left the window after a minute")
	}
	if usage.gets != 0 || len(usage.m) != 0 {
		t.Fatalf("RPD 0 must not touch daily counters: gets=%d m=%v", usage.gets, usage.m)
	}
}

func TestUnlimitedModel(t *testing.T) {
	s := server(t, 200, okBody)
	usage := newUsage()
	ch, _ := chain(t, usage, 0.5,
		config.Provider{Name: "local", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "qwen"}})
	for i := range 20 {
		if _, _, err := ch.Complete(context.Background(), RoleReply, Initiative, nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if usage.gets != 0 || len(usage.m) != 0 {
		t.Fatalf("RPM/RPD 0 means unlimited and untracked: gets=%d m=%v", usage.gets, usage.m)
	}
}

func TestDailyQuotaResetsInProviderTZ(t *testing.T) {
	s := server(t, 200, okBody)
	usage := newUsage()
	ch, clk := chain(t, usage, 0,
		config.Provider{Name: "g", BaseURL: s.URL, DailyResetTZ: "America/Los_Angeles", Reply: &config.ModelCfg{Model: "x", RPD: 1}})
	ctx := context.Background()
	clk.t = time.Date(2026, 11, 3, 7, 59, 0, 0, time.UTC) // 23:59 PST, Nov 2
	if _, _, err := ch.Complete(ctx, RoleReply, Direct, nil); err != nil {
		t.Fatal(err)
	}
	if usage.m["g|x|2026-11-02"] != 1 {
		t.Fatalf("usage must be counted in the provider's day: %v", usage.m)
	}
	if ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("RPD 1 is used up")
	}
	clk.Advance(time.Minute)
	if !ch.Available(ctx, RoleReply, Direct) {
		t.Fatal("quota must reset at Pacific midnight")
	}
}

func TestQuotaReadErrorMeansUnavailable(t *testing.T) {
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.Write([]byte(okBody)) }))
	defer s.Close()
	usage := newUsage()
	usage.failGet = errors.New("db locked")
	ch, _ := chain(t, usage, 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPD: 10}})
	if ch.Available(context.Background(), RoleReply, Direct) {
		t.Fatal("unknown usage must be treated as unavailable")
	}
	if _, _, err := ch.Complete(context.Background(), RoleReply, Direct, nil); !errors.Is(err, ErrUnavailable) || calls != 0 {
		t.Fatalf("err %v, http calls %d", err, calls)
	}
}

func TestDailyLimit(t *testing.T) {
	for _, c := range []struct {
		rpd     int
		reserve float64
		prio    Priority
		want    int
	}{
		{5, 0.5, Direct, 5},
		{5, 0.5, Initiative, 2},
		{3, 0.2, Initiative, 2},
		{10, 0, Initiative, 10},
	} {
		ch := &Chain{reserve: c.reserve}
		if got := ch.dailyLimit(&endpoint{cfg: config.ModelCfg{RPD: c.rpd}}, c.prio); got != c.want {
			t.Errorf("dailyLimit(rpd=%d reserve=%v prio=%v)=%d want %d", c.rpd, c.reserve, c.prio, got, c.want)
		}
	}
}

func TestBlock(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) // 02:00 PST
	midnight := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		want time.Time
	}{
		{"daily 429", &APIError{Status: 429, Daily: true, RetryAfter: 37 * time.Second}, midnight},
		{"minute 429 with retry", &APIError{Status: 429, RetryAfter: 37 * time.Second}, now.Add(37 * time.Second)},
		{"minute 429 without retry", &APIError{Status: 429}, now.Add(time.Minute)},
		{"400", &APIError{Status: 400}, now.Add(10 * time.Minute)},
		{"402 no credits", &APIError{Status: 402, LimitSource: "openrouter_credits"}, now.Add(10 * time.Minute)},
		{"402 key limit", &APIError{Status: 402, LimitSource: LimitKey}, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		{"403 key limit", &APIError{Status: 403, LimitSource: LimitKey}, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		{"402 in flight with retry", &APIError{Status: 402, LimitSource: LimitInFlight, RetryAfter: 5 * time.Second}, now.Add(5 * time.Second)},
		{"402 in flight without retry", &APIError{Status: 402, LimitSource: LimitInFlight}, now.Add(time.Minute)},
		{"499", &APIError{Status: 499}, now.Add(10 * time.Minute)},
		{"500", &APIError{Status: 500}, now.Add(30 * time.Second)},
		{"399", &APIError{Status: 399}, now.Add(30 * time.Second)},
		{"network", errors.New("connection reset"), now.Add(30 * time.Second)},
	}
	for _, c := range cases {
		e := &endpoint{loc: la}
		if got := (&Chain{}).block(e, c.err, now); !got.Equal(c.want) || !e.blockedUntil.Equal(c.want) {
			t.Errorf("%s: until %v (stored %v), want %v", c.name, got, e.blockedUntil, c.want)
		}
	}

	e := &endpoint{loc: la}
	(&Chain{}).block(e, &APIError{Status: 429, Daily: true}, now)
	if got := (&Chain{}).block(e, &APIError{Status: 500}, now); !got.Equal(now.Add(30*time.Second)) || !e.blockedUntil.Equal(midnight) {
		t.Fatalf("a shorter block must not shorten a longer one: got %v stored %v", got, e.blockedUntil)
	}
}

func TestNextMidnightAcrossDST(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC) // Oct 31, 22:00 PDT
	if got, want := nextMidnight(now, la), time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNewChain(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.TimeoutSeconds = 7
	cfg.LLM.Providers = []config.Provider{{Name: "a", BaseURL: "http://x", DailyResetTZ: "Mars/Olympus", Reply: &config.ModelCfg{}}}
	if _, err := NewChain(cfg, newUsage()); err == nil || !strings.Contains(err.Error(), "provider a") {
		t.Fatalf("bad time zone must fail, got %v", err)
	}
	cfg.LLM.Providers[0].DailyResetTZ = "UTC"
	cfg.LLM.Providers[0].APIKey = "k"
	ch, err := NewChain(cfg, newUsage())
	if err != nil {
		t.Fatal(err)
	}
	if c := ch.replies[0].client; c.HTTP.Timeout != 7*time.Second || c.APIKey != "k" || c.BaseURL != "http://x" {
		t.Fatalf("client %+v timeout %v", c, c.HTTP.Timeout)
	}
}

func TestRoleString(t *testing.T) {
	if RoleFilter.String() != "filter" || RoleReply.String() != "reply" || RoleCommand.String() != "command" || RoleThread.String() != "thread" ||
		RoleSummary.String() != "summary" || RoleNotes.String() != "notes" || RoleLore.String() != "lore" ||
		RoleTone.String() != "tone" || RoleLoreSelect.String() != "lore_select" {
		t.Fatal("role names")
	}
}

func TestClientRequestBuildErrors(t *testing.T) {
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.Write([]byte(okBody)) }))
	defer s.Close()
	ctx := context.Background()
	if _, err := (&Client{BaseURL: s.URL, HTTP: s.Client()}).Complete(ctx, Request{Model: "m", Extra: map[string]any{"bad": func() {}}}); err == nil {
		t.Error("an unencodable extra field must fail before sending")
	}
	if _, err := (&Client{BaseURL: "://no-scheme", HTTP: s.Client()}).Complete(ctx, Request{Model: "m"}); err == nil {
		t.Error("a malformed base_url must fail")
	}
	if calls != 0 {
		t.Fatalf("nothing should reach the server, got %d calls", calls)
	}
}

func TestRPMOne(t *testing.T) {
	s := server(t, 200, okBody)
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPM: 1}})
	ch.Complete(context.Background(), RoleReply, Direct, nil)
	if ch.Available(context.Background(), RoleReply, Direct) {
		t.Fatal("RPM 1 allows a single request per minute")
	}
}

// ---- retries ----

// flaky serves the given statuses in order (then 200 OK) and counts requests.
func flaky(t *testing.T, statuses ...int) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		if i < len(statuses) {
			w.WriteHeader(statuses[i])
			w.Write([]byte(`{"error":"busy"}`))
			return
		}
		w.Write([]byte(okBody))
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// withRetries enables retries and replaces sleeping with recording the requested pauses.
func withRetries(ch *Chain, n int) *[]time.Duration {
	var slept []time.Duration
	ch.retries, ch.retryDelay = n, 3*time.Second
	ch.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return &slept
}

func TestChainRetriesTransientErrors(t *testing.T) {
	s, calls := flaky(t, 503, 502)
	usage := newUsage()
	ch, _ := chain(t, usage, 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPD: 100}})
	slept := withRetries(ch, 2)
	text, prov, err := ch.Complete(context.Background(), RoleReply, Direct, nil)
	if err != nil || prov != "a" || text != "привет" {
		t.Fatalf("got %q %q %v", text, prov, err)
	}
	if *calls != 3 || !reflect.DeepEqual(*slept, []time.Duration{3 * time.Second, 3 * time.Second}) {
		t.Fatalf("calls=%d slept=%v, want 3 calls with two 3s pauses", *calls, *slept)
	}
	if !ch.replies[0].blockedUntil.IsZero() {
		t.Fatal("a request that finally succeeded must not block the provider")
	}
	if got, _ := usage.QuotaGet(context.Background(), "a", "x", "2026-11-02"); got != 1 {
		t.Fatalf("only the successful attempt counts against the daily quota, got %d", got)
	}
}

func TestChainRetriesExhaustedFallsBack(t *testing.T) {
	bad := server(t, 500, `{"error":"boom"}`)
	good := server(t, 200, okBody)
	usage := newUsage()
	ch, clk := chain(t, usage, 0,
		config.Provider{Name: "bad", BaseURL: bad.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPD: 100}},
		config.Provider{Name: "good", BaseURL: good.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "y"}})
	slept := withRetries(ch, 2)
	if _, prov, err := ch.Complete(context.Background(), RoleReply, Direct, nil); err != nil || prov != "good" {
		t.Fatalf("got %q %v", prov, err)
	}
	if len(*slept) != 2 || len(ch.replies[0].window) != 3 {
		t.Fatalf("want 3 attempts on the failing provider, slept=%v window=%d", *slept, len(ch.replies[0].window))
	}
	if want := clk.Now().Add(30 * time.Second); !ch.replies[0].blockedUntil.Equal(want) {
		t.Fatalf("after the last retry the provider is blocked for 30s, got %v", ch.replies[0].blockedUntil)
	}
	if got, _ := usage.QuotaGet(context.Background(), "bad", "x", "2026-11-02"); got != 0 {
		t.Fatalf("failed requests must not count against the daily quota, got %d", got)
	}
}

func TestChainRetriesNetworkError(t *testing.T) {
	dead := server(t, 200, okBody)
	dead.Close()
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "dead", BaseURL: dead.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x"}})
	slept := withRetries(ch, 1)
	if _, _, err := ch.Complete(context.Background(), RoleReply, Direct, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if len(*slept) != 1 {
		t.Fatalf("connection refused is transient: want one retry, slept=%v", *slept)
	}
}

func TestChainDoesNotRetryPermanentErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", 400, `{"error":"bad"}`},
		{"rate limited", 429, `{"error":"slow down"}`},
		{"not implemented", 501, `{"error":"no"}`},
		{"empty completion", 200, `{"choices":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := server(t, c.status, c.body)
			ch, _ := chain(t, newUsage(), 0,
				config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x"}})
			slept := withRetries(ch, 3)
			if _, _, err := ch.Complete(context.Background(), RoleReply, Direct, nil); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
			if len(*slept) != 0 || len(ch.replies[0].window) != 1 {
				t.Fatalf("must not retry: slept=%v attempts=%d", *slept, len(ch.replies[0].window))
			}
		})
	}
}

func TestChainRetryRespectsRPM(t *testing.T) {
	s, calls := flaky(t, 503)
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x", RPM: 1}})
	withRetries(ch, 2)
	if _, _, err := ch.Complete(context.Background(), RoleReply, Direct, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("a retry must not exceed RPM 1, calls=%d", *calls)
	}
}

func TestChainRetryCancelledDuringPause(t *testing.T) {
	s, calls := flaky(t, 503)
	ch, _ := chain(t, newUsage(), 0,
		config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "x"}})
	ctx, cancel := context.WithCancel(context.Background())
	withRetries(ch, 2)
	ch.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if _, _, err := ch.Complete(ctx, RoleReply, Direct, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if *calls != 1 || !ch.replies[0].blockedUntil.IsZero() {
		t.Fatalf("shutdown during the pause: no more attempts and no block; calls=%d blocked=%v", *calls, ch.replies[0].blockedUntil)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestTransient(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"500", &APIError{Status: 500}, true},
		{"502", &APIError{Status: 502}, true},
		{"503", &APIError{Status: 503}, true},
		{"504", &APIError{Status: 504}, true},
		{"501", &APIError{Status: 501}, false},
		{"429", &APIError{Status: 429}, false},
		{"404", &APIError{Status: 404}, false},
		{"connection refused", &url.Error{Op: "Post", URL: "x", Err: errors.New("connect: connection refused")}, true},
		{"timeout", &url.Error{Op: "Post", URL: "x", Err: timeoutErr{}}, false},
		{"decode", errors.New("decode response: bad json"), false},
	} {
		if got := transient(c.err); got != c.want {
			t.Errorf("%s: transient=%v want %v", c.name, got, c.want)
		}
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("plain sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("cancelled sleep must return at once: %v", err)
	}
}

func TestChainPassesSession(t *testing.T) {
	var mu sync.Mutex
	var sessions []any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		sessions = append(sessions, b["session_id"])
		mu.Unlock()
		w.Write([]byte(okBody))
	}))
	defer s.Close()
	ch, _ := chain(t, newUsage(), 0, config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC", Reply: &config.ModelCfg{Model: "m"}})
	ch.Complete(WithSession(context.Background(), "-100:7"), RoleReply, Direct, nil)
	ch.Complete(context.Background(), RoleReply, Direct, nil)
	if !reflect.DeepEqual(sessions, []any{"-100:7", nil}) {
		t.Fatalf("sessions sent: %v", sessions)
	}
}

// The schema goes only to the models whose config enables structured outputs (SPEC 7).
func TestChainPassesSchemaWhereEnabled(t *testing.T) {
	var mu sync.Mutex
	formats := map[string]any{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		formats[b["model"].(string)] = b["response_format"]
		mu.Unlock()
		w.Write([]byte(okBody))
	}))
	defer s.Close()
	ch, _ := chain(t, newUsage(), 0, config.Provider{Name: "a", BaseURL: s.URL, DailyResetTZ: "UTC",
		Filter: &config.ModelCfg{Model: "strict", StructuredOutputs: true}, Reply: &config.ModelCfg{Model: "plain"}})
	schema := &Schema{Name: "answer", Schema: map[string]any{"type": "object"}}
	ctx := WithSchema(context.Background(), schema)
	ch.Complete(ctx, RoleFilter, Direct, nil)
	ch.Complete(ctx, RoleReply, Direct, nil)
	if formats["strict"] == nil {
		t.Errorf("a model with structured_outputs must get the schema: %v", formats)
	}
	if f, sent := formats["plain"]; !sent || f != nil {
		t.Errorf("a model without structured_outputs must not get it: %v", formats)
	}
	if SchemaOf(context.Background()) != nil || SchemaOf(ctx) != schema {
		t.Error("SchemaOf must return what WithSchema set, nil without it")
	}
}
