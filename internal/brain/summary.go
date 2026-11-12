package brain

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// The running summary of a topic's older messages (SPEC 4.5). The model sees a topic's last
// context_messages as they are; older ones it knows from the summary. Once summary_chunk_messages
// of them pile up, the filter model rewrites the summary with them, in the background. Messages
// about to be pruned (retention_days) are summarized even as a partial chunk.

// maxSummaryChunks caps the chunks summarized in one go (the first run over an old history).
const maxSummaryChunks = 10

// summarizeTopics updates the summaries of the given topics in the background.
func (b *Brain) summarizeTopics(ctx context.Context, chatID int64, topics []int) {
	ctx = context.WithoutCancel(ctx)
	for _, t := range topics {
		b.spawn(func() { b.summarize(ctx, chatID, t) })
	}
}

// summarizeExpiring summarizes the messages that are pruned within a day (or already due).
func (b *Brain) summarizeExpiring(ctx context.Context) {
	ts, err := b.store.ExpiringTopics(ctx, b.expiring())
	if err != nil {
		slog.Error("expiring topics", "err", err)
		return
	}
	for _, t := range ts {
		b.summarize(ctx, t.ChatID, t.Topic)
	}
}

// expiring: messages sent before it are pruned within a day.
func (b *Brain) expiring() time.Time {
	return b.now().Add(-time.Duration(b.cfg.RetentionDays)*24*time.Hour + 24*time.Hour)
}

// summarize folds the topic's messages that left the model's window into its summary, a chunk at a time.
func (b *Brain) summarize(ctx context.Context, chatID int64, topic int) {
	k := topicKey{chatID, topic}
	b.mu.Lock()
	if b.summarizing[k] {
		b.mu.Unlock()
		return
	}
	b.summarizing[k] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.summarizing, k)
		b.mu.Unlock()
	}()

	chunk := b.cfg.SummaryChunkMessages
	for range maxSummaryChunks {
		prev, err := b.store.Summary(ctx, chatID, topic)
		if err != nil {
			slog.Error("load summary", "chat_id", chatID, "topic", topic, "err", err)
			return
		}
		expiring := b.expiring()
		ms, err := b.store.Unsummarized(ctx, chatID, topic, prev.UpTo, b.cfg.ContextMessages, chunk, expiring)
		if err != nil {
			slog.Error("load messages to summarize", "chat_id", chatID, "topic", topic, "err", err)
			return
		}
		if len(ms) == 0 || len(ms) < chunk && !ms[0].Date.Before(expiring) {
			return
		}
		if !b.llm.Available(ctx, llm.RoleSummary, llm.Initiative) {
			return
		}
		var r roster
		if r.roles, err = b.store.Roles(ctx, chatID); err != nil {
			slog.Error("load chat roles", "chat_id", chatID, "err", err)
		}
		raw, provider, err := b.llm.Complete(ctx, llm.RoleSummary, llm.Initiative, summaryPrompt(prev.Text, ms, b.cfg.SummaryMaxChars, r))
		if err != nil {
			slog.Warn("summary not updated", "chat_id", chatID, "topic", topic, "err", err)
			return
		}
		text := cleanSummary(raw, b.cfg.SummaryMaxChars)
		if text == "" {
			slog.Warn("summary not updated: empty answer", "chat_id", chatID, "topic", topic, "provider", provider)
			return
		}
		upTo := ms[len(ms)-1].MessageID
		ok, err := b.store.SetSummary(ctx, chatID, topic, prev, text, upTo, b.now())
		if err != nil {
			slog.Error("save summary", "chat_id", chatID, "topic", topic, "err", err)
			return
		}
		if !ok {
			slog.Info("summary not updated: changed meanwhile (erased by the admin?)", "chat_id", chatID, "topic", topic)
			return
		}
		slog.Info("summary updated", "chat_id", chatID, "topic", topic, "messages", len(ms), "up_to", upTo,
			"chars", len([]rune(text)), "provider", provider)
		// The same chunk tells about the people who wrote it (notes.go), about the chat (lore.go) and
		// about how people talk in the topic (tone.go).
		b.updateNotes(ctx, chatID, ms, r)
		b.updateLore(ctx, chatID, topic, ms, r)
		b.updateTone(ctx, chatID, topic, ms, r)
	}
}

var monthsGenitive = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа",
	"сентября", "октября", "ноября", "декабря"}

func dayLabel(t time.Time) string { return fmt.Sprintf("%d %s", t.Day(), monthsGenitive[t.Month()-1]) }

// summaryPrompt asks the model to fold new messages into the previous summary.
func summaryPrompt(prev string, ms []storage.Message, maxChars int, r roster) []llm.Message {
	sys := `Ты ведёшь сводку старой переписки одного топика группового чата в Telegram. Её читает бот — участник чата ` +
		`(его сообщения помечены «Ты»), чтобы помнить, о чём здесь говорили раньше; последние сообщения топика он видит сам, их в сводке не будет.
Обнови сводку: новая сводка = прежняя сводка + новые сообщения. Прежняя сводка — единственная память бота о том, что было ещё раньше: ` +
		`переноси её содержание в новую (можно короче), а не начинай заново; выбрасывай из неё только то, что устарело или стало неважным. Важно:
- темы разговоров и к чему пришли; договорённости, планы и события — с датами (по пометкам дней в переписке);
- вопросы и просьбы, которые остались без ответа;
- кто что предлагал, обещал, рассказывал о себе — по именам, как в переписке;
- местные шутки и словечки, если они повторяются.
Приветствия, флуд, стикеры и реакции пропускай. Не выдумывай и не оценивай людей. О боте (сообщения «Ты») пиши во втором лице: «ты посоветовал…».
` + fmt.Sprintf("Не длиннее %d символов: если не помещается, сжимай сначала самое старое — свежее важнее. ", maxChars) +
		`Пиши коротко, обычным текстом без markdown, на языке переписки. Выведи только текст сводки.`

	var u strings.Builder
	u.WriteString("Прежняя сводка:\n")
	if strings.TrimSpace(prev) == "" {
		u.WriteString("(нет)\n")
	} else {
		u.WriteString(strings.TrimSpace(prev))
		u.WriteByte('\n')
	}
	u.WriteString("\nНовые сообщения:\n")
	day := ""
	for _, m := range ms {
		if d := dayLabel(m.Date); d != day {
			day = d
			fmt.Fprintf(&u, "— %s —\n", d)
		}
		u.WriteString(formatLine(m, r))
		u.WriteByte('\n')
	}
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}

// cleanSummary strips what models add around the text and keeps it within reach of maxChars.
func cleanSummary(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	for _, p := range []string{"Сводка:", "Обновлённая сводка:", "Новая сводка:"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	return clip(s, maxChars)
}

// clip keeps a text within reach of maxChars: a slightly longer one is kept as is, a much longer
// one is cut at a sentence end.
func clip(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxChars+maxChars/4 {
		return s
	}
	cut := string(r[:maxChars])
	if i := strings.LastIndexAny(cut, ".!?\n"); i >= len(cut)/2 {
		return strings.TrimSpace(cut[:i+1])
	}
	return strings.TrimSpace(cut) + "…"
}

// summaryBlock renders the summaries for a prompt: the topic's own (thread >= 0) or those of all
// topics in question (thread < 0, the evaluation).
func summaryBlock(sums map[int]string, thread int, r roster) string {
	if thread >= 0 {
		if s := sums[thread]; s != "" {
			return "\nО чём здесь говорили раньше (сводка старой переписки; последние сообщения — ниже):\n" + s + "\n"
		}
		return ""
	}
	var topics []int
	for t, s := range sums {
		if s != "" {
			topics = append(topics, t)
		}
	}
	if len(topics) == 0 {
		return ""
	}
	slices.Sort(topics)
	var b strings.Builder
	b.WriteString("\nО чём говорили раньше (сводка старой переписки; последние сообщения — ниже):\n")
	for _, t := range topics {
		if len(topics) > 1 || t != 0 {
			if !r.tagged(t) {
				b.WriteString("[вне топиков] ")
			} else {
				fmt.Fprintf(&b, "[%s] ", r.topic(t))
			}
		}
		b.WriteString(sums[t])
		b.WriteByte('\n')
	}
	return b.String()
}
