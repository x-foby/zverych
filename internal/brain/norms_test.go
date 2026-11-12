package brain

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// react stores a participant's reactions to a message.
func (e *env) react(msgID int, userID int64, emojis ...string) {
	e.t.Helper()
	e.b.OnReaction(context.Background(), Reaction{ChatID: chat, ChatType: "supergroup", MessageID: msgID,
		UserID: userID, Emojis: emojis, Date: e.clk.Now()})
}

func (e *env) inTopic(in Incoming, thread int) Incoming {
	in.ThreadID = thread
	return in
}

func TestLearnHabit(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	m := func(at time.Time, emojis ...string) storage.Message {
		var rs []storage.ReactionCount
		for _, e := range emojis {
			rs = append(rs, storage.ReactionCount{Emoji: e, Count: 1})
		}
		return storage.Message{Date: at, Reactions: rs}
	}
	settled := now.Add(-reactionSettle)
	for _, c := range []struct {
		name string
		ms   []storage.Message
		want habit
		ok   bool
	}{
		{"no messages", nil, habit{}, false},
		{"every one", []storage.Message{m(old, "🤝"), m(old, "🤝", "🔥"), m(old, "🤝")}, habit{"🤝", 1}, true},
		{"too few", []storage.Message{m(old, "🤝"), m(old, "🤝")}, habit{}, false},
		{"too rare", []storage.Message{m(old, "🤝"), m(old, "🤝"), m(old, "🤝"), m(old), m(old), m(old), m(old)}, habit{}, false},
		{"exactly half", []storage.Message{m(old, "🔥"), m(old, "🔥"), m(old, "🔥"), m(old), m(old), m(old)}, habit{"🔥", 0.5}, true},
		// Fresh messages are still collecting reactions: they neither count as seen nor as reacted.
		{"fresh skipped", []storage.Message{m(now), m(now), m(now, "🤝"), m(old, "🤝"), m(old, "🤝"), m(old, "🤝")}, habit{"🤝", 1}, true},
		{"variation selector", []storage.Message{m(old, "❤️"), m(old, "❤"), m(old, "❤️")}, habit{"❤", 1}, true},
		{"the bot cannot set it", []storage.Message{m(old, "[свой эмодзи]"), m(old, "[свой эмодзи]"), m(old, "[свой эмодзи]")}, habit{}, false},
		{"the most popular", []storage.Message{m(old, "🔥"), m(old, "👍", "🔥"), m(old, "👍", "🔥"), m(old, "👍")}, habit{"🔥", 0.75}, true},
		{"custom first, then a standard one", []storage.Message{m(old, "[свой эмодзи]", "🔥"), m(old, "[свой эмодзи]", "🔥"), m(old, "🔥")}, habit{"🔥", 1}, true},
		// Ties go by first appearance (the newest message), not by when the count was reached.
		{"tie: first seen wins", []storage.Message{m(old, "👍"), m(old, "🔥"), m(old, "🔥"), m(old, "🔥"), m(old, "👍"), m(old, "👍")}, habit{"👍", 0.5}, true},
		{"tie: the most recent", []storage.Message{m(old, "👍"), m(old, "🔥"), m(old, "👍", "🔥"), m(old, "🔥", "👍")}, habit{"👍", 0.75}, true},
	} {
		got, ok := learnHabit(c.ms, settled, 3, 0.5)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAdminsOnly(t *testing.T) {
	admins := map[int64]bool{1: true, 2: true}
	isAdmin := func(id int64) bool { return admins[id] }
	msgs := func(n int, users ...int64) []storage.Message {
		var out []storage.Message
		for i := range n {
			out = append(out, storage.Message{UserID: users[i%len(users)]})
		}
		return out
	}
	for _, c := range []struct {
		name string
		a    storage.TopicActivity
		want bool
	}{
		{"members react", storage.TopicActivity{Messages: msgs(10, 1, 2), Reactors: []int64{1, 30}}, true},
		{"members join", storage.TopicActivity{Messages: msgs(10, 1), Joins: []storage.Message{{UserID: 31}}}, true},
		{"too few messages", storage.TopicActivity{Messages: msgs(9, 1), Reactors: []int64{30}}, false},
		{"a member writes", storage.TopicActivity{Messages: append(msgs(10, 1), storage.Message{UserID: 30}), Reactors: []int64{30}}, false},
		// Nobody else is around: a chat of admins is not a topic where members keep silent.
		{"only admins around", storage.TopicActivity{Messages: msgs(10, 1, 2), Reactors: []int64{2}, Joins: []storage.Message{{UserID: 1}}}, false},
	} {
		if got := adminsOnly(c.a, isAdmin); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// seedJoins stores n joins in the topic, each with the given reactions from members, and lets
// them settle.
func (e *env) seedJoins(thread, n int, emojis ...string) {
	e.t.Helper()
	for i := range n {
		j := e.inTopic(e.join(int64(1000+i), "Новичок", ""), thread)
		e.say(j)
		for k, emoji := range emojis {
			e.react(j.MessageID, int64(2000+k), emoji)
		}
	}
	e.clk.Advance(reactionSettle + time.Second)
}

// Habits are hints for the model now: the code does not react by itself (2026-11-08).
func TestHabitIsAHintNotAnAction(t *testing.T) {
	e := setup(t)
	e.seedJoins(3, 3, "🤝")
	e.initiative() // the seeded joins themselves

	j := e.inTopic(e.join(petyaID, "Петя", "petya"), 3)
	other := e.inTopic(e.join(vasyaID, "Вася", "vasya"), 4) // another topic has no such habit
	e.llm.set(func(f *fakeLLM) { f.online = false })
	e.say(j, other)
	if got := e.reactionsTo(j.MessageID); got != nil {
		t.Fatalf("no reaction without the model: %v", got)
	}
	// The bot's own reactions are not observations: they would reinforce the habit by themselves.
	e.react(j.MessageID, botID, "🤝")
	a, _ := e.st.TopicActivity(context.Background(), chat, 3, 10)
	if len(a.Joins) != 4 || a.Joins[0].Reactions != nil {
		t.Fatalf("the bot's reaction is not stored: %+v", a.Joins[0])
	}

	e.llm.set(func(f *fakeLLM) {
		f.online = true
		f.decision = `{"actions":[{"id":` + strconv.Itoa(j.MessageID) + `,"react":"🤝"}]}`
	})
	j2 := e.inTopic(e.join(int64(77), "Оля", ""), 3)
	e.say(j2)
	e.llm.set(func(f *fakeLLM) {
		f.decision = `{"actions":[{"id":` + strconv.Itoa(j2.MessageID) + `,"react":"🤝"}]}`
	})
	e.initiative()
	if got := e.reactionsTo(j2.MessageID); !reflect.DeepEqual(got, []string{"🤝"}) {
		t.Fatalf("the model follows the custom: %v", got)
	}
	f := e.llm.callsOf(llm.RoleFilter)
	if sys := f[len(f)-1].system(); !strings.Contains(sys, "Местные обычаи") ||
		!strings.Contains(sys, "- [топик 3] на вступления принято ставить 🤝 (примерно на 100%)") {
		t.Fatalf("the evaluator knows the topic's customs:\n%s", sys)
	}
}

func TestHabitsCanBeDisabled(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Behavior.HabitMinShare = 2 })
	e.seedJoins(0, 5, "🤝")
	e.say(e.join(petyaID, "Петя", "petya"))
	e.initiative()
	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) == 0 || strings.Contains(f[len(f)-1].system(), "Местные обычаи") {
		t.Fatalf("habit_min_share above 1 disables habits: %+v", f)
	}
}

func TestNormsAreCached(t *testing.T) {
	e := setup(t)
	e.seedJoins(0, 3, "🤝")
	ctx := context.Background()
	if _, ok := e.b.topicNorms(ctx, chat, 0).habits[storage.KindJoin]; !ok {
		t.Fatal("habit learned")
	}
	// New observations wait for the cache to expire.
	for i := range 3 {
		e.react(1+i, int64(3000+i), "🔥")
		e.react(1+i, int64(3100+i), "🔥")
	}
	if h := e.b.topicNorms(ctx, chat, 0).habits[storage.KindJoin]; h.emoji != "🤝" {
		t.Fatalf("cached: %+v", h)
	}
	e.clk.Advance(normsTTL)
	if h := e.b.topicNorms(ctx, chat, 0).habits[storage.KindJoin]; h.emoji != "🔥" {
		t.Fatalf("recomputed after normsTTL: %+v", h)
	}
}

// seedAdminTopic fills the topic with announcements by a chat admin that members react to.
func (e *env) seedAdminTopic(thread int, admin int64) {
	e.t.Helper()
	for i := range quietMinMessages {
		m := e.inTopic(e.from(admin, "Модератор", "mod", "анонс номер "+strconv.Itoa(i)+" для всех"), thread)
		e.say(m)
		e.react(m.MessageID, vasyaID, "👍")
	}
	e.clk.Advance(normsTTL) // the norms cached on the first announcement expire
}

// A topic where only admins write is a hint for the model, not a ban (2026-11-08).
func TestAdminsOnlyTopicIsAHint(t *testing.T) {
	const modID = int64(70)
	e := setup(t)
	e.out.admins = []int64{modID}
	e.seedAdminTopic(5, modID)
	e.initiative() // the announcements themselves are not the point

	q := e.inTopic(e.from(modID, "Модератор", "mod", "кто придёт на встречу в субботу?"), 5)
	e.say(q, e.msg("кто-нибудь знает хороший компрессор?"))
	e.llm.set(func(f *fakeLLM) { f.decision = `{"actions":[{"id":` + strconv.Itoa(q.MessageID) + `,"reply":true}]}` })
	e.initiative()
	f := e.llm.callsOf(llm.RoleFilter)
	if sys := f[len(f)-1].system(); !strings.Contains(sys, "- [топик 5] пишут только админы, остальные лишь реагируют — тебе здесь лучше не писать") {
		t.Fatalf("the evaluator is told about the topic:\n%s", sys)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 1 || sent[0].replyTo != q.MessageID || sent[0].thread != 5 {
		t.Fatalf("the model decides: %+v", sent)
	}
	if e.out.adminReqs != 1 {
		t.Errorf("the admin list is cached: %d requests", e.out.adminReqs)
	}
}

func TestAdminListUnavailable(t *testing.T) {
	const modID = int64(70)
	e := setup(t)
	e.out.admins = []int64{modID}
	e.out.adminsErr = errors.New("not enough rights")
	e.seedAdminTopic(5, modID)
	if e.b.topicNorms(context.Background(), chat, 5).adminsOnly {
		t.Fatal("without the admin list nothing is known: the bot keeps writing")
	}
	// The bot's own admins count as admins even without Telegram's list.
	e = setup(t)
	e.seedAdminTopic(5, adminID)
	e.out.adminsErr = nil
	if !e.b.topicNorms(context.Background(), chat, 5).adminsOnly {
		t.Fatal("the bot's admins are admins")
	}
}

func TestNormsLines(t *testing.T) {
	norms := map[int]topicNorms{
		0: {habits: map[string]habit{storage.KindJoin: {"🤝", 1}}},
		5: {adminsOnly: true, habits: map[string]habit{"": {"🔥", 0.6}}},
		7: {habits: map[string]habit{}},
	}
	want := []string{
		"- на вступления принято ставить 🤝 (примерно на 100%)",
		"- [топик 5] пишут только админы, остальные лишь реагируют — тебе здесь лучше не писать; на сообщения часто ставят 🔥 (примерно на 60%)",
	}
	if got := normsLines(norms, -1, roster{}); !reflect.DeepEqual(got, want) {
		t.Errorf("all topics:\n%q", got)
	}
	if got := normsLines(norms, 5, roster{}); !reflect.DeepEqual(got, []string{strings.Replace(want[1], "[топик 5] ", "", 1)}) {
		t.Errorf("one topic, untagged: %q", got)
	}
	if got := normsLines(norms, 0, roster{}); !reflect.DeepEqual(got, want[:1]) {
		t.Errorf("the general topic only: %q", got)
	}
	if got := normsLines(norms, 7, roster{}); got != nil {
		t.Errorf("nothing learned: %q", got)
	}
}

// The chat's admin list is cached for adminsTTL, independently of the topic norms' cache.
func TestAdminListCached(t *testing.T) {
	const modID = int64(70)
	e := setup(t)
	e.out.admins = []int64{modID}
	e.seedAdminTopic(5, modID)
	ctx := context.Background()
	if !e.b.topicNorms(ctx, chat, 5).adminsOnly || e.out.adminReqs != 1 {
		t.Fatalf("admins-only topic, one request: %d", e.out.adminReqs)
	}
	e.clk.Advance(adminsTTL - normsTTL - time.Second) // the norms expire, the admin list does not
	e.b.topicNorms(ctx, chat, 5)
	if e.out.adminReqs != 1 {
		t.Fatalf("the admin list is reused: %d requests", e.out.adminReqs)
	}
	e.out.admins = nil // the moderator lost the rights
	e.clk.Advance(normsTTL + time.Second)
	if e.b.topicNorms(ctx, chat, 5).adminsOnly || e.out.adminReqs != 2 {
		t.Fatalf("reloaded after adminsTTL: %d requests", e.out.adminReqs)
	}
}
