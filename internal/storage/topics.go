package storage

import "context"

// Topic names of forum groups. The Bot API cannot ask for a topic's name: the bot learns it from
// updates — the topic's creation and renaming, and any message of the topic written without a
// reply, which carries the creation message (SPEC 4.6). The general topic (thread 0: its messages
// carry no thread id) has a row while the group has topics; its name is "" until it is renamed —
// the only time Telegram tells it.
const topicsSchema = `
CREATE TABLE IF NOT EXISTS topics (
	chat_id   INTEGER NOT NULL,
	thread_id INTEGER NOT NULL,
	name      TEXT NOT NULL,
	PRIMARY KEY (chat_id, thread_id)
);
`

// SetTopicName records the topic's name; it reports whether the name is new or changed.
func (s *Store) SetTopicName(ctx context.Context, chatID int64, thread int, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO topics(chat_id, thread_id, name) VALUES(?,?,?)
		ON CONFLICT(chat_id, thread_id) DO UPDATE SET name=excluded.name WHERE topics.name<>excluded.name`,
		chatID, thread, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetForum records whether the group has topics: the general topic's row is there or not. A name
// learned before stays.
func (s *Store) SetForum(ctx context.Context, chatID int64, forum bool) error {
	q := `INSERT OR IGNORE INTO topics(chat_id, thread_id, name) VALUES(?, 0, '')`
	if !forum {
		q = `DELETE FROM topics WHERE chat_id=? AND thread_id=0`
	}
	_, err := s.db.ExecContext(ctx, q, chatID)
	return err
}

// TopicNames returns the known names of the group's topics, by thread; the general topic (0) is
// there, maybe with no name, if the group has topics.
func (s *Store) TopicNames(ctx context.Context, chatID int64) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id, name FROM topics WHERE chat_id=?`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var t int
		var name string
		if err := rows.Scan(&t, &name); err != nil {
			return nil, err
		}
		out[t] = name
	}
	return out, rows.Err()
}
