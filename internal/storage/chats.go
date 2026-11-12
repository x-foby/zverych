package storage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// Group states (SPEC 3.1).
const (
	ChatOff     = "off"     // added, but not turned on by the creator: nothing is stored, nothing is done
	ChatPending = "pending" // turned on by the creator, waiting to find out who the admin is
	ChatActive  = "active"  // the bot works in the chat's mode
)

// Modes of an active group (SPEC 4.1a).
const (
	ModeSilent    = "silent"    // neither writes nor reacts
	ModeReactions = "reactions" // reacts, but does not write
	ModeTalk      = "talk"      // everything
)

// ValidMode reports whether m is one of the modes.
func ValidMode(m string) bool { return m == ModeSilent || m == ModeReactions || m == ModeTalk }

// Chat is a group the bot is a member of.
type Chat struct {
	ID      int64
	Title   string
	State   string
	Mode    string // the mode of an active group, the mode to turn on with for a pending one
	AddedBy int64
	AddedAt time.Time
}

// WantedAdmin is someone the creator named as a group's admin before the bot knew who they are
// (SPEC 3.2): by @username, by name, or both.
type WantedAdmin struct {
	ChatID      int64
	Username    string
	Name        string
	RequestedBy int64
	RequestedAt time.Time
}

// Label is a human-readable "Имя (@username)".
func (w WantedAdmin) Label() string { return Person{Name: w.Name, Username: w.Username}.Label() }

const chatsSchema = `
CREATE TABLE IF NOT EXISTS pending_admins (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id      INTEGER NOT NULL,
	username     TEXT NOT NULL DEFAULT '',
	user_name    TEXT NOT NULL DEFAULT '',
	requested_by INTEGER NOT NULL,
	requested_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pending_admins_chat ON pending_admins(chat_id);
`

// chatTables hold a group's data: everything goes when the group is turned off or the bot is removed.
// The private chats' choice of the group goes too (the private chats themselves stay).
var chatTables = []string{"messages", "reactions", "chat_state", "chat_roles", "lore", "summaries", "notes", "pending_admins", "dm_state", "topics", "personas", "persona_drafts"}

// RegisterChat records a group the bot is in. A new one is off; a known one keeps its state, and
// only a non-empty title replaces the stored one.
func (s *Store) RegisterChat(ctx context.Context, chatID int64, title string, addedBy int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats(chat_id, title, added_by, added_at, state, mode) VALUES(?,?,?,?,?,?)
		ON CONFLICT(chat_id) DO UPDATE SET title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE title END`,
		chatID, title, addedBy, at.Unix(), ChatOff, ModeTalk)
	return err
}

const chatColumns = `chat_id, title, state, mode, added_by, added_at`

func scanChat(sc interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	var at int64
	err := sc.Scan(&c.ID, &c.Title, &c.State, &c.Mode, &c.AddedBy, &at)
	c.AddedAt = time.Unix(at, 0)
	return c, err
}

// Chat returns the group; ok is false if the bot does not know it.
func (s *Store) Chat(ctx context.Context, chatID int64) (Chat, bool, error) {
	c, err := scanChat(s.db.QueryRowContext(ctx, `SELECT `+chatColumns+` FROM chats WHERE chat_id=?`, chatID))
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, false, nil
	}
	return c, err == nil, err
}

// Chats lists every group the bot knows, by title.
func (s *Store) Chats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatColumns+` FROM chats ORDER BY chat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	// By title, in Go: SQLite's lower() folds ASCII only.
	slices.SortStableFunc(out, func(a, b Chat) int { return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)) })
	return out, rows.Err()
}

// SetChatState changes the group's state and mode; false if the bot does not know the group.
func (s *Store) SetChatState(ctx context.Context, chatID int64, state, mode string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE chats SET state=?, mode=? WHERE chat_id=?`, state, mode, chatID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ResetChat deletes everything the bot knows about the group and turns it off; the group stays known.
func (s *Store) ResetChat(ctx context.Context, chatID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := wipeChat(ctx, tx, chatID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE chats SET state=?, mode=? WHERE chat_id=?`, ChatOff, ModeTalk, chatID)
		return err
	})
}

// ForgetChat deletes everything about the group, the group itself included (the bot was removed).
func (s *Store) ForgetChat(ctx context.Context, chatID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := wipeChat(ctx, tx, chatID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM chats WHERE chat_id=?`, chatID)
		return err
	})
}

func wipeChat(ctx context.Context, tx *sql.Tx, chatID int64) error {
	for _, t := range chatTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE chat_id=?`, chatID); err != nil {
			return err
		}
	}
	return nil
}

// AddWantedAdmin records someone the creator named as the group's admin; the same person twice
// (case aside) is one record.
func (s *Store) AddWantedAdmin(ctx context.Context, w WantedAdmin) error {
	have, err := s.WantedAdmins(ctx, w.ChatID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(have, func(h WantedAdmin) bool {
		return strings.EqualFold(h.Username, w.Username) && strings.EqualFold(h.Name, w.Name)
	}) {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pending_admins(chat_id, username, user_name, requested_by, requested_at) VALUES(?,?,?,?,?)`,
		w.ChatID, w.Username, w.Name, w.RequestedBy, w.RequestedAt.Unix())
	return err
}

// WantedAdmins lists the people the group is waiting for, in the order they were named.
func (s *Store) WantedAdmins(ctx context.Context, chatID int64) ([]WantedAdmin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id, username, user_name, requested_by, requested_at
		FROM pending_admins WHERE chat_id=? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WantedAdmin
	for rows.Next() {
		var w WantedAdmin
		var at int64
		if err := rows.Scan(&w.ChatID, &w.Username, &w.Name, &w.RequestedBy, &at); err != nil {
			return nil, err
		}
		w.RequestedAt = time.Unix(at, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}

// ClearWantedAdmins forgets whom the group was waiting for (an admin is found, or the group is turned off).
func (s *Store) ClearWantedAdmins(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_admins WHERE chat_id=?`, chatID)
	return err
}
