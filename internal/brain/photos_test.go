package brain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
)

type photoOut struct {
	*fakeOut
	fail  bool
	calls int
}

func (o *photoOut) Photo(context.Context, string) (string, error) {
	o.calls++
	if o.fail {
		return "", errors.New("failed")
	}
	return "data:image/jpeg;base64,YQ==", nil
}
func TestWithPhoto(t *testing.T) {
	for _, fail := range []bool{false, true} {
		out := &photoOut{fakeOut: &fakeOut{}, fail: fail}
		b := &Brain{out: out}
		original := []llm.Message{{Role: "system", Content: abilities}, {Role: "user", Content: "[фото]"}}
		msgs := b.withPhoto(context.Background(), original, "id", 1, 2)
		if out.calls != 1 {
			t.Fatal("photo not downloaded")
		}
		last := msgs[len(msgs)-1]
		if fail {
			if len(last.Images) != 0 || !strings.Contains(last.Content, "не удалось") {
				t.Fatal("missing failure instruction")
			}
		} else {
			if len(last.Images) != 1 || !strings.Contains(msgs[0].Content, "НАСТОЯЩЕЕ ИЗОБРАЖЕНИЕ") {
				t.Fatal("image or capability missing")
			}
			if strings.Contains(original[0].Content, "НАСТОЯЩЕЕ ИЗОБРАЖЕНИЕ") {
				t.Fatal("shared prompt mutated")
			}
		}
	}
}
