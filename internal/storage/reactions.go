package storage

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// Participants' reactions to messages (SPEC 4.6). Telegram delivers them only when the bot is a
// chat administrator, as the user's whole new set of reactions to the message.
const reactionSchema = `
CREATE TABLE IF NOT EXISTS reactions (
	chat_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	user_id    INTEGER NOT NULL,
	emoji      TEXT NOT NULL,
	date       INTEGER NOT NULL,
	PRIMARY KEY (chat_id, message_id, user_id, emoji)
);
`

// ReactionCount is how many participants put the emoji on a message.
type ReactionCount struct {
	Emoji string
	Count int
}

// SetReactions replaces the user's reactions to a message (an empty set removes them).
func (s *Store) SetReactions(ctx context.Context, chatID int64, msgID int, userID int64, emojis []string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE chat_id=? AND message_id=? AND user_id=?`,
			chatID, msgID, userID); err != nil {
			return err
		}
		for _, e := range emojis {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO reactions(chat_id, message_id, user_id, emoji, date)
				VALUES (?,?,?,?,?)`, chatID, msgID, userID, e, at.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

// reactionCounts returns reactions to messages fromID..toID by message, the most popular first.
func (s *Store) reactionCounts(ctx context.Context, chatID int64, fromID, toID int) (map[int][]ReactionCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, emoji, COUNT(*), MIN(date) FROM reactions
		WHERE chat_id=? AND message_id BETWEEN ? AND ? GROUP BY message_id, emoji`, chatID, fromID, toID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		ReactionCount
		first int64
	}
	byMsg := map[int][]row{}
	for rows.Next() {
		var id int
		var r row
		if err := rows.Scan(&id, &r.Emoji, &r.Count, &r.first); err != nil {
			return nil, err
		}
		byMsg[id] = append(byMsg[id], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[int][]ReactionCount, len(byMsg))
	for id, rs := range byMsg {
		// The most popular first; ties — whichever appeared first, then by emoji for stability.
		slices.SortFunc(rs, func(a, b row) int {
			switch {
			case a.Count != b.Count:
				return b.Count - a.Count
			case a.first != b.first:
				return int(a.first - b.first)
			case a.Emoji < b.Emoji:
				return -1
			case a.Emoji > b.Emoji:
				return 1
			}
			return 0
		})
		counts := make([]ReactionCount, len(rs))
		for i, r := range rs {
			counts[i] = r.ReactionCount
		}
		out[id] = counts
	}
	return out, nil
}

// TopicActivity is what the bot observes in a topic to learn its norms (SPEC 4.6).
type TopicActivity struct {
	Messages []Message // the latest messages people wrote (newest first), with reactions
	Joins    []Message // the latest join events (newest first), with reactions
	Reactors []int64   // everyone who reacted to anything in the topic, ascending
}

// TopicActivity returns up to `limit` latest messages and joins of a topic (the bot's own
// messages excluded) and who reacts there.
func (s *Store) TopicActivity(ctx context.Context, chatID int64, threadID, limit int) (TopicActivity, error) {
	var a TopicActivity
	for _, k := range []struct {
		kind string
		dst  *[]Message
	}{{"", &a.Messages}, {KindJoin, &a.Joins}} {
		rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages
			WHERE chat_id=? AND thread_id=? AND kind=? AND is_self=0 ORDER BY message_id DESC LIMIT ?`,
			chatID, threadID, k.kind, limit)
		if err != nil {
			return a, err
		}
		ms, err := scanMessages(rows)
		if err != nil {
			return a, err
		}
		if len(ms) > 0 {
			counts, err := s.reactionCounts(ctx, chatID, ms[len(ms)-1].MessageID, ms[0].MessageID)
			if err != nil {
				return a, err
			}
			for i := range ms {
				ms[i].Reactions = counts[ms[i].MessageID]
			}
		}
		*k.dst = ms
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT r.user_id FROM reactions r
		JOIN messages m ON m.chat_id=r.chat_id AND m.message_id=r.message_id
		WHERE r.chat_id=? AND m.thread_id=? ORDER BY r.user_id`, chatID, threadID)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return a, err
		}
		a.Reactors = append(a.Reactors, id)
	}
	return a, rows.Err()
}
