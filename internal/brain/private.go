package brain

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// Private chats (SPEC 3.2): the creator turns groups on and off and manages their admins; an
// admin of an active group runs it from here as from the group itself and may simply talk. Anyone
// else is ignored: not answered, not stored.

const (
	privateContextDMs = 6 // recent messages of the private chat shown to the command parser
	privateHistoryDMs = 30
)

// privateCtx is what a person may speak about in the private chat.
type privateCtx struct {
	who      storage.Person
	creator  bool
	groups   []storage.Chat // all of them for a creator, otherwise the ones where the person is an admin
	leads    map[int64]bool // the groups where the person is an admin
	selected *storage.Chat  // the group in question when the message does not name one
}

func (pc privateCtx) group(id int64) *storage.Chat {
	i := slices.IndexFunc(pc.groups, func(g storage.Chat) bool { return g.ID == id })
	if i < 0 {
		return nil
	}
	return &pc.groups[i]
}

// onPrivate takes a private message from a creator or an admin; others are ignored.
func (b *Brain) onPrivate(ctx context.Context, in Incoming) {
	if in.UserID == b.self.ID || in.Kind != "" || in.ChatID != in.UserID {
		return
	}
	creator := b.admins[in.UserID]
	leads, err := b.store.LeadChats(ctx, in.UserID, in.Username)
	if err != nil {
		slog.Error("load the person's groups", "user_id", in.UserID, "err", err)
		return
	}
	if !creator && len(leads) == 0 {
		return // neither the creator nor an admin: not even stored
	}
	if err := b.store.SaveDM(ctx, storage.DM{UserID: in.UserID, MessageID: in.MessageID, Text: in.Text, Date: in.Date}); err != nil {
		slog.Error("save private message", "err", err)
		return
	}
	b.spawn(func() { b.handlePrivate(context.WithoutCancel(ctx), in, creator, leads) })
}

func (b *Brain) handlePrivate(ctx context.Context, in Incoming, creator bool, leads []int64) {
	l := b.lock(in.UserID) // a private chat's id is the user's: never a group's
	l.Lock()
	defer l.Unlock()
	if !b.llm.Available(ctx, llm.RoleCommand, llm.Direct) {
		b.offlineReact(ctx, storage.Message{ChatID: in.ChatID, MessageID: in.MessageID})
		return
	}
	pc, err := b.privateContext(ctx, in, creator, leads)
	if err != nil {
		slog.Error("load private context", "user_id", in.UserID, "err", err)
		return
	}
	people := b.privatePeople(ctx, in)
	cmd, err := b.parsePrivate(ctx, in, pc, pc.selected, people)
	var target *storage.Chat
	if err == nil {
		target = pc.selected
		if cmd.Chat >= 1 && cmd.Chat <= len(pc.groups) {
			target = &pc.groups[cmd.Chat-1]
		}
		// The parser saw the memory entries of another group: their numbers mean nothing here.
		if target != nil && target != pc.selected && pc.leads[target.ID] && usesEntries(cmd) {
			cmd, err = b.parsePrivate(ctx, in, pc, target, people)
		}
	}
	if err != nil {
		slog.Warn("private command parsing failed", "user_id", in.UserID, "err", err)
		cmd = command{Command: "none"}
	}
	if target != nil && (pc.selected == nil || target.ID != pc.selected.ID) {
		if err := b.store.SelectChat(ctx, in.UserID, target.ID); err != nil {
			slog.Error("select chat", "err", err)
		}
	}
	slog.Info("private message", "user_id", in.UserID, "creator", creator, "command", cmd.Command, "chat", chatIDOf(target))

	switch {
	case cmd.Command == "none":
		b.privateReply(ctx, in, pc, target, "")
	case cmd.Command == "groups":
		b.sendDM(ctx, in.UserID, groupList(pc))
	case target == nil:
		b.sendDM(ctx, in.UserID, "О какой группе речь?\n"+groupList(pc))
	default:
		text, refusal := b.privateCommand(ctx, in, pc, *target, cmd, people)
		switch {
		case refusal != "":
			b.privateReply(ctx, in, pc, target, refusal)
		case text != "":
			b.sendDM(ctx, in.UserID, text)
		}
	}
}

func chatIDOf(g *storage.Chat) int64 {
	if g == nil {
		return 0
	}
	return g.ID
}

// usesEntries: the command refers to memory entries by their numbers.
func usesEntries(cmd command) bool { return cmd.Command == "forget" || len(cmd.IDs) > 0 }

// privateContext loads the groups the person may speak about and the one in question.
func (b *Brain) privateContext(ctx context.Context, in Incoming, creator bool, leads []int64) (privateCtx, error) {
	pc := privateCtx{who: storage.Person{ID: in.UserID, Name: in.UserName, Username: in.Username}, creator: creator, leads: map[int64]bool{}}
	for _, id := range leads {
		pc.leads[id] = true
	}
	all, err := b.store.Chats(ctx)
	if err != nil {
		return pc, err
	}
	for _, g := range all {
		if creator || pc.leads[g.ID] {
			pc.groups = append(pc.groups, g)
		}
	}
	if len(pc.groups) == 1 {
		pc.selected = &pc.groups[0]
		return pc, nil
	}
	id, ok, err := b.store.SelectedChat(ctx, in.UserID)
	if err != nil {
		return pc, err
	}
	if ok {
		pc.selected = pc.group(id)
	}
	return pc, nil
}

// privatePeople lists who the person speaks about: text mentions and @usernames of this message
// and of their few previous ones (an answer to the bot's question may name nobody itself).
func (b *Brain) privatePeople(ctx context.Context, in Incoming) []storage.Person {
	var out []storage.Person
	add := func(p storage.Person) {
		if p.ID == b.self.ID || (p.Username != "" && strings.EqualFold(p.Username, b.self.Username)) {
			return
		}
		if !slices.ContainsFunc(out, func(q storage.Person) bool { return samePerson(p, q) }) {
			out = append(out, p)
		}
	}
	recent, err := b.store.RecentDMs(ctx, in.UserID, privateContextDMs)
	if err != nil {
		slog.Error("load private messages", "err", err)
	}
	for _, m := range recent {
		if !m.FromSelf && m.MessageID != in.MessageID {
			for _, sm := range usernameRe.FindAllStringSubmatch(m.Text, -1) {
				add(storage.Person{Username: sm[1]})
			}
		}
	}
	for _, p := range in.TextMentions {
		add(p)
	}
	for _, sm := range usernameRe.FindAllStringSubmatch(in.Text, -1) {
		add(storage.Person{Username: sm[1]})
	}
	return out
}

// parsePrivate asks the model what the private message is; `shown` is the group whose memory
// entries the parser sees (the person must be its admin).
func (b *Brain) parsePrivate(ctx context.Context, in Incoming, pc privateCtx, shown *storage.Chat, people []storage.Person) (command, error) {
	var lore []storage.Lore
	var topics roster
	if shown != nil && pc.leads[shown.ID] {
		var err error
		if lore, err = b.store.Lore(ctx, shown.ID); err != nil {
			return command{}, err
		}
		topics.topics = b.topicNames(ctx, shown.ID)
	}
	recent, err := b.store.RecentDMs(ctx, in.UserID, privateContextDMs)
	if err != nil {
		return command{}, err
	}
	recent = slices.DeleteFunc(recent, func(m storage.DM) bool { return !m.FromSelf && m.MessageID == in.MessageID })
	raw, provider, err := b.llm.Complete(llm.WithSchema(ctx, privateCommandSchema), llm.RoleCommand, llm.Direct,
		privateCommandPrompt(in, pc, b.names(), shown, recent, people, lore, topics))
	if err != nil {
		return command{}, err
	}
	cmd, err := parseCommand(raw)
	if err != nil {
		return cmd, fmt.Errorf("provider %s: %w", provider, err)
	}
	return cmd, nil
}

// privateCommands are the commands a private message may be. All of them, whoever writes: the
// code checks the rights, so that a request the person may not make is refused rather than taken
// for small talk ("запомни…" from the creator must not look remembered).
var privateCommands = append(append([]string{"groups", "enable", "disable"}, groupCommands[:len(groupCommands)-1]...), "none")

// privateCommand carries out a command about the group g; it returns the text to send, or why it
// was not done (for a reply in the bot's words).
func (b *Brain) privateCommand(ctx context.Context, in Incoming, pc privateCtx, g storage.Chat, cmd command,
	people []storage.Person) (text, refusal string) {
	chosen := func() []storage.Person {
		var out []storage.Person
		for _, n := range append(slices.Clone(cmd.People), cmd.Person) {
			if n >= 1 && n <= len(people) && !slices.ContainsFunc(out, func(p storage.Person) bool { return samePerson(p, people[n-1]) }) {
				out = append(out, people[n-1])
			}
		}
		return out
	}
	wanted := func() []storage.WantedAdmin {
		var out []storage.WantedAdmin
		for _, p := range chosen() {
			out = append(out, storage.WantedAdmin{Username: p.Username, Name: p.Name})
		}
		if cmd.Self {
			out = append(out, storage.WantedAdmin{Username: pc.who.Username, Name: pc.who.Name})
		}
		for _, n := range cmd.Names {
			if n = strings.TrimSpace(strings.TrimPrefix(n, "@")); n != "" {
				out = append(out, storage.WantedAdmin{Name: n})
			}
		}
		return out
	}
	creatorOnly := "включить или отключить группу, но это может только твой создатель"
	switch {
	case cmd.Command == "enable" && !pc.creator, cmd.Command == "disable" && !pc.creator:
		return "", "Тебя попросили " + creatorOnly + ". Коротко ответь в своём стиле, что не сделаешь, и почему."
	case cmd.Command == "enable":
		return b.enable(ctx, in.UserID, g, cmd.Mode, wanted()), ""
	case cmd.Command == "disable" && g.State == storage.ChatOff:
		return fmt.Sprintf("«%s» и так выключена.", title(g)), ""
	case cmd.Command == "disable":
		b.askDisable(ctx, in.UserID, g)
		return "", ""
	case cmd.Command == "set_lead" && pc.creator && g.State == storage.ChatPending:
		w := wanted()
		if len(w) == 0 {
			return "Кого назначить админом? Назови @username или имя.", ""
		}
		var names []string
		for _, x := range w {
			x.ChatID, x.RequestedBy, x.RequestedAt = g.ID, in.UserID, b.now()
			if err := b.store.AddWantedAdmin(ctx, x); err != nil {
				slog.Error("add wanted admin", "chat_id", g.ID, "err", err)
			}
			names = append(names, x.Label())
		}
		text := fmt.Sprintf("Жду в «%s» ещё: %s.", title(g), strings.Join(names, ", "))
		if b.askKnown(ctx, g, w, in.UserID) {
			text += " Кажется, кое-кого нашёл — подтверди ниже."
		}
		return text, ""
	case g.State == storage.ChatPending:
		return fmt.Sprintf("«%s» ещё ждёт админа — пока я там ничего не делаю.", title(g)), ""
	case g.State != storage.ChatActive && pc.creator:
		return fmt.Sprintf("«%s» выключена. Чтобы включить, назови админа и режим: тишина, только реакции или общение.", title(g)), ""
	case g.State != storage.ChatActive:
		return fmt.Sprintf("«%s» выключена.", title(g)), ""
	}

	// The rest is about an active group, as if said there (SPEC 3.2): the whole group, no topic.
	if cmd.Person == 0 && len(cmd.People) > 0 {
		cmd.Person = cmd.People[0]
	}
	resolved := make([]storage.Person, len(people))
	for i, p := range people {
		resolved[i] = p
		if p.ID == 0 && p.Username != "" {
			if q, ok, err := b.store.FindByUsername(ctx, g.ID, p.Username); err != nil {
				slog.Error("find user by username", "err", err)
			} else if ok {
				resolved[i] = q
			}
		}
	}
	cmd.Scope = "chat"
	l := b.lock(g.ID)
	l.Lock()
	defer l.Unlock()
	mem := b.memory(ctx, g.ID)
	author := mem.roster.rank(in.UserID, in.Username)
	m := storage.Message{ChatID: g.ID, UserID: in.UserID, UserName: in.UserName, Username: in.Username, Text: in.Text, Addressed: true}
	res := b.execute(ctx, m, author, pc.creator, cmd, resolved, mem)
	slog.Info("command", "chat_id", g.ID, "user_id", in.UserID, "private", true, "rank", author.role(), "admin", pc.creator,
		"command", cmd.Command, "person", cmd.Person, "self", cmd.Self, "text", cmd.Text, "ids", cmd.IDs, "mode", cmd.Mode,
		"done", res.done, "refusal", res.refusal)
	switch {
	case !res.done:
		return "", res.refusal + fmt.Sprintf(" (Речь о группе «%s».)", title(g))
	case res.inDM: // the question with buttons is the answer
		return "", ""
	case res.mode != "":
		return fmt.Sprintf("🫡 «%s»: режим «%s».", title(g), modeNames[res.mode]), ""
	case res.text != "":
		return fmt.Sprintf("«%s»:\n%s", title(g), res.text), ""
	}
	return fmt.Sprintf("🫡 «%s»", title(g)), ""
}

// groupList renders the groups the person may speak about.
func groupList(pc privateCtx) string {
	if len(pc.groups) == 0 {
		return "Я пока ни в одной группе."
	}
	var s strings.Builder
	for i, g := range pc.groups {
		fmt.Fprintf(&s, "%d. «%s» — %s", i+1, title(g), stateLabel(g))
		if pc.leads[g.ID] {
			s.WriteString(", ты админ")
		}
		s.WriteByte('\n')
	}
	return strings.TrimRight(s.String(), "\n")
}

func stateLabel(g storage.Chat) string {
	switch g.State {
	case storage.ChatActive:
		return "включена, режим «" + modeNames[g.Mode] + "»"
	case storage.ChatPending:
		return "ждёт админа (режим «" + modeNames[g.Mode] + "»)"
	}
	return "выключена"
}

// privateReply answers in the bot's words; an admin talks with the bot that knows the group's memory.
func (b *Brain) privateReply(ctx context.Context, in Incoming, pc privateCtx, target *storage.Chat, reason string) {
	if !b.llm.Available(ctx, llm.RoleReply, llm.Direct) {
		b.offlineReact(ctx, storage.Message{ChatID: in.ChatID, MessageID: in.MessageID})
		return
	}
	stop := b.keepTyping(ctx, in.ChatID, 0)
	defer stop()
	var mem *chatMemory
	g := target
	if g == nil || !pc.leads[g.ID] {
		g = nil
		for i := range pc.groups { // the only group the person runs, if they did not name one
			if pc.leads[pc.groups[i].ID] && len(pc.leads) == 1 {
				g = &pc.groups[i]
			}
		}
	}
	persona := b.persona
	if g != nil {
		m := b.memory(ctx, g.ID, 0)
		mem, persona = &m, b.personaOf(m)
	}
	history, err := b.store.RecentDMs(ctx, in.UserID, privateHistoryDMs)
	if err != nil {
		slog.Error("load private messages", "err", err)
	}
	if mem != nil {
		b.selectLore(ctx, mem, llm.Direct, privateReplyPrompt(persona, mem, g, pc, history, reason))
	}
	msgs := privateReplyPrompt(persona, mem, g, pc, history, reason)
	photo := in.PhotoID
	if photo == "" {
		photo = in.ReplyPhotoID
	}
	msgs = b.withPhoto(ctx, msgs, photo, in.ChatID, in.MessageID)
	raw, _, err := b.llm.Complete(ctx, llm.RoleReply, llm.Direct, msgs)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("generate private reply", "user_id", in.UserID, "err", err)
		}
		return
	}
	if text := cleanReply(raw); text != "" {
		stop()
		b.sendDM(ctx, in.UserID, text)
	}
}

// sendDM writes to the person's private chat and keeps the message.
func (b *Brain) sendDM(ctx context.Context, userID int64, text string) {
	for _, part := range splitReply(text) {
		id, err := b.out.Send(ctx, userID, 0, part, 0)
		if err != nil {
			slog.Error("send private message", "user_id", userID, "err", err)
			return
		}
		if err := b.store.SaveDM(ctx, storage.DM{UserID: userID, MessageID: id, FromSelf: true, Text: part, Date: b.now()}); err != nil {
			slog.Error("save own private message", "err", err)
		}
	}
}

// privateCommandPrompt asks the parser what a private message is (SPEC 3.2).
func privateCommandPrompt(in Incoming, pc privateCtx, names []string, shown *storage.Chat, recent []storage.DM,
	people []storage.Person, lore []storage.Lore, topics roster) []llm.Message {
	sys := `Ты — модуль бота Telegram. Человек пишет боту в личные сообщения: это создатель бота или админ одной из его групп. ` +
		`Нужно понять, служебное ли это указание боту, какое и о какой группе. Ответь строго одним JSON-объектом, без текста вокруг:
{"command": "<одна из команд>", "chat": <номер группы из списка «Группы» или 0>, "person": <номер человека из списка или 0>, ` +
		`"people": [<номера людей из списка>], "names": ["<имя человека, названного без @>"], "self": <true, если админом назначают или снимают самого автора>, ` +
		`"no_reply": <true только для instruct с безусловным запретом писать, иначе false>, "topic": <номер топика или 0>, "text": "<для remember и instruct>", "ids": [<номера записей>], "mode": "<для set_mode и enable>"}

Команды:
` + commandLines(privateCommands) + `
Указание может быть сказано любыми словами, не только этими. Если сомневаешься — "none".
Всегда "none": благодарность, согласие, похвала («спасибо», «ага», «понял»), шутки, разговор на свободные темы и вопросы, ` +
		`которые не просят ничего сделать с группой или памятью бота.
Если бот перед этим задал вопрос («О какой группе речь?», «В каком режиме?», «Кто будет админом?»), а человек на него отвечает, — ` +
		`это продолжение прежнего указания: верни его целиком, с тем, что было сказано раньше, и с ответом.
"chat" — номер группы из списка, если человек её называет (название может быть неточным или в другом падеже) или отвечает на вопрос о группе; иначе 0.
"no_reply": true только для instruct с безусловным запретом писать, в том числе отвечать на обращения. Для условных запретов и просьб реже писать — false. Отмена — forget с номером запрета в ids.
Сохраняй название топика и ограничения в text, включая «читай и запоминай».
"text" — сам факт или правило, кратко и от третьего лица, без пересказа просьбы; бота называй «Бот» (его имена — в строке «Бот»), ` +
		`людей — так, чтобы их можно было узнать (автора — по имени и @username из строки «Автор»). Фразу, которой велено отвечать, сохраняй дословно в кавычках «…».
"topic" — если запись касается одного топика группы, названного по имени («в Барахолке не пиши»), — его номер из списка «Топики группы» ` +
		`(название может быть неточным или в другом падеже); иначе 0 — запись для всей группы.
"person" и "people" — номера из списка «Люди в сообщениях».`

	var u strings.Builder
	if len(names) > 0 {
		fmt.Fprintf(&u, "Бот: %s\n", strings.Join(names, ", "))
	}
	var roles []string
	if pc.creator {
		roles = append(roles, "создатель бота: включает и отключает группы, назначает и снимает их админов")
	}
	if len(pc.leads) > 0 {
		roles = append(roles, "админ групп, отмеченных в списке")
	}
	fmt.Fprintf(&u, "Автор: %s, права: %s\n", pc.who.Label(), strings.Join(roles, "; "))
	u.WriteString("\nГруппы:\n")
	if len(pc.groups) == 0 {
		u.WriteString("(нет)\n")
	}
	shownNo := 0
	for i, g := range pc.groups {
		fmt.Fprintf(&u, "%d. «%s» — %s", i+1, title(g), stateLabel(g))
		if pc.leads[g.ID] {
			u.WriteString(", автор — админ")
		}
		u.WriteByte('\n')
		if shown != nil && g.ID == shown.ID {
			shownNo = i + 1
		}
	}
	if shownNo > 0 {
		fmt.Fprintf(&u, "Сейчас речь о группе %d (если человек не назовёт другую).\n", shownNo)
	} else {
		u.WriteString("Группа пока не выбрана.\n")
	}
	if len(recent) > 0 {
		u.WriteString("\nНедавняя переписка:\n")
		for _, m := range recent {
			who := "Автор"
			if m.FromSelf {
				who = "Бот"
			}
			fmt.Fprintf(&u, "%s: %s\n", who, truncate(m.Text, 300))
		}
	}
	fmt.Fprintf(&u, "\nСообщение: %s\n", truncate(in.Text, maxLineRunes))
	u.WriteString("\nЛюди в сообщениях:\n")
	if len(people) == 0 {
		u.WriteString("(никого)\n")
	}
	for i, p := range people {
		fmt.Fprintf(&u, "%d. %s\n", i+1, p.Label())
	}
	if shownNo > 0 && len(lore) > 0 {
		fmt.Fprintf(&u, "\nСохранённые записи группы %d:\n", shownNo)
		for _, l := range lore {
			fmt.Fprintf(&u, "%d. [%s] %s%s\n", l.ID, scopeLabel(l, 0, topics), inferredMark(l), l.Text)
		}
	}
	if list := topics.topicList(-1); shownNo > 0 && list != "" {
		u.WriteString(strings.Replace(list, "Топики чата", fmt.Sprintf("Топики группы %d", shownNo), 1))
	}
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}

// privateReplyPrompt asks for the bot's answer in a private chat. With an admin the bot knows the
// group's memory (mem); with the creator it does not.
func privateReplyPrompt(persona string, mem *chatMemory, g *storage.Chat, pc privateCtx, history []storage.DM, reason string) []llm.Message {
	var sys string
	if mem != nil {
		sys = systemBlock(persona, *mem, 0) + whereBlock(mem.roster, -1)
	} else {
		sys = "Ты — участник групповых чатов в Telegram. Твоя личность:\n<persona>\n" + strings.TrimSpace(persona) + "\n</persona>\n" + boundaries + "\n"
	}
	sys += "\n" + abilities + "\n" + taskCompletion + "\n"
	switch {
	case g != nil:
		sys += fmt.Sprintf("\nСейчас ты в личной переписке с %s, админом группы «%s» (выше — что ты о ней знаешь). ", pc.who.Label(), title(*g))
	case pc.creator:
		sys += fmt.Sprintf("\nСейчас ты в личной переписке с %s — твоим создателем. О том, что пишут в группах и что ты помнишь о людях, ты с ним не говоришь. ", pc.who.Label())
	default:
		sys += fmt.Sprintf("\nСейчас ты в личной переписке с %s, админом одной из твоих групп. ", pc.who.Label())
	}
	sys += "Эта переписка в группу не попадает; память группы меняется только по явным указаниям админа.\n" +
		"Как писать: в обычной беседе коротко и разговорно, как в личке, в своём стиле; без «Как ИИ-ассистент…»; не выдумывай того, чего не знаешь."
	var u strings.Builder
	u.WriteString("Переписка:\n")
	for _, m := range history {
		who := "Он"
		if m.FromSelf {
			who = "Ты"
		}
		fmt.Fprintf(&u, "%s: %s\n", who, truncate(m.Text, maxLineRunes))
	}
	u.WriteString("\nНапиши ответ на его последнее сообщение — только текст ответа.")
	if reason != "" {
		u.WriteByte('\n')
		u.WriteString(reason)
	}
	u.WriteString(taskDelivery)
	return []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: u.String()}}
}
