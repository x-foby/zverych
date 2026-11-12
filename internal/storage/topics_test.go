package storage

import (
	"context"
	"reflect"
	"testing"
)

func TestTopicNames(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if names, err := s.TopicNames(ctx, -1); err != nil || len(names) != 0 {
		t.Fatalf("no names yet: %v %v", names, err)
	}
	for _, c := range []struct {
		thread  int
		name    string
		changed bool
	}{
		{7, "Флуд", true},
		{7, "Флуд", false}, // the same name again: nothing new
		{9, "Правила", true},
		{7, "Болталка", true}, // renamed
	} {
		if changed, err := s.SetTopicName(ctx, -1, c.thread, c.name); err != nil || changed != c.changed {
			t.Fatalf("SetTopicName(%d, %q) = %v, %v; want %v", c.thread, c.name, changed, err, c.changed)
		}
	}
	s.SetTopicName(ctx, -2, 7, "Чужой")

	// The general topic: there while the group has topics, its name kept.
	for _, forum := range []bool{true, true} {
		if err := s.SetForum(ctx, -3, forum); err != nil {
			t.Fatal(err)
		}
	}
	if names, _ := s.TopicNames(ctx, -3); !reflect.DeepEqual(names, map[int]string{0: ""}) {
		t.Fatalf("general topic without a name: %v", names)
	}
	s.SetTopicName(ctx, -3, 0, "Главная")
	s.SetForum(ctx, -3, true)
	if names, _ := s.TopicNames(ctx, -3); !reflect.DeepEqual(names, map[int]string{0: "Главная"}) {
		t.Fatalf("the name stays: %v", names)
	}
	s.SetForum(ctx, -3, false)
	if names, _ := s.TopicNames(ctx, -3); len(names) != 0 {
		t.Fatalf("topics turned off: %v", names)
	}
	names, err := s.TopicNames(ctx, -1)
	if err != nil || !reflect.DeepEqual(names, map[int]string{7: "Болталка", 9: "Правила"}) {
		t.Fatalf("names %v %v", names, err)
	}
}
