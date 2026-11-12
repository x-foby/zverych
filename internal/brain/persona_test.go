package brain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func (e *env) persona() string {
	e.t.Helper()
	p, err := e.st.Persona(context.Background(), chat)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestPersonaNameAddressBypassesCooldown(t *testing.T) {
	for _, groupPersona := range []bool{false, true} {
		t.Run(fmt.Sprint("group=", groupPersona), func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			e.b.persona = "Тебя зовут Зверыч. Ты любишь музыку."
			name := "Зверыч"
			if groupPersona {
				if err := e.st.SetPersona(ctx, chat, "Тебя зовут Витёк. Ты любишь игры.", adminID, e.clk.Now()); err != nil {
					t.Fatal(err)
				}
				name = "Витек"
				if e.b.isAddressedInChat(ctx, e.msg("Зверыч, привет")) {
					t.Fatal("the old default persona name must not address the new persona")
				}
			}
			started := e.clk.Now().Add(-time.Minute)
			if err := e.st.SetLastInitiative(ctx, chat, started); err != nil {
				t.Fatal(err)
			}
			m := e.msg(name + ", сделай адвент-календарь по играм")
			e.say(m)
			stored, err := e.st.Get(ctx, chat, m.MessageID)
			if err != nil || stored == nil || !stored.Addressed || !stored.Answered {
				t.Fatalf("persona address must be answered during cooldown: %+v, %v", stored, err)
			}
			sent, _ := e.out.snapshot()
			if len(sent) != 1 || sent[0].replyTo != m.MessageID {
				t.Fatalf("expected a direct reply, got %+v", sent)
			}
			state, err := e.st.ChatState(ctx, chat)
			if err != nil || !state.LastInitiativeAt.Equal(started) {
				t.Fatalf("a persona address must not restart cooldown: %+v, %v", state, err)
			}
		})
	}
}

func TestPersonaNameMatching(t *testing.T) {
	e := setup(t)
	e.b.persona = "Твоё имя — «Витёк». Любишь группу Ария и игры."
	for _, c := range []struct {
		text string
		want bool
	}{
		{"ВИТЕК, сделай календарь", true},
		{"Витёкь!", true},
		{"витек2", false},
		{"Ария", false},
		{"игры", false},
	} {
		if got := e.b.isAddressed(Incoming{Text: c.text}); got != c.want {
			t.Errorf("isAddressed(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestPersonaAddressDoesNotStartCooldown(t *testing.T) {
	e := setup(t)
	e.b.persona = "Тебя зовут Зверыч."
	m := e.msg("Зверыч, сделай адвент-календарь по играм")
	e.say(m)
	if len(e.sentTo(m.MessageID)) != 1 {
		t.Fatal("expected a reply to the persona name")
	}
	state, err := e.st.ChatState(context.Background(), chat)
	if err != nil || !state.LastInitiativeAt.IsZero() {
		t.Fatalf("a direct answer must not start cooldown: %+v, %v", state, err)
	}
}

// lastButtons is the last question with buttons; its "yes" and "no" data.
func (e *env) lastButtons() (q sentButtons, yes, no string) {
	e.t.Helper()
	bs := e.buttons()
	if len(bs) == 0 {
		e.t.Fatal("no question with buttons")
	}
	q = bs[len(bs)-1]
	return q, q.rows[0][0].Data, q.rows[0][1].Data
}

// groupTexts are the bot's messages to the test group.
func (e *env) groupTexts() []string { return e.dmTexts(chat) }

// An admin of the group sets its persona in the private chat: the model rewrites the current one,
// the admin confirms with a button; the group then talks with it, and may go back to the default.
func TestPersonaFromPrivate(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.command(`{"command":"set_persona"}`)
	e.llm.set(func(f *fakeLLM) {
		f.reply = "```\nТебя зовут Шарманка. Ты фанат джаза.\n```"
	})
	e.say(e.dm(vasyaID, "Вася", "vasya", "теперь ты фанат джаза"))

	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || !strings.Contains(r[0].user(), "тестовая персона") || !strings.Contains(r[0].user(), "теперь ты фанат джаза") ||
		!strings.Contains(r[0].system(), "не травит") {
		t.Fatalf("the model rewrites the current persona as asked: %+v", r)
	}
	q, yes, no := e.lastButtons()
	if q.chat != vasyaID || !strings.Contains(q.text, "«Тестовая группа»") || !strings.Contains(q.text, "Тебя зовут Шарманка. Ты фанат джаза.\n\nСохранить?") {
		t.Fatalf("the draft goes to the admin's private chat, no fence: %+v", q)
	}
	if ts := e.dmTexts(vasyaID); len(ts) != 0 {
		t.Fatalf("the question is the answer: %q", ts)
	}
	if e.persona() != "" {
		t.Fatal("nothing changes before the confirmation")
	}

	// Somebody else's press changes nothing; the creator's — neither.
	e.press(anyaID, yes)
	e.press(adminID, yes)
	if e.persona() != "" || len(e.edits()) != 0 {
		t.Fatalf("only the admin who asked confirms: %q %+v", e.persona(), e.edits())
	}
	e.press(vasyaID, yes)
	if got := e.lastEdit(); !strings.HasPrefix(got, "Готово, теперь в «Тестовая группа» я такой:") || e.persona() != "Тебя зовут Шарманка. Ты фанат джаза." {
		t.Fatalf("saved: %q %q", got, e.persona())
	}
	e.press(vasyaID, no)
	if got := e.lastEdit(); got != "Этот вариант уже неактуален." {
		t.Fatalf("a used question: %q", got)
	}

	// The group talks with the new persona.
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты: джаз!" })
	e.say(e.msg("@testbot привет"))
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "Шарманка") || strings.Contains(sys, "тестовая персона") {
		t.Fatalf("the group's persona in the reply: %s", sys)
	}
	// So does the private chat with its admin.
	e.command(`{"command":"none"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "как сам?"))
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "Шарманка") {
		t.Fatalf("the group's persona in the private chat: %s", sys)
	}

	// Back to the default: no model, a confirmation all the same.
	calls := len(e.llm.callsOf(llm.RoleReply))
	e.command(`{"command":"reset_persona"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "верни обычную личность"))
	q, yes, _ = e.lastButtons()
	if !strings.Contains(q.text, "по умолчанию:\n\nтестовая персона\n\nСохранить?") || len(e.llm.callsOf(llm.RoleReply)) != calls {
		t.Fatalf("the default persona shown: %+v", q)
	}
	e.press(vasyaID, yes)
	if got := e.lastEdit(); got != "Готово: в «Тестовая группа» я снова по умолчанию." || e.persona() != "" {
		t.Fatalf("reset: %q %q", got, e.persona())
	}
	e.say(e.dm(vasyaID, "Вася", "vasya", "верни обычную личность"))
	r = e.llm.callsOf(llm.RoleReply)
	if !strings.Contains(r[len(r)-1].user(), "и так по умолчанию") {
		t.Fatalf("nothing to reset: %s", r[len(r)-1].user())
	}
}

// In the group: the admin asks, the draft goes to the private chat; nobody else may ask.
func TestPersonaFromGroup(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты — Шарманка." })
	e.command(`{"command":"set_persona"}`)

	for _, who := range []Incoming{
		e.from(petyaID, "Петя", "petya", "Бот, будь занудой"),
		e.from(adminID, "Создатель", "creator", "Бот, будь занудой"),
	} {
		e.say(who)
	}
	if len(e.buttons()) != 0 {
		t.Fatal("only the group's admins change its persona")
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 2 || !strings.Contains(r[0].user(), "только админы чата") || !strings.Contains(r[1].user(), "только от админов чата") {
		t.Fatalf("refused in words, the creator too: %+v", r)
	}

	// Spoken about, not to: not a command.
	e.say(e.from(vasyaID, "Вася", "vasya", "надо бы боту личность поменять"))
	if len(e.buttons()) != 0 {
		t.Fatal("the bot was not addressed")
	}

	e.say(e.from(vasyaID, "Вася", "vasya", "Бот, будь занудой"))
	q, _, no := e.lastButtons()
	if q.chat != vasyaID || !strings.Contains(q.text, "Ты — Шарманка.") {
		t.Fatalf("the draft goes to the admin's private chat: %+v", q)
	}
	if ts := e.groupTexts(); len(ts) == 0 || ts[len(ts)-1] != "Прислал в личку — посмотри и подтверди." {
		t.Fatalf("the group is told where to look: %q", ts)
	}
	e.press(vasyaID, no)
	if got := e.lastEdit(); got != "Хорошо, в «Тестовая группа» остаюсь прежним." || e.persona() != "" {
		t.Fatalf("declined: %q %q", got, e.persona())
	}
}

// The admin is no admin any more when they press: nothing changes. A newer draft makes the older
// one's buttons stale.
func TestPersonaConfirmationIsChecked(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeLead(anyaID, "Аня", "anya")
	e.command(`{"command":"set_persona"}`)
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты — Шарманка." })
	e.say(e.dm(vasyaID, "Вася", "vasya", "будь Шарманкой"))
	_, older, _ := e.lastButtons()
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты — Гармонь." })
	e.say(e.dm(vasyaID, "Вася", "vasya", "нет, лучше Гармонью"))
	_, yes, _ := e.lastButtons()
	e.press(vasyaID, older)
	if got := e.lastEdit(); got != "Этот вариант уже неактуален." {
		t.Fatalf("an older draft: %q", got)
	}
	if _, err := e.st.RemoveRole(context.Background(), chat, storage.Person{ID: vasyaID}, storage.RoleLead); err != nil {
		t.Fatal(err)
	}
	e.press(vasyaID, yes)
	if got := e.lastEdit(); !strings.Contains(got, "больше не админ") || e.persona() != "" {
		t.Fatalf("no admin any more: %q %q", got, e.persona())
	}
}

// What can go wrong on the way: a too long description, a model failure, a private chat the bot
// may not write to first.
func TestPersonaFailures(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.command(`{"command":"set_persona"}`)
	lastReason := func() string {
		r := e.llm.callsOf(llm.RoleReply)
		return r[len(r)-1].user()
	}

	e.llm.set(func(f *fakeLLM) { f.reply = strings.Repeat("я", maxPersonaRunes+1) })
	e.say(e.dm(vasyaID, "Вася", "vasya", "вот твоя личность: …"))
	if len(e.buttons()) != 0 || !strings.Contains(lastReason(), "слишком длинным") {
		t.Fatalf("too long: %s", lastReason())
	}

	e.llm.set(func(f *fakeLLM) { f.reply = "  " })
	e.say(e.dm(vasyaID, "Вася", "vasya", "будь Шарманкой"))
	if len(e.buttons()) != 0 || !strings.Contains(lastReason(), "что-то сломалось") {
		t.Fatalf("an empty draft: %s", lastReason())
	}

	// In the group: the admin never wrote to the bot privately.
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты — Шарманка." })
	e.out.mu.Lock()
	e.out.sendErr = errors.New("Forbidden: bot can't initiate conversation with a user")
	e.out.mu.Unlock()
	e.say(e.from(vasyaID, "Вася", "vasya", "Бот, будь Шарманкой"))
	if !strings.Contains(lastReason(), "напишут тебе в личные сообщения") {
		t.Fatalf("asked to write first: %s", lastReason())
	}
	if e.persona() != "" {
		t.Fatal("nothing saved")
	}
}

func TestCleanPersona(t *testing.T) {
	for in, want := range map[string]string{
		"  Ты — Шарманка.  ":                          "Ты — Шарманка.",
		"```markdown\n# Персона\nТы — Шарманка.\n```": "# Персона\nТы — Шарманка.",
		"<persona>\nТы — Шарманка.\n</persona>":       "Ты — Шарманка.",
		"Ты пишешь `код` и ```не только```":           "Ты пишешь `код` и ```не только```",
	} {
		if got := cleanPersona(in); got != want {
			t.Errorf("cleanPersona(%q) = %q, want %q", in, got, want)
		}
	}
}
