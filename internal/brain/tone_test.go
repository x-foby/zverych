package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func (e *env) toneAnswer(json string) { e.llm.set(func(f *fakeLLM) { f.tone = json }) }

func TestToneIsLearnedWithTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.toneAnswer(`{"tone": "Матерятся и стебут друг друга, пишут коротко."}`)
	e.topicChatter(5, 7) // the first 4 messages of topic 5 leave the window
	e.tick()

	calls := e.llm.callsOf(llm.RoleTone)
	if len(calls) != 1 || calls[0].prio != llm.Initiative {
		t.Fatalf("one background tone request expected: %+v", calls)
	}
	if u, sys := calls[0].user(), calls[0].system(); !strings.Contains(u, "Прежнее описание тона:\n(нет)\n") ||
		!strings.Contains(u, "Аня: реплика A") || strings.Contains(u, "реплика E") ||
		!strings.Contains(sys, "не длиннее 250 символов") || !strings.Contains(sys, `{"tone": "<описание>"}`) {
		t.Fatalf("tone prompt:\n%s\n---\n%s", sys, u)
	}
	if s := e.summary(5); s.Tone != "Матерятся и стебут друг друга, пишут коротко." || s.Text != "Обсуждали джем." {
		t.Fatalf("tone saved beside the summary: %+v", s)
	}

	// The reply in topic 5 takes the tone up; in another topic it does not carry over.
	e.say(e.inTopic(e.msg("бот, привет"), 5))
	const line = "манера общения: Матерятся и стебут друг друга, пишут коротко — подстраивайся под неё (только в этом топике и в пределах границ)"
	if s := e.llm.callsOf(llm.RoleReply)[0].system(); !strings.Contains(s, "Местные обычаи") || !strings.Contains(s, "- "+line) {
		t.Fatalf("reply in topic 5:\n%s", s)
	}
	e.say(e.inTopic(e.msg("бот, привет"), 7))
	if s := e.llm.callsOf(llm.RoleReply)[1].system(); strings.Contains(s, "Матерятся") {
		t.Fatalf("reply in topic 7 must not see the tone of topic 5:\n%s", s)
	}

	// The evaluation sees it tagged with its topic.
	e.say(e.inTopic(e.msg("ну чё"), 5))
	e.tick()
	fs := e.llm.callsOf(llm.RoleFilter)
	if s := fs[len(fs)-1].system(); !strings.Contains(s, "- [топик 5] "+line) {
		t.Fatalf("evaluation:\n%s", s)
	}

	// The next chunk refines the previous description; an empty answer keeps it.
	e.toneAnswer(`{"tone": ""}`)
	e.topicChatter(5, 4)
	e.tick()
	calls = e.llm.callsOf(llm.RoleTone)
	if len(calls) < 2 || !strings.Contains(calls[len(calls)-1].user(), "Прежнее описание тона:\nМатерятся и стебут") {
		t.Fatalf("the previous tone goes to the next chunk: %d calls", len(calls))
	}
	if s := e.summary(5); s.Tone != "Матерятся и стебут друг друга, пишут коротко." {
		t.Fatalf("an empty answer keeps the tone: %+v", s)
	}
	e.toneAnswer(`{"tone": "Вежливо и по делу."}`)
	e.topicChatter(5, 4)
	e.tick()
	if s := e.summary(5); s.Tone != "Вежливо и по делу." {
		t.Fatalf("the tone is rewritten: %+v", s)
	}
}

func TestToneFailureKeepsTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.toneAnswer("грубовато")
	e.chatter(7)
	e.tick()
	if s := e.summary(0); s.Text != "Обсуждали джем." || s.Tone != "" || len(e.llm.callsOf(llm.RoleTone)) != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestForgetTone(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.st.SetSummary(ctx, chat, 0, storage.Summary{}, "Обсуждали джем.", 10, e.clk.Now())
	if ok, err := e.st.SetTone(ctx, chat, 0, "", "Матерятся."); err != nil || !ok {
		t.Fatal(ok, err)
	}
	e.makeLead(50, "Вася", "vasya")
	e.makeAuthority(60, "Петя")

	e.command(`{"command": "list"}`)
	e.say(e.from(60, "Петя", "", "бот, что ты помнишь?"))
	if sent, _ := e.out.snapshot(); len(sent) != 1 || !strings.HasSuffix(sent[0].text, "Обсуждали джем.\n\nКак здесь принято общаться (сам заметил):\nМатерятся.") {
		t.Fatalf("the list shows the tone: %v", sent)
	}

	e.command(`{"command": "forget_tone"}`)
	e.say(e.from(60, "Петя", "", "бот, забудь, как тут принято общаться"))
	if e.summary(0).Tone == "" {
		t.Fatal("an authority may not erase the tone")
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только админ чата") {
		t.Fatalf("refusal expected: %+v", r)
	}

	in := e.from(50, "Вася", "vasya", "бот, забудь, как тут принято общаться")
	e.say(in)
	if s := e.summary(0); s.Tone != "" || s.Text != "Обсуждали джем." {
		t.Fatalf("the tone is erased, the summary stays: %+v", s)
	}
	if got := e.reactionsTo(in.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("ack expected: %v", got)
	}
	e.say(e.from(50, "Вася", "vasya", "бот, забудь, как тут принято общаться"))
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 2 || !strings.Contains(r[1].user(), "ты и так этого не знаешь") {
		t.Fatalf("nothing to erase: %+v", r)
	}
}

func TestParseTone(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{`{"tone": "  Жёстко.  "}`, "Жёстко.", true},
		{"Вот: {\"tone\": \"\"}", "", true},
		{`{"mood": "x"}`, "", false},
		{`{"tone": 5}`, "", false},
		{"без JSON", "", false},
	} {
		got, err := parseTone(c.in)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("parseTone(%q) = %q, %v", c.in, got, err)
		}
	}
}
