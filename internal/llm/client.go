// Package llm talks to OpenAI-compatible chat completion endpoints
// (OpenRouter, llama.cpp server, Gemini, Groq, Ollama) and tracks quotas.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	Role    string   `json:"role"`
	Content string   `json:"-"`
	Images  []string `json:"-"`
}

// MarshalJSON preserves text-only requests and uses content parts for vision requests.
func (m Message) MarshalJSON() ([]byte, error) {
	var content any = m.Content
	if len(m.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": m.Content}}
		for _, url := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		}
		content = parts
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{m.Role, content})
}

type Request struct {
	Model       string
	Messages    []Message
	Temperature *float64
	MaxTokens   int
	Extra       map[string]any
	// Session pins related requests to the provider that already has their prompt cached
	// (OpenRouter's session_id, SPEC 7a). Empty: not sent.
	Session string
	// Schema, if set, is sent as response_format: the provider constrains the answer to it.
	Schema *Schema
}

// Schema is the JSON schema of an answer (structured outputs, SPEC 7). Strict mode: every
// property of an object is required and no other is allowed — the subset providers support alike.
type Schema struct {
	Name   string
	Schema map[string]any
}

type schemaKey struct{}

// WithSchema asks for the answers of requests made with ctx to follow s. It is sent only to the
// models whose config enables structured_outputs; the others get the prompt's own instructions.
func WithSchema(ctx context.Context, s *Schema) context.Context {
	return context.WithValue(ctx, schemaKey{}, s)
}

// SchemaOf returns the schema set by WithSchema, or nil.
func SchemaOf(ctx context.Context) *Schema {
	s, _ := ctx.Value(schemaKey{}).(*Schema)
	return s
}

type sessionKey struct{}

// WithSession marks requests made with ctx as one session (a thread of the chat, SPEC 7a).
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionOf returns the session set by WithSession, or "".
func SessionOf(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// APIError is a non-2xx response from the provider.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration // 0 if unknown
	Daily      bool          // the exhausted limit is a per-day one
	// LimitSource is OpenRouter's error.metadata.limit_source on 402/403 budget errors:
	// "openrouter_credits" (account balance), "openrouter_key_limit" (the key's spending limit),
	// "openrouter_in_flight_budget" (too many concurrent paid requests; comes with Retry-After).
	LimitSource string
}

// OpenRouter budget error sources (error.metadata.limit_source).
const (
	LimitKey      = "openrouter_key_limit"
	LimitInFlight = "openrouter_in_flight_budget"
)

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("llm http %d: %s", e.Status, body)
}

func (e *APIError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// TokenUsage is what a request cost, as reported by the server (zero values if it did not say).
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int     // prompt tokens served from the provider's prompt cache
	Cost             float64 // in credits (USD); OpenRouter reports it, llama-server does not
	Upstream         string  // the provider OpenRouter routed the request to
}

// Result is a successful completion.
type Result struct {
	Text  string
	Usage TokenUsage
}

// Complete returns just the text of the completion.
func (c *Client) Complete(ctx context.Context, r Request) (string, error) {
	res, err := c.Do(ctx, r)
	return res.Text, err
}

// Do runs the request and returns the text with the usage the server reported.
func (c *Client) Do(ctx context.Context, r Request) (Result, error) {
	body := map[string]any{"model": r.Model, "messages": r.Messages}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if r.Session != "" {
		body["session_id"] = r.Session
	}
	if r.Schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": r.Schema.Name, "strict": true, "schema": r.Schema.Schema}}
	}
	for k, v := range r.Extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, newAPIError(resp.StatusCode, resp.Header.Get("Retry-After"), respBody)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Provider string `json:"provider"`
		Usage    struct {
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			Cost                float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Result{}, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == nil ||
		strings.TrimSpace(*parsed.Choices[0].Message.Content) == "" {
		return Result{}, fmt.Errorf("empty completion: %.300s", respBody)
	}
	u := parsed.Usage
	return Result{Text: *parsed.Choices[0].Message.Content, Usage: TokenUsage{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		CachedTokens: u.PromptTokensDetails.CachedTokens, Cost: u.Cost, Upstream: parsed.Provider,
	}}, nil
}

var retryDelayRe = regexp.MustCompile(`"retryDelay"\s*:\s*"([\d.]+)s"`)

func newAPIError(status int, retryAfterHeader string, body []byte) *APIError {
	e := &APIError{Status: status, Body: string(body)}
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfterHeader)); err == nil && secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	} else if m := retryDelayRe.FindSubmatch(body); m != nil {
		if f, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
			e.RetryAfter = time.Duration(f * float64(time.Second))
		}
	}
	if status == http.StatusPaymentRequired || status == http.StatusForbidden {
		var parsed struct {
			Error struct {
				Metadata struct {
					LimitSource string `json:"limit_source"`
				} `json:"metadata"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			e.LimitSource = parsed.Error.Metadata.LimitSource
		}
	}
	if status == http.StatusTooManyRequests {
		lower := strings.ToLower(e.Body)
		// Gemini: quotaId "...PerDay..."; OpenRouter: "free-models-per-day".
		e.Daily = strings.Contains(lower, "perday") || strings.Contains(lower, "per-day") ||
			strings.Contains(lower, "per day")
	}
	return e
}
