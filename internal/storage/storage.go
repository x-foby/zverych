// Package storage keeps chat history, per-chat state and quota counters in SQLite.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Message struct {
	PhotoID   string
	ChatID    int64
	MessageID int
	ThreadID  int
	UserID    int64
	UserName  string
	Username  string
	Text      string
	ReplyTo   int
	Date      time.Time
	IsSelf    bool
	Addressed bool // the message was addressed to the bot (mention / reply / alias)
	Answered  bool
	Kind      string // "" for a message written by someone; KindJoin / KindLeave for service events
	RootID    int    // the thread (SPEC 7a): id of its root message; 0 — not in a thread. SaveMessage sets it by the reply

	Reactions []ReactionCount // participants' reactions, filled in by Recent, Fresh and Before
}

// Service message kinds (SPEC 4.6). For them UserID is the person who joined or left, and Text
// is a placeholder like "[вступил(а) в чат]".
const (
	KindJoin  = "join"
	KindLeave = "leave"
)

type ChatState struct {
	LastInitiativeAt time.Time
	LastEvaluatedID  int
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	chat_id    INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	thread_id  INTEGER NOT NULL DEFAULT 0,
	user_id    INTEGER NOT NULL,
	user_name  TEXT NOT NULL,
	username   TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL,
	reply_to   INTEGER NOT NULL DEFAULT 0,
	date       INTEGER NOT NULL,
	is_self    INTEGER NOT NULL DEFAULT 0,
	addressed  INTEGER NOT NULL DEFAULT 0,
	answered   INTEGER NOT NULL DEFAULT 0,
	kind       TEXT NOT NULL DEFAULT '',
	root_id    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_pending ON messages(addressed, answered, date);
CREATE TABLE IF NOT EXISTS chat_state (
	chat_id            INTEGER PRIMARY KEY,
	last_initiative_at INTEGER NOT NULL DEFAULT 0,
	last_evaluated_id  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS quota_usage (
	provider TEXT NOT NULL,
	model    TEXT NOT NULL,
	day      TEXT NOT NULL,
	count    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (provider, model, day)
);
CREATE TABLE IF NOT EXISTS chats (
	chat_id  INTEGER PRIMARY KEY,
	title    TEXT NOT NULL DEFAULT '',
	added_by INTEGER NOT NULL,
	added_at INTEGER NOT NULL
);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// columns added after the first release; CREATE TABLE IF NOT EXISTS does not add them to old databases.
var addedColumns = []struct{ table, column, def string }{
	{"lore", "no_reply", `INTEGER NOT NULL DEFAULT 0`},
	{"messages", "photo_id", `TEXT NOT NULL DEFAULT ''`},
	{"messages", "kind", `TEXT NOT NULL DEFAULT ''`},
	{"messages", "root_id", `INTEGER NOT NULL DEFAULT 0`},
	{"summaries", "tone", `TEXT NOT NULL DEFAULT ''`},
	// Groups allowed before the states (SPEC 3.1) keep working as before: active, talking.
	{"chats", "state", `TEXT NOT NULL DEFAULT 'active'`},
	{"chats", "mode", `TEXT NOT NULL DEFAULT 'talk'`},
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema + memorySchema + reactionSchema + summarySchema + notesSchema + chatsSchema + dmSchema + topicsSchema + personaSchema); err != nil {
		return err
	}
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.column + ` ` + c.def); err != nil {
				return err
			}
			if c.column == "root_id" {
				if err := backfillThreads(db); err != nil {
					return fmt.Errorf("backfill threads: %w", err)
				}
			}
		}
	}
	// Indexes on added columns: only after the columns exist in old databases.
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_root ON messages(chat_id, root_id)`)
	return err
}

// backfillThreads assigns threads to the history saved before them (SPEC 7a). Before that a plain
// message in a forum topic was saved as a reply to the topic's creation message: it is not one.
func backfillThreads(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to=0 WHERE thread_id!=0 AND reply_to=thread_id`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT chat_id, message_id, thread_id, reply_to FROM messages
		WHERE reply_to!=0 ORDER BY chat_id, message_id`)
	if err != nil {
		return err
	}
	type reply struct {
		chat          int64
		id, topic, to int
	}
	var rs []reply
	for rows.Next() {
		var r reply
		if err := rows.Scan(&r.chat, &r.id, &r.topic, &r.to); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range rs {
		root, err := joinThread(ctx, tx, r.chat, r.topic, r.to)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET root_id=? WHERE chat_id=? AND message_id=?`, root, r.chat, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// joinThread returns the thread a reply to message `to` belongs to, making `to` the root of a new
// thread when it was in none. A reply to a message the bot never saw (older than the history) roots
// the thread at that message; a reply across topics joins no thread.
func joinThread(ctx context.Context, tx *sql.Tx, chatID int64, topic, to int) (int, error) {
	var parentTopic, parentRoot int
	err := tx.QueryRowContext(ctx, `SELECT thread_id, root_id FROM messages WHERE chat_id=? AND message_id=?`, chatID, to).
		Scan(&parentTopic, &parentRoot)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return to, nil
	case err != nil:
		return 0, err
	case parentTopic != topic:
		return 0, nil
	case parentRoot != 0:
		return parentRoot, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE messages SET root_id=message_id WHERE chat_id=? AND message_id=?`, chatID, to)
	return to, err
}

func (s *Store) Close() error { return s.db.Close() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SaveMessage stores a message once (a repeated id is ignored). A reply joins the thread of the
// message it answers (SPEC 7a); m.RootID set by the caller is kept.
func (s *Store) SaveMessage(ctx context.Context, m Message) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_id=? AND message_id=?`,
			m.ChatID, m.MessageID).Scan(&exists); err != nil || exists > 0 {
			return err
		}
		if m.ReplyTo != 0 && m.RootID == 0 {
			root, err := joinThread(ctx, tx, m.ChatID, m.ThreadID, m.ReplyTo)
			if err != nil {
				return err
			}
			m.RootID = root
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO messages
			(chat_id, message_id, thread_id, user_id, user_name, username, text, reply_to, date, is_self, addressed, answered, kind, root_id, photo_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.ChatID, m.MessageID, m.ThreadID, m.UserID, m.UserName, m.Username, m.Text, m.ReplyTo,
			m.Date.Unix(), b2i(m.IsSelf), b2i(m.Addressed), b2i(m.Answered), m.Kind, m.RootID, m.PhotoID)
		return err
	})
}

const msgCols = `chat_id, message_id, thread_id, user_id, user_name, username, text, reply_to, date, is_self, addressed, answered, kind, root_id, photo_id`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var date int64
		var self, addr, ans int
		if err := rows.Scan(&m.ChatID, &m.MessageID, &m.ThreadID, &m.UserID, &m.UserName, &m.Username,
			&m.Text, &m.ReplyTo, &date, &self, &addr, &ans, &m.Kind, &m.RootID, &m.PhotoID); err != nil {
			return nil, err
		}
		m.Date = time.Unix(date, 0)
		m.IsSelf, m.Addressed, m.Answered = self == 1, addr == 1, ans == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns a single message or nil if not found.
func (s *Store) Get(ctx context.Context, chatID int64, msgID int) (*Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages WHERE chat_id=? AND message_id=?`, chatID, msgID)
	if err != nil {
		return nil, err
	}
	ms, err := scanMessages(rows)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return &ms[0], nil
}

// Recent returns the last `limit` messages of a chat in chronological order, with reactions.
func (s *Store) Recent(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
		SELECT `+msgCols+` FROM messages WHERE chat_id=? ORDER BY message_id DESC LIMIT ?
	) ORDER BY message_id ASC`, chatID, limit)
	if err != nil {
		return nil, err
	}
	return s.withReactions(ctx, chatID, rows)
}

// withReactions scans chronologically ordered messages and attaches participants' reactions.
func (s *Store) withReactions(ctx context.Context, chatID int64, rows *sql.Rows) ([]Message, error) {
	ms, err := scanMessages(rows)
	if err != nil || len(ms) == 0 {
		return ms, err
	}
	counts, err := s.reactionCounts(ctx, chatID, ms[0].MessageID, ms[len(ms)-1].MessageID)
	if err != nil {
		return nil, err
	}
	for i := range ms {
		ms[i].Reactions = counts[ms[i].MessageID]
	}
	return ms, nil
}

// Fresh returns the last `limit` messages with id > afterID in chronological order, with reactions.
func (s *Store) Fresh(ctx context.Context, chatID int64, afterID, limit int) ([]Message, error) {
	return s.window(ctx, chatID, `message_id>?`, afterID, limit)
}

// Before returns the last `limit` messages with id < beforeID in chronological order, with reactions.
func (s *Store) Before(ctx context.Context, chatID int64, beforeID, limit int) ([]Message, error) {
	return s.window(ctx, chatID, `message_id<?`, beforeID, limit)
}

// window returns the newest `limit` messages matching cond (on message_id), oldest first, with reactions.
func (s *Store) window(ctx context.Context, chatID int64, cond string, id, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
		SELECT `+msgCols+` FROM messages WHERE chat_id=? AND `+cond+` ORDER BY message_id DESC LIMIT ?
	) ORDER BY message_id ASC`, chatID, id, limit)
	if err != nil {
		return nil, err
	}
	return s.withReactions(ctx, chatID, rows)
}

// Thread returns a thread (SPEC 7a) oldest first, with reactions: its first `head` and last `tail`
// messages, and how many messages it has in all (more than returned: there is a gap after the head).
func (s *Store) Thread(ctx context.Context, chatID int64, root, head, tail int) ([]Message, int, error) {
	const in = `chat_id=? AND (message_id=? OR root_id=?)`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE `+in, chatID, root, root).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages WHERE `+in+` AND (
		message_id IN (SELECT message_id FROM messages WHERE `+in+` ORDER BY message_id ASC LIMIT ?) OR
		message_id IN (SELECT message_id FROM messages WHERE `+in+` ORDER BY message_id DESC LIMIT ?)
	) ORDER BY message_id ASC`, chatID, root, root, chatID, root, root, head, chatID, root, root, tail)
	if err != nil {
		return nil, 0, err
	}
	ms, err := s.withReactions(ctx, chatID, rows)
	return ms, total, err
}

// Around returns the last `limit` messages of a topic up to message `upTo`, oldest first, with
// reactions — outside the thread `root` (0: any messages). It is what else goes on next to a thread.
func (s *Store) Around(ctx context.Context, chatID int64, topic, root, upTo, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
		SELECT `+msgCols+` FROM messages WHERE chat_id=? AND thread_id=? AND message_id<=?
			AND (?=0 OR (message_id!=? AND root_id!=?))
		ORDER BY message_id DESC LIMIT ?
	) ORDER BY message_id ASC`, chatID, topic, upTo, root, root, root, limit)
	if err != nil {
		return nil, err
	}
	return s.withReactions(ctx, chatID, rows)
}

// ActiveThreads returns the roots of a topic's threads with messages since `since`, the most
// recently active first, at most `limit` (SPEC 7a: candidates for a message without a reply).
func (s *Store) ActiveThreads(ctx context.Context, chatID int64, topic int, since time.Time, limit int) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT root_id FROM messages
		WHERE chat_id=? AND thread_id=? AND root_id!=0 AND date>=?
		GROUP BY root_id ORDER BY MAX(message_id) DESC LIMIT ?`, chatID, topic, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var r int
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRoot puts a message that is in no thread into the thread of message `of` (assigned by
// meaning, SPEC 7a), making `of` a root if it was in none. It reports whether the message moved;
// one already in a thread stays where it is.
func (s *Store) SetRoot(ctx context.Context, chatID int64, msgID, of int) (bool, error) {
	var moved bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		root := of
		var r int
		switch err := tx.QueryRowContext(ctx, `SELECT root_id FROM messages WHERE chat_id=? AND message_id=?`, chatID, of).Scan(&r); {
		case err == nil && r != 0:
			root = r
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE messages SET root_id=? WHERE chat_id=? AND message_id=? AND root_id=0`,
			root, chatID, msgID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		moved = true
		_, err = tx.ExecContext(ctx, `UPDATE messages SET root_id=message_id WHERE chat_id=? AND message_id=? AND root_id=0`, chatID, root)
		return err
	})
	return moved, err
}

// SelfInThread reports whether the bot has a message in the thread `root`.
func (s *Store) SelfInThread(ctx context.Context, chatID int64, root int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages
		WHERE chat_id=? AND is_self=1 AND (message_id=? OR root_id=?))`, chatID, root, root).Scan(&n)
	return n == 1, err
}

// PendingMentions returns unanswered messages addressed to the bot, newer than `since`.
func (s *Store) PendingMentions(ctx context.Context, since time.Time) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages
		WHERE addressed=1 AND answered=0 AND date>=? ORDER BY chat_id, message_id`, since.Unix())
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// MarkAnsweredUpTo marks all addressed messages in the chat up to msgID as answered.
func (s *Store) MarkAnsweredUpTo(ctx context.Context, chatID int64, msgID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET answered=1
		WHERE chat_id=? AND addressed=1 AND message_id<=?`, chatID, msgID)
	return err
}

// InDialog reports whether the user talked with the bot in the topic since `since`: addressed
// it, or got a reply from it.
func (s *Store) InDialog(ctx context.Context, chatID int64, threadID int, userID int64, since time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM messages m WHERE m.chat_id=? AND m.thread_id=? AND m.date>=? AND (
			(m.user_id=? AND m.addressed=1) OR
			(m.is_self=1 AND EXISTS (SELECT 1 FROM messages t WHERE t.chat_id=m.chat_id AND t.message_id=m.reply_to AND t.user_id=?))
		))`, chatID, threadID, since.Unix(), userID, userID).Scan(&n)
	return n == 1, err
}

// MarkHandled marks a message as addressed and answered: the bot dealt with it (e.g. an
// instruction from an admin), so it is neither a pending mention nor an initiative target.
func (s *Store) MarkHandled(ctx context.Context, chatID int64, msgID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET addressed=1, answered=1 WHERE chat_id=? AND message_id=?`, chatID, msgID)
	return err
}

func (s *Store) ChatState(ctx context.Context, chatID int64) (ChatState, error) {
	var st ChatState
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT last_initiative_at, last_evaluated_id FROM chat_state WHERE chat_id=?`, chatID).
		Scan(&last, &st.LastEvaluatedID)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if last > 0 {
		st.LastInitiativeAt = time.Unix(last, 0)
	}
	return st, err
}

func (s *Store) SetLastEvaluated(ctx context.Context, chatID int64, msgID int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_state(chat_id, last_evaluated_id) VALUES(?,?)
		ON CONFLICT(chat_id) DO UPDATE SET last_evaluated_id=excluded.last_evaluated_id`, chatID, msgID)
	return err
}

func (s *Store) SetLastInitiative(ctx context.Context, chatID int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_state(chat_id, last_initiative_at) VALUES(?,?)
		ON CONFLICT(chat_id) DO UPDATE SET last_initiative_at=excluded.last_initiative_at`, chatID, t.Unix())
	return err
}

func (s *Store) QuotaGet(ctx context.Context, provider, model, day string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count FROM quota_usage WHERE provider=? AND model=? AND day=?`,
		provider, model, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (s *Store) QuotaInc(ctx context.Context, provider, model, day string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO quota_usage(provider, model, day, count) VALUES(?,?,?,1)
		ON CONFLICT(provider, model, day) DO UPDATE SET count=count+1`, provider, model, day)
	return err
}

// Prune deletes messages older than `before` and stale quota rows. Summaries and notes stay (SPEC 4.5).
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	for _, q := range []string{`DELETE FROM messages WHERE date<?`, `DELETE FROM reactions WHERE date<?`, `DELETE FROM dm_messages WHERE date<?`} {
		if _, err := s.db.ExecContext(ctx, q, before.Unix()); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM quota_usage WHERE day<?`, before.UTC().Format("2006-01-02"))
	return err
}

// MigrateChat moves the group's state, roles, lore, summaries, notes about participants and the admins it waits for
// to the new id when a group is upgraded to a supergroup. History stays under the old id: message ids restart in the new chat.
func (s *Store) MigrateChat(ctx context.Context, from, to int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`UPDATE OR REPLACE chats SET chat_id=? WHERE chat_id=?`,
			`UPDATE chat_roles SET chat_id=? WHERE chat_id=?`,
			`UPDATE lore SET chat_id=?, source_msg=0 WHERE chat_id=?`,
			// Message ids restart in the new chat: the summary covers none of them yet.
			`UPDATE OR REPLACE summaries SET chat_id=?, up_to=0 WHERE chat_id=?`,
			`UPDATE OR REPLACE notes SET chat_id=? WHERE chat_id=?`,
			`UPDATE pending_admins SET chat_id=? WHERE chat_id=?`,
			`UPDATE dm_state SET chat_id=? WHERE chat_id=?`,
			`UPDATE OR REPLACE personas SET chat_id=? WHERE chat_id=?`,
			`UPDATE persona_drafts SET chat_id=? WHERE chat_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, to, from); err != nil {
				return err
			}
		}
		return nil
	})
}
