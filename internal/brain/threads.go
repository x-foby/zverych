package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// Threads (SPEC 7a): a reply joins the thread of what it answers when it is saved; a message
// without a reply is put into a thread by its meaning — by the evaluation of a batch, or by a short
// request before the bot answers someone who addressed it without a reply.

// threadView is a thread as the model sees it: its first messages, a gap, its last ones.
type threadView struct {
	root  int
	topic int
	msgs  []storage.Message
	cut   int // messages after msgs[:cut] are left out; 0 — whole
}

// candidateTail is how many of a candidate thread's last messages show what it is about now.
const candidateTail = 3

// loadThread loads a thread's first ThreadHeadMessages and last `tail` messages.
func (b *Brain) loadThread(ctx context.Context, chatID int64, root, tail int) (threadView, error) {
	head := b.cfg.ThreadHeadMessages
	msgs, total, err := b.store.Thread(ctx, chatID, root, head, tail)
	if err != nil || len(msgs) == 0 {
		return threadView{root: root}, err
	}
	v := threadView{root: root, topic: msgs[0].ThreadID, msgs: msgs}
	if total > len(msgs) {
		v.cut = head
	}
	return v, nil
}

// activeThreads returns the roots of a topic's threads a message without a reply may continue.
func (b *Brain) activeThreads(ctx context.Context, chatID int64, topic int) ([]int, error) {
	since := b.now().Add(-time.Duration(b.cfg.ThreadActiveHours * float64(time.Hour)))
	return b.store.ActiveThreads(ctx, chatID, topic, since, b.cfg.ThreadCandidates)
}

// assignThread puts a message addressed to the bot without a reply into the thread it continues
// by meaning, before the bot answers it in that thread's context.
func (b *Brain) assignThread(ctx context.Context, m storage.Message) {
	if m.ReplyTo != 0 || m.Kind != "" || m.RootID != 0 {
		return
	}
	roots, err := b.activeThreads(ctx, m.ChatID, m.ThreadID)
	if err != nil {
		slog.Error("active threads", "chat_id", m.ChatID, "err", err)
		return
	}
	roots = slices.DeleteFunc(roots, func(r int) bool { return r >= m.MessageID })
	if len(roots) == 0 || !b.llm.Available(ctx, llm.RoleThread, llm.Direct) {
		return
	}
	views := make([]threadView, 0, len(roots))
	for _, r := range roots {
		v, err := b.loadThread(ctx, m.ChatID, r, candidateTail)
		if err != nil {
			slog.Error("load thread", "chat_id", m.ChatID, "root", r, "err", err)
			return
		}
		if len(v.msgs) > 0 {
			views = append(views, v)
		}
	}
	slices.Reverse(views) // oldest first, like the chat
	raw, _, err := b.llm.Complete(ctx, llm.RoleThread, llm.Direct, threadPrompt(b.roster(ctx, m.ChatID), views, m))
	if err != nil {
		if !errors.Is(err, llm.ErrUnavailable) {
			slog.Warn("thread assignment", "err", err)
		}
		return
	}
	root, err := parseThread(raw)
	if err != nil {
		slog.Warn("thread assignment returned unparsable answer", "err", err)
		return
	}
	if root == 0 {
		slog.Debug("thread assignment: in no thread", "chat_id", m.ChatID, "message_id", m.MessageID)
		return
	}
	if !slices.Contains(roots, root) {
		slog.Debug("thread assignment: not a candidate", "chat_id", m.ChatID, "message_id", m.MessageID, "thread", root)
		return
	}
	b.setRoot(ctx, m, root, "addressed")
}

func (b *Brain) setRoot(ctx context.Context, m storage.Message, root int, by string) {
	moved, err := b.store.SetRoot(ctx, m.ChatID, m.MessageID, root)
	if err != nil {
		slog.Error("set thread", "chat_id", m.ChatID, "err", err)
		return
	}
	if moved {
		slog.Info("thread assigned by meaning", "chat_id", m.ChatID, "message_id", m.MessageID, "thread", root, "by", by)
	}
}

// roster loads just the chat's roles (to label people in a prompt).
func (b *Brain) roster(ctx context.Context, chatID int64) roster {
	roles, err := b.store.Roles(ctx, chatID)
	if err != nil {
		slog.Error("load chat roles", "chat_id", chatID, "err", err)
	}
	return roster{roles: roles, topics: b.topicNames(ctx, chatID)}
}

// topicNames loads the known names of the chat's topics; on a storage error — none.
func (b *Brain) topicNames(ctx context.Context, chatID int64) map[int]string {
	names, err := b.store.TopicNames(ctx, chatID)
	if err != nil {
		slog.Error("load topic names", "chat_id", chatID, "err", err)
	}
	return names
}

// formatThread writes a thread under its header; mark, if set, adds marks to a line.
func formatThread(u *strings.Builder, v threadView, r roster, mark func(storage.Message) string) {
	fmt.Fprintf(u, "Ветка №%d", v.root)
	if r.tagged(v.topic) {
		fmt.Fprintf(u, " [%s]", r.topic(v.topic))
	}
	u.WriteString(":\n")
	for i, m := range v.msgs {
		if v.cut > 0 && i == v.cut {
			u.WriteString("…\n")
		}
		u.WriteString(formatLine(m, r))
		if mark != nil {
			u.WriteString(mark(m))
		}
		u.WriteByte('\n')
	}
}

// threadPrompt asks which active thread a message without a reply continues.
func threadPrompt(r roster, views []threadView, m storage.Message) []llm.Message {
	sys := `Ты разбираешь переписку группового чата на ветки разговора. Ветка — сообщения, связанные ответами (reply). ` +
		`Новое сообщение написано без reply. Реши по смыслу, продолжает ли оно одну из веток: например, человек ответил в ветке ` +
		`и следующим сообщением дописал мысль, или говорит о том же, что обсуждают в ветке. Отсылка к уже сказанному («он», «там», «это», «а сколько стоит?») — ` +
		`признак продолжения ветки, где это сказано. Если сообщение начинает новую тему, ` +
		`обращено ко всем или по нему нельзя понять, к чему оно, — оно ни к одной ветке не относится.
Ответь строго одним JSON-объектом, без текста вокруг: {"thread": <номер ветки или 0>}`
	var u strings.Builder
	u.WriteString("Ветки (начало и последние сообщения):\n\n")
	for _, v := range views {
		formatThread(&u, v, r, nil)
		u.WriteByte('\n')
	}
	u.WriteString("Новое сообщение:\n")
	u.WriteString(formatLine(m, r))
	u.WriteByte('\n')
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}

func parseThread(s string) (int, error) {
	raw := jsonObjRe.FindString(s)
	if raw == "" {
		return 0, fmt.Errorf("no JSON object in %q", s)
	}
	var v struct {
		Thread *int `json:"thread"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v.Thread == nil {
		return 0, fmt.Errorf("bad thread JSON %q: %v", raw, err)
	}
	return *v.Thread, nil
}

// assignment is the evaluation's verdict that a new message without a reply continues a thread.
type assignment struct {
	ID     int `json:"id"`
	Thread int `json:"thread"`
}

// parseAssignments extracts "threads" from an evaluation answer; a missing or broken list is empty.
func parseAssignments(s string) []assignment {
	var v struct {
		Threads []assignment `json:"threads"`
	}
	if raw := jsonObjRe.FindString(s); raw == "" || json.Unmarshal([]byte(raw), &v) != nil {
		return nil
	}
	return v.Threads
}
