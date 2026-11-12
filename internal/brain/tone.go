package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// The tone of a topic (SPEC 4.6): with each summary chunk, the filter model (in the background)
// rewrites a short description of how people talk in the topic — rough or polite, swearing,
// banter, to the point or chatty. It is shown to the model as a local custom of that topic only:
// the tone of one topic does not carry over to others, and the boundaries hold everywhere.

// toneMaxChars caps the description: it is a hint, not a study.
const toneMaxChars = 250

// updateTone refines the topic's tone with a summarized chunk. Topics are summarized one chunk at a
// time (summarize), so the tone of a topic is never updated concurrently.
func (b *Brain) updateTone(ctx context.Context, chatID int64, topic int, ms []storage.Message, r roster) {
	if !slices.ContainsFunc(ms, func(m storage.Message) bool { return !m.IsSelf && m.Kind == "" }) ||
		!b.llm.Available(ctx, llm.RoleTone, llm.Initiative) {
		return
	}
	sm, err := b.store.Summary(ctx, chatID, topic)
	if err != nil {
		slog.Error("load tone", "chat_id", chatID, "topic", topic, "err", err)
		return
	}
	raw, provider, err := b.llm.Complete(ctx, llm.RoleTone, llm.Initiative, tonePrompt(sm.Tone, ms, r))
	if err != nil {
		slog.Warn("tone not updated", "chat_id", chatID, "topic", topic, "err", err)
		return
	}
	tone, err := parseTone(raw)
	if err != nil {
		slog.Warn("tone not updated: unparsable answer", "chat_id", chatID, "topic", topic, "provider", provider, "err", err)
		return
	}
	tone = clip(tone, toneMaxChars)
	if tone == "" || tone == sm.Tone {
		slog.Debug("tone unchanged", "chat_id", chatID, "topic", topic, "provider", provider)
		return
	}
	ok, err := b.store.SetTone(ctx, chatID, topic, sm.Tone, tone)
	switch {
	case err != nil:
		slog.Error("save tone", "chat_id", chatID, "topic", topic, "err", err)
		return
	case !ok:
		slog.Info("tone not updated: changed meanwhile (erased by the admin?)", "chat_id", chatID, "topic", topic)
		return
	}
	slog.Info("tone updated", "chat_id", chatID, "topic", topic, "tone", tone, "provider", provider)
}

func parseTone(s string) (string, error) {
	raw := jsonObjRe.FindString(s)
	if raw == "" {
		return "", fmt.Errorf("no JSON object in %q", s)
	}
	var wrap struct {
		Tone *string `json:"tone"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return "", fmt.Errorf("bad tone JSON %q: %w", raw, err)
	}
	if wrap.Tone == nil {
		return "", fmt.Errorf("no tone in %q", raw)
	}
	return strings.TrimSpace(*wrap.Tone), nil
}

// tonePrompt asks the model how people talk in the topic, given what it thought before.
func tonePrompt(prev string, ms []storage.Message, r roster) []llm.Message {
	sys := `Ты помогаешь боту — участнику группового чата в Telegram — перенять манеру общения, принятую в одном топике чата ` +
		`(его сообщения помечены «Ты»; их не учитывай — смотри, как пишут люди). Опиши тон топика: как здесь принято общаться.
Что важно:
- жёстко или вежливо: мат, грубые шутки, подколки и стёб друг над другом — норма здесь или нет;
- строго по делу или болтовня и флуд; серьёзно или с юмором, с сарказмом;
- как пишут: коротко или развёрнуто, со смайликами или без, на «ты» или на «вы».
Описывай манеру, а не темы и не людей: без имён и пересказа разговоров. Только то, что видно у многих, а не у одного человека ` +
		`в одном сообщении. Прежнее описание сделано по более ранней переписке: уточни его новыми сообщениями, а не начинай заново; ` +
		`меняй, только если манера и правда другая. Если по сообщениям манеру не понять, а прежнего описания нет — верни пустую строку.
` + fmt.Sprintf("Одно-два предложения, не длиннее %d символов, обычным текстом, на языке переписки. ", toneMaxChars) +
		`Ответь строго одним JSON-объектом, без текста вокруг:
{"tone": "<описание>"}`

	var u strings.Builder
	u.WriteString("Прежнее описание тона:\n")
	if strings.TrimSpace(prev) == "" {
		u.WriteString("(нет)\n")
	} else {
		u.WriteString(strings.TrimSpace(prev))
		u.WriteByte('\n')
	}
	u.WriteString("\nНовые сообщения:\n")
	for _, m := range ms {
		u.WriteString(formatLine(m, r))
		u.WriteByte('\n')
	}
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}
