package storage

import (
	"context"
	"testing"
	"time"
)

func TestNoteLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if ns, err := s.Notes(ctx, -1, []int64{50}); err != nil || len(ns) != 0 {
		t.Fatalf("no notes yet: %+v %v", ns, err)
	}
	if ns, err := s.Notes(ctx, -1, nil); err != nil || len(ns) != 0 {
		t.Fatalf("nobody asked: %+v %v", ns, err)
	}
	n := Note{ChatID: -1, UserID: 50, Name: "Вася", Username: "vasya", Chat: "уважают", Own: "любит Tool", Reputation: 1}
	if ok, err := s.SetNote(ctx, n, t0); err != nil || !ok {
		t.Fatalf("first note: %v %v", ok, err)
	}
	s.SetNote(ctx, Note{ChatID: -1, UserID: 51, Name: "Боря", Reputation: -2}, t0)
	s.SetNote(ctx, Note{ChatID: -2, UserID: 50, Name: "Вася", Reputation: 2}, t0)
	ns, _ := s.Notes(ctx, -1, []int64{50, 51, 52})
	first := ns[50]
	if len(ns) != 2 || first.Chat != "уважают" || first.Own != "любит Tool" || first.Reputation != 1 || first.Version != 1 ||
		first.Username != "vasya" || !first.UpdatedAt.Equal(t0) || first.Person().Label() != "Вася (@vasya)" {
		t.Fatalf("notes %+v", ns)
	}
	if rs, _ := s.Reputations(ctx, -1); len(rs) != 2 || rs[50] != 1 || rs[51] != -2 {
		t.Fatalf("reputations are per chat: %v", rs)
	}

	// Written over a note that changed since it was read: nothing changes.
	stale := n
	stale.Chat = "устарело"
	if ok, err := s.SetNote(ctx, stale, t0); err != nil || ok {
		t.Fatalf("stale write must be refused: %v %v", ok, err)
	}
	next := first
	next.Own, next.Username = "любит Tool и Meshuggah", "vasya2"
	if ok, _ := s.SetNote(ctx, next, t0.Add(time.Hour)); !ok {
		t.Fatal("update over the note read")
	}
	ns, _ = s.Notes(ctx, -1, []int64{50})
	if ns[50].Own != "любит Tool и Meshuggah" || ns[50].Username != "vasya2" || ns[50].Version != 2 {
		t.Fatalf("updated note %+v", ns[50])
	}

	// Erased: an update made from the note before erasure does not bring it back.
	before := ns[50]
	if ok, err := s.ClearNote(ctx, -1, 50); err != nil || !ok {
		t.Fatalf("clear: %v %v", ok, err)
	}
	if ok, _ := s.ClearNote(ctx, -1, 50); ok {
		t.Fatal("nothing left to erase")
	}
	if ok, _ := s.ClearNote(ctx, -1, 99); ok {
		t.Fatal("no note to erase")
	}
	before.Chat = "воскрес"
	if ok, _ := s.SetNote(ctx, before, t0); ok {
		t.Fatal("an erased note must not come back from a stale update")
	}
	ns, _ = s.Notes(ctx, -1, []int64{50})
	if !ns[50].Empty() {
		t.Fatalf("erased note %+v", ns[50])
	}
	if rs, _ := s.Reputations(ctx, -1); len(rs) != 1 {
		t.Fatalf("erased reputation: %v", rs)
	}
	fresh := ns[50]
	fresh.Own = "заново"
	if ok, _ := s.SetNote(ctx, fresh, t0); !ok {
		t.Fatal("a new note over the erased one")
	}

	// Notes outlive the messages and follow the chat to its new id.
	if err := s.Prune(ctx, t0.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if ns, _ := s.Notes(ctx, -1001, []int64{50, 51}); len(ns) != 2 || ns[50].Own != "заново" {
		t.Fatalf("notes after migration %+v", ns)
	}
	if ns, _ := s.Notes(ctx, -1, []int64{50, 51}); len(ns) != 0 {
		t.Fatalf("old id keeps nothing: %+v", ns)
	}
}
