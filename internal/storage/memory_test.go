package storage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func roleSummary(t *testing.T, s *Store, chatID int64) []string {
	t.Helper()
	rs, err := s.Roles(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Role+":"+r.Label())
	}
	return out
}

func TestPersonLabel(t *testing.T) {
	for p, want := range map[Person]string{
		{Name: "Вася", Username: "vasya"}: "Вася (@vasya)",
		{Username: "vasya"}:               "@vasya",
		{Name: "Вася"}:                    "Вася",
		{ID: 5}:                           "?",
	} {
		if got := p.Label(); got != want {
			t.Errorf("%+v.Label()=%q want %q", p, got, want)
		}
	}
}

func TestRoles(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	vasya := Person{ID: 50, Name: "Вася", Username: "vasya"}
	petya := Person{ID: 60, Name: "Петя"}

	if got := roleSummary(t, s, -1); len(got) != 0 {
		t.Fatalf("no roles yet: %v", got)
	}
	if ok, err := s.AddAuthority(ctx, -1, petya, 7, t0); err != nil || !ok {
		t.Fatalf("add authority: %v %v", ok, err)
	}
	if err := s.AddLead(ctx, -1, vasya, 7, t0); err != nil {
		t.Fatal(err)
	}
	if got, want := roleSummary(t, s, -1), []string{"lead:Вася (@vasya)", "authority:Петя"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles %v want %v (lead first)", got, want)
	}
	if ok, _ := s.AddAuthority(ctx, -1, vasya, 7, t0); ok {
		t.Fatal("the lead already holds a role: not added as authority")
	}

	// Admins add up (SPEC 4.7); an authority promoted to admin loses the authority role; the same
	// admin twice is one.
	if err := s.AddLead(ctx, -1, petya, 7, t0); err != nil {
		t.Fatal(err)
	}
	s.AddLead(ctx, -1, vasya, 7, t0)
	if got, want := roleSummary(t, s, -1), []string{"lead:Петя", "lead:Вася (@vasya)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roles %v want %v", got, want)
	}
	if ok, err := s.RemoveRole(ctx, -1, vasya, RoleLead); err != nil || !ok {
		t.Fatalf("remove one admin: %v %v", ok, err)
	}
	if got, want := roleSummary(t, s, -1), []string{"lead:Петя"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the other admin stays: %v", got)
	}
	if got := roleSummary(t, s, -2); len(got) != 0 {
		t.Fatalf("roles are per chat: %v", got)
	}

	if ok, err := s.RemoveRole(ctx, -1, vasya, RoleLead); err != nil || ok {
		t.Fatalf("Вася is no longer an admin: %v %v", ok, err)
	}
	if ok, err := s.RemoveRole(ctx, -1, Person{}, RoleLead); err != nil || !ok {
		t.Fatalf("remove whoever is the lead: %v %v", ok, err)
	}
	if got := roleSummary(t, s, -1); len(got) != 0 {
		t.Fatalf("lead removed: %v", got)
	}
}

func TestRoleByUsernameIsBoundLater(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.AddAuthority(ctx, -1, Person{Username: "Kolya"}, 7, t0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AddAuthority(ctx, -1, Person{Username: "kolya"}, 7, t0); ok {
		t.Fatal("username match is case-insensitive")
	}
	if err := s.BindUsername(ctx, -1, 70, "", "Без ника"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindUsername(ctx, -2, 70, "kolya", "Коля"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Roles(ctx, -1); rs[0].ID != 0 {
		t.Fatal("binding is per chat")
	}
	if err := s.BindUsername(ctx, -1, 70, "KOLYA", "Коля"); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Roles(ctx, -1)
	if len(rs) != 1 || rs[0].ID != 70 || rs[0].Name != "Коля" || rs[0].Username != "Kolya" || rs[0].GrantedBy != 7 ||
		!rs[0].GrantedAt.Equal(t0) {
		t.Fatalf("bound role %+v", rs)
	}
	// Once bound, the role is matched by id.
	if ok, _ := s.RemoveRole(ctx, -1, Person{ID: 70}, RoleAuthority); !ok {
		t.Fatal("remove by id after binding")
	}
}

func TestFindByUsername(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -1, MessageID: 1, UserID: 50, UserName: "Вася", Username: "vasya", Text: "a", Date: t0},
		Message{ChatID: -1, MessageID: 2, UserID: 50, UserName: "Василий", Username: "vasya", Text: "b", Date: t0},
		Message{ChatID: -1, MessageID: 3, UserID: 1, UserName: "Бот", Username: "bot", Text: "c", Date: t0, IsSelf: true},
	)
	if p, ok, err := s.FindByUsername(ctx, -1, "@VASYA"); err != nil || !ok || p != (Person{ID: 50, Name: "Василий", Username: "vasya"}) {
		t.Fatalf("latest name wins: %+v %v %v", p, ok, err)
	}
	if _, ok, _ := s.FindByUsername(ctx, -2, "vasya"); ok {
		t.Fatal("lookup is per chat")
	}
	if _, ok, _ := s.FindByUsername(ctx, -1, "bot"); ok {
		t.Fatal("the bot itself is not a participant")
	}
}

func TestLore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id1, err := s.AddLore(ctx, Lore{ChatID: -1, ChatWide: true, ThreadID: 5, Kind: LoreFact, Text: "Вася — основатель",
		AuthorID: 7, AuthorRole: "admin", SourceMsg: 10, CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.AddLore(ctx, Lore{ChatID: -1, ThreadID: 3, Kind: LoreInstruction, Text: "не писать", AuthorID: 50,
		AuthorRole: "lead", CreatedAt: t0})
	s.AddLore(ctx, Lore{ChatID: -2, ChatWide: true, Kind: LoreFact, Text: "чужое", CreatedAt: t0})

	ls, err := s.Lore(ctx, -1)
	if err != nil || len(ls) != 2 {
		t.Fatalf("lore %+v %v", ls, err)
	}
	want := Lore{ID: id1, ChatID: -1, ChatWide: true, ThreadID: 0, Kind: LoreFact, Text: "Вася — основатель",
		AuthorID: 7, AuthorRole: "admin", SourceMsg: 10, CreatedAt: t0}
	if ls[0].CreatedAt.Equal(t0) {
		ls[0].CreatedAt = t0 // compare the instant, not the location
	}
	if !reflect.DeepEqual(ls[0], want) {
		t.Fatalf("chat-wide entry (thread forced to 0):\n got %+v\nwant %+v", ls[0], want)
	}
	if ls[1].ID != id2 || ls[1].ChatWide || ls[1].ThreadID != 3 || ls[1].Kind != LoreInstruction {
		t.Fatalf("topic entry %+v", ls[1])
	}

	if ok, _ := s.DeleteLore(ctx, -2, id1); ok {
		t.Fatal("cannot delete another chat's entry")
	}
	if ok, err := s.DeleteLore(ctx, -1, id1); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.DeleteLore(ctx, -1, id1); ok {
		t.Fatal("already deleted")
	}
	if ls, _ := s.Lore(ctx, -1); len(ls) != 1 || ls[0].ID != id2 {
		t.Fatalf("after delete %+v", ls)
	}
}

func TestLoreSurvivesPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.AddLore(ctx, Lore{ChatID: -1, ChatWide: true, Kind: LoreFact, Text: "старое", CreatedAt: t0})
	if err := s.Prune(ctx, t0.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.Lore(ctx, -1); len(ls) != 1 {
		t.Fatal("lore is not subject to message retention")
	}
}

func TestMigrateMovesRolesAndLore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.RegisterChat(ctx, -1, "g", 7, t0)
	s.AddLead(ctx, -1, Person{ID: 50, Name: "Вася"}, 7, t0)
	s.AddLore(ctx, Lore{ChatID: -1, ChatWide: true, Kind: LoreFact, Text: "факт", SourceMsg: 12, CreatedAt: t0})
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if got := roleSummary(t, s, -1001); !reflect.DeepEqual(got, []string{"lead:Вася"}) {
		t.Fatalf("roles after migration %v", got)
	}
	ls, _ := s.Lore(ctx, -1001)
	if len(ls) != 1 || ls[0].SourceMsg != 0 {
		t.Fatalf("lore after migration %+v (source message ids do not carry over)", ls)
	}
	if got := roleSummary(t, s, -1); len(got) != 0 {
		t.Fatalf("old id keeps nothing: %v", got)
	}
}

func TestMarkHandled(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s, Message{ChatID: -1, MessageID: 1, UserID: 7, UserName: "Влад", Text: "он главный", Date: t0})
	if err := s.MarkHandled(ctx, -1, 1); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Get(ctx, -1, 1)
	if !m.Addressed || !m.Answered {
		t.Fatalf("handled message %+v", m)
	}
	if p, _ := s.PendingMentions(ctx, t0.Add(-1)); len(p) != 0 {
		t.Fatal("a handled message is not a pending mention")
	}
}

func TestUpdateInferredLore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	bot, _ := s.AddLore(ctx, Lore{ChatID: -1, ThreadID: 5, Kind: LoreFact, Text: "в топике 5 рецепты", AuthorRole: LoreBot, SourceMsg: 3, CreatedAt: t0})
	lead, _ := s.AddLore(ctx, Lore{ChatID: -1, ChatWide: true, Kind: LoreFact, Text: "от админа", AuthorID: 7, AuthorRole: RoleLead, CreatedAt: t0})

	later := t0.Add(time.Hour)
	if ok, err := s.UpdateLore(ctx, Lore{ID: bot, ChatID: -1, ChatWide: true, ThreadID: 5, Text: "везде рецепты", SourceMsg: 9, CreatedAt: later}); err != nil || !ok {
		t.Fatalf("own entry: %v %v", ok, err)
	}
	if ok, _ := s.UpdateLore(ctx, Lore{ID: lead, ChatID: -1, Text: "переписано"}); ok {
		t.Fatal("the leadership's entry is not the bot's to rewrite")
	}
	if ok, _ := s.UpdateLore(ctx, Lore{ID: bot, ChatID: -2, Text: "чужой чат"}); ok {
		t.Fatal("another chat's entry")
	}
	ls, _ := s.Lore(ctx, -1)
	if l := ls[0]; !l.Inferred() || !l.ChatWide || l.ThreadID != 0 || l.Text != "везде рецепты" || l.SourceMsg != 9 || !l.CreatedAt.Equal(later) {
		t.Fatalf("rewritten entry %+v (chat-wide drops the topic)", l)
	}
	if ls[1].Text != "от админа" || ls[1].Inferred() {
		t.Fatalf("%+v", ls[1])
	}

	s.DeleteLore(ctx, -1, bot)
	if ok, _ := s.UpdateLore(ctx, Lore{ID: bot, ChatID: -1, Text: "вернулось"}); ok {
		t.Fatal("a forgotten entry does not come back")
	}
	if ls, _ := s.Lore(ctx, -1); len(ls) != 1 {
		t.Fatalf("%+v", ls)
	}
}
