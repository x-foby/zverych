package brain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/storage"
)

func TestParseActions(t *testing.T) {
	cases := []struct {
		in   string
		want []action
		ok   bool
	}{
		{`{"actions":[]}`, []action{}, true},
		{`{"actions":[{"id":5,"react":"🔥","reason":"смешно"},{"id":7,"reply":true,"rule":3,"text":"С ДР!"}]}`,
			[]action{{ID: 5, React: "🔥", Reason: "смешно"}, {ID: 7, Reply: true, Rule: 3, Text: "С ДР!"}}, true},
		{"```json\n{\"actions\": [{\"id\": 42, \"reply\": true}]}\n```", []action{{ID: 42, Reply: true}}, true},
		{`Думаю... {"id":7,"react":"🔥"}`, []action{{ID: 7, React: "🔥"}}, true}, // a bare action
		{`{"rules":[{"rule":5,"ids":[1,3],"react":"❤"},{"rule":6,"ids":[2],"reply":true,"text":"С ДР!"}],"actions":[{"id":4,"react":"🔥"}]}`,
			[]action{{ID: 1, React: "❤", Rule: 5, Reason: "указание №5"}, {ID: 3, React: "❤", Rule: 5, Reason: "указание №5"},
				{ID: 2, Reply: true, Rule: 6, Text: "С ДР!", Reason: "указание №6"}, {ID: 4, React: "🔥"}}, true},
		{`{"rules":[],"actions":[]}`, []action{}, true},
		{`{"rules":[{"rule":5,"ids":[1]}]}`, []action{{ID: 1, Rule: 5, Reason: "указание №5"}}, true},
		{`{"reason":"скучно"}`, nil, false},
		{`{"actions": }`, nil, false},
		{`{"actions":[{"id":"7"}]}`, nil, false},
		{`no json here`, nil, false},
	}
	for _, c := range cases {
		got, err := parseActions(c.in)
		if (err == nil) != c.ok {
			t.Fatalf("%q: err=%v, want ok=%v", c.in, err, c.ok)
		}
		if c.ok && !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestCleanReply(t *testing.T) {
	long := strings.Repeat("ж", 4000) // Telegram limit, spelled out on purpose
	for in, want := range map[string]string{
		"Ты: привет":                             "привет",
		"[id=5] Ты: ну да":                       "ну да",
		"[id=12]Ты :   текст":                    "текст",
		"Ты → ответ на id=63: ага":               "ага",
		"[id=9] [топик 7] Ты → ответ на id=3: x": "x",
		"[топик 7] Ты: y":                        "y",
		"[топик 7 «Флуд»] Ты: z":                 "z",
		"[основной топик «Главная»] Ты: w":       "w",
		"Ты → ответ на id=: z":                   "Ты → ответ на id=: z",
		"Тыква: овощ":                            "Тыква: овощ",
		"  \"в кавычках\"  ":                     "в кавычках",
		"\"только начало":                        "\"только начало",
		"только конец\"":                         "только конец\"",
		"\"":                                     "\"",
		"\"\"":                                   "",
		"обычный ответ":                          "обычный ответ",
		"Какой-то текст. [реакции: 🔥]":           "Какой-то текст.",
		"Ты: ага [реакции: ❤×2 🔥] ":              "ага",
		"x [реакции: 👍] [реакции: 🔥]":            "x",
		"\"в кавычках\" [реакции: 😁]":            "в кавычках",
		"[реакции: 🔥] в начале":                  "[реакции: 🔥] в начале",
		"[реакции: 🔥]":                           "",
		"Аня: Ты: не трогать":                    "Аня: Ты: не трогать",
		long:                                     long,
		long + "ы":                               long + "ы",
	} {
		if got := cleanReply(in); got != want {
			t.Errorf("cleanReply(%.40q)=%.40q want %.40q", in, got, want)
		}
	}
}

func TestReactions(t *testing.T) {
	for e, want := range map[string]bool{"❤️": true, "❤": true, "🔥": true, "🤷": true, "💤": false, "": false} {
		if got := isAllowedReaction(e); got != want {
			t.Errorf("isAllowedReaction(%q)=%v", e, got)
		}
	}
	if got := normalizeReaction(" ❤️ "); got != "❤" {
		t.Errorf("normalizeReaction=%q", got)
	}
}

func TestFormatReactions(t *testing.T) {
	rs := []storage.ReactionCount{{Emoji: "🔥", Count: 3}, {Emoji: "❤", Count: 2}, {Emoji: "👍", Count: 1}, {Emoji: "😁", Count: 1}, {Emoji: "🤝", Count: 1}, {Emoji: "🎉", Count: 1}}
	if got, want := formatReactions(rs), " [реакции: 🔥×3 ❤×2 👍 😁 🤝]"; got != want {
		t.Errorf("got %q want %q (at most five kinds)", got, want)
	}
	if got := formatReactions(nil); got != "" {
		t.Errorf("no reactions: %q", got)
	}
}

func TestFormatLine(t *testing.T) {
	limit := strings.Repeat("ж", 600) // spelled out: the test must not follow the constant
	cases := []struct {
		m    storage.Message
		want string
	}{
		{storage.Message{MessageID: 5, UserName: "Аня", Text: "привет"}, "[id=5] Аня: привет"},
		{storage.Message{MessageID: 5, UserName: "Аня", Username: "anya", Text: "привет"}, "[id=5] Аня (@anya): привет"},
		{storage.Message{MessageID: 6, UserName: "Бот", Username: "testbot", IsSelf: true, Text: "ок"}, "[id=6] Ты: ок"},
		{storage.Message{MessageID: 7, UserName: "Аня", ReplyTo: 5, Text: "да"}, "[id=7] Аня → ответ на id=5: да"},
		{storage.Message{MessageID: 8, UserName: "Аня", Text: limit}, "[id=8] Аня: " + limit},
		{storage.Message{MessageID: 9, UserName: "Аня", Text: limit + "ы"}, "[id=9] Аня: " + limit + "…"},
		{storage.Message{MessageID: 10, ThreadID: 3, UserID: 50, UserName: "Вася", Username: "vasya", Text: "тихо"},
			"[id=10] [топик 3] Вася (@vasya) [админ]: тихо"},
		{storage.Message{MessageID: 11, UserID: adminID, UserName: "Влад", Text: "ок"}, "[id=11] Влад: ок"}, // the creator has no say in the chat (SPEC 4.7)
		{storage.Message{MessageID: 12, UserID: 60, UserName: "Петя", Username: "Petya", Text: "да"}, "[id=12] Петя (@Petya) [авторитет]: да"},
		{storage.Message{MessageID: 13, UserID: adminID, UserName: "Бот", IsSelf: true, Text: "я"}, "[id=13] Ты: я"},
	}
	r := roster{roles: []storage.ChatRole{
		{Person: storage.Person{ID: 50, Username: "vasya"}, Role: storage.RoleLead},
		{Person: storage.Person{Username: "petya"}, Role: storage.RoleAuthority}, // id not known yet: matched by username
	}}
	for _, c := range cases {
		if got := formatLine(c.m, r); got != c.want {
			t.Errorf("formatLine(%+.60v)=%.60q want %.60q", c.m, got, c.want)
		}
	}
}

func TestPrompts(t *testing.T) {
	msgs := []storage.Message{{MessageID: 1, UserName: "Аня", Text: "привет"}}

	f := evaluatePrompt(" персона ", chatMemory{}, evalView{fresh: msgs}, replyPolicy{any: true})
	if len(f) != 2 || f[0].Role != "system" || f[1].Role != "user" {
		t.Fatalf("evaluation prompt shape %+v", f)
	}
	if !strings.Contains(f[0].Content, "<persona>\nперсона\n</persona>") || !strings.Contains(f[0].Content, "Реплик не по указанию — не больше одной.") {
		t.Errorf("evaluation system prompt:\n%s", f[0].Content)
	}
	if f[1].Content != "Новые сообщения:\n[id=1] Аня: привет\n" {
		t.Errorf("evaluation user prompt %q", f[1].Content)
	}
	// The emoji hint lists exactly the first 20 reactions: 🏆 is the 20th, 💔 the 21st.
	if !strings.Contains(f[0].Content, "🙏 👌 😍 💯 🤣 ⚡ 🏆;") || strings.Contains(f[0].Content, "💔") {
		t.Errorf("emoji hint in evaluation prompt:\n%s", f[0].Content)
	}
	one := []storage.Message{{MessageID: 2, UserName: "Боря", Text: "и тебе"}, {MessageID: 3, UserName: "Аня", Text: "бот, привет", Addressed: true}}
	if f := evaluatePrompt("p", chatMemory{}, evalView{earlier: msgs, fresh: one}, replyPolicy{only: []int{2}}); f[1].Content !=
		"Ранее в чате:\n[id=1] Аня: привет\n\nНовые сообщения:\n[id=2] Боря: и тебе\n[id=3] Аня: бот, привет [обращение к тебе]\n" ||
		!strings.Contains(f[0].Content, "только в ответ на сообщения id=2, если они продолжают твой разговор") {
		t.Errorf("earlier messages, addressed ones marked, dialog policy:\n%s\n%q", f[0].Content, f[1].Content)
	}
	if f := evaluatePrompt("p", chatMemory{}, evalView{fresh: msgs}, replyPolicy{}); !strings.Contains(f[0].Content, "Реплик не по указанию сейчас не пиши") {
		t.Errorf("no own replies:\n%s", f[0].Content)
	}

	flow := replyContext{around: msgs}
	r := replyPrompt("персона", chatMemory{}, flow, msgs[0], "")
	if r[0].Role != "system" || r[1].Role != "user" || strings.Contains(r[1].Content, "Почему") {
		t.Errorf("reply prompt without reason: %q", r[1].Content)
	}
	want := "Переписка:\n[id=1] Аня: привет\n\nНапиши свою следующую реплику в ответ на сообщение [id=1] от Аня." +
		"\nПочему ты решил вмешаться: смешно" + taskDelivery + "\nВыведи только текст реплики."
	if r := replyPrompt("персона", chatMemory{}, flow, msgs[0], "смешно"); r[1].Content != want {
		t.Errorf("reply prompt %q", r[1].Content)
	}
	// An old target that scrolled out of the history window is shown separately.
	old := storage.Message{MessageID: 0, UserName: "Вася", Text: "давнее"}
	if r := replyPrompt("персона", chatMemory{}, flow, old, ""); !strings.Contains(r[1].Content,
		"[id=1] Аня: привет\n…\nСообщение, на которое ты отвечаешь (раньше в переписке):\n[id=0] Вася: давнее\n") {
		t.Errorf("old target: %q", r[1].Content)
	}

	// In a thread: the thread first (its head, a gap, its tail), then what goes on around it (SPEC 7a).
	line := func(id int, text string) storage.Message {
		return storage.Message{MessageID: id, UserName: "Аня", Text: text}
	}
	thread := []storage.Message{line(1, "корень"), line(5, "хвост"), line(7, "вопрос")}
	rc := replyContext{thread: threadView{root: 1, msgs: thread, cut: 1}, around: []storage.Message{line(6, "рядом")}}
	want = "Ветка разговора (сообщения, связанные ответами), в которой ты отвечаешь:\n[id=1] Аня: корень\n…\n" +
		"[id=5] Аня: хвост\n[id=7] Аня: вопрос\n\nЧто ещё пишут рядом, вне этой ветки (последнее):\n[id=6] Аня: рядом\n" +
		"\nНапиши свою следующую реплику в ответ на сообщение [id=7] от Аня." + taskDelivery + "\nВыведи только текст реплики."
	if r := replyPrompt("персона", chatMemory{}, rc, thread[2], ""); r[1].Content != want {
		t.Errorf("thread prompt:\n%s", r[1].Content)
	}
	rc = replyContext{thread: threadView{root: 1, msgs: thread}}
	if r := replyPrompt("персона", chatMemory{}, rc, thread[2], ""); strings.Contains(r[1].Content, "…") ||
		strings.Contains(r[1].Content, "рядом") || strings.Contains(r[1].Content, "раньше в переписке") {
		t.Errorf("whole thread, nothing around:\n%s", r[1].Content)
	}
}

func TestParseCommand(t *testing.T) {
	c, err := parseCommand("вот:\n```json\n{\"command\": \" Remember \", \"scope\": \"TOPIC\", \"text\": \"x\", \"person\": 2, \"ids\": [3, 4]}\n```")
	if err != nil || c.Command != "remember" || c.Scope != "topic" || c.Text != "x" || c.Person != 2 || len(c.IDs) != 2 {
		t.Fatalf("got %+v %v", c, err)
	}
	for _, bad := range []string{"нет json", `{"command": 5}`, `{"command": "dance"}`} {
		if _, err := parseCommand(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	for _, name := range commands {
		if _, err := parseCommand(`{"command":"` + name + `"}`); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCommandPrompt(t *testing.T) {
	m := storage.Message{ChatID: -1, MessageID: 9, ThreadID: 3, UserID: 50, UserName: "Вася", Username: "vasya", Text: "запомни это"}
	replied := &storage.Message{UserName: "Бот", IsSelf: true, Text: "что запомнить?"}
	lore := []storage.Lore{{ID: 4, ChatWide: true, Text: "общее"}, {ID: 5, ThreadID: 3, Text: "тут"}, {ID: 6, ThreadID: 8, Text: "там"}}
	p := commandPrompt(m, rights(rankLead, false), []string{"Бот", "@testbot"}, replied, []storage.Person{{Name: "Петя"}},
		chatMemory{lore: lore, roster: roster{topics: map[int]string{0: "Главная", 3: "Гитары", 8: "Барахолка"}}})
	want := "Бот: Бот, @testbot\nАвтор: Вася (@vasya), права: админ\nКонтекст — автор отвечает на сообщение (бот): что запомнить?\n" +
		"Сообщение: запомни это\n\nТопики чата:\n1. основной топик «Главная»\n3. «Гитары» (этот)\n8. «Барахолка»\n\n" +
		"Люди в сообщении:\n1. Петя\n\nСохранённые записи:\n4. [весь чат] общее\n5. [этот топик] тут\n6. [топик 8 «Барахолка»] там\n"
	if p[1].Content != want {
		t.Fatalf("command prompt:\n%s\nwant:\n%s", p[1].Content, want)
	}
	if !strings.Contains(p[0].Content, `"set_lead"`) || !strings.Contains(p[0].Content, `"none"`) {
		t.Errorf("system prompt lists commands:\n%s", p[0].Content)
	}
	empty := commandPrompt(m, rights(rankNone, true), nil, nil, nil, chatMemory{})[1].Content
	if !strings.Contains(empty, "(никого)") || !strings.Contains(empty, "(нет)") || strings.Contains(empty, "Контекст") || strings.Contains(empty, "Бот:") ||
		strings.Contains(empty, "Топики чата") {
		t.Errorf("empty lists:\n%s", empty)
	}
	if got := truncate("абв", 2); got != "аб…" {
		t.Errorf("truncate=%q", got)
	}
}

func TestSystemBlockMemory(t *testing.T) {
	mem := chatMemory{
		roster: roster{roles: []storage.ChatRole{
			{Person: storage.Person{Name: "Вася"}, Role: storage.RoleLead},
			{Person: storage.Person{Name: "Петя"}, Role: storage.RoleAuthority},
			{Person: storage.Person{Username: "kolya"}, Role: storage.RoleAuthority},
		}},
		lore: []storage.Lore{
			{ID: 1, ChatWide: true, Kind: storage.LoreInstruction, Text: "не матерись"},
			{ID: 2, ThreadID: 3, Kind: storage.LoreInstruction, Text: "в ветке 3 молчи"},
			{ThreadID: 4, Kind: storage.LoreFact, Text: "ветка 4 про гитары"},
			{ThreadID: 6, Kind: storage.LoreFact, Text: "ветка 6 про барабаны"},
		},
		topics: []int{3, 4},
	}
	all := systemBlock("p", mem, -1)
	for _, want := range []string{"админ — Вася, авторитеты — Петя, @kolya", "- №1 [весь чат] не матерись", "- №2 [топик 3] в ветке 3 молчи",
		"Что ты знаешь о чате:\n- [топик 4] ветка 4 про гитары"} {
		if !strings.Contains(all, want) {
			t.Errorf("filter view lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "барабаны") {
		t.Errorf("the filter sees only the topics in question:\n%s", all)
	}
	t3 := systemBlock("p", mem, 3)
	if !strings.Contains(t3, "- №2 [этот топик] в ветке 3 молчи") || strings.Contains(t3, "гитары") {
		t.Errorf("topic 3 view:\n%s", t3)
	}
	if none := systemBlock("p", chatMemory{}, 0); strings.Contains(none, "админ —") || !strings.Contains(none, "Руководство чата не назначено") || strings.Contains(none, "Указания руководства,") ||
		strings.Contains(none, "Что ты знаешь") {
		t.Errorf("no roles, no lore:\n%s", none)
	}

	var many chatMemory
	for i := range 45 { // 45 × 150 characters: the oldest 5 do not fit loreBudget
		many.lore = append(many.lore, storage.Lore{ChatWide: true, Kind: storage.LoreFact, Text: fmt.Sprintf("%03d", i) + strings.Repeat("ф", 147)})
	}
	s := systemBlock("p", many, 0)
	if n := strings.Count(s, "- [весь чат]"); n != 40 || strings.Contains(s, "- [весь чат] 004") || !strings.Contains(s, "- [весь чат] 005") {
		t.Errorf("only the newest entries that fit: got %d", n)
	}
}

func TestParseThread(t *testing.T) {
	for in, want := range map[string]int{`{"thread": 12}`: 12, "```json\n{\"thread\":0}\n```": 0} {
		if got, err := parseThread(in); err != nil || got != want {
			t.Errorf("parseThread(%q) = %d %v", in, got, err)
		}
	}
	for _, in := range []string{"нет", `{"thread": "x"}`, `{}`} {
		if _, err := parseThread(in); err == nil {
			t.Errorf("parseThread(%q) must fail", in)
		}
	}
}

func TestParseAssignments(t *testing.T) {
	got := parseAssignments(`ок {"actions": [], "threads": [{"id": 5, "thread": 2}]}`)
	if !reflect.DeepEqual(got, []assignment{{5, 2}}) {
		t.Errorf("assignments: %v", got)
	}
	for _, in := range []string{`{"actions": []}`, "нет", `{"threads": 5}`} {
		if got := parseAssignments(in); len(got) != 0 {
			t.Errorf("parseAssignments(%q) = %v", in, got)
		}
	}
}

func TestEvaluatePromptThreads(t *testing.T) {
	msgs := []storage.Message{{MessageID: 1, UserName: "Аня", Text: "привет"}}
	if f := evaluatePrompt("p", chatMemory{}, evalView{fresh: msgs}, replyPolicy{any: true}); strings.Contains(f[0].Content, `"threads"`) ||
		strings.Contains(f[1].Content, "Ветк") {
		t.Errorf("no threads, no thread instructions:\n%s\n%s", f[0].Content, f[1].Content)
	}
	th := threadView{root: 1, topic: 3, cut: 1, msgs: []storage.Message{
		{MessageID: 1, ThreadID: 3, UserName: "Аня", Text: "корень"},
		{MessageID: 5, ThreadID: 3, UserName: "Боря", Text: "старое", ReplyTo: 1},
		{MessageID: 7, ThreadID: 3, UserName: "Аня", Text: "бот?", ReplyTo: 5, Addressed: true}}}
	v := evalView{threads: []threadView{th}, isNew: func(m storage.Message) bool { return m.MessageID >= 7 }}
	want := "Ветки разговора:\n\nВетка №1 [топик 3]:\n[id=1] [топик 3] Аня: корень\n…\n[id=5] [топик 3] Боря → ответ на id=1: старое\n" +
		"[id=7] [топик 3] Аня → ответ на id=5: бот? [новое] [обращение к тебе]\n\nВне веток новых сообщений нет.\n"
	if f := evaluatePrompt("p", chatMemory{}, v, replyPolicy{any: true}); f[1].Content != want || !strings.Contains(f[0].Content, `"threads"`) {
		t.Errorf("threads only:\n%s", f[1].Content)
	}
}
