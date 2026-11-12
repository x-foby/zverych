package brain

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// Run explicitly against the reply model; unlike unit tests this checks whether
// a strong persona still produces the requested artifact, including its last day.
func TestLiveCalendarCompletion(t *testing.T) {
	live := newLiveModel(t)
	persona := "Ты Витёк, грубоватый работяга. Говоришь коротко, постоянно вставляешь бля и заканчиваешь предложения на )))."
	if path := os.Getenv("LIVE_PERSONA_PATH"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		persona = string(data)
	}
	q := storage.Message{MessageID: 1071, UserName: "Валентин", Text: "Зверыч, составь адвент-календарь на декабрь 2027 года по русским поэтам: все 31 день, на каждый день поэт и маленькое задание.", Addressed: true}
	for _, private := range []bool{false, true} {
		msgs := replyPrompt(persona, chatMemory{}, replyContext{around: []storage.Message{q}}, q, "")
		if private {
			msgs = privateReplyPrompt(persona, nil, nil, privateCtx{}, []storage.DM{{Text: q.Text}}, "")
		}
		temp := 0.8
		res, err := live.client.Do(context.Background(), llm.Request{Model: live.model, Messages: msgs, Temperature: &temp, MaxTokens: 1000, Extra: live.extra})
		if err != nil {
			t.Fatal(err)
		}
		live.cost += res.Usage.Cost
		t.Logf("private=%v: %s", private, res.Text)
		for day := 1; day <= 31; day++ {
			// Both numbered lists and dates are accepted.
			if !regexp.MustCompile(`(?m)(^|\n)\s*(?:День\s*)?` + strconv.Itoa(day) + `(?:[.) :—–-]|\s+декабря)`).MatchString(res.Text) {
				t.Errorf("private=%v: missing day %d", private, day)
			}
		}
		for _, phrase := range []string{"подожди немного", "пришлю позже", "сейчас составлю"} {
			if strings.Contains(strings.ToLower(res.Text), phrase) {
				t.Errorf("deferred result: %s", phrase)
			}
		}
	}
}
