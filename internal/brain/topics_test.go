package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func (e *env) topicNames() map[int]string {
	e.t.Helper()
	names, err := e.st.TopicNames(context.Background(), chat)
	if err != nil {
		e.t.Fatal(err)
	}
	return names
}

// The bot learns topic names from Telegram's updates and shows them to the model.
func TestTopicNamesAreLearnedAndShown(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.b.OnTopic(ctx, Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: " Флуд "})
	e.b.OnTopic(ctx, Topic{ChatID: chat, ChatType: "private", ThreadID: 8, Name: "Личка"}) // not a group
	e.b.OnTopic(ctx, Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 9, Name: "  "})
	e.b.OnTopic(ctx, Topic{ChatID: -999, ChatType: "supergroup", ThreadID: 7, Name: "Чужая"}) // an unknown group
	in := e.msg("всем привет")
	in.ThreadID, in.TopicName = 4, "Гитары" // from a message in the topic
	e.say(in)
	if got := e.topicNames(); len(got) != 2 || got[7] != "Флуд" || got[4] != "Гитары" {
		t.Fatalf("names %v", got)
	}
	e.b.OnTopic(ctx, Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: "Болталка"}) // renamed
	if got := e.topicNames(); got[7] != "Болталка" {
		t.Fatalf("renamed: %v", got)
	}

	q := e.msg("@TestBot как называется этот топик?")
	q.ThreadID = 7
	e.say(q)
	r := e.llm.callsOf(llm.RoleReply)
	c := r[len(r)-1]
	for _, want := range []string{"Разговор идёт в топике «Болталка» (топик 7 «Болталка»).",
		"Топики чата, названия которых ты знаешь: «Гитары», «Болталка».", "ответь на сам вопрос прямо и коротко"} {
		if !strings.Contains(c.system(), want) {
			t.Errorf("reply system prompt lacks %q:\n%s", want, c.system())
		}
	}
	if want := "[id=" + itoa(int64(q.MessageID)) + "] [топик 7 «Болталка»] Аня: @TestBot"; !strings.Contains(c.user(), want) {
		t.Errorf("history lacks %q:\n%s", want, c.user())
	}

	// A topic whose name the bot has not learned yet: it says it does not know.
	q = e.msg("@TestBot как называется этот топик?")
	q.ThreadID = 11
	e.say(q)
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "Разговор идёт в топике 11; его название тебе пока неизвестно") {
		t.Errorf("unknown name:\n%s", sys)
	}
}

// Off groups keep nothing, topic names included.
func TestTopicNamesOfOffGroupsAreNotKept(t *testing.T) {
	e := setup(t)
	if _, err := e.st.SetChatState(context.Background(), chat, storage.ChatOff, storage.ModeTalk); err != nil {
		t.Fatal(err)
	}
	e.b.dropChat(chat)
	e.b.OnTopic(context.Background(), Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: "Флуд"})
	in := e.msg("привет")
	in.ThreadID, in.TopicName = 4, "Гитары"
	e.say(in)
	if got := e.topicNames(); len(got) != 0 {
		t.Fatalf("names %v", got)
	}
}

// An instruction about another topic, named by its name, is stored for that topic.
func TestInstructionForANamedTopic(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.b.OnTopic(context.Background(), Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: "Барахолка"})

	e.command(`{"command":"instruct","scope":"topic","topic":7,"text":"Бот не пишет в Барахолке"}`)
	m := e.from(vasyaID, "Вася", "vasya", "бот, в Барахолке не пиши")
	m.ThreadID = 3
	e.say(m)
	p := e.llm.callsOf(llm.RoleCommand)
	if u := p[len(p)-1].user(); !strings.Contains(u, "Топики чата:\n7. «Барахолка»\n") {
		t.Errorf("the parser sees the topics:\n%s", u)
	}
	// Outside topics, too.
	e.command(`{"command":"remember","scope":"chat","topic":7,"text":"В Барахолке продают гитары"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, запомни: в Барахолке продают гитары"))
	// A topic the bot does not know: refused, nothing stored.
	e.command(`{"command":"instruct","scope":"topic","topic":12,"text":"Бот молчит в Правилах"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, в Правилах молчи"))

	ls := e.lore()
	if len(ls) != 2 || ls[0].ChatWide || ls[0].ThreadID != 7 || ls[1].ChatWide || ls[1].ThreadID != 7 {
		t.Fatalf("lore %+v", ls)
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || !strings.Contains(r[0].user(), "не знаешь, какой топик имеют в виду") {
		t.Fatalf("an unknown topic is refused: %+v", r)
	}

	// The list names the topic.
	e.command(`{"command":"list"}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, что помнишь?"))
	e.command(`{"command":"none"}`)
	q := e.from(anyaID, "Аня", "anya", "бот, продам гитару")
	q.ThreadID = 7
	e.say(q)
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "[этот топик] Бот не пишет в Барахолке") {
		t.Errorf("the instruction is shown in its topic:\n%s", sys)
	}
}

// The admin names a topic in a private chat.
func TestPrivateInstructionForANamedTopic(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.b.OnTopic(context.Background(), Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: "Барахолка"})
	e.say(e.from(anyaID, "Аня", "anya", "привет"))

	e.command(`{"command":"none"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "привет"))
	e.command(`{"command":"instruct","topic":7,"text":"Бот не пишет в Барахолке"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "в Барахолке не пиши"))
	if ls := e.lore(); len(ls) != 1 || ls[0].ChatWide || ls[0].ThreadID != 7 {
		t.Fatalf("lore %+v", ls)
	}
	p := e.llm.callsOf(llm.RoleCommand)
	if u := p[len(p)-1].user(); !strings.Contains(u, "Топики группы 1:\n7. «Барахолка»\n") {
		t.Errorf("the parser sees the group's topics:\n%s", u)
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) == 0 || !strings.Contains(r[0].system(), "Топики чата, названия которых ты знаешь: «Барахолка».") ||
		!strings.Contains(r[0].system(), "ответь на сам вопрос прямо и коротко") {
		t.Fatalf("the private reply knows the topics and its limits: %+v", r)
	}
}

// In a group with topics, messages without a thread id are the general topic's: the bot knows it
// is a topic, by default called General, and learns its name when it is renamed.
func TestGeneralTopic(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	general := func(in Incoming) Incoming { in.IsForum = true; return in }
	e.say(general(e.msg("всем привет")))
	if got := e.topicNames(); len(got) != 1 || got[0] != "" {
		t.Fatalf("the general topic, no name yet: %v", got)
	}
	q := general(e.msg("@TestBot как называется этот топик?"))
	e.say(q)
	r := e.llm.callsOf(llm.RoleReply)
	c := r[len(r)-1]
	if !strings.Contains(c.system(), "Разговор идёт в основном топике чата. По умолчанию он называется «General»") {
		t.Errorf("reply system prompt:\n%s", c.system())
	}
	if want := "[id=" + itoa(int64(q.MessageID)) + "] [основной топик] Аня: @TestBot"; !strings.Contains(c.user(), want) {
		t.Errorf("history lacks %q:\n%s", want, c.user())
	}

	// Renamed (Telegram tells it without a thread id).
	e.b.OnTopic(context.Background(), Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 0, Name: "Главная"})
	e.b.OnTopic(context.Background(), Topic{ChatID: chat, ChatType: "supergroup", ThreadID: 7, Name: "Барахолка"})
	e.say(general(e.msg("@TestBot а теперь?")))
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "Разговор идёт в основном топике чата — «Главная».") ||
		!strings.Contains(sys, "Топики чата, названия которых ты знаешь: «Главная» (основной), «Барахолка».") {
		t.Errorf("renamed:\n%s", sys)
	}

	// An instruction for this topic in the general one is the general topic's; named from another topic — too.
	e.command(`{"command":"instruct","scope":"topic","text":"Бот не пишет в основном топике"}`)
	e.say(general(e.from(vasyaID, "Вася", "vasya", "бот, в этом топике не пиши")))
	e.command(`{"command":"remember","scope":"topic","topic":1,"text":"В Главной — объявления"}`)
	m := general(e.from(vasyaID, "Вася", "vasya", "бот, запомни: в Главной — объявления"))
	m.ThreadID = 7
	e.say(m)
	p := e.llm.callsOf(llm.RoleCommand)
	if u := p[len(p)-1].user(); !strings.Contains(u, "Топики чата:\n1. основной топик «Главная»\n7. «Барахолка» (этот)\n") {
		t.Errorf("the parser sees the general topic:\n%s", u)
	}
	ls := e.lore()
	if len(ls) != 2 || ls[0].ChatWide || ls[0].ThreadID != 0 || ls[1].ChatWide || ls[1].ThreadID != 0 {
		t.Fatalf("lore %+v", ls)
	}
	e.command(`{"command":"none"}`)
	q = general(e.msg("бот, продам гитару"))
	q.ThreadID = 7
	e.say(q)
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); strings.Contains(sys, "не пишет в основном топике") {
		t.Errorf("the general topic's instruction leaked into another topic:\n%s", sys)
	}
	e.say(general(e.msg("бот, что тут?")))
	r = e.llm.callsOf(llm.RoleReply)
	if sys := r[len(r)-1].system(); !strings.Contains(sys, "[этот топик] Бот не пишет в основном топике") {
		t.Errorf("the instruction is shown in the general topic:\n%s", sys)
	}

	// Topics turned off: no general topic any more.
	e.say(e.msg("топики выключили"))
	if got := e.topicNames(); len(got) != 1 || got[7] != "Барахолка" {
		t.Fatalf("the general topic is gone, the others stay: %v", got)
	}
}
