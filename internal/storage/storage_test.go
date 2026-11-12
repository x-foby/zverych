package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "nested", "dir", "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func save(t *testing.T, s *Store, ms ...Message) {
	t.Helper()
	for _, m := range ms {
		if err := s.SaveMessage(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
}

func ids(ms []Message) []int {
	out := []int{}
	for _, m := range ms {
		out = append(out, m.MessageID)
	}
	return out
}

func TestMessageRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	full := Message{ChatID: -1, MessageID: 5, ThreadID: 3, UserID: 42, UserName: "Аня", Username: "anya", Text: "привет",
		ReplyTo: 4, Date: t0, IsSelf: true, Addressed: true, Answered: true, Kind: KindJoin, RootID: 4}
	plain := Message{ChatID: -1, MessageID: 6, UserID: 43, UserName: "Боря", Text: "ок", Date: t0}
	save(t, s, full, plain)

	got, err := s.Get(ctx, -1, 5)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	got.Date = got.Date.UTC()
	if !reflect.DeepEqual(*got, full) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", *got, full)
	}
	if got, _ := s.Get(ctx, -1, 6); got.IsSelf || got.Addressed || got.Answered || got.Kind != "" {
		t.Fatalf("flags must default to false: %+v", got)
	}
	if got, err := s.Get(ctx, -1, 404); got != nil || err != nil {
		t.Fatalf("missing message: %v %v", got, err)
	}
	if got, _ := s.Get(ctx, -2, 5); got != nil {
		t.Fatal("messages are scoped by chat")
	}

	// INSERT OR IGNORE: a redelivered update does not overwrite the stored message.
	save(t, s, Message{ChatID: -1, MessageID: 5, UserID: 42, UserName: "Аня", Text: "другой", Date: t0})
	if got, _ := s.Get(ctx, -1, 5); got.Text != "привет" || !got.Answered {
		t.Fatalf("duplicate overwrote: %+v", got)
	}
}

func TestRecentFreshBefore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, id := range []int{3, 1, 4, 2, 5} {
		save(t, s, Message{ChatID: -1, MessageID: id, UserName: "u", Text: "x", Date: t0})
	}
	save(t, s, Message{ChatID: -2, MessageID: 9, UserName: "u", Text: "x", Date: t0})
	if err := s.SetReactions(ctx, -1, 4, 7, []string{"🔥"}, t0); err != nil {
		t.Fatal(err)
	}

	recent, err := s.Recent(ctx, -1, 3)
	if err != nil || !reflect.DeepEqual(ids(recent), []int{3, 4, 5}) {
		t.Fatalf("Recent: %v %v", ids(recent), err)
	}
	fresh, err := s.Fresh(ctx, -1, 1, 10)
	if err != nil || !reflect.DeepEqual(ids(fresh), []int{2, 3, 4, 5}) {
		t.Fatalf("Fresh: %v %v", ids(fresh), err)
	}
	if fresh, _ := s.Fresh(ctx, -1, 1, 2); !reflect.DeepEqual(ids(fresh), []int{4, 5}) || len(fresh[0].Reactions) != 1 {
		t.Fatalf("Fresh keeps the newest, with reactions: %+v", fresh)
	}
	if fresh, _ := s.Fresh(ctx, -1, 5, 10); len(fresh) != 0 {
		t.Fatalf("nothing after the last id: %v", ids(fresh))
	}
	before, err := s.Before(ctx, -1, 5, 2)
	if err != nil || !reflect.DeepEqual(ids(before), []int{3, 4}) || len(before[1].Reactions) != 1 {
		t.Fatalf("Before: %+v %v", before, err)
	}
	if before, _ := s.Before(ctx, -1, 1, 10); len(before) != 0 {
		t.Fatalf("nothing before the first id: %v", ids(before))
	}
}

func TestPendingMentionsAndMarkAnswered(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -2, MessageID: 1, UserName: "u", Text: "x", Date: t0, Addressed: true},
		Message{ChatID: -1, MessageID: 7, UserName: "u", Text: "x", Date: t0, Addressed: true},
		Message{ChatID: -1, MessageID: 3, UserName: "u", Text: "x", Date: t0, Addressed: true},
		Message{ChatID: -1, MessageID: 4, UserName: "u", Text: "x", Date: t0},                                  // not addressed
		Message{ChatID: -1, MessageID: 5, UserName: "u", Text: "x", Date: t0, Addressed: true, Answered: true}, // done
		Message{ChatID: -1, MessageID: 2, UserName: "u", Text: "x", Date: t0.Add(-time.Second), Addressed: true},
	)
	pending, err := s.PendingMentions(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		chat int64
		id   int
	}
	var got []key
	for _, m := range pending {
		got = append(got, key{m.ChatID, m.MessageID})
	}
	if want := []key{{-2, 1}, {-1, 3}, {-1, 7}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending %v want %v (since is inclusive, ordered by chat and id)", got, want)
	}

	if err := s.MarkAnsweredUpTo(ctx, -1, 3); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingMentions(ctx, t0.Add(-time.Hour))
	if !reflect.DeepEqual(ids(pending), []int{1, 7}) {
		t.Fatalf("after marking up to 3 in chat -1: %v", ids(pending))
	}
	if m, _ := s.Get(ctx, -1, 4); m.Answered {
		t.Fatal("non-addressed messages are never marked answered")
	}
}

func TestChatState(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	st, err := s.ChatState(ctx, -1)
	if err != nil || !st.LastInitiativeAt.IsZero() || st.LastEvaluatedID != 0 {
		t.Fatalf("empty state: %+v %v", st, err)
	}
	if err := s.SetLastEvaluated(ctx, -1, 10); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.ChatState(ctx, -1); st.LastEvaluatedID != 10 || !st.LastInitiativeAt.IsZero() {
		t.Fatalf("only evaluated id set: %+v", st)
	}
	if err := s.SetLastInitiative(ctx, -1, t0); err != nil {
		t.Fatal(err)
	}
	s.SetLastEvaluated(ctx, -1, 11)
	if st, _ := s.ChatState(ctx, -1); st.LastEvaluatedID != 11 || !st.LastInitiativeAt.Equal(t0) {
		t.Fatalf("both fields must survive each other's updates: %+v", st)
	}
	if err := s.SetLastInitiative(ctx, -2, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.ChatState(ctx, -2); st.LastEvaluatedID != 0 || !st.LastInitiativeAt.Equal(t0) {
		t.Fatalf("initiative on a fresh chat: %+v", st)
	}
}

func TestQuota(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if n, err := s.QuotaGet(ctx, "g", "flash", "2026-11-02"); n != 0 || err != nil {
		t.Fatalf("empty quota: %d %v", n, err)
	}
	for range 3 {
		if err := s.QuotaInc(ctx, "g", "flash", "2026-11-02"); err != nil {
			t.Fatal(err)
		}
	}
	s.QuotaInc(ctx, "g", "lite", "2026-11-02")
	s.QuotaInc(ctx, "g", "flash", "2026-11-03")
	for _, c := range []struct {
		model, day string
		want       int
	}{{"flash", "2026-11-02", 3}, {"lite", "2026-11-02", 1}, {"flash", "2026-11-03", 1}} {
		if n, _ := s.QuotaGet(ctx, "g", c.model, c.day); n != c.want {
			t.Errorf("%s %s: %d want %d", c.model, c.day, n, c.want)
		}
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -1, MessageID: 1, UserName: "u", Text: "old", Date: t0.Add(-time.Second)},
		Message{ChatID: -1, MessageID: 2, UserName: "u", Text: "edge", Date: t0},
	)
	s.QuotaInc(ctx, "g", "m", "2026-11-01")
	s.QuotaInc(ctx, "g", "m", "2026-11-02")
	if err := s.Prune(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.Recent(ctx, -1, 10); !reflect.DeepEqual(ids(ms), []int{2}) {
		t.Fatalf("after prune: %v", ids(ms))
	}
	if n, _ := s.QuotaGet(ctx, "g", "m", "2026-11-01"); n != 0 {
		t.Fatal("old quota rows must be dropped")
	}
	if n, _ := s.QuotaGet(ctx, "g", "m", "2026-11-02"); n != 1 {
		t.Fatal("current quota rows must stay")
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	save(t, s, Message{ChatID: -1, MessageID: 1, UserName: "u", Text: "x", Date: t0})
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, _ := s.Get(context.Background(), -1, 1); m == nil {
		t.Fatal("data must survive reopen (migration is idempotent)")
	}
}

func TestOpenFailsOnBadPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(file, "bot.db")); err == nil || !strings.Contains(err.Error(), "create db dir") {
		t.Fatalf("want a directory error, got %v", err)
	}
}

func TestClosedStoreReturnsErrors(t *testing.T) {
	s := open(t)
	s.Close()
	ctx := context.Background()
	m := Message{ChatID: -1, MessageID: 1, UserName: "u", Text: "x", Date: t0}
	errs := map[string]error{"SaveMessage": s.SaveMessage(ctx, m), "MarkAnsweredUpTo": s.MarkAnsweredUpTo(ctx, -1, 1),
		"SetLastEvaluated": s.SetLastEvaluated(ctx, -1, 1), "SetLastInitiative": s.SetLastInitiative(ctx, -1, t0),
		"QuotaInc": s.QuotaInc(ctx, "p", "m", "d"), "Prune": s.Prune(ctx, t0)}
	got, err := s.Get(ctx, -1, 1)
	if got != nil {
		t.Error("Get returned a message from a closed store")
	}
	errs["Get"] = err
	_, errs["Recent"] = s.Recent(ctx, -1, 10)
	_, errs["Fresh"] = s.Fresh(ctx, -1, 0, 10)
	_, errs["Before"] = s.Before(ctx, -1, 10, 10)
	_, _, errs["Thread"] = s.Thread(ctx, -1, 1, 3, 10)
	_, errs["Around"] = s.Around(ctx, -1, 0, 1, 10, 10)
	_, errs["PendingMentions"] = s.PendingMentions(ctx, t0)
	_, errs["ChatState"] = s.ChatState(ctx, -1)
	_, errs["QuotaGet"] = s.QuotaGet(ctx, "p", "m", "d")
	for name, err := range errs {
		if err == nil {
			t.Errorf("%s must report an error on a closed database", name)
		}
	}
}

func TestInDialog(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -1, MessageID: 1, ThreadID: 3, UserID: 42, UserName: "Аня", Text: "бот, привет", Date: t0, Addressed: true},
		Message{ChatID: -1, MessageID: 2, ThreadID: 3, UserID: 43, UserName: "Боря", Text: "как дела", Date: t0.Add(time.Minute)},
		Message{ChatID: -1, MessageID: 3, ThreadID: 3, UserID: 1, UserName: "Бот", Text: "норм", Date: t0.Add(2 * time.Minute), IsSelf: true, ReplyTo: 2},
		Message{ChatID: -1, MessageID: 4, ThreadID: 5, UserID: 44, UserName: "Вера", Text: "эй", Date: t0.Add(2 * time.Minute)},
		Message{ChatID: -2, MessageID: 1, ThreadID: 3, UserID: 44, UserName: "Вера", Text: "бот", Date: t0, Addressed: true},
	)
	for _, c := range []struct {
		name   string
		thread int
		user   int64
		since  time.Time
		want   bool
	}{
		{"addressed the bot", 3, 42, t0, true},
		{"too long ago", 3, 42, t0.Add(time.Second), false},
		{"got a reply from the bot", 3, 43, t0, true},
		{"reply was before the window", 3, 43, t0.Add(3 * time.Minute), false},
		{"other topic", 5, 42, t0, false},
		{"wrote, but not to the bot", 5, 44, t0, false},
		{"talked with the bot in another chat", 3, 44, t0, false},
	} {
		got, err := s.InDialog(ctx, -1, c.thread, c.user, c.since)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v %v want %v", c.name, got, err, c.want)
		}
	}
}

func TestThreads(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	msg := func(id, topic, replyTo int) Message {
		return Message{ChatID: -1, MessageID: id, ThreadID: topic, UserID: 42, UserName: "Аня", Text: "т", ReplyTo: replyTo, Date: t0}
	}
	save(t, s,
		msg(1, 0, 0),  // becomes a root when answered
		msg(2, 0, 1),  // reply: starts the thread of 1
		msg(3, 0, 2),  // reply to a reply: same thread
		msg(4, 0, 0),  // no reply: no thread (assigned by meaning later)
		msg(5, 0, 1),  // another branch of the same root
		msg(6, 0, 99), // reply to a message the bot never saw: rooted there
		msg(7, 0, 6),
		msg(8, 5, 0),
		msg(9, 7, 8), // reply across topics joins no thread
	)
	m := Message{ChatID: -1, MessageID: 10, UserID: 42, UserName: "Аня", Text: "т", ReplyTo: 4, RootID: 1, Date: t0}
	save(t, s, m)            // a thread set by the caller is kept and does not root the replied message
	save(t, s, msg(2, 0, 4)) // redelivery changes nothing

	for id, want := range map[int]int{1: 1, 2: 1, 3: 1, 4: 0, 5: 1, 6: 99, 7: 99, 8: 0, 9: 0, 10: 1} {
		got, err := s.Get(ctx, -1, id)
		if err != nil || got.RootID != want {
			t.Fatalf("message %d: root %d (%v), want %d", id, got.RootID, err, want)
		}
	}
}

func TestThreadAndAround(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	msg := func(id, topic, replyTo int) Message {
		return Message{ChatID: -1, MessageID: id, ThreadID: topic, UserID: 42, UserName: "Аня", Text: "т", ReplyTo: replyTo, Date: t0}
	}
	save(t, s, msg(1, 0, 0), msg(2, 0, 1), msg(3, 0, 0), msg(4, 0, 2), msg(5, 0, 4), msg(6, 0, 5), msg(7, 0, 0),
		msg(8, 9, 0), msg(9, 0, 6), Message{ChatID: -2, MessageID: 10, UserID: 1, UserName: "Б", Text: "т", ReplyTo: 1, Date: t0})
	if err := s.SetReactions(ctx, -1, 6, 50, []string{"🔥"}, t0); err != nil {
		t.Fatal(err)
	}

	// Thread of 1: 1 2 4 5 6 9.
	ms, total, err := s.Thread(ctx, -1, 1, 2, 2)
	if err != nil || total != 6 || !reflect.DeepEqual(ids(ms), []int{1, 2, 6, 9}) || len(ms[2].Reactions) != 1 {
		t.Fatalf("head and tail: %v total %d %v %+v", ids(ms), total, err, ms)
	}
	if ms, total, _ := s.Thread(ctx, -1, 1, 3, 10); total != 6 || !reflect.DeepEqual(ids(ms), []int{1, 2, 4, 5, 6, 9}) {
		t.Fatalf("short thread is whole: %v %d", ids(ms), total)
	}
	if ms, total, _ := s.Thread(ctx, -1, 404, 3, 10); total != 0 || len(ms) != 0 {
		t.Fatalf("no thread: %v %d", ids(ms), total)
	}

	// Around: the same topic, up to the message, outside the thread.
	if ms, _ := s.Around(ctx, -1, 0, 1, 9, 10); !reflect.DeepEqual(ids(ms), []int{3, 7}) {
		t.Fatalf("around the thread: %v", ids(ms))
	}
	if ms, _ := s.Around(ctx, -1, 0, 0, 6, 3); !reflect.DeepEqual(ids(ms), []int{4, 5, 6}) {
		t.Fatalf("no thread: the topic's last messages: %v", ids(ms))
	}
	if ms, _ := s.Around(ctx, -1, 9, 0, 100, 10); !reflect.DeepEqual(ids(ms), []int{8}) {
		t.Fatalf("other topic: %v", ids(ms))
	}
}

func TestActiveThreadsAndSetRoot(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := func(id, topic, replyTo int, d time.Duration) Message {
		return Message{ChatID: -1, MessageID: id, ThreadID: topic, UserID: 42, UserName: "Аня", Text: "т", ReplyTo: replyTo, Date: t0.Add(d)}
	}
	save(t, s,
		at(1, 0, 0, 0), at(2, 0, 1, 0), // thread 1, old
		at(3, 0, 0, time.Hour), at(4, 0, 3, time.Hour), // thread 3
		at(5, 0, 0, 2*time.Hour), at(6, 0, 5, 2*time.Hour), // thread 5
		at(7, 0, 2, 3*time.Hour),                           // thread 1 again: the most recent
		at(8, 4, 0, 3*time.Hour), at(9, 4, 8, 3*time.Hour), // another topic
		at(10, 0, 0, 3*time.Hour), at(11, 0, 0, 3*time.Hour),
	)
	got, err := s.ActiveThreads(ctx, -1, 0, t0.Add(time.Hour), 10)
	if err != nil || !reflect.DeepEqual(got, []int{1, 5, 3}) {
		t.Fatalf("active threads: %v %v", got, err)
	}
	if got, _ := s.ActiveThreads(ctx, -1, 0, t0.Add(time.Hour), 2); !reflect.DeepEqual(got, []int{1, 5}) {
		t.Fatalf("limit: %v", got)
	}
	if got, _ := s.ActiveThreads(ctx, -1, 4, t0, 10); !reflect.DeepEqual(got, []int{8}) {
		t.Fatalf("topic: %v", got)
	}

	if moved, err := s.SetRoot(ctx, -1, 10, 3); !moved || err != nil || s.mustRoot(t, 10) != 3 {
		t.Fatalf("set root: %v %v", moved, err)
	}
	if moved, _ := s.SetRoot(ctx, -1, 10, 5); moved || s.mustRoot(t, 10) != 3 {
		t.Fatal("a message in a thread stays there")
	}
	// Any message of a thread names it.
	if moved, _ := s.SetRoot(ctx, -1, 11, 10); !moved || s.mustRoot(t, 11) != 3 {
		t.Fatalf("joined the thread of message 10: %d", s.mustRoot(t, 11))
	}
	// A message in no thread becomes a root, like a replied-to one.
	save(t, s, at(12, 0, 0, 0), at(13, 0, 0, 0))
	if moved, _ := s.SetRoot(ctx, -1, 13, 12); !moved || s.mustRoot(t, 13) != 12 || s.mustRoot(t, 12) != 12 {
		t.Fatal("new root")
	}
	if moved, _ := s.SetRoot(ctx, -1, 404, 3); moved {
		t.Fatal("missing message")
	}
	s.Close()
	if _, err := s.SetRoot(ctx, -1, 10, 3); err == nil {
		t.Fatal("closed store")
	}
	if _, err := s.ActiveThreads(ctx, -1, 0, t0, 1); err == nil {
		t.Fatal("closed store")
	}
}

func (s *Store) mustRoot(t *testing.T, id int) int {
	t.Helper()
	m, err := s.Get(context.Background(), -1, id)
	if err != nil || m == nil {
		t.Fatalf("get %d: %v", id, err)
	}
	return m.RootID
}

func TestSelfInThread(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -1, MessageID: 1, UserID: 42, UserName: "Аня", Text: "т", Date: t0},
		Message{ChatID: -1, MessageID: 2, UserID: 1, UserName: "Бот", Text: "т", ReplyTo: 1, IsSelf: true, Date: t0},
		Message{ChatID: -1, MessageID: 3, UserID: 42, UserName: "Аня", Text: "т", Date: t0},
		Message{ChatID: -1, MessageID: 4, UserID: 43, UserName: "Боря", Text: "т", ReplyTo: 3, Date: t0},
		Message{ChatID: -1, MessageID: 5, UserID: 1, UserName: "Бот", Text: "т", IsSelf: true, Date: t0}, // a root itself
		Message{ChatID: -1, MessageID: 6, UserID: 42, UserName: "Аня", Text: "т", ReplyTo: 5, Date: t0},
	)
	for root, want := range map[int]bool{1: true, 3: false, 5: true, 404: false} {
		if got, err := s.SelfInThread(ctx, -1, root); err != nil || got != want {
			t.Errorf("thread %d: %v %v, want %v", root, got, err, want)
		}
	}
	if got, _ := s.SelfInThread(ctx, -2, 1); got {
		t.Error("threads are scoped by chat")
	}
}
