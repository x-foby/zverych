package storage

import (
	"context"
	"testing"
	"time"
)

func TestPhotoRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.SaveMessage(ctx, Message{ChatID: 1, MessageID: 1, PhotoID: "telegram-file", Text: "[фото]", Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(ctx, 1, 1)
	if err != nil || m == nil || m.PhotoID != "telegram-file" {
		t.Fatalf("photo lost: %+v, %v", m, err)
	}
}
