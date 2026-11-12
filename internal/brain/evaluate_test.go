package brain

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// instruction stores an instruction of the leadership and returns its id.
func (e *env) instruction(text string, thread int) int64 {
	e.t.Helper()
	id, err := e.st.AddLore(context.Background(), storage.Lore{ChatID: chat, ChatWide: thread == 0, ThreadID: thread,
		Kind: storage.LoreInstruction, Text: text, AuthorID: adminID, AuthorRole: "admin", CreatedAt: e.clk.Now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) decide(json string) { e.llm.set(func(f *fakeLLM) { f.decision = json }) }

func (e *env) sentTo(id int) []string {
	sent, _ := e.out.snapshot()
	var out []string
	for _, s := range sent {
		if s.replyTo == id {
			out = append(out, s.text)
		}
	}
	return out
}

func act(id int, fields string) string { return `{"id":` + strconv.Itoa(id) + `,` + fields + `}` }

func acts(a ...string) string { return `{"actions":[` + strings.Join(a, ",") + `]}` }

// joining adds to a decision that message id continues the thread `root` (SPEC 7a).
func joining(decision string, id, root int) string {
	return strings.TrimSuffix(decision, "}") + `,"threads":[{"id":` + strconv.Itoa(id) + `,"thread":` + strconv.Itoa(root) + `}]}`
}

// Instructions are carried out by the model's decisions; the code checks them (2026-11-08).
func TestInstructionsAreCarriedOutInTheCooldown(t *testing.T) {
	e := setup(t)
	heart := e.instruction("Бот ставит ❤ на сообщения Ани (@anya)", 0)
	bday := e.instruction("Бот отвечает «С Днём рождения!» на любое упоминание Андрея Степанова", 0)
	greet := e.instruction("Бот поздравляет всех, кто упомянет @vasya", 0)
	if err := e.st.SetLastInitiative(context.Background(), chat, e.clk.Now()); err != nil { // the cooldown runs
		t.Fatal(err)
	}
	short := e.from(anyaID, "Аня", "anya", "ок")
	m1 := e.from(petyaID, "Петя", "petya", "Видел Андрея Степанова в студии")
	m2 := e.from(petyaID, "Петя", "petya", "Степанову привет!") // the same author: once a day
	m3 := e.from(43, "Боря", "borya", "а @vasya придёт?")
	m4 := e.from(43, "Боря", "borya", "кто-нибудь знает хороший компрессор?")
	e.say(short, m1, m2, m3, m4)
	e.decide(acts(
		act(short.MessageID, `"react":"❤️","rule":`+itoa(heart)),
		act(m1.MessageID, `"reply":true,"rule":`+itoa(bday)+`,"text":"С Днём рождения!"`),
		act(m2.MessageID, `"reply":true,"rule":`+itoa(bday)+`,"text":"С Днём рождения!"`),
		act(m3.MessageID, `"reply":true,"rule":`+itoa(greet)+`,"reason":"упомянули Васю"`),
		act(m4.MessageID, `"reply":true,"reason":"знаю ответ"`), // own reply: the cooldown forbids it
	))
	e.initiative()

	if got := e.reactionsTo(short.MessageID); !reflect.DeepEqual(got, []string{"❤"}) {
		t.Errorf("reaction by the instruction: %v", got)
	}
	if got := e.sentTo(m1.MessageID); !reflect.DeepEqual(got, []string{"С Днём рождения!"}) {
		t.Errorf("the exact phrase, without the model: %v", got)
	}
	if got := e.sentTo(m2.MessageID); got != nil {
		t.Errorf("the same author is answered by the instruction once a day: %v", got)
	}
	if got := e.sentTo(m3.MessageID); len(got) != 1 {
		t.Errorf("a task reply, written by the model: %v", got)
	}
	if got := e.sentTo(m4.MessageID); got != nil {
		t.Errorf("own reply in the cooldown: %v", got)
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || r[0].prio != llm.Direct ||
		!strings.Contains(r[0].user(), "Выполни указание руководства: Бот поздравляет всех, кто упомянет @vasya. упомянули Васю") {
		t.Fatalf("one generated reply, by the instruction, with Direct priority: %+v", r)
	}
	f := e.llm.callsOf(llm.RoleFilter)
	if sys := f[0].system(); !strings.Contains(sys, "- №"+itoa(bday)+" [весь чат] Бот отвечает «С Днём рождения!»") {
		t.Errorf("instructions are numbered in the prompt:\n%s", sys)
	}
	if st, _ := e.st.ChatState(context.Background(), chat); e.clk.Now().Sub(st.LastInitiativeAt) != 0 {
		t.Errorf("instruction replies do not restart the cooldown")
	}

	// A day later the same author is answered again.
	e.clk.Advance(ruleHitTTL)
	m5 := e.from(petyaID, "Петя", "petya", "Андрей Степанов сегодня именинник")
	e.say(m5)
	e.decide(acts(act(m5.MessageID, `"reply":true,"rule":`+itoa(bday)+`,"text":"С Днём рождения!"`)))
	e.initiative()
	if got := e.sentTo(m5.MessageID); len(got) != 1 {
		t.Errorf("answered again after ruleHitTTL: %v", got)
	}
	e.b.forgetOldRuleHits()
	e.clk.Advance(ruleHitTTL)
	e.b.forgetOldRuleHits()
	if len(e.b.ruleHits) != 0 {
		t.Errorf("old hits are forgotten: %v", e.b.ruleHits)
	}
}

func TestInstructionChecks(t *testing.T) {
	e := setup(t)
	topic := e.instruction("Бот отвечает «Тут не флудят» на любое сообщение", 3)
	fact, _ := e.st.AddLore(context.Background(), storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact,
		Text: "Аня любит «С Днём рождения!»", CreatedAt: e.clk.Now()})
	bday := e.instruction("Бот поздравляет с днём рождения тех, кто упомянет Андрея", 0)
	if err := e.st.SetLastInitiative(context.Background(), chat, e.clk.Now()); err != nil { // no own replies
		t.Fatal(err)
	}

	other := e.inTopic(e.from(petyaID, "Петя", "petya", "привет"), 5)
	byFact := e.from(petyaID, "Петя", "petya", "а где Аня?")
	made := e.from(43, "Боря", "borya", "Андрей сегодня празднует")
	e.say(other, byFact, made)
	e.decide(acts(
		act(other.MessageID, `"reply":true,"rule":`+itoa(topic)+`,"text":"Тут не флудят"`),    // the rule is for topic 3
		act(byFact.MessageID, `"reply":true,"rule":`+itoa(fact)+`,"text":"С Днём рождения!"`), // a fact is no instruction
		act(made.MessageID, `"reply":true,"rule":`+itoa(bday)+`,"text":"С днюхой, бро!"`),     // not the instruction's words
	))
	e.initiative()
	if got := e.sentTo(other.MessageID); got != nil {
		t.Errorf("another topic's instruction does not apply: %v", got)
	}
	if got := e.sentTo(byFact.MessageID); got != nil {
		t.Errorf("only instructions count: %v", got)
	}
	if got := e.sentTo(made.MessageID); !reflect.DeepEqual(got, []string{"ответ бота"}) {
		t.Errorf("a phrase that is not in the instruction is not sent; the model writes the reply: %v", got)
	}
}

func TestRepliesPerBatchAreCapped(t *testing.T) {
	e := setup(t)
	bday := e.instruction("Бот отвечает «С Днём рождения!» на любое упоминание Андрея Степанова", 0)
	var as []string
	for i := range maxRepliesPerBatch + 2 {
		m := e.from(int64(100+i), "Гость", "", "Андрей Степанов!")
		e.say(m)
		as = append(as, act(m.MessageID, `"reply":true,"rule":`+itoa(bday)+`,"text":"С Днём рождения!"`))
	}
	e.decide(acts(as...))
	e.initiative()
	if sent, _ := e.out.snapshot(); len(sent) != maxRepliesPerBatch {
		t.Fatalf("at most %d replies per batch, sent %d", maxRepliesPerBatch, len(sent))
	}
}

// With an hourly batch the dialog is judged by the message's own time, not by the evaluation's.
func TestDialogIsJudgedAtTheMessageTime(t *testing.T) {
	e := setup(t)
	e.say(e.msg("бот, привет"))
	e.clk.Advance(5 * time.Minute)
	q := e.msg("а ты что умеешь?")
	e.decide(joining(acts(act(q.MessageID, `"reply":true`)), q.MessageID, 1)) // continues the bot's thread
	e.say(q)                                                                  // a dialog: evaluated at once, not in an hour
	if got := e.sentTo(q.MessageID); len(got) != 1 {
		t.Fatalf("reply: %v", got)
	}
	if st, _ := e.st.ChatState(context.Background(), chat); !st.LastInitiativeAt.IsZero() {
		t.Fatal("a reply within the dialog does not start the cooldown")
	}
}

// Someone talking with the bot who writes on without its name or a reply is evaluated at once, not
// in the next batch (SPEC 7a, 2026-11-09); everyone else waits for the batch.
func TestDialogIsEvaluatedAtOnce(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.BatchIntervalSeconds = 3600 })
	e.say(e.msg("бот, где лучше записывать барабаны?")) // the bot's thread is 1
	e.clk.Advance(2 * time.Minute)

	borya := e.from(43, "Боря", "borya", "а я вчера струны поменял")
	e.say(borya)
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != 0 || !e.pending() {
		t.Fatalf("not in a dialog: waits for the batch (filter calls %d, pending %v)", n, e.pending())
	}

	q := e.msg("ты про какой кабинет?")
	e.decide(joining(acts(act(q.MessageID, `"reply":true`)), q.MessageID, 1))
	e.say(q)
	if got := e.sentTo(q.MessageID); len(got) != 1 {
		t.Fatalf("a dialog follow-up is answered without waiting for the batch: %v", got)
	}
	if e.pending() {
		t.Fatal("the evaluation covered the chat's new messages")
	}

	// In someone else's thread — not a dialog, even from the bot's interlocutor.
	calls := len(e.llm.callsOf(llm.RoleFilter))
	ans := e.msg("какие брал?")
	ans.ReplyToID, ans.ReplyToUserID = borya.MessageID, 43
	e.say(ans)
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != calls {
		t.Fatalf("someone else's thread waits for the batch: filter calls %d → %d", calls, n)
	}

	// After dialog_minutes the talk is over.
	e.clk.Advance(30 * time.Minute)
	e.say(e.msg("кстати, а микрофон какой?"))
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != calls {
		t.Fatalf("an old dialog is not a dialog: filter calls %d → %d", calls, n)
	}
}

// The evaluation shows the lore of the whole chat and of the topics with new messages only.
func TestEvaluationSeesTheLoreOfItsTopics(t *testing.T) {
	e := setup(t)
	here := e.instruction("Бот ставит 🔥 на сообщения Пети", 3)
	e.instruction("Бот молчит в топике про барабаны", 4)
	e.instruction("Бот не матерится", 0)
	if _, err := e.st.AddLore(context.Background(), storage.Lore{ChatID: chat, ThreadID: 4, Kind: storage.LoreFact,
		Text: "в топике 4 джем по пятницам", CreatedAt: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	e.say(e.inTopic(e.from(petyaID, "Петя", "petya", "привет"), 3))
	e.decide(acts())
	e.initiative()
	calls := e.llm.callsOf(llm.RoleFilter)
	if len(calls) != 1 {
		t.Fatalf("one evaluation: %d", len(calls))
	}
	sys := calls[0].system()
	for _, want := range []string{"№" + itoa(here) + " [топик 3]", "[весь чат] Бот не матерится"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the evaluation lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "барабаны") || strings.Contains(sys, "джем") {
		t.Errorf("another topic's lore is left out:\n%s", sys)
	}
	if u := calls[0].user(); !strings.Contains(u, "№"+itoa(here)) || strings.Count(u, "№") != 2 {
		t.Errorf("the closing reminder lists the instructions shown:\n%s", u)
	}
}
