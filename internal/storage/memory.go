package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Person identifies a chat participant. ID is 0 when only the @username is known
// (mentioned before the bot ever saw them write); it is filled in by BindUsername.
type Person struct {
	ID       int64
	Name     string
	Username string
}

// Label is a human-readable "Имя (@username)".
func (p Person) Label() string {
	switch {
	case p.Name != "" && p.Username != "":
		return p.Name + " (@" + p.Username + ")"
	case p.Username != "":
		return "@" + p.Username
	case p.Name != "":
		return p.Name
	}
	return "?"
}

// Chat roles (SPEC 4.7).
const (
	RoleLead      = "lead"      // the chat's admin (SPEC 4.7); there may be several
	RoleAuthority = "authority" // trusted participants whose instructions the bot accepts
)

type ChatRole struct {
	Person
	Role      string
	GrantedBy int64
	GrantedAt time.Time
}

// Lore kinds (SPEC 4.5, 4.7).
const (
	LoreFact        = "fact"        // something about the chat to remember
	LoreInstruction = "instruction" // how the bot should (not) behave
)

// LoreBot is the AuthorRole of entries the bot inferred from the conversation itself (SPEC 4.5).
const LoreBot = "bot"

// Inferred reports whether the bot wrote the entry itself.
func (l Lore) Inferred() bool { return l.AuthorRole == LoreBot }

// Lore is a long-term memory entry. ChatWide entries apply to the whole chat,
// otherwise only to the forum topic ThreadID.
type Lore struct {
	ID         int64
	ChatID     int64
	ChatWide   bool
	ThreadID   int
	Kind       string
	NoReply    bool // unconditional instruction prohibiting replies in its scope
	Text       string
	AuthorID   int64
	AuthorRole string // lead / authority (empty for the bot's creator); LoreBot for entries the bot inferred itself (AuthorID 0)
	SourceMsg  int
	CreatedAt  time.Time
}

const memorySchema = `
CREATE TABLE IF NOT EXISTS chat_roles (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    INTEGER NOT NULL,
	user_id    INTEGER NOT NULL DEFAULT 0,
	username   TEXT NOT NULL DEFAULT '',
	user_name  TEXT NOT NULL DEFAULT '',
	role       TEXT NOT NULL,
	granted_by INTEGER NOT NULL,
	granted_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_roles_chat ON chat_roles(chat_id);
CREATE TABLE IF NOT EXISTS lore (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id     INTEGER NOT NULL,
	chat_wide   INTEGER NOT NULL,
	thread_id   INTEGER NOT NULL DEFAULT 0,
	kind        TEXT NOT NULL,
	text        TEXT NOT NULL,
	author_id   INTEGER NOT NULL,
	author_role TEXT NOT NULL,
	source_msg  INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lore_chat ON lore(chat_id);
-- Machine rules were replaced by the model following instructions (2026-11-08).
DROP TABLE IF EXISTS reaction_rules;
DROP TABLE IF EXISTS reply_rules;
`

// Roles returns everyone holding a role in the chat (lead first).
func (s *Store) Roles(ctx context.Context, chatID int64) ([]ChatRole, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, username, user_name, role, granted_by, granted_at
		FROM chat_roles WHERE chat_id=? ORDER BY role='lead' DESC, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRole
	for rows.Next() {
		var r ChatRole
		var at int64
		if err := rows.Scan(&r.ID, &r.Username, &r.Name, &r.Role, &r.GrantedBy, &at); err != nil {
			return nil, err
		}
		r.GrantedAt = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// samePerson matches a role row by user id, or by username when the id is not known yet.
const samePerson = `((?<>0 AND user_id=?) OR (user_id=0 AND ?<>'' AND lower(username)=lower(?)))`

func personArgs(p Person) []any { return []any{p.ID, p.ID, p.Username, p.Username} }

// AddLead makes p one of the chat's admins (dropping p's other role); the other admins stay.
func (s *Store) AddLead(ctx context.Context, chatID int64, p Person, by int64, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_roles WHERE chat_id=? AND `+samePerson,
			append([]any{chatID}, personArgs(p)...)...); err != nil {
			return err
		}
		return insertRole(ctx, tx, chatID, p, RoleLead, by, at)
	})
}

// AddAuthority grants p the authority role. Returns false if p already holds a role.
func (s *Store) AddAuthority(ctx context.Context, chatID int64, p Person, by int64, at time.Time) (bool, error) {
	added := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_roles WHERE chat_id=? AND `+samePerson,
			append([]any{chatID}, personArgs(p)...)...).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		added = true
		return insertRole(ctx, tx, chatID, p, RoleAuthority, by, at)
	})
	return added, err
}

func insertRole(ctx context.Context, tx *sql.Tx, chatID int64, p Person, role string, by int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO chat_roles(chat_id, user_id, username, user_name, role, granted_by, granted_at)
		VALUES (?,?,?,?,?,?,?)`, chatID, p.ID, p.Username, p.Name, role, by, at.Unix())
	return err
}

// RemoveRole takes the given role from p (for the lead p may be zero: whoever holds it).
// Returns whether anything was removed.
func (s *Store) RemoveRole(ctx context.Context, chatID int64, p Person, role string) (bool, error) {
	q := `DELETE FROM chat_roles WHERE chat_id=? AND role=?`
	args := []any{chatID, role}
	if p.ID != 0 || p.Username != "" {
		q += ` AND ` + samePerson
		args = append(args, personArgs(p)...)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BindUsername fills in the user id of roles granted by @username before the user was seen.
func (s *Store) BindUsername(ctx context.Context, chatID, userID int64, username, name string) error {
	if username == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE chat_roles SET user_id=?, user_name=?
		WHERE chat_id=? AND user_id=0 AND username<>'' AND lower(username)=lower(?)`, userID, name, chatID, username)
	return err
}

// FindByUsername looks up a participant who has written in the chat.
func (s *Store) FindByUsername(ctx context.Context, chatID int64, username string) (Person, bool, error) {
	var p Person
	err := s.db.QueryRowContext(ctx, `SELECT user_id, user_name, username FROM messages
		WHERE chat_id=? AND is_self=0 AND lower(username)=lower(?) ORDER BY message_id DESC LIMIT 1`,
		chatID, strings.TrimPrefix(username, "@")).Scan(&p.ID, &p.Name, &p.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// AddLore stores a memory entry and returns its id.
func (s *Store) AddLore(ctx context.Context, l Lore) (int64, error) {
	thread := l.ThreadID
	if l.ChatWide {
		thread = 0
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO lore(chat_id, chat_wide, thread_id, kind, text, author_id, author_role, source_msg, created_at, no_reply)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, l.ChatID, b2i(l.ChatWide), thread, l.Kind, l.Text, l.AuthorID, l.AuthorRole,
		l.SourceMsg, l.CreatedAt.Unix(), l.NoReply)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Lore returns the chat's memory entries, oldest first.
func (s *Store) Lore(ctx context.Context, chatID int64) ([]Lore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, chat_wide, thread_id, kind, text, author_id, author_role, source_msg, created_at, no_reply
		FROM lore WHERE chat_id=? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lore
	for rows.Next() {
		var l Lore
		var wide int
		var at int64
		if err := rows.Scan(&l.ID, &l.ChatID, &wide, &l.ThreadID, &l.Kind, &l.Text, &l.AuthorID, &l.AuthorRole,
			&l.SourceMsg, &at, &l.NoReply); err != nil {
			return nil, err
		}
		l.ChatWide, l.CreatedAt = wide == 1, time.Unix(at, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateLore rewrites an entry the bot inferred itself (text, scope, source). Returns false if the
// entry is gone (forgotten by the leadership meanwhile) or was not inferred by the bot.
func (s *Store) UpdateLore(ctx context.Context, l Lore) (bool, error) {
	thread := l.ThreadID
	if l.ChatWide {
		thread = 0
	}
	res, err := s.db.ExecContext(ctx, `UPDATE lore SET chat_wide=?, thread_id=?, text=?, source_msg=?, created_at=?
		WHERE chat_id=? AND id=? AND author_role=?`, b2i(l.ChatWide), thread, l.Text, l.SourceMsg, l.CreatedAt.Unix(),
		l.ChatID, l.ID, LoreBot)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteLore removes an entry of the chat. Returns whether it existed.
func (s *Store) DeleteLore(ctx context.Context, chatID, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM lore WHERE chat_id=? AND id=?`, chatID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
