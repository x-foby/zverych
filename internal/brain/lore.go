package brain

import (
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

// Lore the bot infers itself (SPEC 4.5): with each summary chunk, the filter model (in the background)
// sees the chat's memory and the chunk and picks out what is worth remembering for long — inside
// jokes, rules and agreements, traditions and events, local terms — for the topic or the whole chat.
// It may rewrite its own entries (a correction, or a topic entry that turned out to hold for the
// whole chat); the leadership's entries matter more and it never touches them.

// maxLorePerChunk caps the new entries one chunk may add: memory is for what lasts, not for every topic.
const maxLorePerChunk = 3

// updateLore picks out the lore of a summarized chunk of the topic. The lore of a chat is updated
// one chunk at a time: topics are summarized concurrently and may notice the same thing.
func (b *Brain) updateLore(ctx context.Context, chatID int64, topic int, ms []storage.Message, r roster) {
	mu, _ := b.loreLocks.LoadOrStore(chatID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if !slices.ContainsFunc(ms, func(m storage.Message) bool { return !m.IsSelf && m.Kind == "" }) ||
		!b.llm.Available(ctx, llm.RoleLore, llm.Initiative) {
		return
	}
	lore, err := b.store.Lore(ctx, chatID)
	if err != nil {
		slog.Error("load lore", "chat_id", chatID, "err", err)
		return
	}
	free := max(0, b.cfg.LoreMaxEntries-inferredCount(lore))
	raw, provider, err := b.llm.Complete(ctx, llm.RoleLore, llm.Initiative,
		lorePrompt(lore, topic, ms, b.cfg.LoreMaxChars, min(free, maxLorePerChunk), r, b.names()))
	if err != nil {
		slog.Warn("lore not updated", "chat_id", chatID, "topic", topic, "err", err)
		return
	}
	updates, err := parseLore(raw)
	if err != nil {
		slog.Warn("lore not updated: unparsable answer", "chat_id", chatID, "topic", topic, "provider", provider, "err", err)
		return
	}
	added, replaced, skipped := 0, 0, 0
	for _, u := range updates {
		l, ok := b.loreEntry(chatID, topic, r.tagged(topic), ms, lore, u)
		switch {
		case !ok:
			skipped++
			continue
		case l.ID == 0 && (added >= maxLorePerChunk || added >= free):
			slog.Info("lore entry skipped: no room", "chat_id", chatID, "topic", topic, "text", l.Text)
			skipped++
			continue
		}
		if l.ID == 0 {
			id, err := b.store.AddLore(ctx, l)
			if err != nil {
				slog.Error("save lore", "chat_id", chatID, "err", err)
				continue
			}
			l.ID = id
			added++
		} else {
			ok, err := b.store.UpdateLore(ctx, l)
			switch {
			case err != nil:
				slog.Error("save lore", "chat_id", chatID, "id", l.ID, "err", err)
				continue
			case !ok:
				slog.Info("lore entry not updated: forgotten meanwhile", "chat_id", chatID, "id", l.ID)
				continue
			}
			lore = slices.DeleteFunc(lore, func(e storage.Lore) bool { return e.ID == l.ID })
			replaced++
		}
		lore = append(lore, l) // later updates of the answer see it: no duplicates
		slog.Info("lore entry", "chat_id", chatID, "id", l.ID, "chat_wide", l.ChatWide, "thread", l.ThreadID, "text", l.Text)
	}
	slog.Info("lore updated", "chat_id", chatID, "topic", topic, "added", added, "replaced", replaced, "skipped", skipped,
		"provider", provider)
}

// inferredCount: how many entries the bot inferred itself.
func inferredCount(lore []storage.Lore) int {
	n := 0
	for _, l := range lore {
		if l.Inferred() {
			n++
		}
	}
	return n
}

// loreUpdate is one entry of the model's answer: a new one (ID 0) or a rewrite of the bot's own.
type loreUpdate struct {
	ID    int64  `json:"id"`
	Scope string `json:"scope"`
	Text  string `json:"text"`
	Msg   int    `json:"msg"`
}

func parseLore(s string) ([]loreUpdate, error) {
	raw := jsonObjRe.FindString(s)
	if raw == "" {
		return nil, fmt.Errorf("no JSON object in %q", s)
	}
	var wrap struct {
		Lore *[]loreUpdate `json:"lore"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("bad lore JSON %q: %w", raw, err)
	}
	if wrap.Lore == nil {
		return nil, fmt.Errorf("no lore in %q", raw)
	}
	return *wrap.Lore, nil
}

// loreEntry makes the entry to store from the model's update; false if it is to be skipped (empty,
// a copy of an entry already there, a rewrite of an entry that is not the bot's own).
// inTopic: the chunk is a topic's (in a chat with topics, the general one's too), not the whole chat's.
func (b *Brain) loreEntry(chatID int64, topic int, inTopic bool, ms []storage.Message, lore []storage.Lore, u loreUpdate) (storage.Lore, bool) {
	text := clip(u.Text, b.cfg.LoreMaxChars)
	if text == "" || slices.ContainsFunc(lore, func(l storage.Lore) bool { return sameText(l.Text, text) && l.ID != u.ID }) {
		return storage.Lore{}, false
	}
	l := storage.Lore{ChatID: chatID, ChatWide: u.Scope == "chat" || !inTopic, ThreadID: topic, Kind: storage.LoreFact,
		Text: text, AuthorRole: storage.LoreBot, SourceMsg: ms[len(ms)-1].MessageID, CreatedAt: b.now()}
	if slices.ContainsFunc(ms, func(m storage.Message) bool { return m.MessageID == u.Msg }) {
		l.SourceMsg = u.Msg
	}
	if u.ID != 0 {
		i := slices.IndexFunc(lore, func(e storage.Lore) bool { return e.ID == u.ID })
		if i < 0 || !lore[i].Inferred() {
			slog.Info("lore entry skipped: not the bot's own", "chat_id", chatID, "id", u.ID)
			return storage.Lore{}, false
		}
		l.ID = u.ID
		if !lore[i].ChatWide {
			l.ThreadID = lore[i].ThreadID // a topic entry stays in its topic unless it goes chat-wide
		}
	}
	return l, true
}

func sameText(a, b string) bool {
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), ".!") }
	return norm(a) == norm(b)
}

// lorePrompt asks the model what of the chunk is worth remembering about the chat.
func lorePrompt(lore []storage.Lore, topic int, ms []storage.Message, maxChars, room int, r roster, names []string) []llm.Message {
	var sys strings.Builder
	sys.WriteString(`Ты ведёшь память бота — участника группового чата в Telegram — о самом чате (лор): то, что стоит помнить долго, ` +
		`когда подробности переписки забудутся. Сообщения бота в переписке помечены «Ты».
Что записывать:
- внутренние шутки, мемы и словечки чата — что значат и откуда пошли;
- правила и договорённости, о которых условились участники или руководство («в этом топике только по делу», «созвон по пятницам»);
- традиции и памятные события чата (встречи, конкурсы, истории, которые потом вспоминают);
- термины и названия, понятные только здесь.
Чего не записывать: обычные темы разговоров, новости и мелочи — для этого есть сводка переписки; сведения о людях ` +
		`(интересы, характер, кто с кем как) — для этого есть заметки о людях; просьбы к тебе — что тебе делать или не делать ` +
		`(такие указания даёт только руководство чата, сам ты их не записываешь); то, что уже есть в памяти; то, что руководство просило забыть.
Только то, что прямо видно из переписки, без догадок. Записывай редко: обычно в порции сообщений нечего записать — тогда верни пустой список.
`)
	if r.tagged(topic) {
		sys.WriteString(`Область записи: "topic" — относится к этому топику (так по умолчанию: в других топиках свои порядки); "chat" — ко всему ` +
			`чату: правила для всех, общая история, «кто мы», или то же самое уже записано для другого топика — тогда замени ту запись одной общей.
`)
	}
	sys.WriteString(`Записи руководства (помечены [руководство]) важнее твоих: не меняй их и не записывай того, что им противоречит. ` +
		`Если новые сообщения уточняют или опровергают твою запись (помечена [твоя]), замени её: укажи её номер в "id" и новый текст, а не добавляй рядом вторую.
`)
	fmt.Fprintf(&sys, "Каждая запись — одна мысль, не длиннее %d символов, обычным текстом, на языке переписки. ", maxChars)
	if room > 0 {
		fmt.Fprintf(&sys, "Новых записей — не больше %d. ", room)
	} else {
		sys.WriteString("Места для новых записей нет: можно только заменять свои. ")
	}
	sys.WriteString(`О боте (сообщения «Ты») пиши «ты».
Ответь строго одним JSON-объектом, без текста вокруг:
{"lore": [{"id": <номер твоей записи, которую заменяешь, или 0 для новой>, "scope": "topic" или "chat", "text": "<запись>", "msg": <id сообщения, откуда это видно>}]}`)

	var u strings.Builder
	if len(names) > 0 {
		fmt.Fprintf(&u, "Бот (это ты): %s\n", strings.Join(names, ", "))
	}
	switch {
	case topic != 0:
		fmt.Fprintf(&u, "Сообщения из топика %d%s.\n", topic, r.topicName(topic))
	case r.forum():
		fmt.Fprintf(&u, "Сообщения из основного топика%s.\n", r.topicName(0))
	}
	u.WriteString("\nПамять о чате:\n")
	if len(lore) == 0 {
		u.WriteString("(пусто)\n")
	}
	for _, l := range lore {
		who := "руководство"
		if l.Inferred() {
			who = "твоя"
		}
		kind := ""
		if l.Kind == storage.LoreInstruction {
			kind = " (указание тебе)"
		}
		fmt.Fprintf(&u, "№%d [%s] [%s]%s %s\n", l.ID, who, scopeLabel(l, topic, r), kind, l.Text)
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
	return []llm.Message{{Role: "system", Content: sys.String()}, {Role: "user", Content: u.String()}}
}
