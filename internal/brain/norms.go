package brain

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/vvchaschin/communitybot/internal/storage"
)

// Topic norms are learned by watching people, without the model (SPEC 4.6): which reaction is
// customary for which kind of message, and whether ordinary members write in the topic at all.
// They are computed from the database, cached for normsTTL and shown to the model as hints: the
// model decides whether to follow them.

const (
	normsTTL         = 10 * time.Minute
	adminsTTL        = time.Hour
	normsWindow      = 50               // latest messages (and, separately, joins) of a topic to learn from
	reactionSettle   = 10 * time.Minute // younger messages have not collected their reactions yet
	quietMinMessages = 10               // the "admins only" verdict needs at least this many messages
)

// habit is a reaction people customarily put on a kind of message in a topic.
type habit struct {
	emoji string
	share float64 // of the observed messages of this kind that got the emoji
}

type topicNorms struct {
	habits     map[string]habit // by message kind: "" (people's messages) or storage.KindJoin
	adminsOnly bool             // only admins write here, the others just react or join
	tone       string           // how people talk here (tone.go); not cached with the rest: it lives beside the summary
}

type topicKey struct {
	chat   int64
	thread int
}

type cachedNorms struct {
	norms topicNorms
	at    time.Time
}

type cachedAdmins struct {
	ids    map[int64]bool // nil: the list could not be loaded
	people []storage.Person
	at     time.Time
}

// topicNorms returns the topic's norms, recomputing them when the cached ones are stale.
func (b *Brain) topicNorms(ctx context.Context, chatID int64, thread int) topicNorms {
	k := topicKey{chatID, thread}
	b.mu.Lock()
	c, ok := b.norms[k]
	b.mu.Unlock()
	if ok && b.now().Sub(c.at) < normsTTL {
		return c.norms
	}
	a, err := b.store.TopicActivity(ctx, chatID, thread, normsWindow)
	if err != nil {
		slog.Error("load topic activity", "chat_id", chatID, "thread", thread, "err", err)
		return c.norms // the stale ones (or none) are better than nothing
	}
	n := topicNorms{habits: map[string]habit{}}
	settled := b.now().Add(-reactionSettle)
	for kind, ms := range map[string][]storage.Message{"": a.Messages, storage.KindJoin: a.Joins} {
		if h, ok := learnHabit(ms, settled, b.cfg.HabitMinMessages, b.cfg.HabitMinShare); ok {
			n.habits[kind] = h
		}
	}
	if len(a.Messages) >= quietMinMessages { // otherwise the admin list is not needed
		if admins := b.chatAdmins(ctx, chatID); admins != nil {
			n.adminsOnly = adminsOnly(a, func(id int64) bool { return admins[id] || b.admins[id] })
		}
	}
	if !ok || !sameNorms(c.norms, n) {
		slog.Info("topic norms", "chat_id", chatID, "thread", thread, "admins_only", n.adminsOnly, "habits", n.habits)
	}
	b.mu.Lock()
	b.norms[k] = cachedNorms{n, b.now()}
	b.mu.Unlock()
	return n
}

func sameNorms(a, b topicNorms) bool {
	return a.adminsOnly == b.adminsOnly && maps.Equal(a.habits, b.habits)
}

// chatAdmins returns the ids of the chat's administrators from Telegram (cached), or nil if unknown.
func (b *Brain) chatAdmins(ctx context.Context, chatID int64) map[int64]bool {
	return b.tgAdminList(ctx, chatID).ids
}

// tgAdminList returns the chat's administrators from Telegram (cached).
func (b *Brain) tgAdminList(ctx context.Context, chatID int64) cachedAdmins {
	b.mu.Lock()
	c, ok := b.tgAdmins[chatID]
	b.mu.Unlock()
	if ok && b.now().Sub(c.at) < adminsTTL {
		return c
	}
	people, err := b.out.Admins(ctx, chatID)
	c = cachedAdmins{at: b.now()} // a failure is cached too: no point in asking every message
	if err != nil {
		slog.Warn("load chat administrators", "chat_id", chatID, "err", err)
	} else {
		c.people = people
		c.ids = make(map[int64]bool, len(people))
		for _, p := range people {
			c.ids[p.ID] = true
		}
	}
	b.mu.Lock()
	b.tgAdmins[chatID] = c
	b.mu.Unlock()
	return c
}

// learnHabit finds the reaction people put on most of the messages (newest first). Messages
// younger than `settled` are skipped: their reactions are still coming. Only reactions the bot
// can set count.
func learnHabit(ms []storage.Message, settled time.Time, minMessages int, minShare float64) (habit, bool) {
	seen := 0
	with := map[string]int{}
	var order []string // emojis in order of appearance: on a tie the most recent custom wins
	for _, m := range ms {
		if m.Date.After(settled) {
			continue
		}
		seen++
		for _, r := range m.Reactions {
			e := normalizeReaction(r.Emoji)
			if !isAllowedReaction(e) {
				continue
			}
			if with[e] == 0 {
				order = append(order, e)
			}
			with[e]++
		}
	}
	best := ""
	for _, e := range order {
		if with[e] > with[best] {
			best = e
		}
	}
	n := with[best]
	if best == "" || n < minMessages || float64(n) < minShare*float64(seen) {
		return habit{}, false
	}
	return habit{emoji: best, share: float64(n) / float64(seen)}, true
}

// adminsOnly reports whether only admins write in the topic while ordinary members are around
// (they react or join) and stay silent — like a topic with join announcements.
func adminsOnly(a storage.TopicActivity, isAdmin func(int64) bool) bool {
	if len(a.Messages) < quietMinMessages {
		return false
	}
	for _, m := range a.Messages {
		if !isAdmin(m.UserID) {
			return false
		}
	}
	for _, id := range a.Reactors {
		if !isAdmin(id) {
			return true
		}
	}
	for _, j := range a.Joins {
		if !isAdmin(j.UserID) {
			return true
		}
	}
	return false
}

// normsLines renders learned norms for the prompt. thread < 0: all the given topics (the
// filter), tagged like messages are; otherwise only that topic, untagged.
func normsLines(norms map[int]topicNorms, thread int, r roster) []string {
	var lines []string
	for _, t := range slices.Sorted(maps.Keys(norms)) {
		if thread >= 0 && t != thread {
			continue
		}
		n := norms[t]
		var parts []string
		if n.tone != "" {
			parts = append(parts, "манера общения: "+strings.TrimRight(n.tone, ".")+
				" — подстраивайся под неё (только в этом топике и в пределах границ)")
		}
		if n.adminsOnly {
			parts = append(parts, "пишут только админы, остальные лишь реагируют — тебе здесь лучше не писать")
		}
		if h, ok := n.habits[storage.KindJoin]; ok {
			parts = append(parts, fmt.Sprintf("на вступления принято ставить %s (примерно на %d%%)", h.emoji, int(h.share*100+0.5)))
		}
		if h, ok := n.habits[""]; ok {
			parts = append(parts, fmt.Sprintf("на сообщения часто ставят %s (примерно на %d%%)", h.emoji, int(h.share*100+0.5)))
		}
		if len(parts) == 0 {
			continue
		}
		tag := ""
		if thread < 0 && r.tagged(t) {
			tag = "[" + r.topic(t) + "] "
		}
		lines = append(lines, "- "+tag+strings.Join(parts, "; "))
	}
	return lines
}
