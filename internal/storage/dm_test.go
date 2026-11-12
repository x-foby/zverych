package storage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestDMsAreKeptApart(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i, m := range []DM{
		{UserID: 7, MessageID: 1, Text: "привет", Date: t0},
		{UserID: 7, MessageID: 2, FromSelf: true, Text: "здорово", Date: t0},
		{UserID: 8, MessageID: 1, Text: "чужая личка", Date: t0},
		{UserID: 7, MessageID: 3, Text: "как дела", Date: t0.Add(time.Hour)},
	} {
		if err := s.SaveDM(ctx, m); err != nil {
			t.Fatal(i, err)
		}
	}
	ms, err := s.RecentDMs(ctx, 7, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Text != "здорово" || !ms[0].FromSelf || ms[1].Text != "как дела" || !ms[1].Date.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the last ones, oldest first: %+v", ms)
	}
	// Private chats never reach a group's history (their ids are the users' ids).
	if got, _ := s.Recent(ctx, 7, 10); len(got) != 0 {
		t.Fatalf("not group messages: %+v", got)
	}
	if err := s.Prune(ctx, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.RecentDMs(ctx, 7, 10); len(ms) != 1 || ms[0].MessageID != 3 {
		t.Fatalf("retention applies to private chats: %+v", ms)
	}
}

func TestSelectedChat(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, ok, err := s.SelectedChat(ctx, 7); err != nil || ok {
		t.Fatal("nothing selected yet", ok, err)
	}
	s.SelectChat(ctx, 7, -1)
	s.SelectChat(ctx, 7, -2)
	s.SelectChat(ctx, 8, -1)
	if id, ok, _ := s.SelectedChat(ctx, 7); !ok || id != -2 {
		t.Fatalf("the last choice: %d", id)
	}
	s.RegisterChat(ctx, -1, "g", 7, t0)
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.SelectedChat(ctx, 8); id != -1001 {
		t.Fatalf("a migrated group stays selected: %d", id)
	}
	s.ResetChat(ctx, -1001)
	if _, ok, _ := s.SelectedChat(ctx, 8); ok {
		t.Fatal("a turned-off group is not selected any more")
	}
}

func TestLeadChatsAndFindPerson(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, id := range []int64{-1, -2, -3} {
		s.RegisterChat(ctx, id, "g", 7, t0)
	}
	s.SetChatState(ctx, -1, ChatActive, ModeTalk)
	s.SetChatState(ctx, -2, ChatActive, ModeSilent)
	s.AddLead(ctx, -1, Person{ID: 50, Name: "Вася"}, 7, t0)
	s.AddLead(ctx, -2, Person{Username: "Vasya"}, 7, t0)         // by @username: the id is not known yet
	s.AddLead(ctx, -3, Person{ID: 50}, 7, t0)                    // an off group does not count
	s.AddAuthority(ctx, -1, Person{ID: 60, Name: "Петя"}, 7, t0) // nor does another role
	got, err := s.LeadChats(ctx, 50, "vasya")
	if err != nil || !reflect.DeepEqual(got, []int64{-2, -1}) {
		t.Fatalf("lead chats %v %v", got, err)
	}
	if got, _ := s.LeadChats(ctx, 50, ""); !reflect.DeepEqual(got, []int64{-1}) {
		t.Fatalf("by id only %v", got)
	}
	if got, _ := s.LeadChats(ctx, 60, "petya"); len(got) != 0 {
		t.Fatalf("an authority is not an admin %v", got)
	}

	if _, ok, _ := s.FindPerson(ctx, "@vasya"); ok {
		t.Fatal("not seen yet")
	}
	s.SaveMessage(ctx, Message{ChatID: -1, MessageID: 1, UserID: 50, UserName: "Вася", Username: "vasya", Text: "a", Date: t0})
	s.SaveMessage(ctx, Message{ChatID: -2, MessageID: 1, UserID: 50, UserName: "Василий", Username: "Vasya", Text: "b", Date: t0.Add(time.Hour)})
	s.SaveMessage(ctx, Message{ChatID: -2, MessageID: 2, UserID: 1, UserName: "Бот", Username: "vasya", Text: "c", Date: t0.Add(2 * time.Hour), IsSelf: true})
	if p, ok, err := s.FindPerson(ctx, "@VASYA"); err != nil || !ok || p != (Person{ID: 50, Name: "Василий", Username: "Vasya"}) {
		t.Fatalf("the latest of any group, the bot never: %+v %v %v", p, ok, err)
	}
}
