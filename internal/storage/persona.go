package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A group's own persona (SPEC 5): its admins set it in their own words, the bot rewrites the
// description and asks the admin to confirm it; until then the change waits as a draft. A group
// without its own persona has the default one (persona.md).
const personaSchema = `
CREATE TABLE IF NOT EXISTS personas (
	chat_id INTEGER PRIMARY KEY,
	text    TEXT NOT NULL,
	set_by  INTEGER NOT NULL,
	set_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS persona_drafts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL,
	user_id    INTEGER NOT NULL,
	text       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_persona_drafts_chat ON persona_drafts(chat_id);
`

// PersonaDraft is a persona an admin asked for, waiting for their confirmation; an empty text
// means going back to the default persona.
type PersonaDraft struct {
	ID        int64
	ChatID    int64
	UserID    int64
	Text      string
	CreatedAt time.Time
}

// Persona returns the group's own persona, "" if it has none.
func (s *Store) Persona(ctx context.Context, chatID int64) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM personas WHERE chat_id=?`, chatID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// SetPersona replaces the group's persona; an empty text brings back the default one.
func (s *Store) SetPersona(ctx context.Context, chatID int64, text string, by int64, at time.Time) error {
	if text == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM personas WHERE chat_id=?`, chatID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO personas(chat_id, text, set_by, set_at) VALUES(?,?,?,?)
		ON CONFLICT(chat_id) DO UPDATE SET text=excluded.text, set_by=excluded.set_by, set_at=excluded.set_at`,
		chatID, text, by, at.Unix())
	return err
}

// AddPersonaDraft keeps a persona waiting for confirmation and returns its id. A group has one
// draft at a time: a new one replaces the previous (its buttons stop working).
func (s *Store) AddPersonaDraft(ctx context.Context, d PersonaDraft) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM persona_drafts WHERE chat_id=?`, d.ChatID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO persona_drafts(chat_id, user_id, text, created_at) VALUES(?,?,?,?)`,
			d.ChatID, d.UserID, d.Text, d.CreatedAt.Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// PersonaDraft returns the group's draft by id; ok is false if it is gone (confirmed, declined
// or replaced by a newer one).
func (s *Store) PersonaDraft(ctx context.Context, chatID, id int64) (PersonaDraft, bool, error) {
	d := PersonaDraft{ID: id, ChatID: chatID}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, text, created_at FROM persona_drafts WHERE chat_id=? AND id=?`, chatID, id).
		Scan(&d.UserID, &d.Text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonaDraft{}, false, nil
	}
	d.CreatedAt = time.Unix(at, 0)
	return d, err == nil, err
}

// DeletePersonaDraft drops the group's draft.
func (s *Store) DeletePersonaDraft(ctx context.Context, chatID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM persona_drafts WHERE chat_id=? AND id=?`, chatID, id)
	return err
}
