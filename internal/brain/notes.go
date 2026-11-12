package brain

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// Notes about participants (SPEC 4.5): how the chat treats a person — in words and as a reputation
// of −2…+2 — and the bot's own impression of them. They are updated with each summary chunk, for
// the people who wrote in it, by the filter model in the background; replies and the evaluation
// see the notes of the people in question, and the reputation marks the history lines.

const (
	maxNotesPerChunk   = 12 // people whose notes one chunk updates (the most active ones)
	maxNotesInReply    = 5  // notes in a reply prompt: the person answered first
	maxNotesInEvaluate = 8  // notes in an evaluation prompt: authors of the new messages
	maxReputation      = 2
)

// repLabel says how the chat treats a person with the reputation.
func repLabel(rep int) string {
	switch {
	case rep >= 2:
		return "очень уважают"
	case rep == 1:
		return "уважают"
	case rep == -1:
		return "не в чести"
	case rep <= -2:
		return "совсем не в чести"
	}
	return ""
}

// noteSubject is a person whose note a chunk updates, with the note as it was.
type noteSubject struct {
	note storage.Note
	rank rank
}

// updateNotes updates the notes about the authors of a summarized chunk. Notes of a chat are
// updated one chunk at a time: topics are summarized concurrently.
func (b *Brain) updateNotes(ctx context.Context, chatID int64, ms []storage.Message, r roster) {
	mu, _ := b.notesLocks.LoadOrStore(chatID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	people := chunkAuthors(ms)
	if len(people) == 0 || !b.llm.Available(ctx, llm.RoleNotes, llm.Initiative) {
		return
	}
	ids := make([]int64, len(people))
	for i, p := range people {
		ids[i] = p.ID
	}
	prev, err := b.store.Notes(ctx, chatID, ids)
	if err != nil {
		slog.Error("load notes", "chat_id", chatID, "err", err)
		return
	}
	subjects := make([]noteSubject, len(people))
	for i, p := range people {
		n := prev[p.ID]
		n.ChatID, n.UserID, n.Name, n.Username = chatID, p.ID, p.Name, p.Username
		subjects[i] = noteSubject{note: n, rank: r.rank(p.ID, p.Username)}
	}
	raw, provider, err := b.llm.Complete(ctx, llm.RoleNotes, llm.Initiative, notesPrompt(subjects, ms, b.cfg.NoteMaxChars, r, b.names()))
	if err != nil {
		slog.Warn("notes not updated", "chat_id", chatID, "err", err)
		return
	}
	updates, err := parseNotes(raw)
	if err != nil {
		slog.Warn("notes not updated: unparsable answer", "chat_id", chatID, "provider", provider, "err", err)
		return
	}
	written := 0
	for _, u := range updates {
		if u.Person < 1 || u.Person > len(subjects) {
			continue
		}
		n := applyNote(subjects[u.Person-1].note, u, b.cfg.NoteMaxChars)
		if n == subjects[u.Person-1].note {
			continue
		}
		ok, err := b.store.SetNote(ctx, n, b.now())
		switch {
		case err != nil:
			slog.Error("save note", "chat_id", chatID, "user_id", n.UserID, "err", err)
		case !ok:
			slog.Info("note not updated: changed meanwhile (erased by the admin?)", "chat_id", chatID, "user_id", n.UserID)
		default:
			written++
		}
	}
	slog.Info("notes updated", "chat_id", chatID, "people", len(subjects), "written", written, "provider", provider)
}

// chunkAuthors lists the people who wrote in the messages, the most active first, with their
// latest name; the bot and service events (joins) are left out.
func chunkAuthors(ms []storage.Message) []storage.Person {
	count := map[int64]int{}
	last := map[int64]storage.Person{}
	var order []int64
	for _, m := range ms {
		if m.IsSelf || m.Kind != "" || m.UserID == 0 {
			continue
		}
		if count[m.UserID] == 0 {
			order = append(order, m.UserID)
		}
		count[m.UserID]++
		last[m.UserID] = storage.Person{ID: m.UserID, Name: m.UserName, Username: m.Username}
	}
	slices.SortStableFunc(order, func(a, b int64) int { return cmp.Compare(count[b], count[a]) })
	out := make([]storage.Person, 0, min(len(order), maxNotesPerChunk))
	for _, id := range order[:min(len(order), maxNotesPerChunk)] {
		out = append(out, last[id])
	}
	return out
}

// noteUpdate is the model's new note about one person.
type noteUpdate struct {
	Person     int    `json:"person"`
	Chat       string `json:"chat"`
	Own        string `json:"own"`
	Reputation *int   `json:"reputation"`
}

func parseNotes(s string) ([]noteUpdate, error) {
	raw := jsonObjRe.FindString(s)
	if raw == "" {
		return nil, fmt.Errorf("no JSON object in %q", s)
	}
	var wrap struct {
		People *[]noteUpdate `json:"people"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("bad notes JSON %q: %w", raw, err)
	}
	if wrap.People == nil {
		return nil, fmt.Errorf("no people in %q", raw)
	}
	return *wrap.People, nil
}

// applyNote makes the new note from the old one and the model's update: an empty part keeps the
// old text, the reputation moves by at most one step per chunk (one quarrel does not make an outcast).
func applyNote(n storage.Note, u noteUpdate, maxChars int) storage.Note {
	if t := clip(u.Chat, maxChars); t != "" {
		n.Chat = t
	}
	if t := clip(u.Own, maxChars); t != "" {
		n.Own = t
	}
	if u.Reputation != nil {
		n.Reputation = max(n.Reputation-1, min(n.Reputation+1, *u.Reputation, maxReputation), -maxReputation)
	}
	return n
}

// notesPrompt asks the model to update the notes about the people with what they wrote.
func notesPrompt(people []noteSubject, ms []storage.Message, maxChars int, r roster, names []string) []llm.Message {
	sys := `Ты ведёшь заметки бота — участника группового чата в Telegram — о людях из этого чата. ` +
		`Бот читает заметку, когда разговаривает с человеком или решает, откликнуться ли на его сообщение. Сообщения бота в переписке помечены «Ты».
В заметке о человеке:
- "chat" — как к нему относятся в чате: реакции на его сообщения, как ему отвечают (благодарят, спрашивают совета, поддерживают, ` +
		`смеются вместе — или спорят, подкалывают, раздражаются, не замечают), что о нём говорят другие, что сказало руководство чата ([админ], [авторитет]);
- "reputation" — то же числом: 2 — очень уважают, 1 — уважают, 0 — обычно или пока неясно, -1 — не в чести (раздражение, насмешки), ` +
		`-2 — совсем не в чести. Это отношение людей, а не твоё мнение; меняй оценку постепенно, по повторяющимся признакам, а не по одной перепалке;
- "own" — твоё впечатление: интересы, чем занимается, что рассказывал о себе, манера общения, как с ним лучше разговаривать ` +
		`(любит ли шутки, спорить, обижается ли), как он относится к тебе. Оно не должно идти вразрез с тем, как к человеку относится чат: ` +
		`не превозноси того, кого чат не уважает, и не принижай того, кого ценят.
Если в разных топиках человек ведёт себя по-разному — так и напиши, с номером топика.
Обнови заметки людей из списка по новым сообщениям: новая заметка = прежняя + новое. Прежняя заметка — единственная память о том, что было раньше: ` +
		`переноси её содержание (можно короче), выбрасывай только устаревшее или опровергнутое. Если о человеке ничего нового — не включай его в ответ.
Только то, что видно из переписки: не выдумывай, не ставь диагнозов, не оценивай внешность, национальность, здоровье и подобные признаки. ` +
		fmt.Sprintf("Каждая часть — не длиннее %d символов, коротко, обычным текстом, на языке переписки. ", maxChars) + `О людях пиши в третьем лице, о боте — «ты».
Ответь строго одним JSON-объектом, без текста вокруг:
{"people": [{"person": <номер из списка>, "chat": "<как относятся в чате>", "reputation": <от -2 до 2>, "own": "<твоё впечатление>"}]}`

	var u strings.Builder
	if len(names) > 0 {
		fmt.Fprintf(&u, "Бот (это ты): %s\n\n", strings.Join(names, ", "))
	}
	u.WriteString("Люди:\n")
	for i, p := range people {
		label := p.note.Person().Label()
		if p.rank != rankNone {
			label += " [" + p.rank.label() + "]"
		}
		fmt.Fprintf(&u, "%d. %s\n", i+1, label)
		if p.note.Empty() {
			u.WriteString("   (заметки ещё нет)\n")
			continue
		}
		fmt.Fprintf(&u, "   Как относятся в чате: %s\n   Репутация: %d\n   Твоё впечатление: %s\n",
			cmp.Or(p.note.Chat, "—"), p.note.Reputation, cmp.Or(p.note.Own, "—"))
	}
	u.WriteString("\nНовые сообщения:\n")
	day := ""
	for _, m := range ms {
		if d := dayLabel(m.Date); d != day {
			day = d
			fmt.Fprintf(&u, "— %s —\n", d)
		}
		u.WriteString(formatLine(m, r))
		u.WriteByte('\n')
	}
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}

// withNotes adds the notes about the people (in order of importance, at most limit) to the memory.
func (b *Brain) withNotes(ctx context.Context, mem *chatMemory, chatID int64, people []int64, limit int) {
	var ids []int64
	for _, id := range people {
		if id != 0 && id != b.self.ID && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	ns, err := b.store.Notes(ctx, chatID, ids)
	if err != nil {
		slog.Error("load notes", "chat_id", chatID, "err", err)
		return
	}
	for _, id := range ids {
		if n, ok := ns[id]; ok && !n.Empty() && len(mem.notes) < limit {
			mem.notes = append(mem.notes, n)
		}
	}
}

// replyPeople: whose notes a reply needs — the person answered, those they mention, then the
// authors of the conversation around, the latest first.
func (b *Brain) replyPeople(ctx context.Context, target storage.Message, rc replyContext) []int64 {
	ids := []int64{target.UserID}
	for _, p := range b.mentions(ctx, target) {
		ids = append(ids, p.ID)
	}
	for _, ms := range [][]storage.Message{rc.thread.msgs, rc.around} {
		for i := len(ms) - 1; i >= 0; i-- {
			if !ms[i].IsSelf {
				ids = append(ids, ms[i].UserID)
			}
		}
	}
	return ids
}

// evaluationPeople: whose notes an evaluation needs — the authors of the new messages.
func evaluationPeople(fresh []storage.Message, targets map[int]storage.Message) []int64 {
	var ids []int64
	for _, m := range fresh {
		if _, ok := targets[m.MessageID]; ok {
			ids = append(ids, m.UserID)
		}
	}
	return ids
}

// notesBlock renders the notes for a system prompt.
func notesBlock(mem chatMemory) string {
	var s strings.Builder
	if len(mem.notes) > 0 {
		s.WriteString("\nЧто ты знаешь о людях (твои заметки — для тебя; не пересказывай их и не называй оценок):\n")
		for _, n := range mem.notes {
			fmt.Fprintf(&s, "- %s", n.Person().Label())
			if l := repLabel(n.Reputation); l != "" {
				fmt.Fprintf(&s, " [%s]", l)
			}
			if n.Chat != "" {
				fmt.Fprintf(&s, ". В чате: %s", strings.TrimSuffix(n.Chat, "."))
			}
			if n.Own != "" {
				fmt.Fprintf(&s, ". Твоё впечатление: %s", strings.TrimSuffix(n.Own, "."))
			}
			s.WriteString(".\n")
		}
	}
	if len(mem.notes) > 0 || len(mem.roster.rep) > 0 {
		s.WriteString("Как к человеку относятся в чате, видно по пометкам [уважают], [не в чести]: мнение и просьбы уважаемых весят больше, " +
			"спорь с ними мягче; тех, кого чат не уважает, не превозноси и не подхватывай их нападки на других. " +
			"Твоё впечатление не идёт вразрез с отношением чата; указания руководства важнее.\n")
	}
	return s.String()
}

// noteText renders a note for the chat's admin ("что ты думаешь о @vasya?").
func noteText(p storage.Person, n storage.Note, ok bool) string {
	if !ok || n.Empty() {
		return fmt.Sprintf("О %s у меня пока заметок нет.", p.Label())
	}
	var s strings.Builder
	s.WriteString(n.Person().Label())
	rep := fmt.Sprintf("%+d", n.Reputation)
	if n.Reputation == 0 {
		rep = "0"
	}
	if l := repLabel(n.Reputation); l != "" {
		rep += " (" + l + ")"
	}
	fmt.Fprintf(&s, "\nРепутация в чате: %s", rep)
	if n.Chat != "" {
		fmt.Fprintf(&s, "\nКак к нему относятся: %s", n.Chat)
	}
	if n.Own != "" {
		fmt.Fprintf(&s, "\nМоё впечатление: %s", n.Own)
	}
	return s.String()
}
