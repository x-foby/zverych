package storage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSummaryLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if sm, err := s.Summary(ctx, -1, 5); err != nil || sm != (Summary{}) {
		t.Fatalf("no summary yet: %+v %v", sm, err)
	}
	if ok, err := s.SetSummary(ctx, -1, 5, Summary{}, "первая", 10, t0); err != nil || !ok {
		t.Fatalf("first summary: %v %v", ok, err)
	}
	first, _ := s.Summary(ctx, -1, 5)
	if first.Text != "первая" || first.UpTo != 10 || !first.UpdatedAt.Equal(t0) {
		t.Fatalf("summary %+v", first)
	}
	if sm, _ := s.Summary(ctx, -1, 0); sm.Text != "" {
		t.Fatal("summaries are per topic")
	}

	// Written over a summary that changed since it was read: nothing changes.
	if ok, err := s.SetSummary(ctx, -1, 5, Summary{}, "устаревшая", 20, t0); err != nil || ok {
		t.Fatalf("stale write must be refused: %v %v", ok, err)
	}
	if ok, _ := s.SetSummary(ctx, -1, 5, first, "вторая", 20, t0.Add(time.Hour)); !ok {
		t.Fatal("update over the summary read")
	}
	second, _ := s.Summary(ctx, -1, 5)
	if second.Text != "вторая" || second.UpTo != 20 {
		t.Fatalf("summary %+v", second)
	}

	if ok, err := s.ClearSummary(ctx, -1, 5); err != nil || !ok {
		t.Fatalf("clear: %v %v", ok, err)
	}
	if ok, _ := s.ClearSummary(ctx, -1, 5); ok {
		t.Fatal("nothing left to clear")
	}
	if sm, _ := s.Summary(ctx, -1, 5); sm.Text != "" || sm.UpTo != 20 {
		t.Fatalf("cleared summary keeps how far it got: %+v", sm)
	}
	if ok, _ := s.SetSummary(ctx, -1, 5, second, "после стирания", 30, t0); ok {
		t.Fatal("a summary erased meanwhile is not written back")
	}

	// The tone lives beside the summary: written over the tone read, erased apart from the summary.
	if ok, _ := s.SetTone(ctx, -1, 7, "", "мат — норма"); ok {
		t.Fatal("no summary row, no tone")
	}
	if ok, err := s.SetTone(ctx, -1, 5, "", "мат — норма"); err != nil || !ok {
		t.Fatalf("set tone: %v %v", ok, err)
	}
	if ok, _ := s.SetTone(ctx, -1, 5, "", "вежливо"); ok {
		t.Fatal("stale tone write must be refused")
	}
	if ok, _ := s.SetSummary(ctx, -1, 5, Summary{UpTo: 20}, "третья", 40, t0); !ok {
		t.Fatal("summary update")
	}
	if sm, _ := s.Summary(ctx, -1, 5); sm.Tone != "мат — норма" || sm.Text != "третья" {
		t.Fatalf("a summary update keeps the tone: %+v", sm)
	}
	if ok, err := s.ClearTone(ctx, -1, 5); err != nil || !ok {
		t.Fatalf("clear tone: %v %v", ok, err)
	}
	if ok, _ := s.ClearTone(ctx, -1, 5); ok {
		t.Fatal("no tone left to clear")
	}
	if sm, _ := s.Summary(ctx, -1, 5); sm.Tone != "" || sm.Text != "третья" {
		t.Fatalf("clearing the tone keeps the summary: %+v", sm)
	}

	// Prune leaves summaries alone; migration moves them, from the start of the new chat's ids.
	if err := s.Prune(ctx, t0.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.SetSummary(ctx, -1, 0, Summary{}, "общая", 7, t0)
	s.SetTone(ctx, -1, 0, "", "по делу")
	if err := s.MigrateChat(ctx, -1, -1001); err != nil {
		t.Fatal(err)
	}
	if sm, _ := s.Summary(ctx, -1001, 0); sm.Text != "общая" || sm.UpTo != 0 || sm.Tone != "по делу" {
		t.Fatalf("migrated summary %+v", sm)
	}
	if sm, _ := s.Summary(ctx, -1, 0); sm.Text != "" {
		t.Fatal("old id keeps nothing")
	}
}

func TestUnsummarized(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 1; i <= 8; i++ {
		topic := 5
		if i == 4 {
			topic = 9 // another topic: never mixed in
		}
		save(t, s, Message{ChatID: -1, MessageID: i, ThreadID: topic, UserID: 42, UserName: "Аня", Text: "x",
			Date: t0.Add(time.Duration(i) * time.Hour)})
	}
	never := t0
	got := func(after, keep, limit int, expiring time.Time) []int {
		t.Helper()
		ms, err := s.Unsummarized(ctx, -1, 5, after, keep, limit, expiring)
		if err != nil {
			t.Fatal(err)
		}
		return ids(ms)
	}
	// Topic 5 has 1 2 3 5 6 7 8; the last 3 (6 7 8) stay out.
	for _, c := range []struct {
		after, keep, limit int
		expiring           time.Time
		want               []int
	}{
		{0, 3, 10, never, []int{1, 2, 3, 5}},
		{0, 3, 2, never, []int{1, 2}},
		{2, 3, 10, never, []int{3, 5}},
		{5, 3, 10, never, []int{}},
		{0, 7, 10, never, []int{}},                                          // the whole topic is in the window
		{0, 7, 10, t0.Add(2*time.Hour + time.Minute), []int{1, 2}},          // but these are about to be pruned
		{0, 3, 10, t0.Add(6*time.Hour + time.Minute), []int{1, 2, 3, 5, 6}}, // the window does not save them
	} {
		if g := got(c.after, c.keep, c.limit, c.expiring); !reflect.DeepEqual(g, c.want) {
			t.Errorf("after %d keep %d limit %d expiring %v: %v, want %v", c.after, c.keep, c.limit, c.expiring, g, c.want)
		}
	}
}

func TestExpiringTopics(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	save(t, s,
		Message{ChatID: -1, MessageID: 1, ThreadID: 5, UserID: 42, UserName: "Аня", Text: "старое", Date: t0},
		Message{ChatID: -1, MessageID: 2, ThreadID: 9, UserID: 42, UserName: "Аня", Text: "старое", Date: t0},
		Message{ChatID: -1, MessageID: 3, ThreadID: 9, UserID: 42, UserName: "Аня", Text: "новое", Date: t0.Add(48 * time.Hour)},
		Message{ChatID: -2, MessageID: 1, UserID: 42, UserName: "Аня", Text: "новое", Date: t0.Add(48 * time.Hour)},
	)
	before := t0.Add(time.Hour)
	ts, err := s.ExpiringTopics(ctx, before)
	if err != nil || !reflect.DeepEqual(ts, []TopicRef{{-1, 5}, {-1, 9}}) {
		t.Fatalf("expiring topics %v %v", ts, err)
	}
	s.SetSummary(ctx, -1, 9, Summary{}, "свёрнуто", 2, t0)
	if ts, _ := s.ExpiringTopics(ctx, before); !reflect.DeepEqual(ts, []TopicRef{{-1, 5}}) {
		t.Fatalf("a covered topic is not expiring: %v", ts)
	}
}
