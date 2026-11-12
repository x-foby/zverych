package brain

import (
	"context"
	"github.com/vvchaschin/communitybot/internal/storage"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestSplitReply(t *testing.T) {
	for _, text := range []string{"", "коротко", strings.Repeat("ж", 4000), strings.Repeat("ж", 9000), strings.Repeat("🔥", 4000), strings.Repeat("День: задание 🔥\n", 700), strings.Repeat("слово ", 2000)} {
		parts := splitReply(text)
		if strings.Join(parts, "") != text {
			t.Fatal("reply content lost")
		}
		for _, part := range parts {
			if part == "" || !utf8.ValidString(part) || len(utf16.Encode([]rune(part))) > 4000 {
				t.Fatalf("invalid chunk: %d bytes", len(part))
			}
		}
	}
}

func TestLongReplySentAndStored(t *testing.T) {
	for _, private := range []bool{false, true} {
		e := setup(t)
		ctx := context.Background()
		text := strings.Repeat("День: задание 🔥\n", 700)
		if private {
			e.b.sendDM(ctx, anyaID, text)
		} else if !e.b.send(ctx, storage.Message{ChatID: chat, MessageID: 9, ThreadID: 3}, text) {
			t.Fatal("send failed")
		}
		sent, _ := e.out.snapshot()
		var delivered strings.Builder
		for _, m := range sent {
			delivered.WriteString(m.text)
			if !private && (m.replyTo != 9 || m.thread != 3) {
				t.Fatal("lost reply target or topic")
			}
		}
		if len(sent) < 2 || delivered.String() != text {
			t.Fatal("incomplete delivery")
		}
		var saved strings.Builder
		if private {
			rows, err := e.st.RecentDMs(ctx, anyaID, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				saved.WriteString(row.Text)
			}
		} else {
			rows, err := e.st.Recent(ctx, chat, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.IsSelf {
					saved.WriteString(row.Text)
				}
			}
		}
		if saved.String() != text {
			t.Fatal("incomplete history")
		}
	}
}
