// Package tg adapts github.com/go-telegram/bot to the brain's Incoming/Sender types.
package tg

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/vvchaschin/communitybot/internal/brain"
	"github.com/vvchaschin/communitybot/internal/storage"
)

type Adapter struct {
	b *bot.Bot
}

// Handlers receives converted updates. Nil fields are ignored.
type Handlers struct {
	Message    func(context.Context, brain.Incoming)
	Membership func(context.Context, brain.Membership)
	Migrate    func(ctx context.Context, from, to int64)
	Reaction   func(context.Context, brain.Reaction)
	Callback   func(context.Context, brain.Callback)
	Topic      func(context.Context, brain.Topic)
}

// New creates the Telegram client.
// Updates are handled one at a time, in order: "bot added" must be processed before the
// first message from that chat. Handlers must therefore return quickly.
// Extra options (e.g. bot.WithServerURL in tests) are applied after the defaults.
func New(token string, h Handlers, extra ...bot.Option) (*Adapter, error) {
	opts := []bot.Option{
		// message_reaction arrives only when the bot is a chat administrator (SPEC 4.6); callback_query
		// is a press of a button in a private chat (SPEC 3.2).
		bot.WithAllowedUpdates(bot.AllowedUpdates{models.AllowedUpdateMessage, models.AllowedUpdateMyChatMember,
			models.AllowedUpdateMessageReaction, models.AllowedUpdateCallbackQuery}),
		bot.WithNotAsyncHandlers(),
		bot.WithWorkers(1),
		bot.WithErrorsHandler(func(err error) { slog.Warn("telegram", "err", err) }),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, u *models.Update) { h.dispatch(ctx, u) }),
	}
	b, err := bot.New(token, append(opts, extra...)...)
	if err != nil {
		return nil, err
	}
	return &Adapter{b: b}, nil
}

func (h Handlers) dispatch(ctx context.Context, u *models.Update) {
	switch m := u.Message; {
	case u.MyChatMember != nil:
		warnIfNotAdmin(u.MyChatMember)
		if ev, ok := convertMembership(u.MyChatMember); ok && h.Membership != nil {
			h.Membership(ctx, ev)
		}
	case u.CallbackQuery != nil:
		if cb, ok := convertCallback(u.CallbackQuery); ok && h.Callback != nil {
			h.Callback(ctx, cb)
		}
	case u.MessageReaction != nil:
		if r, ok := convertReaction(u.MessageReaction); ok && h.Reaction != nil {
			h.Reaction(ctx, r)
		}
	case m != nil && m.MigrateToChatID != 0:
		if h.Migrate != nil {
			h.Migrate(ctx, m.Chat.ID, m.MigrateToChatID)
		}
	case m != nil && m.MigrateFromChatID != 0:
		if h.Migrate != nil {
			h.Migrate(ctx, m.MigrateFromChatID, m.Chat.ID)
		}
	case m != nil && (m.ForumTopicCreated != nil || m.ForumTopicEdited != nil):
		if t, ok := convertTopic(m); ok && h.Topic != nil {
			h.Topic(ctx, t)
		}
	default:
		if in, ok := convert(u); ok && h.Message != nil {
			h.Message(ctx, in)
		}
	}
}

func isMember(cm models.ChatMember) bool {
	switch cm.Type {
	case models.ChatMemberTypeOwner, models.ChatMemberTypeAdministrator, models.ChatMemberTypeMember:
		return true
	case models.ChatMemberTypeRestricted:
		return cm.Restricted != nil && cm.Restricted.IsMember
	}
	return false
}

// warnIfNotAdmin logs that participants' reactions will not be seen: Telegram sends them to
// administrators only.
func warnIfNotAdmin(u *models.ChatMemberUpdated) {
	if t := u.NewChatMember.Type; isMember(u.NewChatMember) && t != models.ChatMemberTypeAdministrator && t != models.ChatMemberTypeOwner {
		slog.Warn("the bot is not an administrator of the chat: participants' reactions will not be delivered",
			"chat_id", u.Chat.ID, "title", u.Chat.Title)
	}
}

// convertReaction turns a user's new set of reactions into emojis. Anonymous reactions (on behalf
// of a chat) are skipped; custom emojis become a generic mark, paid ones — ⭐.
func convertReaction(u *models.MessageReactionUpdated) (brain.Reaction, bool) {
	if u.User == nil || u.User.IsBot {
		return brain.Reaction{}, false
	}
	r := brain.Reaction{
		ChatID: u.Chat.ID, ChatType: string(u.Chat.Type), MessageID: u.MessageID, UserID: u.User.ID,
		Date: time.Unix(int64(u.Date), 0), Emojis: []string{},
	}
	for _, t := range u.NewReaction {
		switch {
		case t.ReactionTypeEmoji != nil:
			r.Emojis = append(r.Emojis, t.ReactionTypeEmoji.Emoji)
		case t.ReactionTypeCustomEmoji != nil:
			r.Emojis = append(r.Emojis, "[свой эмодзи]")
		case t.ReactionTypePaid != nil:
			r.Emojis = append(r.Emojis, "⭐")
		}
	}
	return r, true
}

// convertCallback turns a press of a button under a message into the brain's type.
func convertCallback(q *models.CallbackQuery) (brain.Callback, bool) {
	m := q.Message.Message
	if m == nil {
		return brain.Callback{}, false // the message is too old or inaccessible
	}
	return brain.Callback{ID: q.ID, UserID: q.From.ID, Username: q.From.Username, ChatID: m.Chat.ID, MessageID: m.ID, Data: q.Data}, true
}

// convertMembership reports joins and removals of the bot; other status changes are ignored.
func convertMembership(u *models.ChatMemberUpdated) (brain.Membership, bool) {
	was, is := isMember(u.OldChatMember), isMember(u.NewChatMember)
	if was == is {
		return brain.Membership{}, false // e.g. promoted to admin
	}
	name := strings.TrimSpace(u.From.FirstName + " " + u.From.LastName)
	if u.From.Username != "" {
		name += " (@" + u.From.Username + ")"
	}
	return brain.Membership{
		ChatID: u.Chat.ID, ChatType: string(u.Chat.Type), ChatTitle: u.Chat.Title,
		ByUserID: u.From.ID, ByUserName: name, Joined: is, Removed: was,
	}, true
}

// Start blocks, long-polling for updates until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context) { a.b.Start(ctx) }

func (a *Adapter) Me(ctx context.Context) (*models.User, error) { return a.b.GetMe(ctx) }

func convert(u *models.Update) (brain.Incoming, bool) {
	m := u.Message
	if m == nil || m.From == nil {
		return brain.Incoming{}, false
	}
	if in, ok := convertService(m); ok {
		return in, true
	}
	text, isMedia := messageText(m)
	if text == "" {
		return brain.Incoming{}, false // other service messages (pin, topic edits…)
	}
	in := brain.Incoming{
		ChatID:     m.Chat.ID,
		ChatType:   string(m.Chat.Type),
		ChatTitle:  m.Chat.Title,
		MessageID:  m.ID,
		UserID:     m.From.ID,
		UserName:   strings.TrimSpace(m.From.FirstName + " " + m.From.LastName),
		Username:   m.From.Username,
		Text:       text,
		IsMedia:    isMedia,
		HasCaption: m.Caption != "", // only media messages carry captions
		Date:       time.Unix(int64(m.Date), 0),
		IsForum:    m.Chat.IsForum,
	}
	if len(m.Photo) > 0 {
		in.PhotoID = m.Photo[len(m.Photo)-1].FileID
	}
	if m.ReplyToMessage != nil && len(m.ReplyToMessage.Photo) > 0 {
		in.ReplyPhotoID = m.ReplyToMessage.Photo[len(m.ReplyToMessage.Photo)-1].FileID
	}
	if m.IsTopicMessage {
		in.ThreadID = m.MessageThreadID
		if r := m.ReplyToMessage; r != nil && r.ID == m.MessageThreadID && r.ForumTopicCreated != nil {
			in.TopicName = r.ForumTopicCreated.Name // the Bot API has no other way to learn it
		}
	}
	// In a forum topic a message without a reply still points at the topic's creation message:
	// that is not a reply (SPEC 7a).
	if r := m.ReplyToMessage; r != nil && !(m.IsTopicMessage && r.ID == m.MessageThreadID) {
		in.ReplyToID = r.ID
		switch {
		case len(r.NewChatMembers) > 0:
			// "X added Y" / "Y joined": the reply is about the newcomer, not whoever added them.
			in.ReplyToUserID = r.NewChatMembers[0].ID
			in.ReplyToUser = person(&r.NewChatMembers[0])
		case r.From != nil:
			in.ReplyToUserID = r.From.ID
			in.ReplyToUser = person(r.From)
		}
	}
	for _, e := range append(m.Entities, m.CaptionEntities...) {
		if e.Type == models.MessageEntityTypeTextMention && e.User != nil {
			in.TextMentions = append(in.TextMentions, person(e.User))
		}
	}
	return in, true
}

// convertTopic reports a topic created or renamed. A topic is renamed in General without a thread id.
func convertTopic(m *models.Message) (brain.Topic, bool) {
	t := brain.Topic{ChatID: m.Chat.ID, ChatType: string(m.Chat.Type)}
	if m.IsTopicMessage {
		t.ThreadID = m.MessageThreadID
	}
	switch {
	case m.ForumTopicCreated != nil:
		t.Name = m.ForumTopicCreated.Name
	case m.ForumTopicEdited != nil:
		t.Name = m.ForumTopicEdited.Name // empty if only the icon changed
	}
	return t, t.Name != ""
}

// convertService turns "joined" / "left" service messages into events about the person who joined
// or left (SPEC 4.6). Bots are not counted: the bot's own membership comes via my_chat_member.
func convertService(m *models.Message) (brain.Incoming, bool) {
	var who []models.User
	kind := storage.KindJoin
	switch {
	case len(m.NewChatMembers) > 0:
		for _, u := range m.NewChatMembers {
			if !u.IsBot {
				who = append(who, u)
			}
		}
	case m.LeftChatMember != nil && !m.LeftChatMember.IsBot:
		who, kind = []models.User{*m.LeftChatMember}, storage.KindLeave
	}
	if len(who) == 0 {
		return brain.Incoming{}, false
	}
	names := make([]string, len(who))
	for i := range who {
		names[i] = person(&who[i]).Label()
	}
	by := ""
	if m.From.ID != who[0].ID {
		by = person(m.From).Label()
	}
	var text string
	switch {
	case kind == storage.KindLeave && by == "":
		text = "[вышел(а) из чата]"
	case kind == storage.KindLeave:
		text = "[удалён(а) из чата, удалил(а) " + by + "]"
	case len(who) == 1 && by == "":
		text = "[вступил(а) в чат]"
	case by == "":
		text = "[вступили в чат: " + strings.Join(names, ", ") + "]"
	default:
		text = "[" + by + " добавил(а) в чат: " + strings.Join(names, ", ") + "]"
	}
	p := person(&who[0])
	in := brain.Incoming{
		ChatID: m.Chat.ID, ChatType: string(m.Chat.Type), ChatTitle: m.Chat.Title, MessageID: m.ID,
		UserID: p.ID, UserName: p.Name, Username: p.Username, Text: text, Kind: kind,
		Date: time.Unix(int64(m.Date), 0), IsForum: m.Chat.IsForum,
	}
	if m.IsTopicMessage {
		in.ThreadID = m.MessageThreadID
	}
	return in, true
}

func person(u *models.User) storage.Person {
	return storage.Person{ID: u.ID, Name: strings.TrimSpace(u.FirstName + " " + u.LastName), Username: u.Username}
}

// messageText returns text for the history; media gets a short placeholder.
func messageText(m *models.Message) (string, bool) {
	if m.Text != "" {
		return m.Text, false
	}
	var kind string
	switch {
	case m.Sticker != nil:
		return "[стикер " + m.Sticker.Emoji + "]", true
	case len(m.Photo) > 0:
		kind = "[фото]"
	case m.Video != nil:
		kind = "[видео]"
	case m.Animation != nil:
		kind = "[гифка]"
	case m.Voice != nil:
		kind = "[голосовое]"
	case m.VideoNote != nil:
		kind = "[кружок]"
	case m.Document != nil:
		kind = "[файл]"
	default:
		return "", false
	}
	if m.Caption != "" {
		return kind + " " + m.Caption, true
	}
	return kind, true
}

func (a *Adapter) Send(ctx context.Context, chatID int64, threadID int, text string, replyTo int) (int, error) {
	p := &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            text,
	}
	if replyTo != 0 {
		p.ReplyParameters = &models.ReplyParameters{MessageID: replyTo, AllowSendingWithoutReply: true}
	}
	msg, err := a.b.SendMessage(ctx, p)
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// SendButtons posts a message with buttons under it (rows of them).
func (a *Adapter) SendButtons(ctx context.Context, chatID int64, text string, rows [][]brain.Button) (int, error) {
	kb := make([][]models.InlineKeyboardButton, len(rows))
	for i, row := range rows {
		for _, b := range row {
			kb[i] = append(kb[i], models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
	}
	msg, err := a.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: kb}})
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// Edit replaces the text of the bot's message; the buttons under it go.
func (a *Adapter) Edit(ctx context.Context, chatID int64, msgID int, text string) error {
	_, err := a.b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}})
	return err
}

// AnswerCallback stops the button's spinner, optionally with a short notice.
func (a *Adapter) AnswerCallback(ctx context.Context, id, text string) error {
	_, err := a.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text})
	return err
}

// Member reports whether the user is a member of the chat now, and who they are.
func (a *Adapter) Member(ctx context.Context, chatID, userID int64) (storage.Person, bool, error) {
	cm, err := a.b.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return storage.Person{}, false, err
	}
	var u *models.User
	switch {
	case cm.Owner != nil:
		u = cm.Owner.User
	case cm.Administrator != nil:
		u = &cm.Administrator.User
	case cm.Member != nil:
		u = cm.Member.User
	case cm.Restricted != nil:
		u = cm.Restricted.User
	case cm.Left != nil:
		u = cm.Left.User
	case cm.Banned != nil:
		u = cm.Banned.User
	}
	p := storage.Person{ID: userID}
	if u != nil {
		p = person(u)
	}
	return p, isMember(*cm), nil
}

func (a *Adapter) React(ctx context.Context, chatID int64, msgID int, emoji string) error {
	_, err := a.b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: msgID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Type: models.ReactionTypeTypeEmoji, Emoji: emoji},
		}},
	})
	return err
}

func (a *Adapter) Typing(ctx context.Context, chatID int64, threadID int) error {
	_, err := a.b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: chatID, MessageThreadID: threadID, Action: models.ChatActionTyping,
	})
	return err
}

// Admins returns the chat's administrators, the owner included.
func (a *Adapter) Admins(ctx context.Context, chatID int64) ([]storage.Person, error) {
	ms, err := a.b.GetChatAdministrators(ctx, &bot.GetChatAdministratorsParams{ChatID: chatID})
	if err != nil {
		return nil, err
	}
	var out []storage.Person
	for _, m := range ms {
		switch {
		case m.Owner != nil && m.Owner.User != nil:
			out = append(out, person(m.Owner.User))
		case m.Administrator != nil:
			out = append(out, person(&m.Administrator.User))
		}
	}
	return out, nil
}
