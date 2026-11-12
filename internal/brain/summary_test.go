package brain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// smallWindow: the model sees the last 3 messages of a topic; 4 older ones make a chunk.
func smallWindow(c *config.Config) {
	c.Behavior.ContextMessages = 3
	c.Behavior.SummaryChunkMessages = 4
}

func (e *env) summary(topic int) storage.Summary {
	e.t.Helper()
	s, err := e.st.Summary(context.Background(), chat, topic)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) summarizeAnswer(text string) { e.llm.set(func(f *fakeLLM) { f.summary = text }) }

// chatter: n messages from Аня, «реплика A» for message 1, «реплика B» for message 2…
func (e *env) chatter(n int) []int {
	e.t.Helper()
	var ids []int
	for range n {
		in := e.msg("")
		in.Text = "реплика " + string(rune('A'+in.MessageID-1))
		ids = append(ids, in.MessageID)
		e.say(in)
	}
	return ids
}

func TestSummaryFoldsMessagesThatLeftTheWindow(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Сводка: Аня рассказывала про джем.")

	ids := e.chatter(6)
	e.tick()
	if got := e.llm.callsOf(llm.RoleSummary); len(got) != 0 {
		t.Fatalf("3 messages outside the window are not a chunk yet: %d summary calls", len(got))
	}

	ids = append(ids, e.chatter(1)...) // the 4th message leaves the window
	e.tick()
	calls := e.llm.callsOf(llm.RoleSummary)
	if len(calls) != 1 || calls[0].prio != llm.Initiative {
		t.Fatalf("one background summary request expected: %+v", calls)
	}
	u := calls[0].user()
	if !strings.Contains(u, "Прежняя сводка:\n(нет)") || !strings.Contains(u, "— 2 ноября —") ||
		!strings.Contains(u, "реплика A") || !strings.Contains(u, "реплика D") || strings.Contains(u, "реплика E") {
		t.Fatalf("summary prompt must hold the 4 oldest messages only:\n%s", u)
	}
	if !strings.Contains(calls[0].system(), "Не длиннее 1500 символов") {
		t.Fatalf("summary limit missing:\n%s", calls[0].system())
	}
	if s := e.summary(0); s.Text != "Аня рассказывала про джем." || s.UpTo != ids[3] {
		t.Fatalf("summary %+v, want up to %d", s, ids[3])
	}

	// The summary goes to replies and to the evaluation.
	e.say(e.msg("бот, о чём мы тут?"))
	replies := e.llm.callsOf(llm.RoleReply)
	if len(replies) != 1 || !strings.Contains(replies[0].system(), "О чём здесь говорили раньше") ||
		!strings.Contains(replies[0].system(), "Аня рассказывала про джем.") {
		t.Fatalf("reply prompt must carry the summary:\n%s", replies[0].system())
	}
	e.say(e.msg("ещё реплика"))
	e.tick()
	evals := e.llm.callsOf(llm.RoleFilter)
	if last := evals[len(evals)-1].system(); !strings.Contains(last, "Аня рассказывала про джем.") || strings.Contains(last, "[топик") {
		t.Fatalf("evaluation prompt must carry the summary (no topic label in a chat without topics):\n%s", last)
	}

	// The next chunk is folded into the previous summary.
	e.summarizeAnswer("Аня рассказывала про джем, потом спросила бота.")
	e.chatter(2)
	e.tick()
	calls = e.llm.callsOf(llm.RoleSummary)
	if len(calls) != 2 || !strings.Contains(calls[1].user(), "Прежняя сводка:\nАня рассказывала про джем.\n") ||
		strings.Contains(calls[1].user(), "реплика A") {
		t.Fatalf("second chunk must start from the previous summary: %d calls\n%s", len(calls), calls[len(calls)-1].user())
	}
	if s := e.summary(0); s.Text != "Аня рассказывала про джем, потом спросила бота." || s.UpTo <= ids[3] {
		t.Fatalf("summary after the second chunk %+v", s)
	}
}

func TestSummaryIsPerTopic(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("В топике 5 спорили о гитарах.")
	for range 7 {
		e.say(e.inTopic(e.msg("про гитары"), 5))
	}
	e.say(e.inTopic(e.msg("про барабаны"), 9))
	e.tick()
	if s := e.summary(5); s.Text != "В топике 5 спорили о гитарах." {
		t.Fatalf("topic 5 summary %+v", s)
	}
	if s := e.summary(9); s.Text != "" || len(e.llm.callsOf(llm.RoleSummary)) != 1 {
		t.Fatalf("topic 9 has no chunk yet: %+v", s)
	}

	e.say(e.inTopic(e.msg("бот, а ты что думаешь?"), 9))
	if r := e.llm.callsOf(llm.RoleReply); strings.Contains(r[0].system(), "гитарах") {
		t.Fatalf("another topic's summary must stay out of the reply:\n%s", r[0].system())
	}
	e.say(e.inTopic(e.msg("и ещё про гитары"), 5), e.inTopic(e.msg("и ещё про барабаны"), 9))
	e.tick()
	evals := e.llm.callsOf(llm.RoleFilter)
	if last := evals[len(evals)-1].system(); !strings.Contains(last, "[топик 5] В топике 5 спорили о гитарах.") {
		t.Fatalf("evaluation shows the summaries of the batch's topics, labelled:\n%s", last)
	}
}

func TestSummaryKeepsExpiringMessages(t *testing.T) {
	e := setup(t, smallWindow)
	e.summarizeAnswer("Давно обсуждали сведение.")
	old := e.chatter(2) // fewer than a chunk, and inside the window
	e.clk.Advance(29*24*time.Hour + time.Hour)
	within(t, "prune", func() { e.b.prune(context.Background()); e.b.wait() })
	if s := e.summary(0); s.Text != "Давно обсуждали сведение." || s.UpTo != old[1] {
		t.Fatalf("messages pruned within a day are summarized even as a partial chunk: %+v", s)
	}
	if e.get(old[0]) == nil {
		t.Fatal("not pruned yet")
	}

	e.clk.Advance(24 * time.Hour)
	within(t, "prune", func() { e.b.prune(context.Background()); e.b.wait() })
	if e.get(old[0]) != nil || e.summary(0).Text != "Давно обсуждали сведение." {
		t.Fatal("messages are pruned, the summary stays")
	}
	if n := len(e.llm.callsOf(llm.RoleSummary)); n != 1 {
		t.Fatalf("nothing new to summarize: %d calls", n)
	}
}

func TestSummaryNotUpdatedWithoutAnAnswer(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("  ")
	e.chatter(7)
	e.tick()
	if s := e.summary(0); s.UpTo != 0 {
		t.Fatalf("an empty answer must not move the summary on: %+v", s)
	}
	e.llm.set(func(f *fakeLLM) { f.online = false })
	e.chatter(1)
	e.tick()
	if s := e.summary(0); s.UpTo != 0 {
		t.Fatalf("offline: summary untouched %+v", s)
	}
}

func TestForgetSummary(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.st.SetSummary(ctx, chat, 0, storage.Summary{}, "Обсуждали джем.", 10, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	e.makeLead(50, "Вася", "vasya")
	e.makeAuthority(60, "Петя")

	e.command(`{"command": "list"}`)
	e.say(e.from(60, "Петя", "", "бот, что ты помнишь?"))
	if sent, _ := e.out.snapshot(); len(sent) != 1 || !strings.HasSuffix(sent[0].text, "О чём здесь говорили раньше (сводка):\nОбсуждали джем.") {
		t.Fatalf("the list shows the summary: %v", sent)
	}

	e.command(`{"command": "forget_summary"}`)
	e.say(e.from(60, "Петя", "", "бот, забудь, о чём тут говорили"))
	if e.summary(0).Text == "" {
		t.Fatal("an authority may not erase the summary")
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только админ чата") {
		t.Fatalf("refusal expected: %+v", r)
	}

	in := e.from(50, "Вася", "vasya", "бот, забудь, о чём тут говорили")
	e.say(in)
	if s := e.summary(0); s.Text != "" || s.UpTo != 10 {
		t.Fatalf("erased, but the covered messages are not summarized again: %+v", s)
	}
	if got := e.reactionsTo(in.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("ack expected: %v", got)
	}
	e.say(e.from(50, "Вася", "vasya", "бот, забудь, о чём тут говорили"))
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 2 || !strings.Contains(r[1].user(), "сводки старой переписки у тебя и так нет") {
		t.Fatalf("nothing to erase: %+v", r)
	}
}

func TestCleanSummary(t *testing.T) {
	long := strings.Repeat("Тема. ", 400) // 2400 runes
	for _, c := range []struct{ in, want string }{
		{"  Сводка: текст  ", "текст"},
		{"```\nОбновлённая сводка: текст\n```", "текст"},
		{strings.Repeat("я", 1800), strings.Repeat("я", 1800)}, // a bit over the limit is kept
	} {
		if got := cleanSummary(c.in, 1500); got != c.want {
			t.Errorf("cleanSummary(%.30q) = %.30q, want %.30q", c.in, got, c.want)
		}
	}
	got := cleanSummary(long, 1500)
	if n := len([]rune(got)); n > 1500 || !strings.HasSuffix(got, "Тема.") {
		t.Errorf("a much longer summary is cut at a sentence end: %d runes, ends %q", n, got[len(got)-10:])
	}
	if got := cleanSummary(strings.Repeat("я", 2000), 1500); len([]rune(got)) != 1501 || !strings.HasSuffix(got, "…") {
		t.Errorf("no sentence end: cut with an ellipsis, %d runes", len([]rune(got)))
	}
}

func TestSummaryBlock(t *testing.T) {
	sums := map[int]string{0: "общий", 5: "про гитары", 9: ""}
	if got := summaryBlock(sums, 5, roster{}); !strings.Contains(got, "О чём здесь говорили раньше") || !strings.Contains(got, "про гитары") || strings.Contains(got, "общий") {
		t.Errorf("topic block %q", got)
	}
	if got := summaryBlock(sums, 9, roster{}); got != "" {
		t.Errorf("no summary, no block: %q", got)
	}
	if got := summaryBlock(sums, -1, roster{}); !strings.Contains(got, "[вне топиков] общий\n[топик 5] про гитары\n") {
		t.Errorf("evaluation block %q", got)
	}
	if got := summaryBlock(map[int]string{0: "общий"}, -1, roster{}); strings.Contains(got, "[") {
		t.Errorf("a chat without topics needs no labels: %q", got)
	}
}
