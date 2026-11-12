package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReactions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		save(t, s, Message{ChatID: -1, MessageID: i, UserID: 1, UserName: "Аня", Text: "x", Date: t0})
	}
	set := func(msg int, user int64, at time.Time, emojis ...string) {
		t.Helper()
		if err := s.SetReactions(ctx, -1, msg, user, emojis, at); err != nil {
			t.Fatal(err)
		}
	}
	set(2, 10, t0, "🔥")
	set(2, 11, t0.Add(time.Second), "❤", "🔥")
	set(2, 12, t0.Add(2*time.Second), "👍")
	set(2, 13, t0.Add(3*time.Second), "❤")
	set(2, 14, t0.Add(4*time.Second), "😁", "😁") // duplicates are counted once
	set(3, 10, t0, "👎")
	set(3, 10, t0, "👍") // the new set replaces the old one
	set(4, 10, t0, "🔥")
	set(4, 10, t0)                                                            // all removed
	set(9, 10, t0, "🔥")                                                       // outside the window below
	if err := s.SetReactions(ctx, -2, 2, 10, []string{"🎉"}, t0); err != nil { // another chat
		t.Fatal(err)
	}

	ms, err := s.Recent(ctx, -1, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int][]ReactionCount{}
	for _, m := range ms {
		got[m.MessageID] = m.Reactions
	}
	want := map[int][]ReactionCount{
		// Most popular first; equal counts — whichever appeared first.
		2: {{"🔥", 2}, {"❤", 2}, {"👍", 1}, {"😁", 1}},
		3: {{"👍", 1}},
		4: nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions:\n got %v\nwant %v", got, want)
	}

	// Pruned with the history.
	set(3, 11, t0.Add(48*time.Hour), "🤝")
	if err := s.Prune(ctx, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&n)
	if n != 1 {
		t.Fatalf("old reactions must be pruned, %d left", n)
	}
	if ms, err := s.Recent(ctx, -1, 3); err != nil || len(ms) != 0 {
		t.Fatalf("empty history: %v %v", ms, err)
	}
}

// A database created before kind / event existed gets the columns on open.
func TestOpenMigratesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
		thread_id INTEGER NOT NULL DEFAULT 0, user_id INTEGER NOT NULL, user_name TEXT NOT NULL, username TEXT NOT NULL DEFAULT '',
		text TEXT NOT NULL, reply_to INTEGER NOT NULL DEFAULT 0, date INTEGER NOT NULL, is_self INTEGER NOT NULL DEFAULT 0,
		addressed INTEGER NOT NULL DEFAULT 0, answered INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (chat_id, message_id));
	INSERT INTO messages(chat_id, message_id, user_id, user_name, text, date) VALUES (-1, 1, 5, 'Аня', 'старое', 0);
	INSERT INTO messages(chat_id, message_id, thread_id, user_id, user_name, text, reply_to, date) VALUES
		(-1, 10, 3, 5, 'Аня', 'в топике без reply', 3, 0),
		(-1, 11, 3, 6, 'Боря', 'ответ', 10, 0),
		(-1, 12, 3, 5, 'Аня', 'ответ на ответ', 11, 0);
	CREATE TABLE reaction_rules (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL, lore_id INTEGER NOT NULL,
		chat_wide INTEGER NOT NULL, thread_id INTEGER NOT NULL DEFAULT 0, user_id INTEGER NOT NULL DEFAULT 0,
		username TEXT NOT NULL DEFAULT '', user_name TEXT NOT NULL DEFAULT '', emoji TEXT NOT NULL);
	INSERT INTO reaction_rules(chat_id, lore_id, chat_wide, emoji) VALUES (-1, 1, 1, '❤');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for range 2 { // the second open must not try to add the columns again
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if m, err := s.Get(ctx, -1, 1); err != nil || m == nil || m.Text != "старое" || m.Kind != "" {
			t.Fatalf("old message: %+v %v", m, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('reaction_rules', 'reply_rules')`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("machine rule tables must be dropped: %d %v", n, err)
		}
		save(t, s, Message{ChatID: -1, MessageID: 2, UserID: 5, UserName: "Аня", Text: "[вступил(а) в чат]", Kind: KindJoin, Date: t0})
		// Threads are assigned to the old history; a plain topic message is not a reply.
		for id, want := range map[int][2]int{1: {0, 0}, 10: {0, 10}, 11: {10, 10}, 12: {11, 10}} {
			if m, _ := s.Get(ctx, -1, id); m.ReplyTo != want[0] || m.RootID != want[1] {
				t.Fatalf("message %d: reply_to %d root %d, want %v", id, m.ReplyTo, m.RootID, want)
			}
		}
		if m, _ := s.Get(ctx, -1, 2); m.Kind != KindJoin {
			t.Fatalf("new column is used: %+v", m)
		}
		s.Close()
	}
}

func TestTopicActivity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	add := func(id, thread int, user int64, kind string, self bool) {
		t.Helper()
		save(t, s, Message{ChatID: -1, MessageID: id, ThreadID: thread, UserID: user, UserName: "x", Text: "x", Date: t0, Kind: kind, IsSelf: self})
	}
	add(1, 5, 10, "", false)
	add(2, 5, 11, KindJoin, false)
	add(3, 0, 12, "", false)        // another topic
	add(4, 5, 1, "", true)          // the bot's own
	add(5, 5, 13, KindLeave, false) // neither a message nor a join
	add(6, 5, 14, "", false)
	add(7, 5, 15, "", false)
	save(t, s, Message{ChatID: -2, MessageID: 8, ThreadID: 5, UserID: 16, UserName: "x", Text: "x", Date: t0}) // another chat
	for _, r := range []struct {
		msg  int
		user int64
		e    string
	}{{6, 20, "🔥"}, {6, 21, "🔥"}, {2, 22, "👋"}, {3, 23, "👍"}, {4, 24, "❤"}} {
		if err := s.SetReactions(ctx, -1, r.msg, r.user, []string{r.e}, t0); err != nil {
			t.Fatal(err)
		}
	}

	a, err := s.TopicActivity(ctx, -1, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ms []Message) []int {
		var out []int
		for _, m := range ms {
			out = append(out, m.MessageID)
		}
		return out
	}
	if got := ids(a.Messages); !reflect.DeepEqual(got, []int{7, 6}) {
		t.Errorf("messages %v: the latest people wrote in the topic, newest first, limited", got)
	}
	if !reflect.DeepEqual(a.Messages[1].Reactions, []ReactionCount{{"🔥", 2}}) || a.Messages[0].Reactions != nil {
		t.Errorf("reactions %+v", a.Messages)
	}
	if got := ids(a.Joins); !reflect.DeepEqual(got, []int{2}) || !reflect.DeepEqual(a.Joins[0].Reactions, []ReactionCount{{"👋", 1}}) {
		t.Errorf("joins %+v", a.Joins)
	}
	// Reactions to the bot's messages show who is around too; other topics do not count.
	if !reflect.DeepEqual(a.Reactors, []int64{20, 21, 22, 24}) {
		t.Errorf("reactors %v", a.Reactors)
	}
	if a, err := s.TopicActivity(ctx, -1, 9, 10); err != nil || a.Messages != nil || a.Joins != nil || a.Reactors != nil {
		t.Errorf("empty topic: %+v %v", a, err)
	}
}
