package brain

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/vvchaschin/communitybot/internal/storage"
)

// Turning a group on (SPEC 3.2): the creator names the admins and the mode. Bot API finds nobody
// by @username, and an off group keeps no history, so the bot looks for the admin among the people
// it knows and then among the authors of the group's messages; the creator confirms with a button.

// candidate is someone who may be the admin a group waits for.
type candidate struct {
	chat int64
	user int64
}

// enable turns the group on: it waits for the named admins in the given mode.
func (b *Brain) enable(ctx context.Context, by int64, g storage.Chat, mode string, wanted []storage.WantedAdmin) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch {
	case g.State == storage.ChatActive:
		return fmt.Sprintf("«%s» уже включена. Могу назначить или снять админа или отключить её.", title(g))
	case len(wanted) == 0:
		return fmt.Sprintf("Кто будет админом в «%s»? Назови его — @username или имя.", title(g))
	case !storage.ValidMode(mode):
		return fmt.Sprintf("В каком режиме включить «%s»: тишина, только реакции или общение?", title(g))
	}
	if _, err := b.store.SetChatState(ctx, g.ID, storage.ChatPending, mode); err != nil {
		slog.Error("turn group on", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	}
	b.dropChat(g.ID)
	if err := b.store.ClearWantedAdmins(ctx, g.ID); err != nil {
		slog.Error("clear wanted admins", "chat_id", g.ID, "err", err)
	}
	b.forgetCandidates(g.ID)
	var names []string
	for _, w := range wanted {
		w.ChatID, w.RequestedBy, w.RequestedAt = g.ID, by, b.now()
		if err := b.store.AddWantedAdmin(ctx, w); err != nil {
			slog.Error("add wanted admin", "chat_id", g.ID, "err", err)
		}
		names = append(names, w.Label())
	}
	slog.Info("group turned on, waiting for the admin", "chat_id", g.ID, "mode", mode, "wanted", names, "by", by)
	g.Mode = mode
	asked := b.askKnown(ctx, g, wanted, by)
	text := fmt.Sprintf("«%s» включается в режиме «%s», жду админа: %s. ", title(g), modeNames[mode], strings.Join(names, ", "))
	if asked {
		return text + "Кажется, кое-кого нашёл — подтверди ниже."
	}
	return text + "Как увижу его в группе, спрошу тебя, он ли это."
}

// askKnown asks the creator about the people the bot already knows who look like the wanted admins:
// the group's administrators in Telegram and, by @username, people of the active groups.
func (b *Brain) askKnown(ctx context.Context, g storage.Chat, wanted []storage.WantedAdmin, by int64) bool {
	known := b.tgAdminList(ctx, g.ID).people
	for _, w := range wanted {
		if w.Username == "" {
			continue
		}
		p, ok, err := b.store.FindPerson(ctx, w.Username)
		if err != nil {
			slog.Error("find person", "err", err)
		}
		if ok {
			known = append(known, p)
		}
	}
	asked := false
	for _, p := range known {
		if p.ID == 0 || p.ID == b.self.ID || !wantedOne(wanted, p) {
			continue
		}
		if _, member, err := b.out.Member(ctx, g.ID, p.ID); err != nil || !member {
			continue // the admin must be in the group
		}
		asked = b.askAdmin(ctx, g, p, by) || asked
	}
	return asked
}

// watchForAdmin looks for the admin a pending group waits for among the authors of its messages
// (and the people who join it). Nothing is stored.
func (b *Brain) watchForAdmin(ctx context.Context, g storage.Chat, in Incoming) {
	if in.UserID == 0 {
		return
	}
	wanted, err := b.store.WantedAdmins(ctx, g.ID)
	if err != nil {
		slog.Error("load wanted admins", "chat_id", g.ID, "err", err)
		return
	}
	p := storage.Person{ID: in.UserID, Name: in.UserName, Username: in.Username}
	for _, w := range wanted {
		if matchesWanted(w, p) {
			b.spawn(func() { b.askAdmin(context.WithoutCancel(ctx), g, p, w.RequestedBy) })
			return
		}
	}
}

func wantedOne(ws []storage.WantedAdmin, p storage.Person) bool {
	for _, w := range ws {
		if matchesWanted(w, p) {
			return true
		}
	}
	return false
}

// matchesWanted: the same @username, or the same name (case and ё aside); a one-word name is
// compared with the first word of the person's name. The creator confirms, so a namesake is harmless.
func matchesWanted(w storage.WantedAdmin, p storage.Person) bool {
	if w.Username != "" && strings.EqualFold(strings.TrimPrefix(w.Username, "@"), p.Username) {
		return true
	}
	want, have := foldName(w.Name), foldName(p.Name)
	if want == "" || have == "" {
		return false
	}
	if want == have {
		return true
	}
	first, _, _ := strings.Cut(have, " ")
	return !strings.Contains(want, " ") && want == first
}

func foldName(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(s), "ё", "е")), " ")
}

// askAdmin sends the creator a question with buttons: is this the admin? Each person is asked
// about once (until a restart); reports whether the question was sent.
func (b *Brain) askAdmin(ctx context.Context, g storage.Chat, p storage.Person, creator int64) bool {
	k := candidate{g.ID, p.ID}
	b.mu.Lock()
	if _, asked := b.candidates[k]; asked {
		b.mu.Unlock()
		return false
	}
	b.candidates[k] = p
	b.mu.Unlock()
	text := fmt.Sprintf("В «%s» вижу %s. Это он? Назначу админом и включу бота в режиме «%s».", title(g), p.Label(), modeNames[g.Mode])
	data := fmt.Sprintf("adm:%d:%d:", g.ID, p.ID)
	if _, err := b.out.SendButtons(ctx, creator, text, [][]Button{{{Text: "Да, это он", Data: data + "y"}, {Text: "Нет", Data: data + "n"}}}); err != nil {
		slog.Error("ask the creator about the admin", "chat_id", g.ID, "user_id", p.ID, "err", err)
		b.mu.Lock()
		delete(b.candidates, k) // ask again next time
		b.mu.Unlock()
		return false
	}
	slog.Info("asked the creator about the admin", "chat_id", g.ID, "user_id", p.ID, "creator", creator)
	return true
}

// forgetCandidates lets the bot ask about the group's people again (it is turned on anew).
func (b *Brain) forgetCandidates(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.candidates {
		if k.chat == chatID {
			delete(b.candidates, k)
		}
	}
}

// askDisable asks the creator to confirm turning the group off: everything about it goes.
func (b *Brain) askDisable(ctx context.Context, creator int64, g storage.Chat) {
	text := fmt.Sprintf("Отключить бота в «%s»? Всё, что я о ней знаю (история, память, заметки, роли), сотрётся; в группе я останусь, но буду молчать.", title(g))
	data := fmt.Sprintf("off:%d:0:", g.ID)
	if _, err := b.out.SendButtons(ctx, creator, text, [][]Button{{{Text: "Да, отключить", Data: data + "y"}, {Text: "Нет", Data: data + "n"}}}); err != nil {
		slog.Error("ask to turn the group off", "chat_id", g.ID, "err", err)
	}
}

// OnCallback handles a press of a button under the bot's question (SPEC 3.2). Only creators press them.
func (b *Brain) OnCallback(ctx context.Context, cb Callback) {
	b.spawn(func() { b.handleCallback(context.WithoutCancel(ctx), cb) })
}

func (b *Brain) handleCallback(ctx context.Context, cb Callback) {
	answer := func(text string) {
		if err := b.out.AnswerCallback(ctx, cb.ID, text); err != nil {
			slog.Warn("answer callback", "err", err)
		}
	}
	parts := strings.Split(cb.Data, ":")
	// The group's admin answers about its persona (persona.go); the rest only creators.
	if len(parts) != 4 || (parts[0] != "per" && !b.admins[cb.UserID]) {
		answer("")
		return
	}
	chatID, err1 := strconv.ParseInt(parts[1], 10, 64)
	userID, err2 := strconv.ParseInt(parts[2], 10, 64) // a draft's id for "per"
	if err1 != nil || err2 != nil {
		answer("")
		return
	}
	l := b.lock(chatID)
	l.Lock()
	defer l.Unlock()
	g, known, err := b.chat(ctx, chatID)
	if err != nil {
		answer("Что-то сломалось")
		return
	}
	var text string
	switch {
	case !known:
		text = "Меня в этой группе уже нет."
	case parts[0] == "adm" && parts[3] == "y":
		text = b.confirmAdmin(ctx, g, userID, cb.UserID)
	case parts[0] == "adm":
		text = "Понял, не он. Жду дальше."
	case parts[0] == "off" && parts[3] == "y":
		text = b.disable(ctx, g, cb.UserID)
	case parts[0] == "off":
		text = fmt.Sprintf("Хорошо, «%s» не трогаю.", title(g))
	case parts[0] == "per":
		text = b.confirmPersona(ctx, g, userID, cb, parts[3] == "y")
	default:
		answer("")
		return
	}
	answer("")
	if text == "" {
		return
	}
	if err := b.out.Edit(ctx, cb.ChatID, cb.MessageID, text); err != nil {
		slog.Warn("edit the question", "err", err)
	}
}

// confirmAdmin appoints the confirmed person and turns a pending group on.
func (b *Brain) confirmAdmin(ctx context.Context, g storage.Chat, userID, creator int64) string {
	if g.State == storage.ChatOff {
		return fmt.Sprintf("«%s» уже отключена — сначала включи её снова.", title(g))
	}
	p, member, err := b.out.Member(ctx, g.ID, userID)
	b.mu.Lock()
	asked := b.candidates[candidate{g.ID, userID}]
	b.mu.Unlock()
	if p.Name == "" && p.Username == "" { // Telegram did not say: whom the creator was asked about
		p.Name, p.Username = asked.Name, asked.Username
	}
	switch {
	case err != nil:
		slog.Error("check the admin's membership", "chat_id", g.ID, "user_id", userID, "err", err)
		return "Не смог проверить, состоит ли он в группе. Попробуй позже."
	case !member:
		return fmt.Sprintf("%s не состоит в «%s» — админом не назначаю.", p.Label(), title(g))
	}
	if err := b.store.AddLead(ctx, g.ID, p, creator, b.now()); err != nil {
		slog.Error("appoint the admin", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	}
	if g.State != storage.ChatPending {
		b.dropChat(g.ID)
		return fmt.Sprintf("Готово: %s — админ в «%s».", p.Label(), title(g))
	}
	rest, err := b.store.WantedAdmins(ctx, g.ID)
	if err != nil {
		slog.Error("load wanted admins", "chat_id", g.ID, "err", err)
	}
	var others []string
	for _, w := range rest {
		if !matchesWanted(w, p) {
			others = append(others, w.Label())
		}
	}
	if _, err := b.store.SetChatState(ctx, g.ID, storage.ChatActive, g.Mode); err != nil {
		slog.Error("activate group", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	}
	if err := b.store.ClearWantedAdmins(ctx, g.ID); err != nil {
		slog.Error("clear wanted admins", "chat_id", g.ID, "err", err)
	}
	b.dropChat(g.ID)
	b.forgetCandidates(g.ID)
	slog.Info("group is on", "chat_id", g.ID, "mode", g.Mode, "admin", p.ID, "by", creator)
	text := fmt.Sprintf("Готово: %s — админ в «%s», бот включён в режиме «%s».", p.Label(), title(g), modeNames[g.Mode])
	if len(others) > 0 {
		text += fmt.Sprintf(" Остальных (%s) назначь, когда они появятся в группе: теперь я её читаю и узнаю их.", strings.Join(others, ", "))
	}
	return text
}

// disable turns the group off and forgets everything about it (SPEC 3.1).
func (b *Brain) disable(ctx context.Context, g storage.Chat, by int64) string {
	if err := b.store.ResetChat(ctx, g.ID); err != nil {
		slog.Error("turn group off", "chat_id", g.ID, "err", err)
		return "Что-то сломалось, повтори позже."
	}
	b.dropChat(g.ID)
	b.forgetCandidates(g.ID)
	b.mu.Lock()
	delete(b.pending, g.ID)
	delete(b.tgAdmins, g.ID)
	for k := range b.norms {
		if k.chat == g.ID {
			delete(b.norms, k)
		}
	}
	b.mu.Unlock()
	slog.Info("group turned off, its data is gone", "chat_id", g.ID, "by", by)
	return fmt.Sprintf("Готово: «%s» отключена, всё о ней забыл.", title(g))
}

// title is how a group is called to people.
func title(g storage.Chat) string {
	if g.Title != "" {
		return g.Title
	}
	return strconv.FormatInt(g.ID, 10)
}
