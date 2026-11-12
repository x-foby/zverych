package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func chatOf(t *testing.T, s *Store, id int64) (Chat, bool) {
	t.Helper()
	c, ok, err := s.Chat(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c, ok
}

func TestRegisterChatStartsOffAndKeepsState(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, ok := chatOf(t, s, -1); ok {
		t.Fatal("unknown chat")
	}
	if err := s.RegisterChat(ctx, -1, "Группа", 42, t0); err != nil {
		t.Fatal(err)
	}
	c, ok := chatOf(t, s, -1)
	if !ok || c.State != ChatOff || c.Mode != ModeTalk || c.Title != "Группа" || c.AddedBy != 42 || !c.AddedAt.Equal(t0) {
		t.Fatalf("new chat %+v", c)
	}
	if ok, err := s.SetChatState(ctx, -1, ChatActive, ModeReactions); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Registering again (a status echo, a message) keeps the state; an empty title keeps the old one.
	s.RegisterChat(ctx, -1, "", 8, t0)
	if c, _ := chatOf(t, s, -1); c.State != ChatActive || c.Mode != ModeReactions || c.Title != "Группа" || c.AddedBy != 42 {
		t.Fatalf("re-registered %+v", c)
	}
	s.RegisterChat(ctx, -1, "Переименована", 8, t0)
	if c, _ := chatOf(t, s, -1); c.Title != "Переименована" {
		t.Fatalf("title %+v", c)
	}
	if ok, err := s.SetChatState(ctx, -99, ChatActive, ModeTalk); err != nil || ok {
		t.Fatal("an unknown chat is not changed", ok, err)
	}
	s.RegisterChat(ctx, -2, "аккорды", 7, t0)
	cs, err := s.Chats(ctx)
	if err != nil || len(cs) != 2 || cs[0].Title != "аккорды" || cs[1].ID != -1 {
		t.Fatalf("chats by title %+v %v", cs, err)
	}
}

// fillChat puts something into every table that holds a group's data.
func fillChat(t *testing.T, s *Store, id int64) {
	t.Helper()
	ctx := context.Background()
	for _, err := range []error{
		s.RegisterChat(ctx, id, "g", 7, t0),
		s.SaveMessage(ctx, Message{ChatID: id, MessageID: 1, UserID: 50, UserName: "Вася", Text: "привет", Date: t0}),
		s.SetReactions(ctx, id, 1, 51, []string{"🔥"}, t0),
		s.SetLastEvaluated(ctx, id, 1),
		s.AddLead(ctx, id, Person{ID: 50, Name: "Вася"}, 7, t0),
		s.AddWantedAdmin(ctx, WantedAdmin{ChatID: id, Username: "petya", RequestedBy: 7, RequestedAt: t0}),
		s.SelectChat(ctx, -id, id), // a private chat of its own per group
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddLore(ctx, Lore{ChatID: id, ChatWide: true, Kind: LoreFact, Text: "факт", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSummary(ctx, id, 0, Summary{}, "сводка", 1, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNote(ctx, Note{ChatID: id, UserID: 50, Chat: "ценят", Reputation: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTopicName(ctx, id, 7, "Флуд"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPersona(ctx, id, "Ты — Шарманка", 50, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPersonaDraft(ctx, PersonaDraft{ChatID: id, UserID: 50, Text: "Ты — Гармонь", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
}

// rowsOf counts the group's rows in every data table.
func rowsOf(t *testing.T, s *Store, id int64) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tb := range chatTables {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+tb+` WHERE chat_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tb] = n
	}
	return out
}

func TestResetAndForgetChatWipeEverything(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	fillChat(t, s, -1)
	fillChat(t, s, -2)
	s.SetChatState(ctx, -1, ChatActive, ModeSilent)
	for tb, n := range rowsOf(t, s, -1) {
		if n == 0 {
			t.Fatalf("fillChat left %s empty: the test would prove nothing", tb)
		}
	}
	if err := s.ResetChat(ctx, -1); err != nil {
		t.Fatal(err)
	}
	for tb, n := range rowsOf(t, s, -1) {
		if n != 0 {
			t.Errorf("reset left %d rows in %s", n, tb)
		}
	}
	if c, ok := chatOf(t, s, -1); !ok || c.State != ChatOff || c.Mode != ModeTalk {
		t.Fatalf("a reset chat stays known and off: %+v", c)
	}
	if err := s.ForgetChat(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if _, ok := chatOf(t, s, -1); ok {
		t.Fatal("a forgotten chat is not known")
	}
	for tb, n := range rowsOf(t, s, -2) {
		if n == 0 {
			t.Errorf("another chat lost its %s", tb)
		}
	}
	s.ForgetChat(ctx, -2)
	for tb, n := range rowsOf(t, s, -2) {
		if n != 0 {
			t.Errorf("forget left %d rows in %s", n, tb)
		}
	}
}

func TestWantedAdmins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	add := func(w WantedAdmin) {
		w.RequestedBy, w.RequestedAt = 7, t0
		if err := s.AddWantedAdmin(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	add(WantedAdmin{ChatID: -1, Username: "vasya"})
	add(WantedAdmin{ChatID: -1, Name: "Петя Иванов"})
	add(WantedAdmin{ChatID: -1, Username: "Vasya"})   // the same person again
	add(WantedAdmin{ChatID: -1, Name: "петя иванов"}) // and again
	add(WantedAdmin{ChatID: -2, Username: "other"})
	ws, err := s.WantedAdmins(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.Label())
	}
	if !reflect.DeepEqual(got, []string{"@vasya", "Петя Иванов"}) || ws[0].RequestedBy != 7 || !ws[0].RequestedAt.Equal(t0) {
		t.Fatalf("wanted %v %+v", got, ws)
	}
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if ws, _ := s.WantedAdmins(ctx, -1001); len(ws) != 2 {
		t.Fatalf("migration moves the wanted admins: %+v", ws)
	}
	if err := s.ClearWantedAdmins(ctx, -1001); err != nil {
		t.Fatal(err)
	}
	if ws, _ := s.WantedAdmins(ctx, -1001); len(ws) != 0 {
		t.Fatal("cleared")
	}
	if ws, _ := s.WantedAdmins(ctx, -2); len(ws) != 1 {
		t.Fatal("another chat keeps its wanted admins")
	}
}

func TestMigrateChatMovesState(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.RegisterChat(ctx, -1, "g", 7, t0)
	s.SetChatState(ctx, -1, ChatActive, ModeReactions)
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if _, ok := chatOf(t, s, -1); ok {
		t.Fatal("the old id is gone")
	}
	if c, ok := chatOf(t, s, -1001); !ok || c.State != ChatActive || c.Mode != ModeReactions {
		t.Fatalf("migrated %+v", c)
	}
	// Migrating onto an id that already exists replaces it instead of failing.
	s.RegisterChat(ctx, -2, "", 7, t0)
	if err := s.MigrateChat(ctx, -2, -1001); err != nil {
		t.Fatal(err)
	}
	if c, _ := chatOf(t, s, -1001); c.State != ChatOff {
		t.Fatalf("replaced %+v", c)
	}
}

// Groups allowed before the states keep working: the added columns default to active and talking.
func TestLegacyChatsBecomeActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE chats (chat_id INTEGER PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
		added_by INTEGER NOT NULL, added_at INTEGER NOT NULL);
		INSERT INTO chats VALUES(-1, 'Старая', 7, 1700000000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c, ok := chatOf(t, s, -1); !ok || c.State != ChatActive || c.Mode != ModeTalk || c.Title != "Старая" {
		t.Fatalf("legacy chat %+v", c)
	}
	s.RegisterChat(context.Background(), -2, "Новая", 7, t0)
	if c, _ := chatOf(t, s, -2); c.State != ChatOff {
		t.Fatalf("a new chat after the upgrade is off: %+v", c)
	}
}
