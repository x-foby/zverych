package brain

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

const (
	vasyaID = int64(50)
	petyaID = int64(60)
)

// from builds the next chat message from the given participant.
func (e *env) from(userID int64, name, username, text string) Incoming {
	in := e.msg(text)
	in.UserID, in.UserName, in.Username = userID, name, username
	return in
}

func (e *env) roles() []string {
	e.t.Helper()
	rs, err := e.st.Roles(context.Background(), chat)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Role+":"+r.Label())
	}
	return out
}

func (e *env) lore() []storage.Lore {
	e.t.Helper()
	ls, err := e.st.Lore(context.Background(), chat)
	if err != nil {
		e.t.Fatal(err)
	}
	return ls
}

func (e *env) command(json string) { e.llm.set(func(f *fakeLLM) { f.command = json }) }

func (e *env) makeLead(id int64, name, username string) {
	e.t.Helper()
	if err := e.st.AddLead(context.Background(), chat, storage.Person{ID: id, Name: name, Username: username}, adminID, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) makeAuthority(id int64, name string) {
	e.t.Helper()
	if _, err := e.st.AddAuthority(context.Background(), chat, storage.Person{ID: id, Name: name}, adminID, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func TestAdminAppointsLeadByMention(t *testing.T) {
	e := setup(t)
	e.say(e.from(vasyaID, "Вася", "vasya", "всем привет")) // the bot has seen @vasya
	e.command(`{"command":"set_lead","person":1}`)
	in := e.from(adminID, "Влад", "", "бот, @vasya тут главный")
	e.say(in)

	if got, want := e.roles(), []string{"lead:Вася (@vasya)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles %v want %v", got, want)
	}
	sent, reactions := e.out.snapshot()
	if len(sent) != 0 || !reflect.DeepEqual(reactions, []reaction{{in.MessageID, "🫡"}}) {
		t.Fatalf("a done command is acknowledged with 🫡 only: sent %v reactions %v", sent, reactions)
	}
	if !e.get(in.MessageID).Answered {
		t.Fatal("the instruction counts as answered")
	}
	cmds := e.llm.callsOf(llm.RoleCommand)
	if len(cmds) != 1 || cmds[0].prio != llm.Direct || len(e.llm.callsOf(llm.RoleReply)) != 0 {
		t.Fatalf("command calls %d, reply calls %d", len(cmds), len(e.llm.callsOf(llm.RoleReply)))
	}
	for _, want := range []string{"Автор: Влад, права: создатель бота", "Сообщение: бот, @vasya тут главный", "1. Вася (@vasya)"} {
		if !strings.Contains(cmds[0].user(), want) {
			t.Errorf("command prompt lacks %q:\n%s", want, cmds[0].user())
		}
	}
}

func TestAdminAppointsLeadByReplyWithoutAddressingBot(t *testing.T) {
	e := setup(t)
	v := e.from(vasyaID, "Вася", "vasya", "я тут")
	e.say(v)
	e.initiative()
	e.command(`{"command":"set_lead","person":1}`)
	in := e.from(adminID, "Влад", "", "ты тут главный")
	in.ReplyToID, in.ReplyToUserID, in.ReplyToUser = v.MessageID, vasyaID, storage.Person{Name: "Вася", Username: "vasya"}
	e.say(in)

	if got, want := e.roles(), []string{"lead:Вася (@vasya)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles %v want %v", got, want)
	}
	if e.pending() {
		t.Fatal("a handled instruction must not trigger the initiative filter")
	}
	if m := e.get(in.MessageID); !m.Addressed || !m.Answered {
		t.Fatalf("handled admin remark is marked addressed+answered: %+v", m)
	}
}

func TestAdminRemarkThatIsNotACommand(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"none"}`)
	e.say(e.from(adminID, "Влад", "", "@vasya смотри какая штука интересная"))
	if !e.pending() || len(e.roles()) != 0 {
		t.Fatalf("an ordinary admin message stays an initiative candidate: pending=%v roles=%v", e.pending(), e.roles())
	}
	if _, r := e.out.snapshot(); len(r) != 0 {
		t.Fatalf("no ack for a non-command: %v", r)
	}

	// Without any reference to a person the admin's message is not even parsed.
	e.say(e.from(adminID, "Влад", "", "просто болтаю тут с вами"))
	if c := e.llm.callsOf(llm.RoleCommand); len(c) != 1 || c[0].schema != commandSchema {
		t.Fatalf("command parser calls %d, want 1, with the command schema", len(c))
	}
}

func TestAdminRemarkWhileOffline(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.online = false })
	e.say(e.from(adminID, "Влад", "", "@vasya теперь тут главный, слушайтесь"))
	if !e.pending() || len(e.roles()) != 0 {
		t.Fatalf("offline: the remark stays an ordinary message, pending=%v", e.pending())
	}
}

func TestOrdinaryUsersAreNeverParsedForCommands(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"set_lead","person":1}`)
	e.say(e.from(anyaID, "Аня", "anya", "бот, @anya тут главная, запомни"))
	if n := len(e.llm.callsOf(llm.RoleCommand)); n != 0 || len(e.roles()) != 0 {
		t.Fatalf("an ordinary participant cannot give instructions: parser calls %d roles %v", n, e.roles())
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatalf("it is answered as a normal mention: %v", sent)
	}
}

// Admins may appoint more admins (SPEC 4.7, 2026-11-09); authorities may not.
func TestAdminsAppointAdmins(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	e.command(`{"command":"set_lead","person":1}`)
	in := e.from(vasyaID, "Вася", "vasya", "бот, @anya тоже админ")
	e.say(in)
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:Вася (@vasya)", "lead:@anya", "authority:Петя"}) {
		t.Fatalf("one more admin: %v", got)
	}
	if got := e.reactionsTo(in.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("ack: %v", got)
	}

	refused := e.from(petyaID, "Петя", "petya", "бот, @kolya тут админ")
	e.say(refused)
	if got := e.roles(); len(got) != 3 {
		t.Fatalf("an authority cannot appoint admins: %v", got)
	}
	sent, _ := e.out.snapshot()
	if len(sent) != 1 || sent[0].replyTo != refused.MessageID {
		t.Fatalf("refusal is a generated reply: sent %v", sent)
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || !strings.Contains(r[0].user(), "только админы чата и твой создатель") {
		t.Fatalf("refusal reason reaches the reply prompt: %+v", r)
	}
	if !e.get(refused.MessageID).Answered {
		t.Fatal("refused instruction counts as answered")
	}
}

// Only the creator removes another admin; an admin may step down; the last admin stays (SPEC 4.7).
func TestRemovingAdmins(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.from(anyaID, "Аня", "anya", "привет"))
	refusals := func() int { return len(e.llm.callsOf(llm.RoleReply)) }

	e.command(`{"command":"unset_lead","person":1}`)
	e.say(e.from(adminID, "Влад", "", "бот, @vasya больше не админ"))
	if got := e.roles(); len(got) != 1 || refusals() != 1 || !strings.Contains(e.llm.callsOf(llm.RoleReply)[0].user(), "последнего админа") {
		t.Fatalf("the last admin is not removed: %v", got)
	}

	e.makeLead(anyaID, "Аня", "anya")
	e.command(`{"command":"unset_lead","person":1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, @anya больше не админ"))
	if got := e.roles(); len(got) != 2 || refusals() != 2 || !strings.Contains(e.llm.callsOf(llm.RoleReply)[1].user(), "только твой создатель") {
		t.Fatalf("an admin does not remove another one: %v", got)
	}
	e.command(`{"command":"unset_lead","person":1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, @petya больше не админ"))
	if refusals() != 3 || !strings.Contains(e.llm.callsOf(llm.RoleReply)[2].user(), "и так не админ") {
		t.Fatal("not an admin: refused")
	}

	e.command(`{"command":"unset_lead","self":true}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, я больше не админ"))
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:Аня (@anya)"}) {
		t.Fatalf("an admin steps down: %v", got)
	}

	e.makeLead(vasyaID, "Вася", "vasya")
	e.command(`{"command":"unset_lead","person":1}`)
	e.say(e.from(adminID, "Влад", "", "бот, @anya больше не админ"))
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:Вася (@vasya)"}) {
		t.Fatalf("the creator removes an admin: %v", got)
	}
}

func TestLeadManagesAuthorities(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.from(petyaID, "Петя", "petya", "ку"))

	e.command(`{"command":"add_authority","person":1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, @petya слушай тоже"))
	if got, want := e.roles(), []string{"lead:Вася (@vasya)", "authority:Петя (@petya)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles %v want %v", got, want)
	}

	// An authority cannot appoint other authorities.
	e.command(`{"command":"add_authority","person":1}`)
	e.say(e.from(petyaID, "Петя", "petya", "бот, @anya тоже авторитет"))
	if got := e.roles(); len(got) != 2 {
		t.Fatalf("authority must not appoint: %v", got)
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только админ чата") {
		t.Fatalf("refusal reason: %+v", r)
	}

	// Appointing the lead as authority changes nothing.
	e.command(`{"command":"add_authority","person":1}`)
	e.say(e.from(adminID, "Влад", "", "бот, @vasya авторитет"))
	if got := e.roles(); len(got) != 2 {
		t.Fatalf("lead stays lead: %v", got)
	}

	e.command(`{"command":"remove_authority","person":1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, @petya больше не слушай"))
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:Вася (@vasya)"}) {
		t.Fatalf("authority removed: %v", got)
	}

}

func TestCommandWithoutClearPerson(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"set_lead","person":0}`)
	e.say(e.from(adminID, "Влад", "", "бот, назначь главного"))
	if len(e.roles()) != 0 {
		t.Fatal("nobody to appoint")
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "непонятно кого") {
		t.Fatalf("asks who: %+v", r)
	}
}

func TestSinglePersonIsPickedWhenIndexIsOff(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"set_lead","person":7}`)
	e.say(e.from(adminID, "Влад", "", "бот, @kolya главный"))
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:@kolya"}) {
		t.Fatalf("only candidate wins; unknown @username is stored as is: %v", got)
	}
	// When Коля writes, his id is bound and his messages are tagged as the lead's.
	e.say(e.from(70, "Коля", "kolya", "привет всем, я Коля"))
	rs, _ := e.st.Roles(context.Background(), chat)
	if len(rs) != 1 || rs[0].ID != 70 || rs[0].Name != "Коля" {
		t.Fatalf("role bound to the user: %+v", rs)
	}
}

func TestBotIsNeverAPerson(t *testing.T) {
	e := setup(t)
	in := e.from(adminID, "Влад", "", "@testbot ты тут главный")
	people := e.b.people(context.Background(), storage.Message{ChatID: chat, Text: in.Text}, in.refs(botID))
	if len(people) != 0 {
		t.Fatalf("the bot cannot be appointed: %+v", people)
	}
	in.ReplyToUserID, in.TextMentions = botID, []storage.Person{{ID: botID}, {ID: petyaID, Name: "Петя"}, {ID: petyaID, Name: "Петя"}}
	if got := in.refs(botID); len(got) != 2 {
		t.Fatalf("refs exclude the bot: %+v", got)
	}
	people = e.b.people(context.Background(), storage.Message{ChatID: chat}, in.refs(botID))
	if len(people) != 1 || people[0].ID != petyaID {
		t.Fatalf("duplicates collapse: %+v", people)
	}
}

func TestRememberAndInstruct(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")

	e.command(`{"command":"remember","scope":"chat","text":"Вася — основатель чата"}`)
	m1 := e.from(vasyaID, "Вася", "vasya", "бот, запомни: я основал этот чат")
	e.say(m1)

	e.command(`{"command":"instruct","scope":"topic","text":"Бот не пишет в этом топике"}`)
	m2 := e.from(vasyaID, "Вася", "vasya", "бот, в этой ветке молчи")
	m2.ThreadID = 3
	e.say(m2)

	e.command(`{"command":"instruct","scope":"topic","text":"Бот здоровается с новичками"}`)
	m3 := e.from(vasyaID, "Вася", "vasya", "бот, здоровайся тут с новенькими")
	e.say(m3) // no topic: stored for the whole chat

	ls := e.lore()
	if len(ls) != 3 {
		t.Fatalf("lore %+v", ls)
	}
	if l := ls[0]; !l.ChatWide || l.Kind != storage.LoreFact || l.AuthorID != vasyaID || l.AuthorRole != "lead" || l.SourceMsg != m1.MessageID {
		t.Fatalf("fact %+v", l)
	}
	if l := ls[1]; l.ChatWide || l.ThreadID != 3 || l.Kind != storage.LoreInstruction {
		t.Fatalf("topic instruction %+v", l)
	}
	if !ls[2].ChatWide {
		t.Fatalf("topic scope outside a topic means the whole chat: %+v", ls[2])
	}

	// Empty text is refused.
	e.command(`{"command":"remember","text":"  "}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, запомни"))
	if len(e.lore()) != 3 {
		t.Fatal("nothing to remember")
	}

	// Lore and the leadership show up in later prompts; topic entries only in their topic.
	q := e.from(anyaID, "Аня", "anya", "бот, кто тут главный?")
	q.ThreadID = 5
	e.say(q)
	r := e.llm.callsOf(llm.RoleReply)
	sys := r[len(r)-1].system()
	for _, want := range []string{"админ — Вася (@vasya)", "[весь чат] Вася — основатель чата", "[весь чат] Бот здоровается с новичками", "не травишь"} {
		if !strings.Contains(sys, want) {
			t.Errorf("reply system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "не пишет в этом топике") {
		t.Errorf("topic 3 instruction leaked into topic 5:\n%s", sys)
	}
}

func TestForget(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	ctx := context.Background()
	byLead, _ := e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "от главного", AuthorID: vasyaID, CreatedAt: e.clk.Now()})
	byPetya, _ := e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "от Пети", AuthorID: petyaID, CreatedAt: e.clk.Now()})

	e.command(`{"command":"forget","ids":[` + itoa(byLead) + `]}`)
	e.say(e.from(petyaID, "Петя", "", "бот, забудь про главного"))
	if len(e.lore()) != 2 {
		t.Fatal("an authority cannot delete the lead's entry")
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только свои") {
		t.Fatalf("refusal reason: %+v", r)
	}

	cmds := e.llm.callsOf(llm.RoleCommand)
	if !strings.Contains(cmds[0].user(), itoa(byLead)+". [весь чат] от главного") {
		t.Fatalf("the parser sees stored entries:\n%s", cmds[0].user())
	}

	e.command(`{"command":"forget","ids":[` + itoa(byPetya) + `]}`)
	e.say(e.from(petyaID, "Петя", "", "бот, забудь моё"))
	if ls := e.lore(); len(ls) != 1 || ls[0].ID != byLead {
		t.Fatalf("an authority deletes own entries: %+v", ls)
	}

	e.command(`{"command":"forget","ids":[999,` + itoa(byLead) + `]}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, забудь всё"))
	if len(e.lore()) != 0 {
		t.Fatal("the lead deletes any entry; unknown ids are skipped")
	}

	e.command(`{"command":"forget","ids":[999]}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, забудь то, чего нет"))
	if r := e.llm.callsOf(llm.RoleReply); !strings.Contains(r[len(r)-1].user(), "не нашёл") {
		t.Fatalf("nothing found: %s", r[len(r)-1].user())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestCorrectionReplacesEntry(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	ctx := context.Background()
	old, _ := e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "нужен DB_KICK9", AuthorID: vasyaID, CreatedAt: e.clk.Now()})

	// An authority's correction of the lead's entry adds the new entry but keeps the old one.
	e.command(`{"command":"remember","text":"нужен DB_KICK11","ids":[` + itoa(old) + `]}`)
	e.say(e.from(petyaID, "Петя", "", "бот, нет, нужен DB_KICK11"))
	if ls := e.lore(); len(ls) != 2 {
		t.Fatalf("authority cannot replace someone else's entry: %+v", ls)
	}

	e.command(`{"command":"remember","text":"нужен DB_KICK10","ids":[` + itoa(old) + `]}`)
	in := e.from(vasyaID, "Вася", "vasya", "бот, нет, не так, запомни что нужен DB_KICK10")
	e.say(in)
	ls := e.lore()
	if len(ls) != 2 || ls[0].Text != "нужен DB_KICK11" || ls[1].Text != "нужен DB_KICK10" {
		t.Fatalf("the lead's correction replaces the old entry: %+v", ls)
	}
	if _, r := e.out.snapshot(); r[len(r)-1] != (reaction{in.MessageID, "🫡"}) {
		t.Fatalf("acknowledged: %v", r)
	}
}

func TestListMemory(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	ctx := context.Background()
	e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: "общий факт", CreatedAt: e.clk.Now()})
	e.st.AddLore(ctx, storage.Lore{ChatID: chat, ThreadID: 3, Kind: storage.LoreInstruction, Text: "в топике 3", CreatedAt: e.clk.Now()})
	e.st.AddLore(ctx, storage.Lore{ChatID: chat, ThreadID: 4, Kind: storage.LoreInstruction, Text: "в топике 4", CreatedAt: e.clk.Now()})

	e.command(`{"command":"list"}`)
	in := e.from(petyaID, "Петя", "", "бот, что ты помнишь?")
	in.ThreadID = 3
	e.say(in)
	sent, reactions := e.out.snapshot()
	if len(sent) != 1 || len(reactions) != 0 || sent[0].replyTo != in.MessageID {
		t.Fatalf("list is sent as a reply: %v %v", sent, reactions)
	}
	want := "Админ: Вася (@vasya)\nАвторитеты: Петя\nПомню:\n1. [весь чат] общий факт\n2. [этот топик] в топике 3"
	if sent[0].text != want {
		t.Fatalf("list:\n%s\nwant:\n%s", sent[0].text, want)
	}
	if got := listMemory(chatMemory{}, 0, "", ""); got != "Админ не назначен.\nНичего не просили запомнить." {
		t.Fatalf("empty list %q", got)
	}
	if got := listMemory(chatMemory{}, 0, "Обсуждали джем.", ""); got != "Админ не назначен.\nНичего не просили запомнить.\n\nО чём здесь говорили раньше (сводка):\nОбсуждали джем." {
		t.Fatalf("list with a summary %q", got)
	}
}

func TestUnparsableCommandFallsBackToReply(t *testing.T) {
	for _, answer := range []string{"не знаю", `{"command":"fly"}`, `{"command":"none"}`} {
		e := setup(t)
		e.command(answer)
		e.say(e.from(adminID, "Влад", "", "бот, как дела?"))
		if sent, _ := e.out.snapshot(); len(sent) != 1 || sent[0].text != "ответ бота" {
			t.Fatalf("%q: normal reply expected, sent %v", answer, sent)
		}
	}
}

func TestRankAndRoster(t *testing.T) {
	r := roster{roles: []storage.ChatRole{
		{Person: storage.Person{ID: vasyaID}, Role: storage.RoleAuthority},
		{Person: storage.Person{ID: vasyaID}, Role: storage.RoleLead},
		{Person: storage.Person{Username: "Petya"}, Role: storage.RoleAuthority},
	}}
	for _, c := range []struct {
		id       int64
		username string
		want     rank
	}{
		{adminID, "", rankNone}, {vasyaID, "", rankLead}, {0, "petya", rankAuthority}, {61, "PETYA", rankAuthority},
		{anyaID, "anya", rankNone}, {0, "", rankNone},
	} {
		if got := r.rank(c.id, c.username); got != c.want {
			t.Errorf("rank(%d,%q)=%v want %v", c.id, c.username, got, c.want)
		}
	}
	for rk, want := range map[rank][2]string{
		rankLead: {"админ", "lead"}, rankAuthority: {"авторитет", "authority"}, rankNone: {"", ""},
	} {
		if rk.label() != want[0] || rk.role() != want[1] {
			t.Errorf("rank %d: %q %q", rk, rk.label(), rk.role())
		}
	}
	if !samePerson(storage.Person{ID: 1, Username: "a"}, storage.Person{ID: 1, Username: "b"}) ||
		samePerson(storage.Person{ID: 1, Username: "a"}, storage.Person{ID: 2, Username: "a"}) ||
		samePerson(storage.Person{}, storage.Person{}) {
		t.Error("samePerson: ids win when both known; empty usernames never match")
	}
}

func (e *env) reactionsTo(id int) []string {
	_, rs := e.out.snapshot()
	var out []string
	for _, r := range rs {
		if r.msgID == id {
			out = append(out, r.emoji)
		}
	}
	return out
}

// Bug of 2026-11-08: a follow-up in reply to one's own message made the author the only
// candidate, and the model appointed the admin instead of the person named a message earlier.
func TestFollowUpToOwnMessageIsNotAboutTheAuthor(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"set_lead","person":1}`)
	first := e.from(adminID, "Влад", "vlad", "бот, @evgen_jr тут главный")
	e.say(first)
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:@evgen_jr"}) {
		t.Fatalf("roles %v", got)
	}
	follow := e.from(adminID, "Влад", "vlad", "и его зовут Евгений Евгеньевич")
	follow.ReplyToID, follow.ReplyToUserID, follow.ReplyToUser = first.MessageID, adminID, storage.Person{Name: "Влад", Username: "vlad"}
	if refs := follow.refs(botID); len(refs) != 0 {
		t.Fatalf("replying to oneself refers to nobody: %+v", refs)
	}
	e.say(follow) // the model still says set_lead with person 1 — but there is no such person
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:@evgen_jr"}) {
		t.Fatalf("the lead must not change to the author: %v", got)
	}
	// Catch-up path (people restored from storage) excludes the author too.
	stored := e.get(follow.MessageID)
	if p := e.b.people(context.Background(), *stored, nil); len(p) != 0 {
		t.Fatalf("people from storage: %+v", p)
	}
}

// The creator (admin_ids) appoints the lead but has no other say over the bot (SPEC 12a, 2026-11-09).
func TestCreatorOnlyAppointsLead(t *testing.T) {
	e := setup(t)
	e.command(`{"command":"remember","text":"по пятницам джем"}`)
	in := e.from(adminID, "Влад", "vlad", "бот, запомни: по пятницам джем")
	e.say(in)
	if len(e.lore()) != 0 {
		t.Fatal("the creator without a role cannot teach the bot")
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только от админов чата и авторитетов") {
		t.Fatalf("refusal reason: %+v", r)
	}
	if _, rs := e.out.snapshot(); len(rs) != 0 {
		t.Fatalf("no ack: %v", rs)
	}

	e.command(`{"command":"add_authority","person":1}`)
	e.say(e.from(adminID, "Влад", "vlad", "бот, @petya слушай тоже"))
	if len(e.roles()) != 0 {
		t.Fatalf("nor appoint authorities: %v", e.roles())
	}

	// Nor see the memory (SPEC 3.1, 2026-11-09: it was open to the creator before).
	e.command(`{"command":"list"}`)
	e.say(e.from(adminID, "Влад", "vlad", "бот, что помнишь?"))
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 3 || !strings.Contains(r[2].user(), "только от админов чата") {
		t.Fatalf("list is refused: %+v", r)
	}

	// The creator can make themselves the lead, and then teaches the bot as the lead.
	e.command(`{"command":"set_lead","self":true}`)
	e.say(e.from(adminID, "Влад", "vlad", "бот, я тут админ"))
	if got := e.roles(); !reflect.DeepEqual(got, []string{"lead:Влад (@vlad)"}) {
		t.Fatalf("self-appointment: %v", got)
	}
	e.command(`{"command":"remember","text":"по пятницам джем"}`)
	e.say(e.from(adminID, "Влад", "vlad", "бот, запомни: по пятницам джем"))
	cmds := e.llm.callsOf(llm.RoleCommand)
	if u := cmds[len(cmds)-1].user(); !strings.HasPrefix(u, "Бот: Бот, @testbot\nАвтор: Влад (@vlad), права: админ, создатель бота\n") {
		t.Fatalf("parser sees both powers and the bot's names:\n%s", u)
	}
	if ls := e.lore(); len(ls) != 1 || ls[0].AuthorRole != "lead" {
		t.Fatalf("lore %+v", ls)
	}

	// "self" means nothing outside appointing the lead, and nobody else can appoint themselves.
	e.makeAuthority(petyaID, "Петя")
	e.command(`{"command":"set_lead","self":true}`)
	e.say(e.from(petyaID, "Петя", "petya", "бот, я тут админ"))
	if got := e.roles(); len(got) != 2 || got[0] != "lead:Влад (@vlad)" {
		t.Fatalf("an authority cannot become the lead: %v", got)
	}
}

// Memory changes only when the bot is addressed: a creator's remark that mentions someone may
// only appoint the lead (SPEC 12a, 2026-11-09).
func TestUnaddressedRemarkNeverChangesMemory(t *testing.T) {
	e := setup(t)
	e.makeLead(adminID, "Влад", "vlad")
	e.command(`{"command":"instruct","text":"Бот ставит ❤ на сообщения Васи (@vasya)"}`)
	in := e.from(adminID, "Влад", "vlad", "надо бы, чтобы @vasya получал сердечки от Зверыча")
	e.say(in)
	if len(e.lore()) != 0 || !e.pending() {
		t.Fatalf("talk about the bot stays an ordinary message: lore %+v pending %v", e.lore(), e.pending())
	}
	if m := e.get(in.MessageID); m.Addressed || m.Answered {
		t.Fatalf("not handled: %+v", m)
	}
	if _, rs := e.out.snapshot(); len(rs) != 0 {
		t.Fatalf("no ack: %v", rs)
	}
}

func TestNoReplyInstructionScopeAndRevocation(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.command(`{"command":"instruct","scope":"topic","no_reply":true,"text":"Бот не пишет в топике Обсуждение, даже в ответ на обращения, но читает и запоминает"}`)
	in := e.from(vasyaID, "Вася", "vasya", "бот, в этом топике ничего не пиши, даже если обращаются. Но читай и запоминай")
	in.ThreadID = 3
	e.say(in)
	ls := e.lore()
	if len(ls) != 1 || !ls[0].NoReply || ls[0].ChatWide || ls[0].ThreadID != 3 {
		t.Fatalf("lost prohibition or scope: %+v", ls)
	}
	ctx := context.Background()
	target := storage.Message{ChatID: chat, ThreadID: 3}
	if !e.b.repliesForbidden(ctx, target) {
		t.Fatal("topic must prohibit replies")
	}
	other := target
	other.ThreadID = 5
	if e.b.repliesForbidden(ctx, other) {
		t.Fatal("prohibition leaked to another topic")
	}
	e.command(`{"command":"none"}`)
	addressed := e.msg("бот, ответь мне")
	addressed.ThreadID = 3
	e.say(addressed)
	sent, _ := e.out.snapshot()
	if len(sent) != 0 || !e.get(addressed.MessageID).Answered {
		t.Fatalf("direct reply must be consumed silently: %v", sent)
	}
	if e.get(addressed.MessageID).Text == "" {
		t.Fatal("silent topic must still retain history")
	}
	before := len(e.llm.callsOf(llm.RoleReply))
	e.b.reply(ctx, chat, plannedReply{target: target})
	if len(e.llm.callsOf(llm.RoleReply)) != before {
		t.Fatal("initiative in forbidden topic reached generation")
	}
	if e.b.send(ctx, target, "must not escape") {
		t.Fatal("send bypassed prohibition")
	}
	if _, err := e.st.DeleteLore(ctx, chat, ls[0].ID); err != nil {
		t.Fatal(err)
	}
	if e.b.repliesForbidden(ctx, target) {
		t.Fatal("forgotten instruction still enforced")
	}
}
