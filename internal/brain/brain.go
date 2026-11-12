// Package brain decides when and how the bot participates in a chat.
package brain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// Incoming is a chat message as seen by the brain (already converted from Telegram types).
type Incoming struct {
	PhotoID       string
	ReplyPhotoID  string
	ChatID        int64
	ChatType      string // "group", "supergroup", "private", "channel"
	ChatTitle     string // the group's title (empty in a private chat)
	MessageID     int
	ThreadID      int // forum topic id; 0 if none
	UserID        int64
	UserName      string
	Username      string
	Text          string
	IsMedia       bool // sticker / photo / voice etc. (Text holds a placeholder)
	HasCaption    bool // media with a user-written caption (appended to the placeholder)
	ReplyToID     int
	ReplyToUserID int64
	ReplyToUser   storage.Person   // author of the replied-to message (ID == ReplyToUserID)
	TextMentions  []storage.Person // users mentioned via text_mention entities (no @username)
	Date          time.Time
	Kind          string // "" or storage.KindJoin / KindLeave: then the user is who joined or left
	TopicName     string // the topic's name, when Telegram told it with the message (ThreadID != 0)
	IsForum       bool   // the group has topics: a message with ThreadID 0 is in the general topic
}

// Topic is a forum topic created or renamed: Telegram tells names only this way (SPEC 4.6).
type Topic struct {
	ChatID   int64
	ChatType string
	ThreadID int // 0 — the general topic
	Name     string
}

// Reaction is a participant's whole new set of reactions to a message (empty: all removed).
type Reaction struct {
	ChatID    int64
	ChatType  string
	MessageID int
	UserID    int64
	Emojis    []string
	Date      time.Time
}

// Button is a button under a message in a private chat; Data comes back in the Callback.
type Button struct {
	Text string
	Data string
}

// Callback is a press of a button under the bot's message (Telegram callback_query).
type Callback struct {
	ID        string
	UserID    int64 // who pressed
	Username  string
	ChatID    int64 // where the message with the button is
	MessageID int
	Data      string
}

type Identity struct {
	ID       int64
	Username string
	Name     string
}

type Sender interface {
	Send(ctx context.Context, chatID int64, threadID int, text string, replyTo int) (int, error)
	React(ctx context.Context, chatID int64, msgID int, emoji string) error
	Typing(ctx context.Context, chatID int64, threadID int) error
	Admins(ctx context.Context, chatID int64) ([]storage.Person, error) // the chat's administrators in Telegram
	SendButtons(ctx context.Context, chatID int64, text string, rows [][]Button) (int, error)
	Edit(ctx context.Context, chatID int64, msgID int, text string) error // the buttons go
	AnswerCallback(ctx context.Context, id, text string) error
	Member(ctx context.Context, chatID, userID int64) (storage.Person, bool, error) // who they are; whether a member now
}

type Completer interface {
	Available(ctx context.Context, role llm.Role, prio llm.Priority) bool
	Complete(ctx context.Context, role llm.Role, prio llm.Priority, msgs []llm.Message) (string, string, error)
}

type Store interface {
	SaveMessage(ctx context.Context, m storage.Message) error
	SaveDM(ctx context.Context, m storage.DM) error
	RecentDMs(ctx context.Context, userID int64, limit int) ([]storage.DM, error)
	SelectedChat(ctx context.Context, userID int64) (int64, bool, error)
	SelectChat(ctx context.Context, userID, chatID int64) error
	LeadChats(ctx context.Context, userID int64, username string) ([]int64, error)
	FindPerson(ctx context.Context, username string) (storage.Person, bool, error)
	Get(ctx context.Context, chatID int64, msgID int) (*storage.Message, error)
	Fresh(ctx context.Context, chatID int64, afterID, limit int) ([]storage.Message, error)
	Before(ctx context.Context, chatID int64, beforeID, limit int) ([]storage.Message, error)
	PendingMentions(ctx context.Context, since time.Time) ([]storage.Message, error)
	MarkAnsweredUpTo(ctx context.Context, chatID int64, msgID int) error
	ChatState(ctx context.Context, chatID int64) (storage.ChatState, error)
	SetLastEvaluated(ctx context.Context, chatID int64, msgID int) error
	SetLastInitiative(ctx context.Context, chatID int64, t time.Time) error
	Prune(ctx context.Context, before time.Time) error
	RegisterChat(ctx context.Context, chatID int64, title string, addedBy int64, at time.Time) error
	Chat(ctx context.Context, chatID int64) (storage.Chat, bool, error)
	Chats(ctx context.Context) ([]storage.Chat, error)
	SetChatState(ctx context.Context, chatID int64, state, mode string) (bool, error)
	ResetChat(ctx context.Context, chatID int64) error
	ForgetChat(ctx context.Context, chatID int64) error
	MigrateChat(ctx context.Context, from, to int64) error
	AddWantedAdmin(ctx context.Context, w storage.WantedAdmin) error
	WantedAdmins(ctx context.Context, chatID int64) ([]storage.WantedAdmin, error)
	ClearWantedAdmins(ctx context.Context, chatID int64) error
	MarkHandled(ctx context.Context, chatID int64, msgID int) error
	InDialog(ctx context.Context, chatID int64, threadID int, userID int64, since time.Time) (bool, error)
	Thread(ctx context.Context, chatID int64, root, head, tail int) ([]storage.Message, int, error)
	Around(ctx context.Context, chatID int64, topic, root, upTo, limit int) ([]storage.Message, error)
	ActiveThreads(ctx context.Context, chatID int64, topic int, since time.Time, limit int) ([]int, error)
	SetRoot(ctx context.Context, chatID int64, msgID, of int) (bool, error)
	SelfInThread(ctx context.Context, chatID int64, root int) (bool, error)
	SetReactions(ctx context.Context, chatID int64, msgID int, userID int64, emojis []string, at time.Time) error
	TopicActivity(ctx context.Context, chatID int64, threadID, limit int) (storage.TopicActivity, error)

	Roles(ctx context.Context, chatID int64) ([]storage.ChatRole, error)
	AddLead(ctx context.Context, chatID int64, p storage.Person, by int64, at time.Time) error
	AddAuthority(ctx context.Context, chatID int64, p storage.Person, by int64, at time.Time) (bool, error)
	RemoveRole(ctx context.Context, chatID int64, p storage.Person, role string) (bool, error)
	BindUsername(ctx context.Context, chatID, userID int64, username, name string) error
	FindByUsername(ctx context.Context, chatID int64, username string) (storage.Person, bool, error)
	AddLore(ctx context.Context, l storage.Lore) (int64, error)
	UpdateLore(ctx context.Context, l storage.Lore) (bool, error)
	Lore(ctx context.Context, chatID int64) ([]storage.Lore, error)
	DeleteLore(ctx context.Context, chatID, id int64) (bool, error)

	Summary(ctx context.Context, chatID int64, topic int) (storage.Summary, error)
	SetSummary(ctx context.Context, chatID int64, topic int, prev storage.Summary, text string, upTo int, at time.Time) (bool, error)
	ClearSummary(ctx context.Context, chatID int64, topic int) (bool, error)
	SetTone(ctx context.Context, chatID int64, topic int, prev, tone string) (bool, error)
	ClearTone(ctx context.Context, chatID int64, topic int) (bool, error)
	Unsummarized(ctx context.Context, chatID int64, topic, after, keep, limit int, expiring time.Time) ([]storage.Message, error)
	ExpiringTopics(ctx context.Context, before time.Time) ([]storage.TopicRef, error)

	Notes(ctx context.Context, chatID int64, userIDs []int64) (map[int64]storage.Note, error)
	Reputations(ctx context.Context, chatID int64) (map[int64]int, error)
	SetNote(ctx context.Context, n storage.Note, at time.Time) (bool, error)
	ClearNote(ctx context.Context, chatID, userID int64) (bool, error)

	SetTopicName(ctx context.Context, chatID int64, thread int, name string) (bool, error)
	SetForum(ctx context.Context, chatID int64, forum bool) error
	TopicNames(ctx context.Context, chatID int64) (map[int]string, error)

	Persona(ctx context.Context, chatID int64) (string, error)
	SetPersona(ctx context.Context, chatID int64, text string, by int64, at time.Time) error
	AddPersonaDraft(ctx context.Context, d storage.PersonaDraft) (int64, error)
	PersonaDraft(ctx context.Context, chatID, id int64) (storage.PersonaDraft, bool, error)
	DeletePersonaDraft(ctx context.Context, chatID, id int64) error
}

type Brain struct {
	cfg     config.Behavior
	persona string // the default persona: groups without their own (SPEC 5)
	store   Store
	llm     Completer
	out     Sender
	self    Identity
	admins  map[int64]bool
	aliases []string

	locks   sync.Map // chatID -> *sync.Mutex
	mu      sync.Mutex
	pending map[int64]bool       // chats with unevaluated messages
	chats   map[int64]cachedChat // cache of the groups' records (access.go)
	forums  map[int64]bool       // whether a group has topics, as last saved (learnForum)

	candidates map[candidate]storage.Person // people the creator was asked about as a group's admin (pending.go)

	norms    map[topicKey]cachedNorms // learned topic norms (norms.go)
	tgAdmins map[int64]cachedAdmins   // chat administrators from Telegram
	ruleHits map[ruleHit]time.Time    // when a reply required by an instruction last answered an author
	dialogs  map[dialogKey]*dialogRun // dialog evaluations waiting or under way (dialog.go)

	summarizing map[topicKey]bool // topics whose summary is being updated (summary.go)
	notesLocks  sync.Map          // chatID -> *sync.Mutex: one notes update at a time (notes.go)
	loreLocks   sync.Map          // chatID -> *sync.Mutex: one lore update at a time (lore.go)

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)
	rand  func() float64 // [0,1), picks the reply delay
	wg    sync.WaitGroup // background handlers started by OnMessage / tick
}

func New(cfg *config.Config, persona string, store Store, completer Completer, out Sender, self Identity) *Brain {
	admins := map[int64]bool{}
	for _, id := range cfg.Telegram.AdminIDs {
		admins[id] = true
	}
	var aliases []string
	for _, a := range cfg.Telegram.Aliases {
		if a = normalizeWord(strings.TrimSpace(a)); a != "" {
			aliases = append(aliases, a)
		}
	}
	return &Brain{
		cfg: cfg.Behavior, persona: persona, store: store, llm: completer, out: out, self: self,
		admins: admins, aliases: aliases,
		pending: map[int64]bool{}, chats: map[int64]cachedChat{}, candidates: map[candidate]storage.Person{},
		norms: map[topicKey]cachedNorms{}, tgAdmins: map[int64]cachedAdmins{}, ruleHits: map[ruleHit]time.Time{},
		dialogs: map[dialogKey]*dialogRun{}, summarizing: map[topicKey]bool{}, forums: map[int64]bool{},
		now: time.Now, sleep: sleepCtx, rand: rand.Float64,
	}
}

// spawn runs f in a tracked goroutine (see wait).
func (b *Brain) spawn(f func()) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		f()
	}()
}

// wait blocks until all handlers started so far have finished.
func (b *Brain) wait() { b.wg.Wait() }

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (b *Brain) lock(chatID int64) *sync.Mutex {
	m, _ := b.locks.LoadOrStore(chatID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// OnMessage is called for every incoming message. It must return quickly.
func (b *Brain) OnMessage(ctx context.Context, in Incoming) {
	if in.ChatType == "private" {
		b.onPrivate(ctx, in)
		return
	}
	if !isGroup(in.ChatType) {
		return
	}
	c, known, err := b.chat(ctx, in.ChatID)
	switch {
	case err != nil:
		return
	case !known:
		// Added while the bot was down: off, like any new group (SPEC 3.1).
		b.registerChat(ctx, in.ChatID, in.ChatTitle, 0)
		return
	case in.UserID == b.self.ID:
		return // own messages are saved when sent
	case c.State == storage.ChatPending:
		b.watchForAdmin(ctx, c, in)
		return
	case c.State != storage.ChatActive:
		return // an off group: nothing is stored, nothing is done
	}
	talk := c.Mode == storage.ModeTalk
	b.learnForum(ctx, in.ChatID, in.IsForum)
	if in.TopicName != "" && in.ThreadID != 0 {
		b.learnTopic(ctx, in.ChatID, in.ThreadID, in.TopicName)
	}
	if err := b.store.BindUsername(ctx, in.ChatID, in.UserID, in.Username, in.UserName); err != nil {
		slog.Error("bind username to role", "err", err)
	}

	addressed := in.Kind == "" && b.isAddressedInChat(ctx, in)
	m := storage.Message{
		ChatID: in.ChatID, MessageID: in.MessageID, ThreadID: in.ThreadID, UserID: in.UserID,
		UserName: in.UserName, Username: in.Username, Text: in.Text, ReplyTo: in.ReplyToID,
		Date: in.Date, Addressed: addressed, Kind: in.Kind, PhotoID: in.PhotoID,
	}
	if err := b.store.SaveMessage(ctx, m); err != nil {
		slog.Error("save message", "err", err)
		return
	}

	if in.Kind != "" {
		// Service events are history and reaction targets, never instructions. A newcomer is worth
		// a look from the evaluator (instructions like "greet newcomers"); someone leaving is not.
		if in.Kind == storage.KindJoin {
			b.markPending(in.ChatID)
		}
		return
	}

	refs := in.refs(b.self.ID)
	switch {
	case addressed:
		if talk && b.dialogStarted(m) {
			// The bot is about to answer this person directly: the evaluation of what they wrote
			// before waits again, so as not to answer next to the direct reply.
			b.dialog(ctx, m)
		}
		b.spawn(func() { b.handleDirect(context.WithoutCancel(ctx), m, true, refs) })
	case b.admins[in.UserID] && (len(refs) > 0 || usernameRe.MatchString(in.Text)):
		// The admin (the bot's creator) may appoint the lead without addressing the bot: "@vasya тут админ".
		b.spawn(func() { b.handleAdminRemark(context.WithoutCancel(ctx), m, refs) })
	default:
		// Every message people write is evaluated by the model (SPEC 4.3), even a short one or a
		// sticker: instructions like "❤ on my messages" apply to them too. Someone talking with the
		// bot is not kept waiting for the batch (SPEC 7a).
		b.markPending(in.ChatID)
		if talk && (b.dialogStarted(m) || b.inDialogNow(ctx, m)) {
			b.dialog(ctx, m)
		}
	}
}

// OnTopic records the name of a topic created or renamed in an active group.
func (b *Brain) OnTopic(ctx context.Context, t Topic) {
	if !isGroup(t.ChatType) || strings.TrimSpace(t.Name) == "" {
		return
	}
	if c, known, err := b.chat(ctx, t.ChatID); err != nil || !known || c.State != storage.ChatActive {
		return
	}
	b.learnForum(ctx, t.ChatID, true)
	b.learnTopic(ctx, t.ChatID, t.ThreadID, t.Name)
}

// learnForum records whether the group has topics, when that changes (cached: every message tells it).
func (b *Brain) learnForum(ctx context.Context, chatID int64, forum bool) {
	b.mu.Lock()
	was, known := b.forums[chatID]
	b.mu.Unlock()
	if known && was == forum {
		return
	}
	if err := b.store.SetForum(ctx, chatID, forum); err != nil {
		slog.Error("save forum flag", "chat_id", chatID, "err", err)
		return
	}
	b.mu.Lock()
	b.forums[chatID] = forum
	b.mu.Unlock()
	if known {
		slog.Info("group topics", "chat_id", chatID, "forum", forum)
	}
}

func (b *Brain) learnTopic(ctx context.Context, chatID int64, thread int, name string) {
	name = strings.TrimSpace(name)
	changed, err := b.store.SetTopicName(ctx, chatID, thread, name)
	switch {
	case err != nil:
		slog.Error("save topic name", "chat_id", chatID, "topic", thread, "err", err)
	case changed:
		slog.Info("topic name", "chat_id", chatID, "topic", thread, "name", name)
	}
}

// OnReaction stores a participant's reactions (SPEC 4.6), in active groups only.
func (b *Brain) OnReaction(ctx context.Context, r Reaction) {
	if !isGroup(r.ChatType) || r.UserID == b.self.ID {
		return
	}
	if c, known, err := b.chat(ctx, r.ChatID); err != nil || !known || c.State != storage.ChatActive {
		return
	}
	if err := b.store.SetReactions(ctx, r.ChatID, r.MessageID, r.UserID, r.Emojis, r.Date); err != nil {
		slog.Error("save reactions", "chat_id", r.ChatID, "err", err)
		return
	}
	slog.Debug("reactions", "chat_id", r.ChatID, "message_id", r.MessageID, "user_id", r.UserID, "emojis", r.Emojis)
}

func (b *Brain) markPending(chatID int64) {
	b.mu.Lock()
	b.pending[chatID] = true
	b.mu.Unlock()
}

// refs lists people the message points at via Telegram metadata: the replied-to author
// and text mentions. The bot itself is excluded, and so is the author replying to their own
// message (a follow-up like "и его зовут …" is not about the author).
func (in Incoming) refs(self int64) []storage.Person {
	var out []storage.Person
	if in.ReplyToUserID != 0 && in.ReplyToUserID != self && in.ReplyToUserID != in.UserID {
		p := in.ReplyToUser
		p.ID = in.ReplyToUserID
		out = append(out, p)
	}
	for _, p := range in.TextMentions {
		if p.ID != self {
			out = append(out, p)
		}
	}
	return out
}

// handleAdminRemark checks a non-addressed admin message that refers to someone for an
// instruction (e.g. appointing the lead); otherwise it is an ordinary message.
func (b *Brain) handleAdminRemark(ctx context.Context, m storage.Message, refs []storage.Person) {
	l := b.lock(m.ChatID)
	l.Lock()
	defer l.Unlock()
	if b.llm.Available(ctx, llm.RoleCommand, llm.Direct) && b.tryCommand(ctx, m, refs) {
		if err := b.store.MarkHandled(ctx, m.ChatID, m.MessageID); err != nil {
			slog.Error("mark handled", "err", err)
		}
		return
	}
	b.markPending(m.ChatID)
}

func (b *Brain) isAddressed(in Incoming) bool {
	return b.addressedWithPersona(in, b.persona)
}

func (b *Brain) isAddressedInChat(ctx context.Context, in Incoming) bool {
	persona, err := b.store.Persona(ctx, in.ChatID)
	if err != nil {
		slog.Error("load persona for address", "chat_id", in.ChatID, "err", err)
	}
	if persona == "" {
		persona = b.persona
	}
	return b.addressedWithPersona(in, persona)
}

func (b *Brain) addressedWithPersona(in Incoming, persona string) bool {
	if in.ReplyToUserID == b.self.ID {
		return true
	}
	if slices.ContainsFunc(in.TextMentions, func(p storage.Person) bool { return p.ID == b.self.ID }) {
		return true
	}
	lower := strings.ToLower(in.Text)
	if b.self.Username != "" && strings.Contains(lower, "@"+strings.ToLower(b.self.Username)) {
		return true
	}
	aliases := append(slices.Clone(b.aliases), personaNames(persona)...)
	if len(aliases) > 0 {
		for _, w := range strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if slices.Contains(aliases, normalizeWord(w)) {
				return true
			}
		}
	}
	return false
}

// normalizeWord folds spelling slips that don't change the name: case, ё→е and a trailing soft or
// hard sign ("Зверычь" is still "зверыч"). Case endings are left alone on purpose: for a short
// alias like "бот" they would turn ordinary words ("боты") into addresses.
func normalizeWord(w string) string {
	w = strings.ReplaceAll(strings.ToLower(w), "ё", "е")
	return strings.TrimRight(w, "ьъ")
}

// catchUpInterval is how often mentions missed while offline are retried. It does not depend on
// batch_interval_seconds: the batch may be evaluated as rarely as once an hour.
const catchUpInterval = 30 * time.Second

// Run drives periodic work until ctx is cancelled.
func (b *Brain) Run(ctx context.Context) {
	batch := time.NewTicker(time.Duration(b.cfg.BatchIntervalSeconds) * time.Second)
	defer batch.Stop()
	catchUp := time.NewTicker(catchUpInterval)
	defer catchUp.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	b.prune(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-catchUp.C:
			b.catchUp(ctx)
		case <-batch.C:
			b.evaluatePending(ctx)
		case <-prune.C:
			b.prune(ctx)
		}
	}
}

// prune deletes old history in the background: first the messages about to go are folded into the
// summaries (SPEC 4.5), then they are deleted.
func (b *Brain) prune(ctx context.Context) {
	b.spawn(func() {
		b.summarizeExpiring(ctx)
		before := b.now().Add(-time.Duration(b.cfg.RetentionDays) * 24 * time.Hour)
		if err := b.store.Prune(ctx, before); err != nil {
			slog.Error("prune", "err", err)
		}
		b.forgetOldRuleHits()
	})
}

// tick does all periodic work at once (tests): catch-up first, then the evaluation, so that they
// do not compete for the chat lock.
func (b *Brain) tick(ctx context.Context) {
	b.catchUp(ctx)
	b.wait()
	b.evaluatePending(ctx)
}

// catchUp answers mentions missed while offline: the latest fresh one per chat.
func (b *Brain) catchUp(ctx context.Context) {
	if b.llm.Available(ctx, llm.RoleReply, llm.Direct) {
		since := b.now().Add(-time.Duration(b.cfg.StaleAfterMinutes * float64(time.Minute)))
		ms, err := b.store.PendingMentions(ctx, since)
		if err != nil {
			slog.Error("pending mentions", "err", err)
		}
		latest := map[int64]storage.Message{}
		for _, m := range ms {
			latest[m.ChatID] = m // ordered by message_id, so the last one wins
		}
		for _, m := range latest {
			b.spawn(func() { b.handleDirect(ctx, m, false, nil) })
		}
	}
}

// evaluatePending evaluates the new messages of every chat that has some.
func (b *Brain) evaluatePending(ctx context.Context) {
	b.mu.Lock()
	chats := make([]int64, 0, len(b.pending))
	for id := range b.pending {
		chats = append(chats, id)
	}
	b.mu.Unlock()
	for _, id := range chats {
		b.spawn(func() { b.evaluate(ctx, id) })
	}
}

// handleDirect answers a message addressed to the bot (or carries out an instruction in it).
// immediate=true on first arrival (may show the offline reaction), false for catch-up retries.
// refs are the people the message points at (nil on catch-up: restored from storage).
func (b *Brain) handleDirect(ctx context.Context, m storage.Message, immediate bool, refs []storage.Person) {
	l := b.lock(m.ChatID)
	if !l.TryLock() {
		if !immediate {
			return // busy; the next tick will retry
		}
		l.Lock()
	}
	defer l.Unlock()

	if cur, err := b.store.Get(ctx, m.ChatID, m.MessageID); err != nil || cur == nil || cur.Answered {
		return
	}
	if b.mode(ctx, m.ChatID) != storage.ModeTalk {
		b.handleQuietly(ctx, m, refs)
		return
	}
	if !b.llm.Available(ctx, llm.RoleReply, llm.Direct) {
		if immediate {
			b.offlineReact(ctx, m)
		}
		return
	}

	if b.tryCommand(ctx, m, refs) {
		if err := b.store.MarkAnsweredUpTo(ctx, m.ChatID, m.MessageID); err != nil {
			slog.Error("mark answered", "err", err)
		}
		return
	}

	if b.repliesForbidden(ctx, m) {
		if err := b.store.MarkAnsweredUpTo(ctx, m.ChatID, m.MessageID); err != nil {
			slog.Error("mark answered", "err", err)
		}
		return
	}
	b.assignThread(ctx, m)
	text, err := b.generate(ctx, m, "", llm.Direct)
	if errors.Is(err, llm.ErrUnavailable) {
		if immediate {
			b.offlineReact(ctx, m)
		}
		return
	}
	if err != nil {
		slog.Error("generate direct reply", "chat_id", m.ChatID, "err", err)
		return
	}
	if b.send(ctx, m, text) {
		if err := b.store.MarkAnsweredUpTo(ctx, m.ChatID, m.MessageID); err != nil {
			slog.Error("mark answered", "err", err)
		}
	}
}

// handleQuietly handles a message addressed to the bot in a silent group (SPEC 4.1a): an
// instruction of the leadership is carried out, people get no answer. The message is marked
// answered, so the bot does not answer it once it may talk again.
func (b *Brain) handleQuietly(ctx context.Context, m storage.Message, refs []storage.Person) {
	if b.llm.Available(ctx, llm.RoleCommand, llm.Direct) {
		b.tryCommand(ctx, m, refs)
	}
	if err := b.store.MarkAnsweredUpTo(ctx, m.ChatID, m.MessageID); err != nil {
		slog.Error("mark answered", "err", err)
	}
}

func (b *Brain) offlineReact(ctx context.Context, m storage.Message) {
	if b.cfg.OfflineReaction == "" {
		return
	}
	slog.Info("offline: reacting instead of replying", "chat_id", m.ChatID, "message_id", m.MessageID)
	if err := b.out.React(ctx, m.ChatID, m.MessageID, b.cfg.OfflineReaction); err != nil {
		slog.Warn("offline reaction failed", "err", err)
	}
}

func sortedIDs(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func ownStreak(history []storage.Message) int {
	n := 0
	for i := len(history) - 1; i >= 0 && history[i].IsSelf; i-- {
		n++
	}
	return n
}

// generate produces reply text while showing "typing…" and waiting a human-like delay.
func (b *Brain) generate(ctx context.Context, target storage.Message, reason string, prio llm.Priority) (string, error) {
	stopTyping := b.keepTyping(ctx, target.ChatID, target.ThreadID)
	defer stopTyping()

	start := b.now()
	rc, root, err := b.replyContext(ctx, target)
	if err != nil {
		return "", fmt.Errorf("load reply context: %w", err)
	}
	// One session per thread: the provider that has its start cached serves it again (SPEC 7a).
	// A message in no thread starts one with the bot's reply.
	ctx = llm.WithSession(ctx, fmt.Sprintf("%d:%d", target.ChatID, cmp.Or(root, target.MessageID)))
	mem := b.memory(ctx, target.ChatID, target.ThreadID)
	b.withNotes(ctx, &mem, target.ChatID, b.replyPeople(ctx, target, rc), maxNotesInReply)
	b.selectLore(ctx, &mem, prio, replyPrompt(b.personaOf(mem), mem, rc, target, reason))
	msgs := replyPrompt(b.personaOf(mem), mem, rc, target, reason)
	msgs = b.withPhoto(ctx, msgs, target.PhotoID, target.ChatID, target.MessageID)
	if target.PhotoID == "" && target.ReplyTo != 0 {
		if parent, e := b.store.Get(ctx, target.ChatID, target.ReplyTo); e == nil && parent != nil {
			msgs = b.withPhoto(ctx, msgs, parent.PhotoID, parent.ChatID, parent.MessageID)
		}
	}
	raw, provider, err := b.llm.Complete(ctx, llm.RoleReply, prio, msgs)
	if err != nil {
		return "", err
	}
	text := cleanReply(raw)
	if text == "" {
		return "", errors.New("empty reply after cleanup")
	}
	slog.Debug("reply generated", "provider", provider, "chars", len([]rune(text)))

	b.sleep(ctx, b.replyDelay()-b.now().Sub(start))
	if err := ctx.Err(); err != nil {
		return "", err // cancelled while "typing"
	}
	return text, nil
}

// replyContext is the conversation a reply is written in (SPEC 7a).
type replyContext struct {
	thread threadView        // the target's thread; no messages — the target is in none
	around []storage.Message // the topic's last messages outside the thread (all of them, if no thread)
}

// replyContext loads the target's thread and what goes on around it, and returns the thread's root.
func (b *Brain) replyContext(ctx context.Context, target storage.Message) (replyContext, int, error) {
	var rc replyContext
	root := target.RootID
	if cur, err := b.store.Get(ctx, target.ChatID, target.MessageID); err != nil {
		return rc, 0, err
	} else if cur != nil {
		root = cur.RootID // the caller's copy may predate the thread (it is assigned on save)
	}
	if root == 0 {
		var err error
		rc.around, err = b.store.Around(ctx, target.ChatID, target.ThreadID, 0, target.MessageID, b.cfg.ContextMessages)
		return rc, 0, err
	}
	var err error
	if rc.thread, err = b.loadThread(ctx, target.ChatID, root, b.cfg.ContextMessages); err != nil {
		return rc, 0, err
	}
	rc.around, err = b.store.Around(ctx, target.ChatID, target.ThreadID, root, target.MessageID, b.cfg.AroundMessages)
	return rc, root, err
}

// replyDelay is a random human-like pause before a reply (reply_delay_*_seconds).
func (b *Brain) replyDelay() time.Duration {
	delay := b.cfg.ReplyDelayMinSeconds + b.rand()*(b.cfg.ReplyDelayMaxSeconds-b.cfg.ReplyDelayMinSeconds)
	return time.Duration(delay * float64(time.Second))
}

func (b *Brain) keepTyping(ctx context.Context, chatID int64, threadID int) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(4 * time.Second) // the indicator lasts ~5s
		defer t.Stop()
		for {
			if err := b.out.Typing(ctx, chatID, threadID); err != nil && ctx.Err() == nil {
				slog.Debug("typing failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}

// send posts the reply as a quote of the target and stores it. The target is always quoted,
// even when it is the latest message: in a busy chat a plain message loses its context.
func (b *Brain) repliesForbidden(ctx context.Context, target storage.Message) bool {
	lore, err := b.store.Lore(ctx, target.ChatID)
	if err != nil {
		slog.Error("check reply prohibition", "err", err)
		return true
	}
	return slices.ContainsFunc(lore, func(l storage.Lore) bool {
		return l.Kind == storage.LoreInstruction && l.NoReply && !l.Inferred() &&
			(l.ChatWide || l.ThreadID == target.ThreadID)
	})
}

func (b *Brain) send(ctx context.Context, target storage.Message, text string) bool {
	if b.repliesForbidden(ctx, target) {
		return false
	}
	replyTo := target.MessageID
	for _, part := range splitReply(text) {
		id, err := b.out.Send(ctx, target.ChatID, target.ThreadID, part, replyTo)
		if err != nil {
			slog.Error("send message", "chat_id", target.ChatID, "err", err)
			return false
		}
		slog.Info("sent message", "chat_id", target.ChatID, "message_id", id, "reply_to", replyTo)
		err = b.store.SaveMessage(ctx, storage.Message{
			ChatID: target.ChatID, MessageID: id, ThreadID: target.ThreadID, UserID: b.self.ID,
			UserName: b.self.Name, Username: b.self.Username, Text: part, ReplyTo: replyTo,
			Date: b.now(), IsSelf: true,
		})
		if err != nil {
			slog.Error("save own message", "err", err)
		}
	}

	return true
}
