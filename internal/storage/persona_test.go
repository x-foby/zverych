package storage

import (
	"context"
	"testing"
)

func TestPersona(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if p, err := s.Persona(ctx, -1); err != nil || p != "" {
		t.Fatalf("no persona of its own: %q %v", p, err)
	}
	if err := s.SetPersona(ctx, -1, "Ты — Шарманка", 50, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPersona(ctx, -1, "Ты — Гармонь", 51, t0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Persona(ctx, -1); p != "Ты — Гармонь" {
		t.Fatalf("replaced: %q", p)
	}
	if p, _ := s.Persona(ctx, -2); p != "" {
		t.Fatalf("another group's: %q", p)
	}
	if err := s.SetPersona(ctx, -1, "", 50, t0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Persona(ctx, -1); p != "" {
		t.Fatalf("back to the default: %q", p)
	}
}

func TestPersonaDrafts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	first, err := s.AddPersonaDraft(ctx, PersonaDraft{ChatID: -1, UserID: 50, Text: "первая", CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.AddPersonaDraft(ctx, PersonaDraft{ChatID: -2, UserID: 50, Text: "чужая", CreatedAt: t0})
	second, _ := s.AddPersonaDraft(ctx, PersonaDraft{ChatID: -1, UserID: 51, Text: "", CreatedAt: t0})
	if _, ok, _ := s.PersonaDraft(ctx, -1, first); ok {
		t.Fatal("a new draft replaces the group's previous one")
	}
	d, ok, err := s.PersonaDraft(ctx, -1, second)
	if err != nil || !ok || d.UserID != 51 || d.Text != "" || !d.CreatedAt.Equal(t0) {
		t.Fatalf("draft %+v %v %v", d, ok, err)
	}
	if _, ok, _ := s.PersonaDraft(ctx, -1, other); ok {
		t.Fatal("a draft is found in its own group only")
	}
	if _, ok, _ := s.PersonaDraft(ctx, -2, other); !ok {
		t.Fatal("another group keeps its draft")
	}
	if err := s.DeletePersonaDraft(ctx, -1, second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PersonaDraft(ctx, -1, second); ok {
		t.Fatal("deleted")
	}
}

func TestMigrateChatMovesPersona(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.RegisterChat(ctx, -1, "g", 7, t0)
	s.SetPersona(ctx, -1, "Ты — Шарманка", 50, t0)
	id, _ := s.AddPersonaDraft(ctx, PersonaDraft{ChatID: -1, UserID: 50, Text: "Ты — Гармонь", CreatedAt: t0})
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Persona(ctx, -1001); p != "Ты — Шарманка" {
		t.Fatalf("the persona moves: %q", p)
	}
	if _, ok, _ := s.PersonaDraft(ctx, -1001, id); !ok {
		t.Fatal("the draft moves")
	}
	if p, _ := s.Persona(ctx, -1); p != "" {
		t.Fatalf("nothing left behind: %q", p)
	}
}
