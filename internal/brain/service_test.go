package brain

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// join builds a "joined the chat" event about the user (the newcomer is its subject).
func (e *env) join(userID int64, name, username string) Incoming {
	in := e.from(userID, name, username, "[вступил(а) в чат]")
	in.Kind = storage.KindJoin
	return in
}

func TestServiceEventsAreHistoryNotInstructions(t *testing.T) {
	e := setup(t)
	// A newcomer named like the bot's alias does not address it; joins are never commands.
	j := e.join(adminID, "Бот", "")
	e.say(j)
	got := e.get(j.MessageID)
	if got == nil || got.Kind != storage.KindJoin || got.Addressed || got.UserID != adminID {
		t.Fatalf("join stored as a service event: %+v", got)
	}
	if len(e.llm.callsOf(llm.RoleCommand)) != 0 || len(e.llm.callsOf(llm.RoleReply)) != 0 {
		t.Fatal("a join is not handled as a message to the bot")
	}
	if !e.pending() {
		t.Fatal("a newcomer is worth a look from the evaluator")
	}

	e.initiative()
	l := e.from(anyaID, "Аня", "anya", "[вышел(а) из чата]")
	l.Kind = storage.KindLeave
	e.say(l)
	if got := e.get(l.MessageID); got == nil || got.Kind != storage.KindLeave {
		t.Fatalf("leave stored: %+v", got)
	}
	if e.pending() {
		t.Fatal("someone leaving does not wake the evaluator")
	}
}

func TestOnReactionStoresAndShowsReactions(t *testing.T) {
	e := setup(t)
	m := e.msg("смотрите какой закат сегодня")
	e.say(m)
	ctx := context.Background()
	e.b.OnReaction(ctx, Reaction{ChatID: chat, ChatType: "supergroup", MessageID: m.MessageID, UserID: vasyaID, Emojis: []string{"🔥"}, Date: e.clk.Now()})
	e.b.OnReaction(ctx, Reaction{ChatID: chat, ChatType: "supergroup", MessageID: m.MessageID, UserID: petyaID, Emojis: []string{"🔥", "❤"}, Date: e.clk.Now()})
	// Ignored: the bot's own, private chats, chats the bot does not serve.
	e.b.OnReaction(ctx, Reaction{ChatID: chat, ChatType: "supergroup", MessageID: m.MessageID, UserID: botID, Emojis: []string{"👎"}})
	e.b.OnReaction(ctx, Reaction{ChatID: chat, ChatType: "private", MessageID: m.MessageID, UserID: anyaID, Emojis: []string{"👎"}})
	e.b.OnReaction(ctx, Reaction{ChatID: -999, ChatType: "supergroup", MessageID: m.MessageID, UserID: anyaID, Emojis: []string{"👎"}})

	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[]}`
	})
	e.initiative()
	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) != 1 || !strings.Contains(f[0].user(), "смотрите какой закат сегодня [реакции: 🔥×2 ❤]") {
		t.Fatalf("reactions must reach the model: %+v", f)
	}
	if !strings.Contains(f[0].system(), "служебное событие") {
		t.Fatal("the system prompt explains service events and reactions")
	}
}

func TestReplyToJoinEvent(t *testing.T) {
	e := setup(t)
	j := e.join(petyaID, "Петя", "petya")
	e.say(j)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":` + strconv.Itoa(j.MessageID) + `,"reply":true,"reason":"новичок"}]}`
	})
	e.initiative()
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || !strings.Contains(r[0].user(), "по поводу события [id="+strconv.Itoa(j.MessageID)+"]: Петя [вступил(а) в чат]") {
		t.Fatalf("reply prompt about the event: %+v", r)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 || sent[0].replyTo != j.MessageID {
		t.Fatalf("greeting quotes the join: %+v", sent)
	}
}
