package brain

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

const studio = int64(-300) // «Звукарня»: sorts before «Тестовая группа»

// dm builds the next private message from the given person.
func (e *env) dm(userID int64, name, username, text string) Incoming {
	return Incoming{ChatID: userID, ChatType: "private", MessageID: int(e.ids.Add(1)), UserID: userID,
		UserName: name, Username: username, Text: text, Date: e.clk.Now()}
}

// dmTexts are the bot's messages to the person's private chat.
func (e *env) dmTexts(userID int64) []string {
	sent, _ := e.out.snapshot()
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	var out []string
	for i, s := range sent {
		if e.out.sentTo[i] == userID {
			out = append(out, s.text)
		}
	}
	return out
}

func (e *env) lastDM(userID int64) string {
	e.t.Helper()
	ts := e.dmTexts(userID)
	if len(ts) == 0 {
		e.t.Fatal("no private message")
	}
	return ts[len(ts)-1]
}

func (e *env) buttons() []sentButtons {
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	return append([]sentButtons(nil), e.out.buttons...)
}

func (e *env) edits() []sentMsg {
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	return append([]sentMsg(nil), e.out.edits...)
}

// press presses a button as the given person.
func (e *env) press(userID int64, data string) {
	e.t.Helper()
	within(e.t, "callback", func() {
		e.b.OnCallback(context.Background(), Callback{ID: data, UserID: userID, ChatID: userID, MessageID: 999, Data: data})
		e.b.wait()
	})
}

func (e *env) studio(state, mode string) {
	e.t.Helper()
	if err := e.st.RegisterChat(context.Background(), studio, "Звукарня", anyaID, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
	if state != storage.ChatOff {
		e.setGroup(studio, state, mode)
	}
}

func (e *env) rolesIn(id int64) []string {
	e.t.Helper()
	rs, err := e.st.Roles(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Role+":"+r.Label())
	}
	return out
}

// Only the creator and the admins of active groups may write to the bot; the rest are ignored.
func TestStrangersArePrivatelyIgnored(t *testing.T) {
	e := setup(t)
	e.makeAuthority(petyaID, "Петя")
	e.studio(storage.ChatOff, "")
	e.st.AddLead(context.Background(), studio, storage.Person{ID: anyaID}, adminID, e.clk.Now()) // an off group: no rights
	e.say(e.dm(anyaID, "Аня", "anya", "привет, бот"), e.dm(petyaID, "Петя", "petya", "бот, запомни: джем"))
	if len(e.llm.calls) != 0 {
		t.Fatalf("no tokens spent: %+v", e.llm.calls)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 0 {
		t.Fatalf("not answered: %+v", sent)
	}
	for _, id := range []int64{anyaID, petyaID} {
		if ms, _ := e.st.RecentDMs(context.Background(), id, 10); len(ms) != 0 {
			t.Fatalf("not stored: %+v", ms)
		}
	}
}

// The creator turns a group on: the admin is unknown, so the bot waits for them in the group and
// asks the creator to confirm (SPEC 3.2).
func TestCreatorTurnsGroupOnAndConfirmsTheAdmin(t *testing.T) {
	e := setup(t)
	e.studio(storage.ChatOff, "")
	e.command(`{"command":"enable","chat":1,"people":[1],"mode":"silent"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "включи бота в Звукарне, админ @vasya, режим тишина"))

	c, _ := e.group(studio)
	if c.State != storage.ChatPending || c.Mode != storage.ModeSilent {
		t.Fatalf("waiting for the admin: %+v", c)
	}
	if got := e.lastDM(adminID); !strings.Contains(got, "«Звукарня» включается в режиме «тишина», жду админа: @vasya") {
		t.Fatalf("the creator is told: %q", got)
	}
	cmds := e.llm.callsOf(llm.RoleCommand)
	if u := cmds[0].user(); !strings.Contains(u, "1. «Звукарня» — выключена\n2. «Тестовая группа» — включена, режим «общение»\n") ||
		!strings.Contains(u, "1. @vasya") || !strings.Contains(u, "права: создатель бота") {
		t.Fatalf("the parser sees the groups and the people:\n%s", u)
	}

	// A pending group stores nothing; a stranger is not asked about.
	e.say(inChat(e.from(petyaID, "Петя", "petya", "всем привет"), studio))
	if len(e.buttons()) != 0 || e.stored(studio) != 0 {
		t.Fatal("nobody to ask about, nothing stored")
	}
	e.say(inChat(e.from(vasyaID, "Вася", "vasya", "здорово"), studio), inChat(e.from(vasyaID, "Вася", "vasya", "ещё раз"), studio))
	bs := e.buttons()
	if len(bs) != 1 || bs[0].chat != adminID || !strings.Contains(bs[0].text, "В «Звукарня» вижу Вася (@vasya). Это он?") {
		t.Fatalf("one question to the creator: %+v", bs)
	}
	yes := bs[0].rows[0][0].Data
	if yes != fmt.Sprintf("adm:%d:%d:y", studio, vasyaID) || e.stored(studio) != 0 {
		t.Fatalf("button %q; still nothing stored", yes)
	}

	e.press(anyaID, yes) // not a creator: nothing happens
	if c, _ := e.group(studio); c.State != storage.ChatPending {
		t.Fatal("only a creator confirms")
	}
	e.out.members = map[int64]storage.Person{vasyaID: {ID: vasyaID, Name: "Вася", Username: "vasya"}}
	e.press(adminID, yes)
	if c, _ := e.group(studio); c.State != storage.ChatActive || c.Mode != storage.ModeSilent {
		t.Fatalf("on in silence: %+v", c)
	}
	if got := e.rolesIn(studio); len(got) != 1 || got[0] != "lead:Вася (@vasya)" {
		t.Fatalf("the admin: %v", got)
	}
	if ed := e.edits(); len(ed) != 1 || !strings.Contains(ed[0].text, "Готово: Вася (@vasya) — админ в «Звукарня», бот включён в режиме «тишина»") {
		t.Fatalf("the question becomes the answer: %+v", ed)
	}
	if ws, _ := e.st.WantedAdmins(context.Background(), studio); len(ws) != 0 {
		t.Fatal("nobody is waited for any more")
	}
	e.say(inChat(e.from(petyaID, "Петя", "petya", "теперь видно?"), studio))
	if e.stored(studio) != 1 {
		t.Fatal("an active group keeps its history")
	}
	if sent, _ := e.out.snapshot(); len(e.dmTexts(adminID)) != len(sent) {
		t.Fatalf("nothing is said in the group: %+v", sent)
	}
}

// By name; "no" is remembered; the admin must be a member of the group.
func TestWaitingForTheAdminByName(t *testing.T) {
	e := setup(t)
	e.studio(storage.ChatOff, "")
	e.command(`{"command":"enable","chat":1,"names":["Петя Иванов"],"mode":"reactions"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "включи Звукарню, админ — Петя Иванов, пусть только реагирует"))

	e.say(inChat(e.from(petyaID, "пётр  иванов", "", "привет"), studio)) // ё and spaces aside — not the same name
	e.say(inChat(e.from(petyaID+1, "петя иванов", "", "привет"), studio))
	bs := e.buttons()
	if len(bs) != 1 || !strings.Contains(bs[0].text, "петя иванов") {
		t.Fatalf("asked about the namesake: %+v", bs)
	}
	e.press(adminID, bs[0].rows[0][1].Data)
	if ed := e.edits(); len(ed) != 1 || !strings.Contains(ed[0].text, "не он") {
		t.Fatalf("no: %+v", ed)
	}
	e.say(inChat(e.from(petyaID+1, "петя иванов", "", "я тут"), studio))
	if len(e.buttons()) != 1 {
		t.Fatal("not asked about the same person again")
	}
	e.say(inChat(e.from(petyaID+2, "Петя Иванов", "pivanov", "всем привет"), studio))
	bs = e.buttons()
	if len(bs) != 2 {
		t.Fatalf("another namesake is asked about: %+v", bs)
	}
	e.out.nonMembers = map[int64]bool{petyaID + 2: true} // left the group before the answer
	e.press(adminID, bs[1].rows[0][0].Data)
	if c, _ := e.group(studio); c.State != storage.ChatPending {
		t.Fatalf("not a member: still waiting %+v", c)
	}
	if ed := e.edits(); !strings.Contains(ed[len(ed)-1].text, "не состоит в «Звукарня»") {
		t.Fatalf("told why: %+v", ed)
	}
}

// People the bot already knows are asked about at once: by @username from the active groups, the
// group's administrators in Telegram; only members of the group.
func TestTurningOnAsksAboutKnownPeople(t *testing.T) {
	e := setup(t)
	e.say(e.from(vasyaID, "Вася", "vasya", "привет")) // seen in the active group
	e.out.tgAdmins = []storage.Person{{ID: 70, Name: "Коля", Username: "kolya"}, {ID: 71, Name: "Оля"}}
	e.out.nonMembers = map[int64]bool{71: true}
	e.studio(storage.ChatOff, "")
	e.command(`{"command":"enable","chat":1,"people":[1,2],"names":["Оля"],"mode":"talk"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "включи Звукарню, админы @vasya, @kolya и Оля, режим общения"))
	var asked []string
	for _, b := range e.buttons() {
		asked = append(asked, b.text)
	}
	if len(asked) != 2 || !strings.Contains(asked[0], "Коля (@kolya)") || !strings.Contains(asked[1], "Вася (@vasya)") {
		t.Fatalf("asked about the known members: %q", asked)
	}
	if got := e.lastDM(adminID); !strings.Contains(got, "жду админа: @vasya, @kolya, Оля") || !strings.Contains(got, "подтверди ниже") {
		t.Fatalf("the creator is told: %q", got)
	}
	e.press(adminID, e.buttons()[0].rows[0][0].Data)
	if got := e.lastEdit(); !strings.Contains(got, "Остальных (@vasya, Оля) назначь") {
		t.Fatalf("the others are mentioned: %q", got)
	}
	// The second question is still answered: one more admin of an active group.
	e.press(adminID, e.buttons()[1].rows[0][0].Data)
	if got := e.rolesIn(studio); len(got) != 2 {
		t.Fatalf("two admins: %v", got)
	}
}

func (e *env) lastEdit() string {
	ed := e.edits()
	if len(ed) == 0 {
		return ""
	}
	return ed[len(ed)-1].text
}

func TestTurningOnAsksForWhatIsMissing(t *testing.T) {
	e := setup(t)
	e.studio(storage.ChatOff, "")
	e.command(`{"command":"enable","chat":1,"people":[1]}`)
	e.say(e.dm(adminID, "Влад", "vlad", "включи Звукарню, админ @vasya"))
	if got := e.lastDM(adminID); !strings.Contains(got, "В каком режиме включить «Звукарня»") {
		t.Fatalf("asks for the mode: %q", got)
	}
	e.command(`{"command":"enable","chat":1,"mode":"talk"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "включи Звукарню в режиме общения"))
	if got := e.lastDM(adminID); !strings.Contains(got, "Кто будет админом в «Звукарня»?") {
		t.Fatalf("asks for the admin: %q", got)
	}
	if c, _ := e.group(studio); c.State != storage.ChatOff {
		t.Fatalf("still off: %+v", c)
	}
	// The parser sees the bot's question: an answer continues the instruction.
	e.command(`{"command":"enable","chat":1,"people":[1],"mode":"talk"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "@vasya"))
	cmds := e.llm.callsOf(llm.RoleCommand)
	if u := cmds[len(cmds)-1].user(); !strings.Contains(u, "Бот: Кто будет админом в «Звукарня»?") || !strings.Contains(u, "Сейчас речь о группе 1") {
		t.Fatalf("the recent private chat and the chosen group reach the parser:\n%s", u)
	}
	if c, _ := e.group(studio); c.State != storage.ChatPending {
		t.Fatalf("waiting now: %+v", c)
	}
	e.command(`{"command":"enable","chat":2,"people":[1],"mode":"talk"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "и тестовую включи"))
	if got := e.lastDM(adminID); !strings.Contains(got, "«Тестовая группа» уже включена") {
		t.Fatalf("an active group: %q", got)
	}
}

// Turning off asks first; then everything about the group goes, the bot stays in it, off.
func TestCreatorTurnsGroupOff(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.msg("привет"))
	e.command(`{"command":"disable","chat":1}`)
	e.say(e.dm(adminID, "Влад", "vlad", "отключи тестовую группу"))
	if c, _ := e.group(chat); c.State != storage.ChatActive {
		t.Fatal("nothing happens before the confirmation")
	}
	bs := e.buttons()
	if len(bs) != 1 || !strings.Contains(bs[0].text, "Отключить бота в «Тестовая группа»?") {
		t.Fatalf("asked: %+v", bs)
	}
	e.press(adminID, bs[0].rows[0][1].Data)
	if c, _ := e.group(chat); c.State != storage.ChatActive || !strings.Contains(e.lastEdit(), "не трогаю") {
		t.Fatal("no: nothing changes")
	}
	e.press(adminID, bs[0].rows[0][0].Data)
	if c, ok := e.group(chat); !ok || c.State != storage.ChatOff {
		t.Fatalf("off, still known: %+v", c)
	}
	if e.stored(chat) != 0 || len(e.roles()) != 0 || !strings.Contains(e.lastEdit(), "всё о ней забыл") {
		t.Fatal("everything is gone")
	}
	e.say(e.msg("@testbot ау"))
	if e.stored(chat) != 0 {
		t.Fatal("an off group stores nothing")
	}
	e.command(`{"command":"disable","chat":1}`)
	e.say(e.dm(adminID, "Влад", "vlad", "отключи тестовую группу"))
	if got := e.lastDM(adminID); !strings.Contains(got, "и так выключена") || len(e.buttons()) != 1 {
		t.Fatalf("already off: %q", got)
	}
}

// The creator manages the admins from the private chat, and nothing else.
func TestCreatorInPrivate(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.from(anyaID, "Аня", "anya", "привет"))
	e.command(`{"command":"set_lead","chat":2,"people":[1]}`)
	e.studio(storage.ChatOff, "")
	e.say(e.dm(adminID, "Влад", "vlad", "@anya тоже админ в тестовой"))
	if got := e.roles(); len(got) != 2 || got[1] != "lead:Аня (@anya)" {
		t.Fatalf("one more admin: %v", got)
	}
	if got := e.lastDM(adminID); got != "🫡 «Тестовая группа»" {
		t.Fatalf("ack names the group: %q", got)
	}

	e.command(`{"command":"remember","text":"Джем по пятницам"}`) // the selection stays: the test group
	e.say(e.dm(adminID, "Влад", "vlad", "запомни: джем по пятницам"))
	if len(e.lore()) != 0 {
		t.Fatal("the creator does not teach the bot")
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 1 || !strings.Contains(r[0].user(), "только от админов чата") || !strings.Contains(r[0].user(), "«Тестовая группа»") {
		t.Fatalf("a refusal in words: %+v", r)
	}
	if strings.Contains(r[0].system(), "Руководство чата") {
		t.Fatal("the creator talks with a bot that knows no group's memory")
	}
	if !strings.Contains(r[0].system(), "твоим создателем") {
		t.Fatalf("the bot knows whom it talks with: %s", r[0].system())
	}
	e.command(`{"command":"groups"}`)
	e.say(e.dm(adminID, "Влад", "vlad", "какие у тебя группы?"))
	if got := e.lastDM(adminID); got != "1. «Звукарня» — выключена\n2. «Тестовая группа» — включена, режим «общение»" {
		t.Fatalf("groups: %q", got)
	}
}

// An admin runs the group from the private chat and talks with the bot that knows it.
func TestAdminInPrivate(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.from(anyaID, "Аня", "anya", "привет"))
	groupMsgs := e.stored(chat)

	e.command(`{"command":"remember","text":"Джем по пятницам","scope":"topic"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "запомни: джем по пятницам"))
	if ls := e.lore(); len(ls) != 1 || !ls[0].ChatWide || ls[0].AuthorRole != storage.RoleLead {
		t.Fatalf("remembered for the whole group: %+v", ls)
	}
	if got := e.lastDM(vasyaID); got != "🫡 «Тестовая группа»" {
		t.Fatalf("ack: %q", got)
	}
	e.command(`{"command":"set_mode","mode":"reactions"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "молчи пока, только реакции ставь"))
	if c, _ := e.group(chat); c.Mode != storage.ModeReactions || e.lastDM(vasyaID) != "🫡 «Тестовая группа»: режим «только реакции»." {
		t.Fatalf("mode: %+v %q", c, e.lastDM(vasyaID))
	}
	e.command(`{"command":"list"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "что помнишь?"))
	if got := e.lastDM(vasyaID); !strings.HasPrefix(got, "«Тестовая группа»:\nАдмин: Вася (@vasya)\n") || !strings.Contains(got, "Джем по пятницам") {
		t.Fatalf("the memory, in the private chat even in silence: %q", got)
	}
	// Not the admin's to give: refused in words.
	e.command(`{"command":"disable","chat":1}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "отключи бота в группе"))
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || !strings.Contains(r[0].user(), "только твой создатель") || len(e.buttons()) != 0 {
		t.Fatalf("refused: %+v", r)
	}
	e.command(`{"command":"none"}`)
	e.llm.set(func(f *fakeLLM) { f.reply = "Ты: ну привет" })
	e.say(e.dm(vasyaID, "Вася", "vasya", "как сам?"))
	if got := e.lastDM(vasyaID); got != "ну привет" {
		t.Fatalf("a conversation: %q", got)
	}
	r := e.llm.callsOf(llm.RoleReply)
	if len(r) != 2 || !strings.Contains(r[1].system(), "Джем по пятницам") || !strings.Contains(r[1].system(), "админом группы «Тестовая группа»") {
		t.Fatalf("the bot knows the group's memory: %+v", r)
	}
	if !strings.Contains(r[1].user(), "Он: как сам?") || !strings.Contains(r[1].user(), "Ты: 🫡 «Тестовая группа»") || strings.Contains(r[1].user(), "Тебя попросили") {
		t.Fatalf("the private chat is the conversation: %s", r[1].user())
	}
	if e.stored(chat) != groupMsgs {
		t.Fatal("the private chat never reaches the group's history")
	}
}

// Several groups: the bot asks which one, remembers the choice.
func TestPrivateGroupChoice(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.studio(storage.ChatActive, storage.ModeTalk)
	e.st.AddLead(context.Background(), studio, storage.Person{ID: vasyaID, Name: "Вася", Username: "vasya"}, adminID, e.clk.Now())

	e.command(`{"command":"remember","text":"Факт"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "запомни факт"))
	if got := e.lastDM(vasyaID); got != "О какой группе речь?\n1. «Звукарня» — включена, режим «общение», ты админ\n2. «Тестовая группа» — включена, режим «общение», ты админ" {
		t.Fatalf("asks: %q", got)
	}
	e.command(`{"command":"remember","text":"Факт","chat":1}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "в звукарне"))
	ls, _ := e.st.Lore(context.Background(), studio)
	if len(ls) != 1 || len(e.lore()) != 0 {
		t.Fatalf("remembered in the named group: %+v", ls)
	}
	e.command(`{"command":"remember","text":"Ещё факт"}`)
	e.say(e.dm(vasyaID, "Вася", "vasya", "и ещё запомни"))
	if ls, _ := e.st.Lore(context.Background(), studio); len(ls) != 2 || e.lastDM(vasyaID) != "🫡 «Звукарня»" {
		t.Fatalf("the choice is remembered: %+v", ls)
	}
	cmds := e.llm.callsOf(llm.RoleCommand)
	if cmds[len(cmds)-1].schema != privateCommandSchema {
		t.Fatal("a private message is parsed with the private command schema")
	}
	if u := cmds[len(cmds)-1].user(); !strings.Contains(u, "Сейчас речь о группе 1") || !strings.Contains(u, "Сохранённые записи группы 1:\n") {
		t.Fatalf("the parser sees the chosen group's memory:\n%s", u)
	}
}

// An admin by @username only (not seen yet) may write too; offline the bot only reacts.
func TestPrivateAdminByUsernameAndOffline(t *testing.T) {
	e := setup(t)
	e.st.AddLead(context.Background(), chat, storage.Person{Username: "vasya"}, adminID, e.clk.Now())
	e.llm.set(func(f *fakeLLM) { f.online = false })
	in := e.dm(vasyaID, "Вася", "Vasya", "ты тут?")
	e.say(in)
	if ms, _ := e.st.RecentDMs(context.Background(), vasyaID, 10); len(ms) != 1 {
		t.Fatal("an admin by @username is let in")
	}
	if got := e.reactionsTo(in.MessageID); len(got) != 1 || got[0] != "😴" {
		t.Fatalf("offline reaction: %v", got)
	}
}
