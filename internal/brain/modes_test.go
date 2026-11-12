package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// In silence the bot neither writes nor reacts and does not evaluate; the leadership's
// instructions are carried out without a word (SPEC 4.1a).
func TestSilentGroup(t *testing.T) {
	e := setup(t)
	e.setGroup(chat, storage.ChatActive, storage.ModeSilent)
	e.makeLead(vasyaID, "Вася", "vasya")

	mention := e.msg("@testbot привет, ты кто?")
	e.say(mention)
	if got := e.get(mention.MessageID); got == nil || !got.Answered {
		t.Fatalf("a mention is stored and marked answered, so it is not answered later: %+v", got)
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 0 {
		t.Fatal("no reply is even generated")
	}
	if c := e.llm.callsOf(llm.RoleCommand); len(c) != 0 {
		t.Fatal("an ordinary participant's mention costs no command parsing")
	}

	e.command(`{"command":"remember","text":"Джем по пятницам"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, запомни: джем по пятницам"))
	if ls := e.lore(); len(ls) != 1 || ls[0].Text != "Джем по пятницам" {
		t.Fatalf("the admin's instruction is carried out: %+v", ls)
	}
	e.command(`{"command":"list"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, что ты помнишь?"))
	e.command(`{"command":"add_authority","person":1}`)
	e.say(e.from(anyaID+1, "Гость", "", "бот, сделай @vasya авторитетом")) // no rights: not even parsed

	plain := e.msg("обычный разговор")
	e.say(plain)
	e.tick()
	if f := e.llm.callsOf(llm.RoleFilter); len(f) != 0 {
		t.Fatal("a silent group is not evaluated")
	}
	if st, _ := e.st.ChatState(context.Background(), chat); st.LastEvaluatedID != plain.MessageID {
		t.Fatalf("the batch is seen, so it does not pile up: %+v", st)
	}
	if e.stored(chat) < 4 {
		t.Fatal("a silent group keeps its history: the memory is what silence is for")
	}

	e.llm.set(func(f *fakeLLM) { f.online = false })
	e.say(e.msg("@testbot ау"))
	if sent, reacts := e.out.snapshot(); len(sent) != 0 || len(reacts) != 0 {
		t.Fatalf("not a word, not a reaction (the offline one neither): %+v %+v", sent, reacts)
	}

	// Allowed to talk again: the old mentions stay unanswered.
	e.llm.set(func(f *fakeLLM) { f.online = true })
	e.setGroup(chat, storage.ChatActive, storage.ModeTalk)
	e.tick()
	if sent, _ := e.out.snapshot(); len(sent) != 0 {
		t.Fatalf("no answers to what was said in silence: %+v", sent)
	}
}

// With reactions only the bot evaluates and reacts, but writes nothing, by instructions neither.
func TestReactionsOnlyGroup(t *testing.T) {
	e := setup(t)
	e.setGroup(chat, storage.ChatActive, storage.ModeReactions)
	e.makeLead(vasyaID, "Вася", "vasya")
	rule := e.instruction("Бот отвечает «С Днём рождения!» на упоминание Андрея", 0)

	m1 := e.msg("Андрей сегодня именинник")
	m2 := e.msg("смешной мем")
	e.say(m1, m2)
	e.decide(`{"rules":[{"rule":` + itoa(rule) + `,"ids":[` + itoa(int64(m1.MessageID)) + `],"reply":true,"text":"С Днём рождения!"}],` +
		`"actions":[` + act(m2.MessageID, `"react":"😁","reply":true,"reason":"смешно"`) + `]}`)
	e.initiative()
	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) != 1 || !strings.Contains(f[0].system(), "можно только ставить реакции") {
		t.Fatalf("the model is told it may only react: %+v", f)
	}
	sent, reacts := e.out.snapshot()
	if len(sent) != 0 || len(e.llm.callsOf(llm.RoleReply)) != 0 {
		t.Fatalf("no replies, by an instruction neither: %+v", sent)
	}
	if len(reacts) != 1 || reacts[0] != (reaction{m2.MessageID, "😁"}) {
		t.Fatalf("the reaction is set: %+v", reacts)
	}

	mention := e.msg("@testbot как тебе мем?")
	e.say(mention)
	if sent, _ := e.out.snapshot(); len(sent) != 0 || !e.get(mention.MessageID).Answered {
		t.Fatal("a mention gets no answer")
	}

	e.command(`{"command":"remember","text":"Мемы — в топике 5"}`)
	cmd := e.from(vasyaID, "Вася", "vasya", "бот, запомни: мемы в топике 5")
	e.say(cmd)
	e.command(`{"command":"list"}`)
	list := e.from(vasyaID, "Вася", "vasya", "бот, что ты помнишь?")
	e.say(list)
	e.makeAuthority(petyaID, "Петя")
	e.command(`{"command":"add_authority","person":1}`)
	refused := e.from(petyaID, "Петя", "petya", "бот, сделай @anya авторитетом")
	e.say(refused)
	if got := e.reactionsTo(cmd.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("a carried-out instruction is acknowledged with 🫡: %v", got)
	}
	if got := e.reactionsTo(list.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("an answer in words becomes 🫡: %v", got)
	}
	if got := e.reactionsTo(refused.MessageID); len(got) != 0 {
		t.Fatalf("a refusal is not shown: %v", got)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 0 {
		t.Fatalf("not a word in the group: %+v", sent)
	}
}

// Someone talking with the bot does not start an early evaluation where the bot may not answer.
func TestNoDialogEvaluationWithoutTalk(t *testing.T) {
	e := dialogEnv(t, 0)
	e.setGroup(chat, storage.ChatActive, storage.ModeReactions)
	e.say(e.msg("@testbot привет"))
	e.say(e.msg("а ещё вопрос"))
	e.b.mu.Lock()
	n := len(e.b.dialogs)
	e.b.mu.Unlock()
	if n != 0 {
		t.Fatal("no dialog evaluation in a group that may not talk")
	}
}

// An admin switches the mode in the group itself; the acknowledgement follows the new mode.
func TestModeSwitchInTheGroup(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	e.command(`{"command":"set_mode","mode":"silent"}`)
	hush := e.from(vasyaID, "Вася", "vasya", "бот, помолчи пока")
	e.say(hush)
	if c, _ := e.group(chat); c.Mode != storage.ModeSilent || len(e.reactionsTo(hush.MessageID)) != 0 {
		t.Fatalf("silent, without a word or a reaction: %+v", c)
	}
	e.command(`{"command":"set_mode","mode":"talk"}`)
	e.say(e.from(petyaID, "Петя", "petya", "бот, можешь говорить")) // an authority may not
	if c, _ := e.group(chat); c.Mode != storage.ModeSilent {
		t.Fatal("only admins switch the mode")
	}
	talk := e.from(vasyaID, "Вася", "vasya", "бот, можешь говорить")
	e.say(talk)
	if c, _ := e.group(chat); c.Mode != storage.ModeTalk {
		t.Fatalf("talking again: %+v", c)
	}
	if got := e.reactionsTo(talk.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("acknowledged in the new mode: %v", got)
	}
	e.command(`{"command":"set_mode","mode":"reactions","scope":"topic"}`)
	e.say(e.inTopic(e.from(vasyaID, "Вася", "vasya", "бот, в этой ветке только реакции"), 5))
	if c, _ := e.group(chat); c.Mode != storage.ModeTalk {
		t.Fatalf("a topic does not silence the whole group: %+v", c)
	}
	e.command(`{"command":"set_mode","mode":"loud"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, ори"))
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 2 || !strings.Contains(r[1].user(), "непонятно какой") {
		t.Fatalf("an unknown mode is refused: %+v", r)
	}
}

// Somebody who is not in the group is not made its admin.
func TestAdminMustBeAMember(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.from(petyaID, "Петя", "petya", "пока всем"))
	e.out.nonMembers = map[int64]bool{petyaID: true} // left since
	e.command(`{"command":"set_lead","person":1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, @petya тоже админ"))
	if got := e.roles(); len(got) != 1 {
		t.Fatalf("not appointed: %v", got)
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "нет в группе") {
		t.Fatalf("refused: %+v", r)
	}
}
