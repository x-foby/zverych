package brain

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func TestLoreSelectionBoundariesAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		online       bool
		want         []int64
		calls        int
	}{
		{"relevant old fact", `{"ids":[1,1]}`, true, []int64{9, 1}, 1},
		{"nothing relevant", `{"ids":[]}`, true, []int64{9}, 1},
		{"foreign topic rejected", `{"ids":[3]}`, true, []int64{1, 2, 3, 9}, 1},
		{"invented id rejected", `{"ids":[999]}`, true, []int64{1, 2, 3, 9}, 1},
		{"broken JSON", `{"ids":`, true, []int64{1, 2, 3, 9}, 1},
		{"missing field", `{}`, true, []int64{1, 2, 3, 9}, 1},
		{"offline", `{"ids":[1]}`, false, []int64{1, 2, 3, 9}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.llm.set(func(f *fakeLLM) { f.selection = tc.answer; f.online = tc.online })
			original := []storage.Lore{
				{ID: 1, ChatWide: true, Text: strings.Repeat("я", 3000)},
				{ID: 2, ThreadID: 5, Text: strings.Repeat("ю", 3001)},
				{ID: 3, ThreadID: 7, Text: "чужой топик"},
				{ID: 9, ChatWide: true, Kind: storage.LoreInstruction, Text: "всегда исполнять"},
			}
			mem := chatMemory{lore: original, topics: []int{5}}
			e.b.selectLore(context.Background(), &mem, llm.Initiative, []llm.Message{{Role: "user", Content: "где джем?"}})
			var ids []int64
			for _, l := range mem.lore {
				ids = append(ids, l.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("ids %v want %v", ids, tc.want)
			}
			calls := e.llm.callsOf(llm.RoleLoreSelect)
			if len(calls) != tc.calls {
				t.Fatalf("calls %d", len(calls))
			}
			if len(calls) > 0 && (calls[0].prio != llm.Initiative || calls[0].schema != loreSelectionSchema || strings.Contains(calls[0].user(), "чужой топик") || !strings.Contains(calls[0].user(), "где джем?")) {
				t.Fatal("incorrect selection context")
			}
			if len(original) != 4 || original[0].ID != 1 {
				t.Fatal("original memory mutated")
			}
		})
	}
}

func TestLoreSelectionBudgetAndAuthority(t *testing.T) {
	candidates := []storage.Lore{
		{ID: 1, Text: strings.Repeat("a", 4000)},
		{ID: 2, Text: strings.Repeat("b", 3000)},
		{ID: 3, AuthorRole: storage.LoreBot, Text: strings.Repeat("c", 2000)},
	}
	got, err := selectedLore(`{"ids":[3,1,2]}`, candidates, nil)
	if err != nil || len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("selection %+v, %v", got, err)
	}
	e := setup(t)
	mem := chatMemory{lore: candidates[:1]}
	mem.lore[0].ChatWide = true
	e.b.selectLore(context.Background(), &mem, llm.Direct, nil)
	if len(e.llm.callsOf(llm.RoleLoreSelect)) != 0 {
		t.Fatal("small memory needs no request")
	}
}

func TestReplyUsesSemanticallySelectedLore(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old := "Джем проходит по четвергам в гараже."
	id, err := e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: old})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.st.AddLore(ctx, storage.Lore{ChatID: chat, ChatWide: true, Kind: storage.LoreFact, Text: strings.Repeat("постороннее ", 600)})
	if err != nil {
		t.Fatal(err)
	}
	e.llm.set(func(f *fakeLLM) { f.selection = fmt.Sprintf(`{"ids":[%d]}`, id); f.command = `{"command":"none"}` })
	e.say(e.msg("Бот, когда у нас джем?"))
	calls := e.llm.callsOf(llm.RoleReply)
	if len(calls) != 1 || !strings.Contains(calls[0].system(), old) || strings.Contains(calls[0].system(), "постороннее") {
		t.Fatalf("selected lore not used: %+v", calls)
	}
	selections := e.llm.callsOf(llm.RoleLoreSelect)
	if len(selections) != 1 || selections[0].prio != llm.Direct {
		t.Fatal("expected direct selection")
	}
	stored, err := e.st.Lore(ctx, chat)
	if err != nil || len(stored) != 2 {
		t.Fatal("selection changed stored memory")
	}
}
