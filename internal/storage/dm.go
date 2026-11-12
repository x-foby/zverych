package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// DM is a message of a private chat with the bot (SPEC 3.2). Private chats are kept apart from
// the groups: they never reach a group's history, summary, notes or lore.
type DM struct {
	UserID    int64 // whose private chat (the person the bot talks with)
	MessageID int
	FromSelf  bool // written by the bot
	Text      string
	Date      time.Time
}

const dmSchema = `
CREATE TABLE IF NOT EXISTS dm_messages (
	user_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	from_self  INTEGER NOT NULL DEFAULT 0,
	text       TEXT NOT NULL,
	date       INTEGER NOT NULL,
	PRIMARY KEY (user_id, message_id)
);
CREATE TABLE IF NOT EXISTS dm_state (
	user_id INTEGER PRIMARY KEY,
	chat_id INTEGER NOT NULL
);
`

// SaveDM stores a message of a private chat (the same id twice is one message).
func (s *Store) SaveDM(ctx context.Context, m DM) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO dm_messages(user_id, message_id, from_self, text, date) VALUES(?,?,?,?,?)`,
		m.UserID, m.MessageID, b2i(m.FromSelf), m.Text, m.Date.Unix())
	return err
}

// RecentDMs returns the last messages of the private chat, oldest first.
func (s *Store) RecentDMs(ctx context.Context, userID int64, limit int) ([]DM, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, message_id, from_self, text, date FROM dm_messages
		WHERE user_id=? ORDER BY message_id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DM
	for rows.Next() {
		var m DM
		var self int
		var at int64
		if err := rows.Scan(&m.UserID, &m.MessageID, &self, &m.Text, &at); err != nil {
			return nil, err
		}
		m.FromSelf, m.Date = self != 0, time.Unix(at, 0)
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// SelectedChat is the group the person last spoke about in the private chat; ok is false if none.
func (s *Store) SelectedChat(ctx context.Context, userID int64) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT chat_id FROM dm_state WHERE user_id=?`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// SelectChat remembers the group the person speaks about in the private chat.
func (s *Store) SelectChat(ctx context.Context, userID, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO dm_state(user_id, chat_id) VALUES(?,?)
		ON CONFLICT(user_id) DO UPDATE SET chat_id=excluded.chat_id`, userID, chatID)
	return err
}

// LeadChats lists the active groups where the person is an admin (by id, or by @username while
// the id is not known yet).
func (s *Store) LeadChats(ctx context.Context, userID int64, username string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT r.chat_id FROM chat_roles r JOIN chats c ON c.chat_id=r.chat_id
		WHERE c.state=? AND r.role=? AND `+strings.ReplaceAll(samePerson, "user_id", "r.user_id")+` ORDER BY r.chat_id`,
		append([]any{ChatActive, RoleLead}, personArgs(Person{ID: userID, Username: username})...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FindPerson looks a @username up in every group's history: the latest message wins.
func (s *Store) FindPerson(ctx context.Context, username string) (Person, bool, error) {
	var p Person
	err := s.db.QueryRowContext(ctx, `SELECT user_id, user_name, username FROM messages
		WHERE is_self=0 AND lower(username)=lower(?) ORDER BY date DESC LIMIT 1`,
		strings.TrimPrefix(username, "@")).Scan(&p.ID, &p.Name, &p.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}
