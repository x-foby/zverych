package storage

import (
	"context"
	"strings"
	"time"
)

// Note is what the bot knows about a participant of a chat (SPEC 4.5): how the chat treats them
// (with Reputation, −2…+2) and the bot's own impression. It outlives the messages (retention_days).
type Note struct {
	ChatID     int64
	UserID     int64
	Name       string
	Username   string
	Chat       string // how the chat treats them
	Own        string // the bot's own impression: interests, manner, how to talk to them
	Reputation int    // −2…+2, the chat's attitude; 0 — ordinary or unclear
	Version    int    // grows on every write and erasure: guards against overwriting an erased note
	UpdatedAt  time.Time
}

// Person is who the note is about.
func (n Note) Person() Person { return Person{ID: n.UserID, Name: n.Name, Username: n.Username} }

// Empty: nothing known (never written, or erased by the chat's admin).
func (n Note) Empty() bool { return n.Chat == "" && n.Own == "" && n.Reputation == 0 }

const notesSchema = `
CREATE TABLE IF NOT EXISTS notes (
	chat_id    INTEGER NOT NULL,
	user_id    INTEGER NOT NULL,
	user_name  TEXT NOT NULL DEFAULT '',
	username   TEXT NOT NULL DEFAULT '',
	chat_view  TEXT NOT NULL DEFAULT '',
	own_view   TEXT NOT NULL DEFAULT '',
	reputation INTEGER NOT NULL DEFAULT 0,
	version    INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (chat_id, user_id)
);
`

const noteCols = `chat_id, user_id, user_name, username, chat_view, own_view, reputation, version, updated_at`

func scanNote(sc interface{ Scan(...any) error }) (Note, error) {
	var n Note
	var at int64
	err := sc.Scan(&n.ChatID, &n.UserID, &n.Name, &n.Username, &n.Chat, &n.Own, &n.Reputation, &n.Version, &at)
	if at > 0 {
		n.UpdatedAt = time.Unix(at, 0)
	}
	return n, err
}

// Notes returns the notes about the given participants that exist (erased ones too), by user id.
func (s *Store) Notes(ctx context.Context, chatID int64, userIDs []int64) (map[int64]Note, error) {
	out := map[int64]Note{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := []any{chatID}
	for _, id := range userIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+noteCols+` FROM notes WHERE chat_id=? AND user_id IN (?`+
		strings.Repeat(",?", len(userIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out[n.UserID] = n
	}
	return out, rows.Err()
}

// Reputations returns the chat's non-zero reputations by user id.
func (s *Store) Reputations(ctx context.Context, chatID int64) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, reputation FROM notes WHERE chat_id=? AND reputation!=0`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var r int
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// SetNote writes the note n over the version it was made from (n.Version; 0 — there was none). It
// reports false and changes nothing if the note changed meanwhile (erased by the chat's admin).
func (s *Store) SetNote(ctx context.Context, n Note, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO notes(`+noteCols+`) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET user_name=excluded.user_name, username=excluded.username,
			chat_view=excluded.chat_view, own_view=excluded.own_view, reputation=excluded.reputation,
			version=excluded.version, updated_at=excluded.updated_at
		WHERE notes.version=?`,
		n.ChatID, n.UserID, n.Name, n.Username, n.Chat, n.Own, n.Reputation, n.Version+1, at.Unix(), n.Version)
	if err != nil {
		return false, err
	}
	k, err := res.RowsAffected()
	return k > 0, err
}

// ClearNote erases what the bot knows about the participant; the note starts anew from the next
// messages. Returns whether there was anything to erase.
func (s *Store) ClearNote(ctx context.Context, chatID, userID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE notes SET chat_view='', own_view='', reputation=0, version=version+1
		WHERE chat_id=? AND user_id=? AND (chat_view!='' OR own_view!='' OR reputation!=0)`, chatID, userID)
	if err != nil {
		return false, err
	}
	k, err := res.RowsAffected()
	return k > 0, err
}
