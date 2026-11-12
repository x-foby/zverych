package brain

import (
	"context"
	"log/slog"

	"github.com/vvchaschin/communitybot/internal/storage"
)

// Membership is a change of the bot's own membership in a chat (Telegram my_chat_member).
type Membership struct {
	ChatID     int64
	ChatType   string
	ChatTitle  string
	ByUserID   int64 // who made the change (who added / removed the bot)
	ByUserName string
	Joined     bool // the bot became a member
	Removed    bool // the bot stopped being a member
}

// OnMembership keeps the list of groups (SPEC 3.1): anyone may add the bot, a new group is off
// until the creator turns it on; when the bot is removed, everything about the group goes.
func (b *Brain) OnMembership(ctx context.Context, ev Membership) {
	if !isGroup(ev.ChatType) {
		return
	}
	switch {
	case ev.Removed:
		slog.Info("bot removed from chat: forgetting it", "chat_id", ev.ChatID, "title", ev.ChatTitle, "by", ev.ByUserID)
		if err := b.store.ForgetChat(ctx, ev.ChatID); err != nil {
			slog.Error("forget chat", "chat_id", ev.ChatID, "err", err)
		}
		b.dropChat(ev.ChatID)
	case ev.Joined:
		b.registerChat(ctx, ev.ChatID, ev.ChatTitle, ev.ByUserID)
	}
}

func isGroup(chatType string) bool { return chatType == "group" || chatType == "supergroup" }

// registerChat records a group the bot is in; a known one keeps its state.
func (b *Brain) registerChat(ctx context.Context, chatID int64, title string, by int64) {
	_, known, err := b.chat(ctx, chatID)
	if err != nil {
		return
	}
	if err := b.store.RegisterChat(ctx, chatID, title, by, b.now()); err != nil {
		slog.Error("register chat", "chat_id", chatID, "err", err)
		return
	}
	b.dropChat(chatID)
	if !known {
		slog.Info("added to a group: off until the creator turns it on", "chat_id", chatID, "title", title, "by", by)
	}
}

// OnMigrate carries the group over when it is upgraded to a supergroup.
func (b *Brain) OnMigrate(ctx context.Context, from, to int64) {
	if _, known, err := b.chat(ctx, from); err != nil || !known {
		return
	}
	if err := b.store.MigrateChat(ctx, from, to); err != nil {
		slog.Error("migrate chat", "from", from, "to", to, "err", err)
		return
	}
	b.dropChat(from)
	b.dropChat(to)
	slog.Info("group upgraded to supergroup", "from", from, "to", to)
}

// chat returns the group's record (cached); known is false for a group the bot has not recorded.
func (b *Brain) chat(ctx context.Context, chatID int64) (c storage.Chat, known bool, err error) {
	b.mu.Lock()
	cc, cached := b.chats[chatID]
	b.mu.Unlock()
	if cached {
		return cc.chat, cc.known, nil
	}
	c, known, err = b.store.Chat(ctx, chatID)
	if err != nil {
		slog.Error("load chat", "chat_id", chatID, "err", err)
		return c, false, err
	}
	b.mu.Lock()
	b.chats[chatID] = cachedChat{chat: c, known: known}
	b.mu.Unlock()
	return c, known, nil
}

type cachedChat struct {
	chat  storage.Chat
	known bool
}

// dropChat forgets the cached record after the group changed.
func (b *Brain) dropChat(chatID int64) {
	b.mu.Lock()
	delete(b.chats, chatID)
	delete(b.forums, chatID) // a reset group's topics are gone: learned anew
	b.mu.Unlock()
}

// mode is the active group's mode; anything else counts as silent.
func (b *Brain) mode(ctx context.Context, chatID int64) string {
	c, known, err := b.chat(ctx, chatID)
	if err != nil || !known || c.State != storage.ChatActive {
		return storage.ModeSilent
	}
	return c.Mode
}
