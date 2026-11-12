package brain

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func (e *env) loreAnswer(json string) { e.llm.set(func(f *fakeLLM) { f.lore = json }) }

// topicChatter: n messages from Аня in the topic.
func (e *env) topicChatter(thread, n int) {
	e.t.Helper()
	for range n {
		in := e.inTopic(e.msg(""), thread)
		in.Text = "реплика " + string(rune('A'+in.MessageID-1))
		e.say(in)
	}
}

func TestLoreIsPickedOutWithTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.makeLead(vasyaID, "Вася", "vasya")
	ctx := context.Background()
	byLead, _ := e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "Мы — клуб любителей джема",
		AuthorID: vasyaID, AuthorRole: storage.RoleLead, CreatedAt: e.clk.Now()})
	e.loreAnswer(`{"lore": [
		{"id": 0, "scope": "topic", "text": "«Джем так джем» — местная присказка: значит, спор окончен", "msg": 3},
		{"id": 0, "text": "  "},
		{"id": 0, "text": "мы — клуб любителей джема."},
		{"id": ` + itoa(byLead) + `, "text": "Мы — клуб любителей варенья"},
		{"id": 0, "scope": "chat", "text": "Созвон по пятницам", "msg": 999}]}`)

	e.say(e.msg("я принесу варенье"), e.from(vasyaID, "Вася", "vasya", "лучше джем"), e.msg("джем так джем"))
	e.chatter(4) // the first 4 messages leave the window
	e.tick()

	calls := e.llm.callsOf(llm.RoleLore)
	if len(calls) != 1 || calls[0].prio != llm.Initiative {
		t.Fatalf("one background lore request expected: %+v", calls)
	}
	u, sys := calls[0].user(), calls[0].system()
	if !strings.Contains(u, "№"+itoa(byLead)+" [руководство] [весь чат] Мы — клуб любителей джема\n") ||
		!strings.Contains(u, "Вася (@vasya) [админ]: лучше джем") || !strings.Contains(u, "Бот (это ты): Бот, @testbot") ||
		strings.Contains(u, "реплика E") || strings.Contains(u, "Сообщения из топика") {
		t.Fatalf("lore prompt must hold the memory and the chunk:\n%s", u)
	}
	if !strings.Contains(sys, "не длиннее 200 символов") || !strings.Contains(sys, "Новых записей — не больше 3") ||
		strings.Contains(sys, "Область записи") {
		t.Fatalf("lore limits (no scope choice in a chat without topics):\n%s", sys)
	}

	ls := e.lore()
	if len(ls) != 3 || ls[0].Text != "Мы — клуб любителей джема" {
		t.Fatalf("the leadership's entry stays, empty and duplicate ones are skipped: %+v", ls)
	}
	joke, call := ls[1], ls[2]
	if !joke.Inferred() || joke.AuthorID != 0 || !joke.ChatWide || joke.SourceMsg != 3 || joke.Kind != storage.LoreFact ||
		joke.Text != "«Джем так джем» — местная присказка: значит, спор окончен" {
		t.Fatalf("inferred entry %+v (chat-wide in a chat without topics)", joke)
	}
	if call.Text != "Созвон по пятницам" || call.SourceMsg != 4 {
		t.Fatalf("a source outside the chunk becomes its last message: %+v", call)
	}

	// Inferred lore goes to replies, apart from the leadership's.
	e.say(e.msg("бот, что у нас за традиции?"))
	rs := e.llm.callsOf(llm.RoleReply)
	if s := rs[0].system(); !strings.Contains(s, "Что ты знаешь о чате:\n- [весь чат] Мы — клуб любителей джема\n") ||
		!strings.Contains(s, "Что ты сам заметил в чате (местные шутки, порядки, традиции; руководство важнее):\n- [весь чат] «Джем так джем»") {
		t.Fatalf("reply prompt:\n%s", s)
	}

	// The next chunk sees the bot's entries as its own and may rewrite them.
	e.loreAnswer(`{"lore": [{"id": ` + itoa(call.ID) + `, "text": "Созвон по субботам", "msg": 5}]}`)
	e.chatter(4)
	e.tick()
	calls = e.llm.callsOf(llm.RoleLore)
	if !strings.Contains(calls[len(calls)-1].user(), "№"+itoa(call.ID)+" [твоя] [весь чат] Созвон по пятницам\n") {
		t.Fatalf("own entries are marked:\n%s", calls[len(calls)-1].user())
	}
	if ls := e.lore(); len(ls) != 3 || ls[2].ID != call.ID || ls[2].Text != "Созвон по субботам" || ls[2].SourceMsg != 5 {
		t.Fatalf("the entry is rewritten in place: %+v", ls)
	}

	// The admin sees and forgets them like any other.
	e.command(`{"command":"list"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, что ты помнишь?"))
	sent, _ := e.out.snapshot()
	if out := sent[len(sent)-1].text; !strings.Contains(out, itoa(joke.ID)+". [весь чат] (сам заметил) «Джем так джем»") ||
		!strings.Contains(out, itoa(byLead)+". [весь чат] Мы — клуб") {
		t.Fatalf("list:\n%s", out)
	}
}

func TestLoreScopeInTopics(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.loreAnswer(`{"lore": [
		{"id": 0, "scope": "topic", "text": "В этом топике выкладывают рецепты"},
		{"id": 0, "scope": "chat", "text": "Чат основан в 2020 году"}]}`)
	e.topicChatter(5, 7)
	e.tick()

	calls := e.llm.callsOf(llm.RoleLore)
	if len(calls) != 1 || !strings.Contains(calls[0].user(), "Сообщения из топика 5.") ||
		!strings.Contains(calls[0].system(), `"topic" — относится к этому топику`) {
		t.Fatalf("a topic's chunk offers the scope choice: %d calls", len(calls))
	}
	ls := e.lore()
	if len(ls) != 2 || ls[0].ChatWide || ls[0].ThreadID != 5 || !ls[1].ChatWide || ls[1].ThreadID != 0 {
		t.Fatalf("scopes: %+v", ls)
	}

	// Another topic sees the entry of topic 5 and makes it chat-wide.
	e.loreAnswer(`{"lore": [{"id": ` + itoa(ls[0].ID) + `, "scope": "chat", "text": "Во всех топиках выкладывают рецепты"},
		{"id": ` + itoa(ls[1].ID) + `, "scope": "topic", "text": "Чат основан в 2021 году"}]}`)
	e.topicChatter(7, 7)
	e.tick()
	calls = e.llm.callsOf(llm.RoleLore)
	if u := calls[len(calls)-1].user(); !strings.Contains(u, "[твоя] [топик 5] В этом топике") {
		t.Fatalf("entries of other topics are labelled:\n%s", u)
	}
	ls = e.lore()
	if !ls[0].ChatWide || ls[0].ThreadID != 0 || ls[0].Text != "Во всех топиках выкладывают рецепты" {
		t.Fatalf("promoted to the whole chat: %+v", ls[0])
	}
	if ls[1].ChatWide || ls[1].ThreadID != 7 {
		t.Fatalf("a chat-wide entry narrowed to a topic goes to the chunk's topic: %+v", ls[1])
	}

	// The reply in topic 5 no longer sees topic 7's entry.
	e.say(e.inTopic(e.msg("бот, привет"), 5))
	if s := e.llm.callsOf(llm.RoleReply)[0].system(); !strings.Contains(s, "[весь чат] Во всех топиках") || strings.Contains(s, "основан") {
		t.Fatalf("reply in topic 5:\n%s", s)
	}
}

func TestLoreLimits(t *testing.T) {
	e := setup(t, smallWindow, func(c *config.Config) { c.Behavior.LoreMaxEntries = 4 })
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.loreAnswer(`{"lore": [{"text": "раз"}, {"text": "два"}, {"text": "три"}, {"text": "четыре"}, {"text": "` +
		strings.Repeat("длинно. ", 60) + `"}]}`)
	e.chatter(7)
	e.tick()
	if ls := e.lore(); len(ls) != 3 {
		t.Fatalf("at most 3 new entries per chunk: %+v", ls)
	}

	e.loreAnswer(`{"lore": [{"text": "` + strings.Repeat("длинно. ", 60) + `"}, {"text": "пять"}]}`)
	e.chatter(4)
	e.tick()
	ls := e.lore()
	if len(ls) != 4 || len([]rune(ls[3].Text)) > 200 {
		t.Fatalf("at most lore_max_entries, each clipped: %+v", ls)
	}
	ctx := context.Background()
	e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "от админа", AuthorID: vasyaID,
		AuthorRole: storage.RoleLead, CreatedAt: e.clk.Now()})

	e.loreAnswer(`{"lore": [{"id": ` + itoa(ls[0].ID) + `, "text": "раз, уточнено"}]}`)
	e.chatter(4)
	e.tick()
	calls := e.llm.callsOf(llm.RoleLore)
	if !strings.Contains(calls[len(calls)-1].system(), "Места для новых записей нет") {
		t.Fatalf("a full memory allows rewrites only:\n%s", calls[len(calls)-1].system())
	}
	if ls := e.lore(); len(ls) != 5 || ls[0].Text != "раз, уточнено" {
		t.Fatalf("the leadership's entries do not count, rewrites still work: %+v", ls)
	}
}

func TestLoreFailureKeepsTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.loreAnswer("ничего")
	e.chatter(7)
	e.tick()
	if s := e.summary(0); s.Text != "Обсуждали джем." || len(e.lore()) != 0 || len(e.llm.callsOf(llm.RoleLore)) != 1 {
		t.Fatalf("summary %+v, lore %+v", s, e.lore())
	}
}

func TestAuthorityForgetsInferredLore(t *testing.T) {
	e := setup(t)
	e.makeAuthority(petyaID, "Петя")
	id, _ := e.st.AddLore(context.Background(), storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact,
		Text: "Петя — главный по джему", AuthorRole: storage.LoreBot, CreatedAt: e.clk.Now()})
	e.command(`{"command":"forget","ids":[` + itoa(id) + `]}`)
	e.say(e.from(petyaID, "Петя", "", "бот, забудь, что я главный по джему"))
	if len(e.lore()) != 0 {
		t.Fatal("the leadership outranks what the bot inferred")
	}
	if u := e.llm.callsOf(llm.RoleCommand)[0].user(); !strings.Contains(u, itoa(id)+". [весь чат] (сам заметил) Петя") {
		t.Fatalf("the parser sees which entries the bot inferred:\n%s", u)
	}
}

func TestPromptLorePrefersTheLeadership(t *testing.T) {
	entry := func(id int64, role, kind string, thread int, chars int) storage.Lore {
		return storage.Lore{ID: id, ChatWide: thread < 0, ThreadID: max(thread, 0), Kind: kind, AuthorRole: role,
			Text: strings.Repeat("ё", chars)}
	}
	var lore []storage.Lore
	for i := range 20 { // 20 instructions, older than everything else: never cut
		lore = append(lore, entry(int64(i+1), storage.RoleLead, storage.LoreInstruction, -1, 200))
	}
	for i := range 20 { // leadership facts 21..40: 4000 of the 6000 characters
		lore = append(lore, entry(int64(21+i), storage.RoleAuthority, storage.LoreFact, -1, 200))
	}
	for i := range 15 { // the bot's entries 41..55: the newest 10 fit, then 41..45 are cut
		lore = append(lore, entry(int64(41+i), storage.LoreBot, storage.LoreFact, -1, 200))
	}
	lore = append(lore,
		entry(56, storage.RoleLead, storage.LoreInstruction, 3, 10), // other topics
		entry(57, storage.RoleLead, storage.LoreFact, 7, 10),
		entry(58, storage.LoreBot, storage.LoreFact, 5, 0)) // this topic, empty: costs nothing

	got := promptLore(lore, 5)
	var ids []int64
	for _, l := range got {
		ids = append(ids, l.ID)
	}
	var want []int64
	for id := int64(1); id <= 40; id++ {
		want = append(want, id)
	}
	for id := int64(46); id <= 55; id++ {
		want = append(want, id)
	}
	want = append(want, 58)
	if !slices.Equal(ids, want) {
		t.Fatalf("every instruction, the leadership's facts, the newest inferred that fit, in id order:\n got %v\nwant %v", ids, want)
	}

	// Leadership facts beyond the budget: the newest of them win, the bot's entries are left out.
	var flood []storage.Lore
	for i := range 40 {
		flood = append(flood, entry(int64(i+1), storage.RoleLead, storage.LoreFact, -1, 200))
	}
	flood = append(flood, entry(41, storage.LoreBot, storage.LoreFact, -1, 1), entry(42, storage.RoleLead, storage.LoreInstruction, -1, 300))
	got = promptLore(flood, 0)
	if len(got) != 31 || got[0].ID != 11 || got[len(got)-1].ID != 42 {
		t.Fatalf("the newest 30 facts and the instruction: %d entries, %d..%d", len(got), got[0].ID, got[len(got)-1].ID)
	}

	if got := promptLore(lore[55:], 3, 7); len(got) != 2 || got[0].ID != 56 || got[1].ID != 57 {
		t.Fatalf("the evaluation sees the topics in question: %+v", got)
	}
	if got := promptLore(lore[55:]); len(got) != 0 {
		t.Fatalf("no topics, no topic entries: %+v", got)
	}
}
