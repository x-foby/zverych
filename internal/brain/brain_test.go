package brain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

const (
	chat    = int64(-100)
	botID   = int64(1)
	anyaID  = int64(42)
	adminID = int64(7)
)

// ---- fakes ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sentMsg struct {
	thread, replyTo int
	text            string
}

type reaction struct {
	msgID int
	emoji string
}

type fakeOut struct {
	ids     *atomic.Int64 // shared with the test so ids stay chronological, like in Telegram
	sendErr error

	mu        sync.Mutex
	sent      []sentMsg
	sentTo    []int64 // chat of each sent message, parallel to sent
	reactions []reaction
	typing    int
	admins    []int64 // chat administrators in Telegram
	adminsErr error
	adminReqs int

	tgAdmins   []storage.Person         // overrides admins with names (the creator's search, SPEC 3.2)
	buttons    []sentButtons            // messages with buttons, to private chats
	edits      []sentMsg                // edited messages: replyTo holds the edited id
	answered   []string                 // answered callback ids
	members    map[int64]storage.Person // who is in the groups (any group); nil: everyone is
	nonMembers map[int64]bool
	memberErr  error
}

type sentButtons struct {
	chat int64
	text string
	rows [][]Button
}

func (f *fakeOut) Send(_ context.Context, chatID int64, thread int, text string, replyTo int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sent = append(f.sent, sentMsg{thread: thread, replyTo: replyTo, text: text})
	f.sentTo = append(f.sentTo, chatID)
	return int(f.ids.Add(1)), nil
}
func (f *fakeOut) React(_ context.Context, _ int64, id int, e string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, reaction{id, e})
	return nil
}
func (f *fakeOut) Admins(context.Context, int64) ([]storage.Person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adminReqs++
	if f.tgAdmins != nil {
		return slices.Clone(f.tgAdmins), f.adminsErr
	}
	var out []storage.Person
	for _, id := range f.admins {
		out = append(out, storage.Person{ID: id})
	}
	return out, f.adminsErr
}
func (f *fakeOut) SendButtons(_ context.Context, chatID int64, text string, rows [][]Button) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.buttons = append(f.buttons, sentButtons{chatID, text, rows})
	return int(f.ids.Add(1)), nil
}
func (f *fakeOut) Edit(_ context.Context, chatID int64, msgID int, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, sentMsg{replyTo: msgID, text: text})
	return nil
}
func (f *fakeOut) AnswerCallback(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, id)
	return nil
}
func (f *fakeOut) Member(_ context.Context, _, userID int64) (storage.Person, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberErr != nil {
		return storage.Person{}, false, f.memberErr
	}
	p, ok := f.members[userID]
	if !ok {
		p = storage.Person{ID: userID}
	}
	return p, (f.members == nil || ok) && !f.nonMembers[userID], nil
}
func (f *fakeOut) Typing(context.Context, int64, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typing++
	return nil
}
func (f *fakeOut) snapshot() ([]sentMsg, []reaction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent), slices.Clone(f.reactions)
}

type call struct {
	role    llm.Role
	prio    llm.Priority
	msgs    []llm.Message
	session string
	schema  *llm.Schema // the answer's schema the request was made with (llm.WithSchema)
}

func (c call) system() string { return c.msgs[0].Content }
func (c call) user() string   { return c.msgs[1].Content }

type fakeLLM struct {
	mu        sync.Mutex
	online    bool
	availFn   func(llm.Role, llm.Priority) bool // overrides online for Available when set
	decision  string                            // answer to RoleFilter (evaluation) requests
	command   string                            // answer to RoleCommand requests
	thread    string                            // answer to RoleThread requests
	summary   string                            // answer to RoleSummary requests
	notes     string                            // answer to RoleNotes requests
	lore      string                            // answer to RoleLore requests
	selection string                            // answer to semantic lore selection
	tone      string                            // answer to RoleTone requests
	reply     string
	replyErr  error
	onReply   func()
	hold      map[llm.Role]chan struct{} // a request of the role waits for the channel (or its context)
	started   chan llm.Role              // a held request has started

	avail []call
	calls []call
}

func (f *fakeLLM) set(fn func(*fakeLLM)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

func (f *fakeLLM) Available(_ context.Context, role llm.Role, prio llm.Priority) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.avail = append(f.avail, call{role: role, prio: prio})
	if f.availFn != nil {
		return f.availFn(role, prio)
	}
	return f.online
}

func (f *fakeLLM) Complete(ctx context.Context, role llm.Role, prio llm.Priority, msgs []llm.Message) (string, string, error) {
	f.mu.Lock()
	gate, started := f.hold[role], f.started
	f.mu.Unlock()
	if gate != nil {
		started <- role
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-gate:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return "", "", llm.ErrUnavailable
	}
	f.calls = append(f.calls, call{role: role, prio: prio, msgs: msgs, session: llm.SessionOf(ctx), schema: llm.SchemaOf(ctx)})
	if role == llm.RoleFilter {
		return f.decision, "fake", nil
	}
	if role == llm.RoleCommand {
		return f.command, "fake", nil
	}
	if role == llm.RoleThread {
		return f.thread, "fake", nil
	}
	if role == llm.RoleSummary {
		return f.summary, "fake", nil
	}
	if role == llm.RoleNotes {
		return f.notes, "fake", nil
	}
	if role == llm.RoleLore {
		return cmp.Or(f.lore, `{"lore": []}`), "fake", nil
	}
	if role == llm.RoleLoreSelect {
		return f.selection, "fake", nil
	}
	if role == llm.RoleTone {
		return cmp.Or(f.tone, `{"tone": ""}`), "fake", nil
	}
	if f.onReply != nil {
		f.onReply()
	}
	if f.replyErr != nil {
		return "", "", f.replyErr
	}
	return f.reply, "fake", nil
}

func (f *fakeLLM) callsOf(role llm.Role) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.role == role {
			out = append(out, c)
		}
	}
	return out
}

// ---- environment ----

type env struct {
	t     *testing.T
	b     *Brain
	out   *fakeOut
	llm   *fakeLLM
	st    *storage.Store
	clk   *clock
	ids   *atomic.Int64
	slept []time.Duration
}

func setup(t *testing.T, tweak ...func(*config.Config)) *env {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{}
	cfg.Telegram.AdminIDs = []int64{adminID}
	if err := st.RegisterChat(context.Background(), chat, "Тестовая группа", adminID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetChatState(context.Background(), chat, storage.ChatActive, storage.ModeTalk); err != nil {
		t.Fatal(err)
	}
	cfg.Telegram.Aliases = []string{" Бот ", "", "  "}
	cfg.Behavior = config.Behavior{BatchIntervalSeconds: 60, ContextMessages: 30, CooldownMinutes: 15,
		ReplyDelayMinSeconds: 3, ReplyDelayMaxSeconds: 12,
		MaxConsecutiveOwn: 2, StaleAfterMinutes: 30, OfflineReaction: "😴", RetentionDays: 30, DialogMinutes: 10,
		HabitMinMessages: 3, HabitMinShare: 0.5, ThreadHeadMessages: 3, AroundMessages: 10, ThreadActiveHours: 6, ThreadCandidates: 10,
		SummaryChunkMessages: 40, SummaryMaxChars: 1500, NoteMaxChars: 300,
		LoreMaxEntries: 30, LoreMaxChars: 200}
	for _, f := range tweak {
		f(cfg)
	}
	e := &env{t: t, st: st, ids: &atomic.Int64{},
		clk: &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}}
	e.out = &fakeOut{ids: e.ids}
	e.llm = &fakeLLM{online: true, reply: "Ты: ответ бота"}
	e.b = New(cfg, "  тестовая персона\n", st, e.llm, e.out, Identity{ID: botID, Username: "testbot", Name: "Бот"})
	e.b.now = e.clk.Now
	e.b.rand = func() float64 { return 0.5 }
	var mu sync.Mutex
	e.b.sleep = func(_ context.Context, d time.Duration) { mu.Lock(); defer mu.Unlock(); e.slept = append(e.slept, d) }
	return e
}

// msg builds the next chat message from Аня (ids are chronological).
func (e *env) msg(text string) Incoming {
	return Incoming{ChatID: chat, ChatType: "supergroup", MessageID: int(e.ids.Add(1)), UserID: anyaID,
		UserName: "Аня", Text: text, Date: e.clk.Now()}
}

// say delivers messages and waits for every handler they started.
// within runs f and fails the test if it does not return in time. A deadlock then
// fails fast with a clear message instead of hanging until `go test -timeout`.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not finish in 3s (deadlock?)", what)
	}
}

func (e *env) say(ins ...Incoming) {
	e.t.Helper()
	within(e.t, "message handling", func() {
		for _, in := range ins {
			e.b.OnMessage(context.Background(), in)
		}
		e.b.wait()
	})
}

func (e *env) tick() {
	e.t.Helper()
	within(e.t, "tick", func() {
		e.b.tick(context.Background())
		e.b.wait()
	})
}

func (e *env) initiative() {
	e.t.Helper()
	within(e.t, "evaluation", func() { e.b.evaluate(context.Background(), chat) })
}

// ownMessage stores a message as if the bot had sent it.
func (e *env) ownMessage(text string) int {
	id := int(e.ids.Add(1))
	err := e.st.SaveMessage(context.Background(), storage.Message{ChatID: chat, MessageID: id, UserID: botID,
		UserName: "Бот", Text: text, Date: e.clk.Now(), IsSelf: true})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) get(id int) *storage.Message {
	e.t.Helper()
	m, err := e.st.Get(context.Background(), chat, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) pending() bool {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return e.b.pending[chat]
}

// newPart returns the "Новые сообщения" section of a filter prompt and everything before it.
func newPart(t *testing.T, c call) (earlier, fresh string) {
	t.Helper()
	before, after, ok := strings.Cut(c.user(), "Новые сообщения:\n")
	if !ok {
		before, after, ok = strings.Cut(c.user(), "Вне веток — новые сообщения:\n")
	}
	if !ok {
		t.Fatalf("no new-messages section in %q", c.user())
	}
	return before, after
}

// ---- direct replies ----

func TestMentionIsAnswered(t *testing.T) {
	e := setup(t)
	in := e.msg("@TestBot как дела?")
	in.ThreadID = 7
	e.say(in)

	sent, reactions := e.out.snapshot()
	if len(sent) != 1 || len(reactions) != 0 {
		t.Fatalf("sent %v reactions %v", sent, reactions)
	}
	if want := (sentMsg{thread: 7, replyTo: in.MessageID, text: "ответ бота"}); in.MessageID == 0 || sent[0] != want {
		t.Fatalf("sent %+v, want %+v (the mention must be quoted even when it is the latest message)", sent[0], want)
	}
	replies := e.llm.callsOf(llm.RoleReply)
	if len(replies) != 1 || replies[0].prio != llm.Direct || len(e.llm.callsOf(llm.RoleFilter)) != 0 {
		t.Fatalf("llm calls %+v", e.llm.calls)
	}
	c := replies[0]
	for _, want := range []string{"<persona>\nтестовая персона\n</persona>"} {
		if !strings.Contains(c.system(), want) {
			t.Errorf("system prompt lacks %q:\n%s", want, c.system())
		}
	}
	for _, want := range []string{"[id=1] [топик 7] Аня: @TestBot как дела?", "в ответ на сообщение [id=1] от Аня"} {
		if !strings.Contains(c.user(), want) {
			t.Errorf("reply prompt lacks %q:\n%s", want, c.user())
		}
	}
	if strings.Contains(c.user(), "Почему ты решил вмешаться") {
		t.Error("direct reply must not carry a filter reason")
	}
	if !e.get(1).Answered {
		t.Error("mention must be marked answered")
	}
	own := e.get(2)
	if own == nil || !own.IsSelf || own.Text != "ответ бота" || own.UserID != botID || own.ThreadID != 7 || !own.Date.Equal(e.clk.Now()) {
		t.Fatalf("own message stored as %+v", own)
	}
	e.out.mu.Lock()
	typing := e.out.typing
	e.out.mu.Unlock()
	if typing == 0 {
		t.Error("typing indicator was never sent")
	}
}

func TestIsAddressed(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name string
		in   Incoming
		want bool
	}{
		{"reply to bot", Incoming{Text: "ну да", ReplyToUserID: botID}, true},
		{"reply to someone else", Incoming{Text: "ну да", ReplyToUserID: anyaID}, false},
		{"text_mention", Incoming{Text: "глянь сюда", TextMentions: []storage.Person{{ID: 7}, {ID: botID}}}, true},
		{"text_mention of another user", Incoming{Text: "Вася", TextMentions: []storage.Person{{ID: 7}}}, false},
		{"@username any case", Incoming{Text: "эй @TESTBOT!"}, true},
		{"alias with punctuation", Incoming{Text: "бот, привет"}, true},
		{"alias capitalised", Incoming{Text: "А ты что думаешь, Бот?"}, true},
		{"alias only as part of a word", Incoming{Text: "работа ботаника"}, false},
		{"alias glued to digits is another word", Incoming{Text: "бот2 опять сломался"}, false},
		{"alias with a stray soft sign", Incoming{Text: "Боть, ты тут?"}, true},
		{"alias with a stray hard sign", Incoming{Text: "ботъ"}, true},
		{"alias in another case is not an address", Incoming{Text: "боты опять сломались"}, false},
		{"plain text", Incoming{Text: "просто разговор"}, false},
	}
	for _, c := range cases {
		if got := e.b.isAddressed(c.in); got != c.want {
			t.Errorf("%s: isAddressed(%q)=%v want %v", c.name, c.in.Text, got, c.want)
		}
	}

	nameless := New(&config.Config{}, "", nil, nil, nil, Identity{ID: botID})
	if nameless.isAddressed(Incoming{Text: "@ кто тут"}) {
		t.Error("an empty username must not match a bare @")
	}
	if len(e.b.aliases) != 1 || e.b.aliases[0] != "бот" {
		t.Errorf("aliases must be trimmed, lowercased and non-empty, got %q", e.b.aliases)
	}
}

func TestCatchUpAnswersOnlyLatestMention(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.online = false })
	m1, m2, m3, m4 := e.msg("Бот, ты тут?"), e.msg("ага"), e.msg("бот, ау"), e.msg("ок")
	e.say(m1, m2, m3, m4)

	sent, reactions := e.out.snapshot()
	slices.SortFunc(reactions, func(a, b reaction) int { return a.msgID - b.msgID })
	if len(sent) != 0 || !slices.Equal(reactions, []reaction{{m1.MessageID, "😴"}, {m3.MessageID, "😴"}}) {
		t.Fatalf("offline: sent %v reactions %v", sent, reactions)
	}

	e.llm.set(func(f *fakeLLM) { f.online = true })
	e.tick()
	sent, reactions = e.out.snapshot()
	if len(sent) != 1 || sent[0].replyTo != m3.MessageID {
		t.Fatalf("expected a single quoted answer to the latest mention %d, got %+v", m3.MessageID, sent)
	}
	if len(reactions) != 2 {
		t.Fatalf("catch-up must not react again, reactions %v", reactions)
	}
	if !e.get(m1.MessageID).Answered || !e.get(m3.MessageID).Answered {
		t.Fatal("all mentions up to the answered one must be marked answered")
	}
	if !strings.Contains(e.llm.callsOf(llm.RoleReply)[0].user(), "[id=3] от Аня") {
		t.Fatal("reply must target the latest mention")
	}

	e.tick()
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatal("must not answer the same mention twice")
	}
}

func TestStaleMentionsAreNotAnswered(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		want int
	}{
		{29*time.Minute + 59*time.Second, 1},
		{30*time.Minute + time.Second, 0},
	} {
		t.Run(c.age.String(), func(t *testing.T) {
			e := setup(t)
			e.llm.set(func(f *fakeLLM) { f.online = false })
			e.say(e.msg("бот, ты где?"))
			e.clk.Advance(c.age)
			e.llm.set(func(f *fakeLLM) { f.online = true })
			e.tick()
			if sent, _ := e.out.snapshot(); len(sent) != c.want {
				t.Fatalf("mention aged %v: sent %d, want %d", c.age, len(sent), c.want)
			}
		})
	}
}

func TestQuotaLostDuringGeneration(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.replyErr = llm.ErrUnavailable })
	m := e.msg("бот, привет")
	e.say(m)
	if sent, reactions := e.out.snapshot(); len(sent) != 0 || len(reactions) != 1 {
		t.Fatalf("first arrival should get the offline reaction: sent %v reactions %v", sent, reactions)
	}
	e.tick() // catch-up retry, also fails
	if _, reactions := e.out.snapshot(); len(reactions) != 1 {
		t.Fatalf("catch-up retries must not react again: %v", reactions)
	}
	if e.get(m.MessageID).Answered {
		t.Fatal("unanswered mention must stay pending")
	}
}

func TestDirectReplyFailures(t *testing.T) {
	for name, tweak := range map[string]func(*env){
		"llm error":            func(e *env) { e.llm.set(func(f *fakeLLM) { f.replyErr = errors.New("boom") }) },
		"empty after cleanup":  func(e *env) { e.llm.set(func(f *fakeLLM) { f.reply = "Ты:  " }) },
		"telegram send failed": func(e *env) { e.out.sendErr = errors.New("flood") },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			tweak(e)
			m := e.msg("бот, привет")
			e.say(m)
			if sent, reactions := e.out.snapshot(); len(sent) != 0 || len(reactions) != 0 {
				t.Fatalf("sent %v reactions %v", sent, reactions)
			}
			if e.get(m.MessageID).Answered {
				t.Fatal("failed reply must leave the mention pending")
			}
			if own := e.get(m.MessageID + 1); own != nil {
				t.Fatalf("nothing should be stored as sent, got %+v", own)
			}
		})
	}
}

func TestOfflineReactionCanBeDisabled(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.OfflineReaction = "" })
	e.llm.set(func(f *fakeLLM) { f.online = false })
	e.say(e.msg("бот, привет"))
	if sent, reactions := e.out.snapshot(); len(sent) != 0 || len(reactions) != 0 {
		t.Fatalf("sent %v reactions %v", sent, reactions)
	}
}

func TestIgnoredMessages(t *testing.T) {
	cases := map[string]func(*Incoming){
		"foreign chat": func(in *Incoming) { in.ChatID = -999 },
		"private chat": func(in *Incoming) { in.ChatType = "private" },
		"channel":      func(in *Incoming) { in.ChatType = "channel" },
		"own message":  func(in *Incoming) { in.UserID = botID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			in := e.msg("@testbot привет, как дела?")
			mutate(&in)
			e.say(in, in) // the second one exercises the "warn once" path
			ms, err := e.st.Recent(context.Background(), in.ChatID, 10)
			if err != nil {
				t.Fatal(err)
			}
			sent, reactions := e.out.snapshot()
			if len(ms) != 0 || len(sent) != 0 || len(reactions) != 0 || len(e.llm.avail) != 0 || e.pending() {
				t.Fatalf("must be ignored entirely: stored %v sent %v reactions %v", ms, sent, reactions)
			}
		})
	}
}

// The reserve exists for mentions: direct replies ask for Direct priority, initiative for Initiative.
func TestReserveIsOnlyForMentions(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) {
		f.availFn = func(_ llm.Role, p llm.Priority) bool { return p == llm.Direct }
		f.decision = `{"actions":[{"id":1,"reply":true}]}`
	})
	e.say(e.msg("кто-нибудь знает, как работает горутина?"))
	e.initiative()
	if len(e.llm.calls) != 0 {
		t.Fatalf("initiative must not use the reserve, calls %+v", e.llm.calls)
	}
	e.say(e.msg("бот, а ты знаешь?"))
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatal("mention must be answered from the reserve")
	}
	for _, a := range e.llm.avail {
		if a.role == llm.RoleReply && a.prio != llm.Direct {
			t.Fatalf("direct path asked for %+v", a)
		}
	}
}

func TestReplyDelayIsHumanLike(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.ReplyDelayMinSeconds, c.Behavior.ReplyDelayMaxSeconds = 4, 10 })
	e.b.rand = func() float64 { return 0.25 }
	e.llm.set(func(f *fakeLLM) { f.onReply = func() { e.clk.Advance(2 * time.Second) } })
	e.say(e.msg("бот, привет"))
	// 4 + 0.25*(10-4) = 5.5s in total, of which 2s were already spent on generation.
	if !slices.Equal(e.slept, []time.Duration{3500 * time.Millisecond}) {
		t.Fatalf("slept %v", e.slept)
	}
}

// ---- initiative ----

func TestInitiativeReplyAndCooldown(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":1,"reply":true,"reason":"интересный вопрос"}]}`
	})
	m1, m2 := e.msg("кто-нибудь знает, как работает горутина?"), e.msg("ок")
	e.say(m1, m2)
	if !e.pending() {
		t.Fatal("new messages make the chat pending")
	}
	e.tick()

	sent, _ := e.out.snapshot()
	if len(sent) != 1 || sent[0].replyTo != m1.MessageID || sent[0].text != "ответ бота" {
		t.Fatalf("expected a quoted reply to %d, got %+v", m1.MessageID, sent)
	}
	if e.pending() {
		t.Fatal("pending flag must be cleared after evaluation")
	}
	filters, replies := e.llm.callsOf(llm.RoleFilter), e.llm.callsOf(llm.RoleReply)
	if len(filters) != 1 || filters[0].prio != llm.Initiative || len(replies) != 1 || replies[0].prio != llm.Initiative {
		t.Fatalf("llm calls %+v", e.llm.calls)
	}
	earlier, fresh := newPart(t, filters[0])
	if strings.Contains(earlier, "Ранее в чате") {
		t.Errorf("nothing was evaluated before, but prompt has an earlier section:\n%s", earlier)
	}
	if fresh != "[id=1] Аня: кто-нибудь знает, как работает горутина?\n[id=2] Аня: ок\n" {
		t.Errorf("fresh section %q", fresh)
	}
	if !strings.Contains(replies[0].user(), "Почему ты решил вмешаться: интересный вопрос") ||
		!strings.Contains(replies[0].user(), "[id=1] от Аня") {
		t.Errorf("reply prompt %q", replies[0].user())
	}
	st, _ := e.st.ChatState(context.Background(), chat)
	if !st.LastInitiativeAt.Equal(e.clk.Now()) {
		t.Fatalf("last initiative %v, want %v", st.LastInitiativeAt, e.clk.Now())
	}

	if f := filters[0].system(); !strings.Contains(f, "Реплик не по указанию — не больше одной.") || strings.Contains(f, "Реплик не по указанию сейчас не пиши") {
		t.Errorf("outside the cooldown the evaluator may reply:\n%s", f)
	}

	// During the cooldown the evaluation still runs, but may only react: an own reply is dropped.
	e.clk.Advance(15*time.Minute - time.Second)
	q := e.msg("а ещё вопрос про каналы есть?")
	e.say(q)
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":` + strconv.Itoa(q.MessageID) + `,"reply":true}]}` })
	e.initiative()
	filters = e.llm.callsOf(llm.RoleFilter)
	if len(filters) != 2 {
		t.Fatalf("cooldown limits replies, not the filter; filter calls=%d", len(filters))
	}
	if f := filters[1].system(); !strings.Contains(f, "Реплик не по указанию сейчас не пиши") {
		t.Errorf("in the cooldown the evaluator is told not to reply on its own:\n%s", f)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 || len(e.llm.callsOf(llm.RoleReply)) != 1 {
		t.Fatalf("no second reply in the cooldown; sent %+v", sent)
	}
	m3 := e.msg("смотрите, я починил дедлок")
	e.say(m3)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":` + strconv.Itoa(m3.MessageID) + `,"react":"🔥"}]}`
	})
	e.initiative()
	if got := e.reactionsTo(m3.MessageID); !reflect.DeepEqual(got, []string{"🔥"}) {
		t.Fatalf("reactions are allowed in the cooldown: %v", got)
	}

	e.clk.Advance(time.Second)
	sel := e.msg("а что насчёт select?")
	e.say(sel)
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":` + strconv.Itoa(sel.MessageID) + `,"reply":true}]}` })
	e.initiative()
	if sent, _ := e.out.snapshot(); len(sent) != 2 {
		t.Fatalf("after the cooldown the bot may speak again; sent %+v", sent)
	}
}

func TestMonologueGuard(t *testing.T) {
	for _, c := range []struct {
		own       int
		wantReply bool
	}{{1, true}, {2, false}} {
		e := setup(t)
		e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":1,"reply":true}]}` })
		e.say(e.msg("кто-нибудь знает, как работает горутина?"))
		for range c.own {
			e.ownMessage("моя реплика")
		}
		e.initiative()
		filters := e.llm.callsOf(llm.RoleFilter)
		sent, _ := e.out.snapshot()
		if len(filters) != 1 || (len(sent) == 1) != c.wantReply {
			t.Errorf("%d own messages in a row: filter calls %d, sent %v, want reply=%v", c.own, len(filters), sent, c.wantReply)
		}
		if len(filters) == 1 && strings.Contains(filters[0].system(), "Реплик не по указанию сейчас не пиши") == c.wantReply {
			t.Errorf("%d own messages in a row: reply option offered=%v", c.own, !c.wantReply)
		}
	}
}

func TestOwnStreak(t *testing.T) {
	self, user := storage.Message{IsSelf: true}, storage.Message{}
	for _, c := range []struct {
		h    []storage.Message
		want int
	}{
		{nil, 0},
		{[]storage.Message{self, user}, 0},
		{[]storage.Message{user, self}, 1},
		{[]storage.Message{self, self}, 2},
		{[]storage.Message{self, user, self, self}, 2},
	} {
		if got := ownStreak(c.h); got != c.want {
			t.Errorf("ownStreak(%v)=%d want %d", c.h, got, c.want)
		}
	}
}

func TestAddressedAndOwnMessagesAreNotInitiativeTargets(t *testing.T) {
	e := setup(t)
	m1 := e.msg("бот, расскажи анекдот про горутины")
	e.say(m1) // answered directly, followed by the bot's own message
	e.initiative()
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != 0 {
		t.Fatalf("only a mention and an own message are fresh: filter must not run, calls=%d", n)
	}

	m3 := e.from(43, "Боря", "borya", "кто-нибудь смотрел вчерашний матч?") // not Аня: she talks with the bot
	m4 := e.msg("бот, а ты смотрел?")
	e.say(m3, m4)                // m4 is answered directly (id 5)
	e.llm.set(func(f *fakeLLM) { // the mention and the bot's answer are context only
		f.decision = `{"actions":[{"id":4,"reply":true,"react":"🔥"},{"id":5,"react":"🔥"},{"id":3,"reply":true}]}`
	})
	e.initiative()
	sent, reactions := e.out.snapshot()
	if len(sent) != 3 || sent[2].replyTo != m3.MessageID || len(reactions) != 0 {
		t.Fatalf("only the new message %d is a target: sent %+v reactions %v", m3.MessageID, sent, reactions)
	}
	// The mention and the bot's answer are a thread; the new ones in it are marked (SPEC 7a).
	threads, fresh := newPart(t, e.llm.callsOf(llm.RoleFilter)[0])
	if fresh != "[id=3] Боря (@borya): кто-нибудь смотрел вчерашний матч?\n" || !strings.Contains(threads,
		"Ветка №4:\n[id=4] Аня: бот, а ты смотрел? [новое] [обращение к тебе]\n[id=5] Ты → ответ на id=4: ответ бота [новое]\n") {
		t.Errorf("addressed messages are marked:\n%s\n%q", threads, fresh)
	}
	if r := e.llm.callsOf(llm.RoleReply); !strings.Contains(r[len(r)-1].user(), "[id=3] от Боря") {
		t.Fatalf("reply target: %s", r[len(r)-1].user())
	}
}

func TestEvaluationPromptSeparatesEarlierFromNew(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[]}` })
	e.say(e.msg("первое длинное сообщение тут"), e.msg("второе длинное сообщение тут"))
	e.initiative()
	e.say(e.msg("третье длинное сообщение тут"), e.msg("четвёртое длинное сообщение тут"))
	e.initiative()

	filters := e.llm.callsOf(llm.RoleFilter)
	if len(filters) != 2 {
		t.Fatalf("filter calls %d", len(filters))
	}
	if s := filters[1].schema; s == nil || s.Name != "evaluation" || hasProperty(s, "threads") {
		t.Fatalf("an evaluation without threads asks for the evaluation schema without them: %+v", s)
	}
	earlier, fresh := newPart(t, filters[1])
	if earlier != "Ранее в чате:\n[id=1] Аня: первое длинное сообщение тут\n[id=2] Аня: второе длинное сообщение тут\n\n" {
		t.Errorf("earlier section %q", earlier)
	}
	if fresh != "[id=3] Аня: третье длинное сообщение тут\n[id=4] Аня: четвёртое длинное сообщение тут\n" {
		t.Errorf("fresh section %q", fresh)
	}
}

// The history before the batch is bounded by context_messages; the batch itself is shown whole.
func TestEvaluationPromptIsBoundedByContext(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.ContextMessages = 2 })
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[]}` })
	e.say(e.msg("раз"), e.msg("два"), e.msg("три"))
	e.initiative()
	e.say(e.msg("четыре"), e.msg("пять"), e.msg("шесть"))
	e.initiative()
	earlier, fresh := newPart(t, e.llm.callsOf(llm.RoleFilter)[1])
	if earlier != "Ранее в чате:\n[id=2] Аня: два\n[id=3] Аня: три\n\n" || fresh != "[id=4] Аня: четыре\n[id=5] Аня: пять\n[id=6] Аня: шесть\n" {
		t.Errorf("earlier %q fresh %q", earlier, fresh)
	}
}

func TestOfflineDoesNotAccumulateBatches(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.online, f.decision = false, `{"actions":[]}` })
	m1, m2 := e.msg("первое длинное сообщение тут"), e.msg("второе длинное сообщение тут")
	e.say(m1, m2)
	e.initiative()
	if st, _ := e.st.ChatState(context.Background(), chat); st.LastEvaluatedID != m2.MessageID {
		t.Fatalf("offline batch must be marked seen, last evaluated %d", st.LastEvaluatedID)
	}

	e.llm.set(func(f *fakeLLM) { f.online = true })
	e.say(e.msg("третье длинное сообщение тут"))
	e.initiative()
	_, fresh := newPart(t, e.llm.callsOf(llm.RoleFilter)[0])
	if fresh != "[id=3] Аня: третье длинное сообщение тут\n" {
		t.Fatalf("messages seen while offline must only be context, fresh %q", fresh)
	}
}

func TestEvaluationActions(t *testing.T) {
	cases := []struct {
		decision string
		react    []reaction
		sent     int
	}{
		{`{"actions":[{"id":2,"react":"❤️"}]}`, []reaction{{2, "❤"}}, 0},
		{`{"actions":[{"id":1,"react":" 🔥 "},{"id":2,"react":"👏"}]}`, []reaction{{1, "🔥"}, {2, "👏"}}, 0},
		{`{"actions":[{"id":2,"react":"💩"}]}`, []reaction{{2, "👍"}}, 0},
		// "none" written out is no reaction, not 👍 for an unknown one.
		{`{"actions":[{"id":1,"react":"none"},{"id":2,"react":"Нет"}]}`, nil, 0},
		{`{"actions":[{"id":2,"react":"🔥"},{"id":2,"react":"👏"}]}`, []reaction{{2, "🔥"}}, 0}, // one per message
		{`{"actions":[{"id":99,"react":"🔥","reply":true}]}`, nil, 0},
		{`{"actions":[]}`, nil, 0},
		{`я думаю, лучше промолчать`, nil, 0},
		{`{"actions":[{"id":2,"reply":true}]}`, nil, 1},
		{`{"actions":[{"id":1,"reply":true},{"id":2,"reply":true}]}`, nil, 1}, // one own reply per batch
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			e := setup(t)
			e.llm.set(func(f *fakeLLM) { f.decision = c.decision })
			e.say(e.msg("ага"))
			if !e.pending() {
				t.Fatal("every message is evaluated, even a short one")
			}
			e.say(e.msg("я наконец-то починил прод"))
			e.tick()
			sent, reactions := e.out.snapshot()
			if !slices.Equal(reactions, c.react) || len(sent) != c.sent {
				t.Fatalf("reactions %v sent %v", reactions, sent)
			}
		})
	}
}

func TestInitiativeNeedsReplyQuota(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":1,"reply":true}]}`
		f.availFn = func(r llm.Role, _ llm.Priority) bool { return r == llm.RoleFilter }
	})
	e.say(e.msg("кто-нибудь знает, как работает горутина?"))
	e.initiative()
	if sent, _ := e.out.snapshot(); len(sent) != 0 || len(e.llm.callsOf(llm.RoleReply)) != 0 {
		t.Fatalf("no reply quota: must stay silent, sent %v", sent)
	}
}

func TestInitiativeReplyFailureKeepsCooldownFree(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":1,"reply":true}]}`
		f.replyErr = errors.New("boom")
	})
	e.say(e.msg("кто-нибудь знает, как работает горутина?"))
	e.initiative()
	if st, _ := e.st.ChatState(context.Background(), chat); !st.LastInitiativeAt.IsZero() {
		t.Fatal("a failed initiative must not start the cooldown")
	}
}

func TestStickersAreEvaluated(t *testing.T) {
	e := setup(t)
	e.say(Incoming{ChatID: chat, ChatType: "supergroup", MessageID: int(e.ids.Add(1)), UserID: anyaID,
		UserName: "Аня", Text: "[стикер 😀]", IsMedia: true, Date: e.clk.Now()})
	if !e.pending() {
		t.Fatal("a sticker is evaluated: instructions may cover it")
	}
}

// ---- housekeeping ----

func TestPruneDropsOldMessages(t *testing.T) {
	e := setup(t)
	old, fresh := e.msg("старое сообщение"), e.msg("свежее сообщение")
	old.Date = e.clk.Now().Add(-31 * 24 * time.Hour)
	fresh.Date = e.clk.Now().Add(-29 * 24 * time.Hour)
	e.say(old, fresh)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.b.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for e.get(old.MessageID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("Run did not prune on start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if e.get(fresh.MessageID) == nil {
		t.Fatal("messages within retention must be kept")
	}
}

func TestSleepCtx(t *testing.T) {
	start := time.Now()
	sleepCtx(context.Background(), -time.Second)
	sleepCtx(context.Background(), 0)
	sleepCtx(context.Background(), time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sleepCtx(ctx, time.Hour)
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx must honour short delays and cancellation")
	}
	start = time.Now()
	sleepCtx(context.Background(), 20*time.Millisecond)
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("sleepCtx returned before the delay")
	}
}

func TestHandleDirectSkipsAnsweredAndUnknownMessages(t *testing.T) {
	e := setup(t)
	m := e.msg("бот, привет")
	e.say(m)
	stored := e.get(m.MessageID)
	if !stored.Answered {
		t.Fatal("precondition: mention answered")
	}
	within(t, "direct reply", func() {
		e.b.handleDirect(context.Background(), *stored, true, nil)
		e.b.handleDirect(context.Background(), storage.Message{ChatID: chat, MessageID: 999}, true, nil) // pruned / never stored
	})
	if sent, reactions := e.out.snapshot(); len(sent) != 1 || len(reactions) != 0 {
		t.Fatalf("answered or unknown messages must be skipped: sent %v reactions %v", sent, reactions)
	}
}

func TestInitiativeWithNothingNew(t *testing.T) {
	e := setup(t)
	e.initiative() // empty chat
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[]}` })
	e.say(e.msg("кто-нибудь знает, как работает горутина?"))
	e.initiative()
	e.initiative() // everything already evaluated
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != 1 {
		t.Fatalf("filter calls %d, want 1", n)
	}
}

// ---- gaps found by go-mutesting ----

func TestPlainGroupIsServed(t *testing.T) {
	e := setup(t)
	in := e.msg("бот, привет")
	in.ChatType = "group"
	e.say(in)
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatalf("regular (non-super) groups must be served, sent %v", sent)
	}
}

func TestNoQuotaMeansNoGeneration(t *testing.T) {
	e := setup(t)
	e.llm.set(func(f *fakeLLM) { f.availFn = func(llm.Role, llm.Priority) bool { return false } })
	e.say(e.msg("бот, привет"))
	_, reactions := e.out.snapshot()
	if len(e.llm.callsOf(llm.RoleReply)) != 0 || len(reactions) != 1 {
		t.Fatalf("without quota the model must not be called: calls %+v reactions %v", e.llm.calls, reactions)
	}
}

func TestBusyChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	mention := storage.Message{ChatID: chat, MessageID: int(e.ids.Add(1)), UserID: anyaID, UserName: "Аня",
		Text: "бот, ты тут?", Date: e.clk.Now(), Addressed: true}
	if err := e.st.SaveMessage(ctx, mention); err != nil {
		t.Fatal(err)
	}
	l := e.b.lock(chat)
	l.Lock()

	retry := make(chan struct{})
	go func() { e.b.handleDirect(ctx, mention, false, nil); close(retry) }()
	select {
	case <-retry:
	case <-time.After(time.Second):
		t.Fatal("a catch-up retry must give up on a busy chat instead of waiting")
	}

	e.say(e.from(43, "Боря", "borya", "кто-нибудь знает, как работает горутина?")) // not in a dialog: no wait for the lock
	e.initiative()
	if !e.pending() || len(e.llm.callsOf(llm.RoleFilter)) != 0 {
		t.Fatal("initiative must skip a busy chat and keep it pending")
	}

	fresh := make(chan struct{})
	go func() { e.b.handleDirect(ctx, mention, true, nil); close(fresh) }()
	time.Sleep(50 * time.Millisecond)
	if sent, _ := e.out.snapshot(); len(sent) != 0 {
		t.Fatal("must not reply while another handler holds the chat")
	}
	l.Unlock()
	select {
	case <-fresh:
	case <-time.After(time.Second):
		t.Fatal("a fresh mention must be handled once the chat is free")
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatalf("a fresh mention waits for the lock instead of being dropped, sent %v", sent)
	}
}

func TestEvaluationWithSingleContextMessage(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.ContextMessages = 1 })
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[]}` })
	e.say(e.msg("раз два три"), e.msg("четыре пять шесть"))
	e.initiative()
	if _, fresh := newPart(t, e.llm.callsOf(llm.RoleFilter)[0]); fresh != "[id=1] Аня: раз два три\n[id=2] Аня: четыре пять шесть\n" {
		t.Fatalf("fresh section %q", fresh)
	}
}

// failStore breaks selected operations of the real store.
type failStore struct {
	Store
	fail string
}

var errDB = errors.New("database is down")

func (s *failStore) SaveMessage(ctx context.Context, m storage.Message) error {
	if s.fail == "save" {
		return errDB
	}
	return s.Store.SaveMessage(ctx, m)
}

func (s *failStore) Around(ctx context.Context, chatID int64, topic, root, upTo, limit int) ([]storage.Message, error) {
	if s.fail == "around" {
		return nil, errDB
	}
	return s.Store.Around(ctx, chatID, topic, root, upTo, limit)
}

func (s *failStore) Thread(ctx context.Context, chatID int64, root, head, tail int) ([]storage.Message, int, error) {
	if s.fail == "thread" {
		return nil, 0, errDB
	}
	return s.Store.Thread(ctx, chatID, root, head, tail)
}

func (s *failStore) Get(ctx context.Context, chatID int64, msgID int) (*storage.Message, error) {
	if s.fail == "get" {
		return nil, errDB
	}
	return s.Store.Get(ctx, chatID, msgID)
}

func (s *failStore) Fresh(ctx context.Context, chatID int64, afterID, limit int) ([]storage.Message, error) {
	if s.fail == "fresh" {
		return nil, errDB
	}
	return s.Store.Fresh(ctx, chatID, afterID, limit)
}

func (s *failStore) Before(ctx context.Context, chatID int64, beforeID, limit int) ([]storage.Message, error) {
	if s.fail == "before" {
		return nil, errDB
	}
	return s.Store.Before(ctx, chatID, beforeID, limit)
}

func (s *failStore) ChatState(ctx context.Context, chatID int64) (storage.ChatState, error) {
	if s.fail == "state" {
		return storage.ChatState{}, errDB
	}
	return s.Store.ChatState(ctx, chatID)
}

func TestStoreFailuresStopProcessing(t *testing.T) {
	for _, c := range []struct {
		fail, text string
	}{
		{"save", "бот, привет"},
		{"around", "бот, привет"},
		{"get", "бот, привет"},
		{"state", "кто-нибудь знает, как работает горутина?"},
		{"fresh", "кто-нибудь знает, как работает горутина?"},
		{"before", "кто-нибудь знает, как работает горутина?"},
	} {
		t.Run(c.fail+"/"+c.text, func(t *testing.T) {
			e := setup(t)
			e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":1,"reply":true}]}` })
			e.b.store = &failStore{Store: e.st, fail: c.fail}
			e.say(e.msg(c.text))
			e.initiative()
			sent, reactions := e.out.snapshot()
			if len(e.llm.calls) != 0 || len(sent) != 0 || len(reactions) != 0 {
				t.Fatalf("a storage error must stop processing: calls %d sent %v reactions %v", len(e.llm.calls), sent, reactions)
			}
		})
	}
}

func TestDialogReplyIgnoresCooldown(t *testing.T) {
	e := setup(t)
	e.say(e.msg("бот, что такое горутина?")) // Аня talks with the bot (answered directly)
	ctx := context.Background()
	started := e.clk.Now().Add(-time.Minute)
	if err := e.st.SetLastInitiative(ctx, chat, started); err != nil { // cooldown is running
		t.Fatal(err)
	}
	e.clk.Advance(5 * time.Minute)
	borya := e.from(43, "Боря", "borya", "а мне кто-нибудь объяснит каналы?")
	anya := e.msg("подожди, а чем она от потока отличается?") // a follow-up without addressing
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":` + strconv.Itoa(borya.MessageID) + `,"reply":true}]}`
	})
	e.say(borya, anya) // Аня is in a dialog: evaluated at once, not in the next batch
	f := e.llm.callsOf(llm.RoleFilter)[0].system()
	if want := "только в ответ на сообщения id=" + strconv.Itoa(anya.MessageID) + ", если они продолжают"; !strings.Contains(f, want) {
		t.Fatalf("in the cooldown the evaluator may reply only within the dialog; want %q in:\n%s", want, f)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatalf("a reply to Боря (not in a dialog) is an interjection and the cooldown forbids it: %v", sent)
	}

	// Аня's follow-up arrives again in the next batch: answering her is allowed and does not restart the cooldown.
	anya2 := e.msg("ну так чем?")
	e.llm.set(func(f *fakeLLM) {
		f.decision = joining(`{"actions":[{"id":`+strconv.Itoa(anya2.MessageID)+`,"reply":true}]}`, anya2.MessageID, 1)
	})
	e.say(anya2)
	sent, _ := e.out.snapshot()
	if len(sent) != 2 || sent[1].replyTo != anya2.MessageID {
		t.Fatalf("dialog reply in the cooldown: %v", sent)
	}
	if st, _ := e.st.ChatState(ctx, chat); !st.LastInitiativeAt.Equal(started) {
		t.Fatalf("a dialog reply must not restart the cooldown: %v want %v", st.LastInitiativeAt, started)
	}
}

func TestDialogReplyDoesNotStartCooldownAndExpires(t *testing.T) {
	e := setup(t)
	e.say(e.msg("бот, привет"))
	e.clk.Advance(time.Minute)
	q := e.msg("а ты что умеешь вообще?")
	e.say(q)
	e.llm.set(func(f *fakeLLM) {
		f.decision = joining(`{"actions":[{"id":`+strconv.Itoa(q.MessageID)+`,"reply":true}]}`, q.MessageID, 1)
	})
	e.initiative()
	if st, _ := e.st.ChatState(context.Background(), chat); !st.LastInitiativeAt.IsZero() {
		t.Fatalf("a dialog reply does not start the cooldown: %v", st.LastInitiativeAt)
	}

	// After dialog_minutes of silence the same person is no longer in a dialog.
	e.clk.Advance(11 * time.Minute)
	q2 := e.msg("кстати, кто-нибудь смотрел матч?")
	e.say(q2)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":` + strconv.Itoa(q2.MessageID) + `,"reply":true}]}`
	})
	e.initiative()
	if st, _ := e.st.ChatState(context.Background(), chat); !st.LastInitiativeAt.Equal(e.clk.Now()) {
		t.Fatalf("an interjection starts the cooldown: %v", st.LastInitiativeAt)
	}
}

func TestNormalizeWord(t *testing.T) {
	for in, want := range map[string]string{"Зверычь": "зверыч", "ЁЖИК": "ежик", "бот": "бот", "ьъ": "", "мышь": "мыш"} {
		if got := normalizeWord(in); got != want {
			t.Errorf("normalizeWord(%q)=%q want %q", in, got, want)
		}
	}
}

// A reply in a thread sees the thread, then the topic around it, and runs in the thread's session (SPEC 7a).
func TestReplyInThread(t *testing.T) {
	e := setup(t)
	root := e.msg("кто играет на Marshall?")
	aside := e.msg("у меня вчера струна лопнула")
	ask := e.msg("бот, а ты что скажешь?")
	ask.ReplyToID, ask.ReplyToUserID = root.MessageID, anyaID
	e.say(root, aside, ask)

	if len(e.llm.calls) != 1 {
		t.Fatalf("calls: %d", len(e.llm.calls))
	}
	c := e.llm.calls[0]
	u := c.msgs[1].Content
	thread, around := strings.Index(u, "Ветка разговора"), strings.Index(u, "вне этой ветки")
	if thread != 0 || around < 0 || !strings.Contains(u[:around], "[id=1]") || !strings.Contains(u[:around], "[id=3]") ||
		strings.Contains(u[:around], "[id=2]") || !strings.Contains(u[around:], "[id=2]") {
		t.Fatalf("thread first, the rest around it:\n%s", u)
	}
	if want := fmt.Sprintf("%d:%d", chat, root.MessageID); c.session != want {
		t.Fatalf("session %q, want %q", c.session, want)
	}

	// The bot's reply joins the thread; answering it continues the same session.
	sent, _ := e.out.snapshot()
	own := int(e.ids.Load()) // the bot's message got the last id
	if len(sent) != 1 || sent[0].replyTo != ask.MessageID || e.get(own).RootID != root.MessageID {
		t.Fatalf("bot's reply must join the thread: %+v %+v", sent, e.get(own))
	}
	more := e.msg("бот, точно?")
	more.ReplyToID, more.ReplyToUserID = own, botID
	e.say(more)
	if len(e.llm.calls) != 2 || e.llm.calls[1].session != c.session {
		t.Fatalf("same thread, same session: %+v", e.llm.calls)
	}

	// A message in no thread: the topic's last messages, and a session of its own.
	plain := e.msg("бот, привет")
	e.say(plain)
	last := e.llm.calls[len(e.llm.calls)-1]
	if !strings.HasPrefix(last.msgs[1].Content, "Переписка:") || last.session != fmt.Sprintf("%d:%d", chat, plain.MessageID) {
		t.Fatalf("no thread: %q session %q", last.msgs[1].Content, last.session)
	}
}

func TestReplyContextStoreFailure(t *testing.T) {
	e := setup(t)
	root := e.msg("корень")
	e.say(root)
	e.b.store = &failStore{Store: e.st, fail: "thread"}
	ask := e.msg("бот, что думаешь?")
	ask.ReplyToID, ask.ReplyToUserID = root.MessageID, anyaID
	e.say(ask)
	if sent, _ := e.out.snapshot(); len(sent) != 0 || len(e.llm.calls) != 0 {
		t.Fatalf("no reply without its context: sent %v calls %d", sent, len(e.llm.calls))
	}
}

// A message addressed to the bot without a reply is put into the thread it continues by meaning,
// and answered in that thread (SPEC 7a).
func TestAssignThreadOnAddress(t *testing.T) {
	for _, c := range []struct {
		name, answer string
		root         int // the asking message's thread after the answer: 1 — joined, its own id — started one
	}{
		{"continues the thread", `{"thread": 1}`, 1},
		{"in no thread", `{"thread": 0}`, 0},
		{"not a candidate", `{"thread": 99}`, 0},
		{"unparsable", `не знаю`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.llm.set(func(f *fakeLLM) { f.thread = c.answer })
			root := e.msg("кто играет на Marshall?")
			answer := e.msg("я, JCM800")
			answer.ReplyToID, answer.ReplyToUserID = root.MessageID, anyaID
			ask := e.msg("бот, а для метала он годится?")
			e.say(root, answer, ask)

			th := e.llm.callsOf(llm.RoleThread)
			if len(th) != 1 || th[0].prio != llm.Direct || !strings.Contains(th[0].user(), "Ветка №1:\n[id=1]") ||
				!strings.Contains(th[0].user(), "Новое сообщение:\n[id=3] Аня: бот, а для метала он годится?") {
				t.Fatalf("thread request: %+v", th)
			}
			want := cmp.Or(c.root, ask.MessageID)
			if got := e.get(ask.MessageID).RootID; got != want {
				t.Fatalf("thread %d, want %d", got, want)
			}
			r := e.llm.callsOf(llm.RoleReply)
			if len(r) != 1 || r[0].session != fmt.Sprintf("%d:%d", chat, want) ||
				strings.HasPrefix(r[0].user(), "Ветка разговора") != (c.root == 1) {
				t.Fatalf("reply: session %q\n%s", r[0].session, r[0].user())
			}
		})
	}
}

func TestAssignThreadOnlyWhenItCould(t *testing.T) {
	e := setup(t)
	e.say(e.msg("бот, привет")) // no threads yet
	root := e.msg("корень")
	e.say(root)
	ask := e.msg("бот, что думаешь?") // a reply: its thread is known without the model
	ask.ReplyToID, ask.ReplyToUserID = root.MessageID, anyaID
	e.say(ask)
	other := e.msg("бот, а тут?") // another topic: the thread is not a candidate
	other.ThreadID = 7
	e.say(other)
	if th := e.llm.callsOf(llm.RoleThread); len(th) != 0 {
		t.Fatalf("no thread request expected: %d", len(th))
	}

	old := e.msg("бот, вспомнил?") // the thread went quiet long ago
	e.clk.Advance(7 * time.Hour)
	old.Date = e.clk.Now()
	e.say(old)
	if th := e.llm.callsOf(llm.RoleThread); len(th) != 0 {
		t.Fatalf("an inactive thread is not a candidate: %d", len(th))
	}
}

// The evaluation sees threads and the flow outside them, and puts new messages without a reply
// into threads they continue (SPEC 7a).
func TestEvaluationAssignsThreads(t *testing.T) {
	e := setup(t)
	root := e.msg("кто играет на Marshall?")
	answer := e.msg("я, JCM800")
	answer.ReplyToID, answer.ReplyToUserID = root.MessageID, anyaID
	more := e.msg("и ещё 5150 есть")
	aside := e.msg("кто-нибудь видел мой медиатор?")
	elsewhere := e.msg("а тут что?")
	elsewhere.ThreadID = 7
	e.say(root, answer, more, aside, elsewhere)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":3,"reply":true}],"threads":[{"id":3,"thread":1},{"id":2,"thread":3},` +
			`{"id":4,"thread":99},{"id":5,"thread":1},{"id":404,"thread":1}]}`
	})
	e.initiative()

	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) != 1 {
		t.Fatalf("filter calls: %d", len(f))
	}
	threads, fresh := newPart(t, f[0])
	if !strings.Contains(threads, "Ветка №1:\n[id=1] Аня: кто играет на Marshall? [новое]\n[id=2] Аня → ответ на id=1: я, JCM800 [новое]\n") ||
		fresh != "[id=3] Аня: и ещё 5150 есть\n[id=4] Аня: кто-нибудь видел мой медиатор?\n[id=5] [топик 7] Аня: а тут что?\n" ||
		!strings.Contains(f[0].system(), `"threads"`) || !hasProperty(f[0].schema, "threads") {
		t.Fatalf("batch split into threads:\n%s\n---\n%s", threads, fresh)
	}
	// Only 3 joins: 2 is in a thread already, 99 and 404 are not shown, 5 is in another topic.
	for id, want := range map[int]int{2: 1, 3: 1, 4: 0, 5: 0} {
		if got := e.get(id).RootID; got != want {
			t.Errorf("message %d: thread %d, want %d", id, got, want)
		}
	}
	if r := e.llm.callsOf(llm.RoleReply); len(r) != 1 || r[0].session != fmt.Sprintf("%d:1", chat) || r[0].schema != nil ||
		!strings.HasPrefix(r[0].user(), "Ветка разговора") || !strings.Contains(r[0].user(), "[id=3]") {
		t.Fatalf("the reply is written in the thread: %+v", r)
	}
}

func TestEvaluationThreadsStoreFailure(t *testing.T) {
	e := setup(t)
	root := e.msg("корень")
	answer := e.msg("ответ")
	answer.ReplyToID, answer.ReplyToUserID = root.MessageID, anyaID
	e.say(root, answer)
	e.b.store = &failStore{Store: e.st, fail: "thread"}
	e.initiative()
	if f := e.llm.callsOf(llm.RoleFilter); len(f) != 0 {
		t.Fatalf("no evaluation without its threads: %d", len(f))
	}
}

// Right after talking with the bot, a new question to the whole chat is not a dialog: in the
// cooldown the bot does not answer it (the shakshuka case, 2026-11-09).
func TestNewTopicAfterDialogIsNotADialog(t *testing.T) {
	e := setup(t)
	e.say(e.msg("бот, что такое горутина?")) // answered directly: the bot's thread is 1
	if err := e.st.SetLastInitiative(context.Background(), chat, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Minute)
	q := e.msg("Ребзя, как шакшуку готовить?")
	e.say(q)
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":` + strconv.Itoa(q.MessageID) + `,"reply":true}]}` })
	e.initiative()
	if got := e.sentTo(q.MessageID); len(got) != 0 {
		t.Fatalf("a new topic is not a dialog: %v", got)
	}

	// A message already in someone else's thread is not a dialog even as a candidate.
	other := e.from(43, "Боря", "borya", "кто идёт на джем?")
	e.say(other)
	ans := e.msg("я иду")
	ans.ReplyToID, ans.ReplyToUserID = other.MessageID, 43
	e.say(ans)
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":` + strconv.Itoa(ans.MessageID) + `,"reply":true}]}` })
	e.initiative()
	if got := e.sentTo(ans.MessageID); len(got) != 0 {
		t.Fatalf("someone else's thread is not a dialog: %v", got)
	}
	if f := e.llm.callsOf(llm.RoleFilter); strings.Contains(f[len(f)-1].system(), "только в ответ на сообщения") {
		t.Fatalf("no dialog candidates expected:\n%s", f[len(f)-1].system())
	}
}
