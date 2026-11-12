package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// A group's own persona (SPEC 5): an admin of the group says in any words who the bot should be
// there, the model rewrites the description, and the admin confirms it with a button in the
// private chat (the persona is not shown to the group). The boundaries and the topics' tone stay
// on top of any persona: they are in the system prompt.

// maxPersonaRunes caps a group's persona: it goes into every prompt of the group, and the preview
// with it must fit into one Telegram message.
const maxPersonaRunes = 3000

var errPersonaTooLong = errors.New("persona too long")

// Only explicit declarations of the bot's name count; interests and names of
// other people in the persona must not become aliases.
var personaNameRe = regexp.MustCompile(`(?i)(?:тебя\s+зовут|тво[её]\s+имя\s*[:—–-]?)\s*[«"„“]*([\p{L}][\p{L}\p{N}]*)`)

func personaNames(persona string) []string {
	var names []string
	for _, match := range personaNameRe.FindAllStringSubmatch(persona, -1) {
		names = append(names, normalizeWord(match[1]))
	}
	return names
}

// proposePersona drafts the group's new persona from the admin's message (empty for going back
// to the default) and sends it to the admin's private chat with buttons. The caller holds the
// group's lock and has checked the rights.
func (b *Brain) proposePersona(ctx context.Context, m storage.Message, g storage.Chat, mem chatMemory, reset bool,
	replied *storage.Message) commandResult {
	var text string
	if !reset {
		if !b.llm.Available(ctx, llm.RoleReply, llm.Direct) {
			return refuse("поменять свою личность, но у тебя сейчас нет сил на это: пусть попросят позже")
		}
		var err error
		if text, err = b.draftPersona(ctx, b.personaOf(mem), m, replied); err != nil {
			slog.Warn("persona draft failed", "chat_id", g.ID, "err", err)
			if errors.Is(err, errPersonaTooLong) {
				return refuse("поменять свою личность, но описание вышло слишком длинным: пусть опишут короче")
			}
			return refuse("поменять свою личность, но у тебя что-то сломалось: пусть повторят позже")
		}
	} else if mem.persona == "" {
		return refuse("вернуть личность по умолчанию, но она у тебя в этой группе и так по умолчанию")
	}
	id, err := b.store.AddPersonaDraft(ctx, storage.PersonaDraft{ChatID: g.ID, UserID: m.UserID, Text: text, CreatedAt: b.now()})
	if err != nil {
		slog.Error("save persona draft", "chat_id", g.ID, "err", err)
		return refuse("поменять свою личность, но у тебя что-то сломалось: пусть повторят позже")
	}
	preview := fmt.Sprintf("Такой я буду в «%s»:\n\n%s\n\nСохранить?", title(g), text)
	if reset {
		preview = fmt.Sprintf("Вернусь в «%s» к личности по умолчанию:\n\n%s\n\nСохранить?", title(g), truncate(strings.TrimSpace(b.persona), maxPersonaRunes))
	}
	data := fmt.Sprintf("per:%d:%d:", g.ID, id)
	if _, err := b.out.SendButtons(ctx, m.UserID, preview, [][]Button{{{Text: "Да, сохранить", Data: data + "y"}, {Text: "Нет", Data: data + "n"}}}); err != nil {
		// Telegram does not let a bot write first to someone who has never written to it.
		slog.Warn("send persona draft", "chat_id", g.ID, "user_id", m.UserID, "err", err)
		if err := b.store.DeletePersonaDraft(ctx, g.ID, id); err != nil {
			slog.Error("delete persona draft", "chat_id", g.ID, "err", err)
		}
		return refuse("поменять свою личность, но новую ты показываешь на подтверждение только в личке, а написать туда первым не можешь: " +
			"пусть сначала напишут тебе в личные сообщения")
	}
	slog.Info("persona draft sent", "chat_id", g.ID, "user_id", m.UserID, "draft", id, "reset", reset, "runes", len([]rune(text)))
	return commandResult{done: true, text: "Прислал в личку — посмотри и подтверди.", inDM: true}
}

// draftPersona asks the model for the new persona: the current one changed as the admin asks, or
// the description the admin sent.
func (b *Brain) draftPersona(ctx context.Context, current string, m storage.Message, replied *storage.Message) (string, error) {
	raw, _, err := b.llm.Complete(ctx, llm.RoleReply, llm.Direct, personaPrompt(current, m.Text, replied))
	if err != nil {
		return "", err
	}
	text := cleanPersona(raw)
	switch {
	case text == "":
		return "", fmt.Errorf("empty persona in %q", raw)
	case len([]rune(text)) > maxPersonaRunes:
		return "", errPersonaTooLong
	}
	return text, nil
}

var fenceRe = regexp.MustCompile("(?s)^```[a-z]*\\n(.*)\\n```$")

// cleanPersona strips what the model may wrap the description in: a code fence, the <persona> tag.
func cleanPersona(s string) string {
	s = strings.TrimSpace(s)
	if sm := fenceRe.FindStringSubmatch(s); sm != nil {
		s = strings.TrimSpace(sm[1])
	}
	s = strings.TrimPrefix(s, "<persona>")
	s = strings.TrimSuffix(s, "</persona>")
	return strings.TrimSpace(s)
}

// personaPrompt asks the model to rewrite the persona as the admin asks.
func personaPrompt(current, request string, replied *storage.Message) []llm.Message {
	sys := `Ты настраиваешь личность бота — участника группового чата в Telegram. Тебе дают текущее описание его личности ` +
		`и сообщение админа чата, который просит её изменить. Напиши новое описание целиком.
- Если админ прислал готовое описание личности — возьми его как есть, поправь разве что явные опечатки, своего не добавляй.
- Если админ просит что-то поменять («теперь ты фанат джаза», «пиши короче», «будь вежливее») — поменяй только это, ` +
		`остальное оставь как было, с теми же разделами и разметкой; противоречащее новому — убери.
- Пиши о боте во втором лице («Тебя зовут…», «Ты…»), как в текущем описании. Имя меняй, только если об этом просят.
- Только характер, интересы, темы, стиль речи и запреты — без правил про конкретных людей, топики и реакции.
- Не длиннее ` + fmt.Sprint(maxPersonaRunes) + ` знаков.
- Бот никогда не травит конкретного человека, не оскорбляет по национальности, полу, ориентации, религии, внешности, здоровью ` +
		`и подобным признакам, не угрожает — это действует всегда. Если просят иначе, эту часть просьбы пропусти; ` +
		`ужесточать можно.
Ответь только текстом нового описания, без пояснений, кавычек и вступлений.`
	var u strings.Builder
	u.WriteString("Текущее описание:\n<persona>\n")
	u.WriteString(strings.TrimSpace(current))
	u.WriteString("\n</persona>\n\n")
	if replied != nil && strings.TrimSpace(replied.Text) != "" {
		fmt.Fprintf(&u, "Админ отвечает на сообщение:\n%s\n\n", replied.Text)
	}
	u.WriteString("Сообщение админа:\n")
	u.WriteString(request)
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}

// confirmPersona handles the admin's answer to the draft; it returns the text the question turns into.
func (b *Brain) confirmPersona(ctx context.Context, g storage.Chat, draftID int64, cb Callback, yes bool) string {
	by := cb.UserID
	d, ok, err := b.store.PersonaDraft(ctx, g.ID, draftID)
	switch {
	case err != nil:
		slog.Error("load persona draft", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	case !ok:
		return "Этот вариант уже неактуален."
	case d.UserID != by:
		return "" // someone else's draft: the question stays
	}
	if err := b.store.DeletePersonaDraft(ctx, g.ID, d.ID); err != nil {
		slog.Error("delete persona draft", "chat_id", g.ID, "err", err)
	}
	if !yes {
		return fmt.Sprintf("Хорошо, в «%s» остаюсь прежним.", title(g))
	}
	mem := b.memory(ctx, g.ID)
	if g.State != storage.ChatActive || mem.roster.rank(by, cb.Username) < rankLead {
		return fmt.Sprintf("Ты больше не админ в «%s» — личность не меняю.", title(g))
	}
	if err := b.store.SetPersona(ctx, g.ID, d.Text, by, b.now()); err != nil {
		slog.Error("save persona", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	}
	slog.Info("persona changed", "chat_id", g.ID, "by", by, "default", d.Text == "")
	if d.Text == "" {
		return fmt.Sprintf("Готово: в «%s» я снова по умолчанию.", title(g))
	}
	return fmt.Sprintf("Готово, теперь в «%s» я такой:\n\n%s", title(g), d.Text)
}
