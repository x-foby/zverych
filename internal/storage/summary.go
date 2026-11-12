package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Summary is the running summary of a topic's older messages (SPEC 4.5). It outlives the messages
// themselves (retention_days), like lore: it is the chat's compressed history.
type Summary struct {
	Text      string // "" — none yet, or erased by the chat's admin
	UpTo      int    // the newest message it covers; later ones are summarized next
	Tone      string // how people talk in the topic (SPEC 4.6), learned with the summary; "" — not yet, or erased
	UpdatedAt time.Time
}

const summarySchema = `
CREATE TABLE IF NOT EXISTS summaries (
	chat_id    INTEGER NOT NULL,
	thread_id  INTEGER NOT NULL,
	text       TEXT NOT NULL,
	up_to      INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	tone       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (chat_id, thread_id)
);
`

// Summary returns the topic's summary (zero if there is none).
func (s *Store) Summary(ctx context.Context, chatID int64, topic int) (Summary, error) {
	var sm Summary
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT text, up_to, updated_at, tone FROM summaries WHERE chat_id=? AND thread_id=?`,
		chatID, topic).Scan(&sm.Text, &sm.UpTo, &at, &sm.Tone)
	if errors.Is(err, sql.ErrNoRows) {
		return sm, nil
	}
	if at > 0 {
		sm.UpdatedAt = time.Unix(at, 0)
	}
	return sm, err
}

// SetSummary replaces the topic's summary `prev` (as read before writing the new one): it now covers
// the messages up to upTo. It reports false and changes nothing if the summary is no longer prev —
// erased or updated meanwhile.
func (s *Store) SetSummary(ctx context.Context, chatID int64, topic int, prev Summary, text string, upTo int, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO summaries(chat_id, thread_id, text, up_to, updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(chat_id, thread_id) DO UPDATE SET text=excluded.text, up_to=excluded.up_to, updated_at=excluded.updated_at
		WHERE summaries.text=? AND summaries.up_to=?`,
		chatID, topic, text, upTo, at.Unix(), prev.Text, prev.UpTo)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearSummary erases the topic's summary text, keeping how far it got: the messages it covered are
// not summarized again. Returns whether there was a text to erase.
func (s *Store) ClearSummary(ctx context.Context, chatID int64, topic int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE summaries SET text='' WHERE chat_id=? AND thread_id=? AND text!=''`, chatID, topic)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetTone replaces the topic's tone `prev` (as read before writing the new one). It reports false and
// changes nothing if the tone is no longer prev — erased by the admin or rewritten meanwhile — or the
// topic has no summary row yet (the tone is learned after the summary is written).
func (s *Store) SetTone(ctx context.Context, chatID int64, topic int, prev, tone string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE summaries SET tone=? WHERE chat_id=? AND thread_id=? AND tone=?`,
		tone, chatID, topic, prev)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearTone erases the topic's tone; later chunks learn it anew. Returns whether there was one.
func (s *Store) ClearTone(ctx context.Context, chatID int64, topic int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE summaries SET tone='' WHERE chat_id=? AND thread_id=? AND tone!=''`, chatID, topic)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Unsummarized returns, oldest first, at most `limit` messages of the topic after message `after`
// that are older than the topic's newest `keep` messages (the model sees those as they are) — or
// were sent before `expiring` (they are about to be pruned, SPEC 4.5). With reactions.
func (s *Store) Unsummarized(ctx context.Context, chatID int64, topic, after, keep, limit int, expiring time.Time) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages
		WHERE chat_id=? AND thread_id=? AND message_id>? AND (
			message_id <= (SELECT message_id FROM messages WHERE chat_id=? AND thread_id=? ORDER BY message_id DESC LIMIT 1 OFFSET ?)
			OR date<?)
		ORDER BY message_id ASC LIMIT ?`, chatID, topic, after, chatID, topic, keep, expiring.Unix(), limit)
	if err != nil {
		return nil, err
	}
	return s.withReactions(ctx, chatID, rows)
}

// TopicRef names a topic of a chat.
type TopicRef struct {
	ChatID int64
	Topic  int
}

// ExpiringTopics lists the topics with messages sent before `before` that their summary does not cover yet.
func (s *Store) ExpiringTopics(ctx context.Context, before time.Time) ([]TopicRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT m.chat_id, m.thread_id FROM messages m
		LEFT JOIN summaries s ON s.chat_id=m.chat_id AND s.thread_id=m.thread_id
		WHERE m.date<? AND m.message_id>COALESCE(s.up_to, 0)
		ORDER BY m.chat_id, m.thread_id`, before.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopicRef
	for rows.Next() {
		var t TopicRef
		if err := rows.Scan(&t.ChatID, &t.Topic); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
