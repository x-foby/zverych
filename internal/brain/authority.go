package brain

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// rank is a participant's place in the chat's trust hierarchy (SPEC 4.7). The bot's creator (admin
// in the code) is not part of it: their only powers are the group's admins and turning it on and off.
type rank int

const (
	rankNone rank = iota
	rankAuthority
	rankLead
)

func (r rank) label() string {
	switch r {
	case rankLead:
		return "админ"
	case rankAuthority:
		return "авторитет"
	}
	return ""
}

func (r rank) role() string {
	switch r {
	case rankLead:
		return storage.RoleLead
	case rankAuthority:
		return storage.RoleAuthority
	}
	return ""
}

// roster knows who is who in one chat.
type roster struct {
	roles  []storage.ChatRole
	rep    map[int64]int  // the chat's attitude to people (notes.go), non-zero only
	topics map[int]string // the known names of the chat's topics, by thread
}

// generalTopic is the general topic's number for the command parser: its messages carry no thread
// id (0 means "no topic" in a command), and 1 is never another topic's — the id of the message that
// created the group. Telegram uses the same number for it internally.
const generalTopic = 1

// forum reports whether the chat has topics: then thread 0 is the general topic.
func (r roster) forum() bool { _, ok := r.topics[0]; return ok }

// topic names a topic in a prompt: "топик 7 «Флуд»", or "топик 7" if its name is unknown; the
// general topic — "основной топик «General»", or "основной топик".
func (r roster) topic(t int) string {
	if t == 0 {
		return "основной топик" + r.topicName(0)
	}
	return fmt.Sprintf("топик %d%s", t, r.topicName(t))
}

// tagged reports whether history lines of the topic carry its tag: topics' lines do, and in a chat
// with topics the general one's too.
func (r roster) tagged(t int) bool { return t != 0 || r.forum() }

// topicName is " «Флуд»", or "" if the topic's name is unknown.
func (r roster) topicName(t int) string {
	if name := r.topics[t]; name != "" {
		return " «" + name + "»"
	}
	return ""
}

// topicList lists the known topics of the chat for a command parser, by number (the general one as
// generalTopic), marking the one the message was written in ("here", -1 — none); "" if none is known.
func (r roster) topicList(here int) string {
	ts := slices.Sorted(maps.Keys(r.topics))
	if len(ts) == 0 {
		return ""
	}
	var s strings.Builder
	s.WriteString("\nТопики чата:\n")
	for _, t := range ts {
		if t == 0 {
			fmt.Fprintf(&s, "%d. основной топик%s", generalTopic, r.generalName())
		} else {
			fmt.Fprintf(&s, "%d.%s", t, r.topicName(t))
		}
		if t == here {
			s.WriteString(" (этот)")
		}
		s.WriteByte('\n')
	}
	return s.String()
}

// generalName is the general topic's name for a prompt; unknown — what it is called by default.
func (r roster) generalName() string {
	if n := r.topicName(0); n != "" {
		return n
	}
	return " (по умолчанию «General»; если его переименовали, нового названия ты не знаешь)"
}

func (r roster) rank(userID int64, username string) rank {
	best := rankNone
	for _, cr := range r.roles {
		if !samePerson(cr.Person, storage.Person{ID: userID, Username: username}) {
			continue
		}
		switch {
		case cr.Role == storage.RoleLead:
			best = max(best, rankLead)
		case cr.Role == storage.RoleAuthority:
			best = max(best, rankAuthority)
		}
	}
	return best
}

func samePerson(a, b storage.Person) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}
	return a.Username != "" && strings.EqualFold(a.Username, b.Username)
}

// leads are the chat's admins (there may be several, SPEC 4.7).
func (r roster) leads() []storage.Person { return r.holding(storage.RoleLead) }

func (r roster) authorities() []storage.Person { return r.holding(storage.RoleAuthority) }

func (r roster) holding(role string) []storage.Person {
	var out []storage.Person
	for _, cr := range r.roles {
		if cr.Role == role {
			out = append(out, cr.Person)
		}
	}
	return out
}

// labels renders people as "Имя (@username), …".
func labels(ps []storage.Person) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Label()
	}
	return strings.Join(out, ", ")
}

// chatMemory is what the prompts need to know about a chat besides its messages.
type chatMemory struct {
	roster roster
	lore   []storage.Lore
	topics []int              // the topics in question: the evaluation shows the lore of these (and of the whole chat) only
	norms  map[int]topicNorms // learned norms of the topics in question, by thread
	sums   map[int]string     // summaries of the older messages of the topics in question, by thread
	notes  []storage.Note     // notes about the people in question (withNotes), the most important first

	persona string // the group's own persona, "" — the default one (personaOf)
}

// memory loads roles, lore and the norms and summaries of the given topics; on a storage error it
// logs and returns what it could.
func (b *Brain) memory(ctx context.Context, chatID int64, threads ...int) chatMemory {
	mem := chatMemory{topics: threads, norms: map[int]topicNorms{}, sums: map[int]string{}}
	for _, t := range threads {
		n := b.topicNorms(ctx, chatID, t)
		if s, err := b.store.Summary(ctx, chatID, t); err != nil {
			slog.Error("load summary", "chat_id", chatID, "topic", t, "err", err)
		} else {
			if s.Text != "" {
				mem.sums[t] = s.Text
			}
			n.tone = s.Tone
		}
		mem.norms[t] = n
	}
	var err error
	if mem.roster.roles, err = b.store.Roles(ctx, chatID); err != nil {
		slog.Error("load chat roles", "chat_id", chatID, "err", err)
	}
	if mem.lore, err = b.store.Lore(ctx, chatID); err != nil {
		slog.Error("load lore", "chat_id", chatID, "err", err)
	}
	if mem.roster.rep, err = b.store.Reputations(ctx, chatID); err != nil {
		slog.Error("load reputations", "chat_id", chatID, "err", err)
	}
	mem.roster.topics = b.topicNames(ctx, chatID)
	if mem.persona, err = b.store.Persona(ctx, chatID); err != nil {
		slog.Error("load persona", "chat_id", chatID, "err", err)
	}
	return mem
}

// personaOf is who the bot is in the chat: the group's own persona or the default one (SPEC 5).
func (b *Brain) personaOf(mem chatMemory) string {
	if mem.persona != "" {
		return mem.persona
	}
	return b.persona
}

var usernameRe = regexp.MustCompile(`@([A-Za-z0-9_]{4,32})`)

// people lists who a message refers to: the replied-to author, text mentions (refs, from
// Telegram) and @usernames in the text. The bot itself is never a candidate.
func (b *Brain) people(ctx context.Context, m storage.Message, refs []storage.Person) []storage.Person {
	var out []storage.Person
	add := func(p storage.Person) {
		if p.ID == b.self.ID || (p.Username != "" && strings.EqualFold(p.Username, b.self.Username)) {
			return
		}
		if slices.ContainsFunc(out, func(q storage.Person) bool { return samePerson(p, q) }) {
			return
		}
		out = append(out, p)
	}
	if len(refs) == 0 && m.ReplyTo != 0 {
		if r, err := b.store.Get(ctx, m.ChatID, m.ReplyTo); err == nil && r != nil && !r.IsSelf && r.UserID != m.UserID {
			add(storage.Person{ID: r.UserID, Name: r.UserName, Username: r.Username})
		}
	}
	for _, p := range refs {
		add(p)
	}
	for _, p := range b.mentions(ctx, m) {
		add(p)
	}
	return out
}

// tryCommand checks whether a message from a trusted participant is an instruction to the bot
// (SPEC 4.7) and carries it out. It reports whether the message was handled as a command;
// otherwise it is an ordinary message. The caller holds the chat lock.
//
// Memory and roles other than the admins change only when the bot is addressed personally; a
// message not addressed to it (the creator's "@vasya тут админ") may only appoint or remove an admin.
func (b *Brain) tryCommand(ctx context.Context, m storage.Message, refs []storage.Person) bool {
	mem := b.memory(ctx, m.ChatID)
	author := mem.roster.rank(m.UserID, m.Username)
	admin := b.admins[m.UserID]
	if author == rankNone && !admin {
		return false
	}
	people := b.people(ctx, m, refs)
	var replied *storage.Message
	if m.ReplyTo != 0 {
		replied, _ = b.store.Get(ctx, m.ChatID, m.ReplyTo)
	}
	raw, provider, err := b.llm.Complete(llm.WithSchema(ctx, commandSchema), llm.RoleCommand, llm.Direct,
		commandPrompt(m, rights(author, admin), b.names(), replied, people, mem))
	if err != nil {
		slog.Warn("command parsing failed", "chat_id", m.ChatID, "err", err)
		return false
	}
	cmd, err := parseCommand(raw)
	if err != nil {
		slog.Warn("command parser returned unparsable answer", "provider", provider, "err", err)
		return false
	}
	if cmd.Command == "none" || slices.Contains(privateOnly, cmd.Command) {
		return false
	}
	if !m.Addressed && !leadCommand(cmd.Command) {
		slog.Info("command ignored: the bot was not addressed", "chat_id", m.ChatID, "user_id", m.UserID,
			"command", cmd.Command, "text", cmd.Text)
		return false
	}
	res := b.execute(ctx, m, author, admin, cmd, people, mem)
	slog.Info("command", "chat_id", m.ChatID, "user_id", m.UserID, "rank", author.role(), "admin", admin, "command", cmd.Command,
		"person", cmd.Person, "self", cmd.Self, "scope", cmd.Scope, "text", cmd.Text, "ids", cmd.IDs, "done", res.done, "refusal", res.refusal)

	// The mode after the command: "можешь говорить" is answered already (SPEC 4.1a).
	mode := b.mode(ctx, m.ChatID)
	switch {
	case mode == storage.ModeSilent:
	case res.done && (res.text == "" || mode == storage.ModeReactions):
		if err := b.out.React(ctx, m.ChatID, m.MessageID, "🫡"); err != nil {
			slog.Warn("command ack reaction failed", "err", err)
		}
	case mode == storage.ModeReactions: // a refusal: no words in a silent group
	case res.done:
		b.send(ctx, m, res.text)
	default:
		text, err := b.generate(ctx, m, res.refusal, llm.Direct)
		if err != nil {
			slog.Warn("command refusal not generated", "err", err)
			return true
		}
		b.send(ctx, m, text)
	}
	return true
}

type commandResult struct {
	done    bool
	text    string // reply to post instead of the 🫡 reaction (e.g. the list)
	refusal string // why it was not done: a hint for the generated answer
	mode    string // set_mode: the new mode
	inDM    bool   // set_persona: the question went to the author's private chat
}

func refuse(format string, a ...any) commandResult {
	return commandResult{refusal: "Тебя попросили: " + fmt.Sprintf(format, a...) +
		". Коротко ответь в своём стиле, что не сделаешь, и почему."}
}

func leadCommand(c string) bool { return c == "set_lead" || c == "unset_lead" }

// rights describes the author's powers for the command parser.
func rights(author rank, admin bool) string {
	switch {
	case admin && author != rankNone:
		return author.label() + ", создатель бота"
	case admin:
		return "создатель бота (назначает и снимает админов чата)"
	}
	return author.label()
}

// modeNames are how the modes are called to people (SPEC 4.1a).
var modeNames = map[string]string{storage.ModeSilent: "тишина", storage.ModeReactions: "только реакции", storage.ModeTalk: "общение"}

// names lists what the bot is called in the chat, for prompts that must tell it from other people.
func (b *Brain) names() []string {
	var out []string
	if b.self.Name != "" {
		out = append(out, b.self.Name)
	}
	if b.self.Username != "" {
		out = append(out, "@"+b.self.Username)
	}
	for _, a := range b.aliases {
		if !slices.ContainsFunc(out, func(n string) bool { return strings.EqualFold(n, a) }) {
			out = append(out, a)
		}
	}
	return out
}

func (b *Brain) execute(ctx context.Context, m storage.Message, author rank, admin bool, cmd command, people []storage.Person,
	mem chatMemory) commandResult {
	now := b.now()
	person := func() (storage.Person, bool) {
		if cmd.Self && leadCommand(cmd.Command) {
			return storage.Person{ID: m.UserID, Name: m.UserName, Username: m.Username}, true
		}
		if cmd.Person >= 1 && cmd.Person <= len(people) {
			return people[cmd.Person-1], true
		}
		if len(people) == 1 {
			return people[0], true
		}
		return storage.Person{}, false
	}
	fail := func(what string, err error) commandResult {
		slog.Error("command failed", "command", what, "err", err)
		return commandResult{refusal: "Тебя попросили выполнить служебное указание, но у тебя что-то сломалось. Коротко скажи, что не вышло, пусть повторят позже."}
	}

	// Apart from the admins, the creator has no say over the bot (SPEC 3.1, 4.7): they can become
	// an admin themselves.
	if !leadCommand(cmd.Command) && author == rankNone {
		return refuse("выполнить служебное указание, но их ты принимаешь только от админов чата и авторитетов")
	}

	switch cmd.Command {
	case "set_lead":
		if !admin && author < rankLead {
			return refuse("назначить админа чата, но это могут только админы чата и твой создатель")
		}
		p, ok := person()
		if !ok {
			return refuse("назначить админа чата, но непонятно кого: пусть ответят на сообщение этого человека или упомянут его")
		}
		// The admin must be in the group (SPEC 3.1). Known by @username only: checked when they write.
		if p.ID != 0 {
			if _, member, err := b.out.Member(ctx, m.ChatID, p.ID); err == nil && !member {
				return refuse("назначить админом %s, но этого человека нет в группе", p.Label())
			}
		}
		if err := b.store.AddLead(ctx, m.ChatID, p, m.UserID, now); err != nil {
			return fail(cmd.Command, err)
		}
		return commandResult{done: true}

	case "unset_lead":
		p, ok := person()
		leads := mem.roster.leads()
		switch {
		case !admin && author < rankLead:
			return refuse("снять админа чата, но это может только твой создатель")
		case !ok:
			return refuse("снять админа чата, но непонятно кого: пусть ответят на сообщение этого человека или упомянут его")
		case !slices.ContainsFunc(leads, func(l storage.Person) bool { return samePerson(l, p) }):
			return refuse("снять админа чата, но %s и так не админ", p.Label())
		case !admin && !samePerson(p, storage.Person{ID: m.UserID, Username: m.Username}):
			return refuse("снять другого админа чата, но это может только твой создатель; админ может снять только себя")
		case len(leads) == 1:
			return refuse("снять последнего админа чата, но без админа чат остаться не может — его можно только отключить")
		}
		if _, err := b.store.RemoveRole(ctx, m.ChatID, p, storage.RoleLead); err != nil {
			return fail(cmd.Command, err)
		}
		return commandResult{done: true}

	case "set_mode":
		mode := strings.ToLower(strings.TrimSpace(cmd.Mode))
		switch {
		case author < rankLead:
			return refuse("сменить режим (молчать или говорить), но это решают только админы чата")
		case cmd.Scope == "topic" && mem.roster.tagged(m.ThreadID):
			// The model took "не пиши в этой ветке" for the mode (live test, 2026-11-09): the mode is the
			// whole group's, a topic of it is an instruction.
			return refuse("помолчать в этом топике, но молчать ты умеешь только во всей группе сразу; про топик пусть скажут как правило: «не пиши в этом топике»")
		case !storage.ValidMode(mode):
			return refuse("сменить режим, но непонятно какой: тишина, только реакции или общение")
		}
		if _, err := b.store.SetChatState(ctx, m.ChatID, storage.ChatActive, mode); err != nil {
			return fail(cmd.Command, err)
		}
		b.dropChat(m.ChatID)
		slog.Info("group mode changed", "chat_id", m.ChatID, "mode", mode, "by", m.UserID)
		return commandResult{done: true, mode: mode}

	case "add_authority", "remove_authority":
		if author < rankLead {
			return refuse("назначить или снять авторитета, но это может только админ чата")
		}
		p, ok := person()
		if !ok {
			return refuse("изменить список авторитетов, но непонятно, о ком речь: пусть ответят на сообщение этого человека или упомянут его")
		}
		var err error
		if cmd.Command == "add_authority" {
			if mem.roster.rank(p.ID, p.Username) == rankNone {
				_, err = b.store.AddAuthority(ctx, m.ChatID, p, m.UserID, now)
			}
		} else {
			_, err = b.store.RemoveRole(ctx, m.ChatID, p, storage.RoleAuthority)
		}
		if err != nil {
			return fail(cmd.Command, err)
		}
		return commandResult{done: true}

	case "remember", "instruct":
		text := strings.TrimSpace(cmd.Text)
		if text == "" {
			return refuse("что-то запомнить, но непонятно что")
		}
		kind := storage.LoreFact
		if cmd.Command == "instruct" {
			kind = storage.LoreInstruction
		}
		thread, chatWide := m.ThreadID, cmd.Scope != "topic" || !mem.roster.tagged(m.ThreadID)
		if cmd.Topic == generalTopic && mem.roster.forum() {
			cmd.Topic = 0
			if m.ThreadID != 0 {
				thread, chatWide = 0, false // the general topic, named from another one or a private chat
			}
		}
		if cmd.Topic != 0 && cmd.Topic != m.ThreadID {
			// Another topic named by its name ("в Флуде не пиши"): only a topic the bot knows the name of.
			// The message's own topic is the scope's business.
			if _, ok := mem.roster.topics[cmd.Topic]; !ok {
				return refuse("запомнить что-то для другого топика, но ты не знаешь, какой топик имеют в виду: " +
					"пусть скажут это в самом топике или назовут его точнее")
			}
			thread, chatWide = cmd.Topic, false
		}
		_, err := b.store.AddLore(ctx, storage.Lore{
			ChatID: m.ChatID, ChatWide: chatWide, ThreadID: thread,
			Kind: kind, NoReply: cmd.Command == "instruct" && cmd.NoReply, Text: text, AuthorID: m.UserID, AuthorRole: author.role(), SourceMsg: m.MessageID, CreatedAt: now,
		})
		if err != nil {
			return fail(cmd.Command, err)
		}
		// The new entry replaces the ones it corrects (same permissions as forget).
		if _, _, err := b.forget(ctx, m, author, cmd.IDs, mem.lore); err != nil {
			return fail(cmd.Command, err)
		}
		return commandResult{done: true}

	case "forget":
		deleted, foreign, err := b.forget(ctx, m, author, cmd.IDs, mem.lore)
		switch {
		case err != nil:
			return fail(cmd.Command, err)
		case deleted > 0:
			return commandResult{done: true}
		case foreign > 0:
			return refuse("забыть чужие записи, но авторитет может отменять только свои; чужие — только админ чата")
		}
		return refuse("что-то забыть, но ты не нашёл такой записи в памяти")

	case "list":
		sum, err := b.store.Summary(ctx, m.ChatID, m.ThreadID)
		if err != nil {
			return fail(cmd.Command, err)
		}
		return commandResult{done: true, text: listMemory(mem, m.ThreadID, sum.Text, sum.Tone)}

	case "about", "forget_person":
		if author < rankLead {
			return refuse("рассказать или забыть, что ты думаешь о человеке, но это ты обсуждаешь только с админом чата")
		}
		p, ok := person()
		if !ok {
			return refuse("рассказать или забыть, что ты думаешь о человеке, но непонятно о ком: пусть ответят на его сообщение или упомянут его")
		}
		if p.ID == 0 {
			if cmd.Command == "about" {
				return commandResult{done: true, text: noteText(p, storage.Note{}, false)}
			}
			return refuse("забыть, что ты думаешь о %s, но заметок о нём у тебя и так нет", p.Label())
		}
		if cmd.Command == "about" {
			ns, err := b.store.Notes(ctx, m.ChatID, []int64{p.ID})
			if err != nil {
				return fail(cmd.Command, err)
			}
			n, ok := ns[p.ID]
			return commandResult{done: true, text: noteText(p, n, ok)}
		}
		ok, err := b.store.ClearNote(ctx, m.ChatID, p.ID)
		switch {
		case err != nil:
			return fail(cmd.Command, err)
		case !ok:
			return refuse("забыть, что ты думаешь о %s, но заметок о нём у тебя и так нет", p.Label())
		}
		return commandResult{done: true}

	case "forget_tone":
		if author < rankLead {
			return refuse("забыть, как принято общаться в этом топике, но это может только админ чата")
		}
		ok, err := b.store.ClearTone(ctx, m.ChatID, m.ThreadID)
		switch {
		case err != nil:
			return fail(cmd.Command, err)
		case !ok:
			return refuse("забыть, как тут принято общаться, но ты и так этого не знаешь")
		}
		return commandResult{done: true}

	case "set_persona", "reset_persona":
		if author < rankLead {
			return refuse("поменять свою личность, но это решают только админы чата")
		}
		g, _, err := b.chat(ctx, m.ChatID)
		if err != nil {
			return fail(cmd.Command, err)
		}
		var replied *storage.Message
		if m.ReplyTo != 0 {
			replied, _ = b.store.Get(ctx, m.ChatID, m.ReplyTo)
		}
		return b.proposePersona(ctx, m, g, mem, cmd.Command == "reset_persona", replied)

	case "forget_summary":
		if author < rankLead {
			return refuse("стереть сводку старой переписки, но это может только админ чата")
		}
		ok, err := b.store.ClearSummary(ctx, m.ChatID, m.ThreadID)
		switch {
		case err != nil:
			return fail(cmd.Command, err)
		case !ok:
			return refuse("забыть, о чём тут говорили раньше, но сводки старой переписки у тебя и так нет")
		}
		return commandResult{done: true}
	}
	return refuse("непонятное служебное указание")
}

// forget deletes the chat's entries by id. An authority may delete only their own entries and those
// the bot inferred itself (others are counted as foreign); unknown ids are skipped.
func (b *Brain) forget(ctx context.Context, m storage.Message, author rank, ids []int64, lore []storage.Lore) (deleted, foreign int, err error) {
	for _, id := range ids {
		i := slices.IndexFunc(lore, func(l storage.Lore) bool { return l.ID == id })
		if i < 0 {
			continue
		}
		if author == rankAuthority && lore[i].AuthorID != m.UserID && !lore[i].Inferred() {
			foreign++
			continue
		}
		ok, err := b.store.DeleteLore(ctx, m.ChatID, id)
		if err != nil {
			return deleted, foreign, err
		}
		if ok {
			deleted++
		}
	}
	return deleted, foreign, nil
}

// ruleHitTTL: a reply required by an instruction answers the same author at most once per this
// period, so a thread of congratulations does not turn into a stream of identical bot messages.
const ruleHitTTL = 24 * time.Hour

type ruleHit struct {
	rule   int64
	author int64
}

// mentions resolves the @usernames in the message text (by the chat history; unknown ones by name only).
func (b *Brain) mentions(ctx context.Context, m storage.Message) []storage.Person {
	var out []storage.Person
	for _, sm := range usernameRe.FindAllStringSubmatch(m.Text, -1) {
		p, ok, err := b.store.FindByUsername(ctx, m.ChatID, sm[1])
		if err != nil {
			slog.Error("find user by username", "err", err)
		}
		if !ok {
			p = storage.Person{Username: sm[1]}
		}
		out = append(out, p)
	}
	return out
}

// forgetOldRuleHits drops instruction reply hits older than ruleHitTTL.
func (b *Brain) forgetOldRuleHits() {
	b.mu.Lock()
	defer b.mu.Unlock()
	maps.DeleteFunc(b.ruleHits, func(_ ruleHit, at time.Time) bool { return b.now().Sub(at) >= ruleHitTTL })
}

// listMemory renders who is in charge and what the bot remembers (as seen from the topic), and the
// topic's summary and tone.
func listMemory(mem chatMemory, thread int, summary, tone string) string {
	var s strings.Builder
	switch leads := mem.roster.leads(); len(leads) {
	case 0:
		s.WriteString("Админ не назначен.\n")
	case 1:
		fmt.Fprintf(&s, "Админ: %s\n", labels(leads))
	default:
		fmt.Fprintf(&s, "Админы: %s\n", labels(leads))
	}
	if auth := mem.roster.authorities(); len(auth) > 0 {
		fmt.Fprintf(&s, "Авторитеты: %s\n", labels(auth))
	}
	var lines []string
	for _, l := range mem.lore {
		if l.ChatWide || l.ThreadID == thread {
			lines = append(lines, fmt.Sprintf("%d. [%s] %s%s", l.ID, scopeLabel(l, thread, mem.roster), inferredMark(l), l.Text))
		}
	}
	if len(lines) == 0 {
		s.WriteString("Ничего не просили запомнить.")
	} else {
		s.WriteString("Помню:\n")
		s.WriteString(strings.Join(lines, "\n"))
	}
	if summary != "" {
		s.WriteString("\n\nО чём здесь говорили раньше (сводка):\n")
		s.WriteString(summary)
	}
	if tone != "" {
		s.WriteString("\n\nКак здесь принято общаться (сам заметил):\n")
		s.WriteString(tone)
	}
	return s.String()
}

// inferredMark marks the entries the bot inferred itself in lists.
func inferredMark(l storage.Lore) string {
	if l.Inferred() {
		return "(сам заметил) "
	}
	return ""
}

func scopeLabel(l storage.Lore, thread int, r roster) string {
	switch {
	case l.ChatWide:
		return "весь чат"
	case l.ThreadID == thread:
		return "этот топик"
	}
	return r.topic(l.ThreadID)
}
