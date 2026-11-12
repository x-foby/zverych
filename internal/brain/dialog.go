package brain

import (
	"context"
	"log/slog"
	"time"

	"github.com/vvchaschin/communitybot/internal/storage"
)

// Dialog (SPEC 4.4, 7a): someone talking with the bot who writes on without its name or a reply is
// not kept waiting for the batch. People often write in several short messages, and Telegram does
// not tell a bot that someone is typing, so the bot waits for dialog_wait_seconds of their silence
// and then evaluates the chat. Their next message meanwhile starts the wait over and cancels the
// evaluation under way — the model request, the reply being written, the typing pause — as long as
// the bot has not done anything visible in the chat yet (see commit).

type dialogKey struct{ chat, user int64 }

type dialogRun struct {
	cancel context.CancelFunc // nil once the run has acted in the chat: it is no longer cancelled
	ended  bool               // cancelled: may not act
}

type commitKey struct{}

// dialogStarted reports whether a dialog evaluation for the author is waiting or under way and may
// still be cancelled.
func (b *Brain) dialogStarted(m storage.Message) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.dialogs[dialogKey{m.ChatID, m.UserID}]
	return r != nil && r.cancel != nil
}

// dialog (re)starts the dialog evaluation for m's author: the previous one, if not acted yet, is
// cancelled and the wait for their silence begins anew.
func (b *Brain) dialog(ctx context.Context, m storage.Message) {
	k := dialogKey{m.ChatID, m.UserID}
	dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.mu.Lock()
	if r := b.dialogs[k]; r != nil && r.cancel != nil {
		r.cancel()
		r.ended = true
		slog.Info("dialog: new message, evaluation restarted", "chat_id", m.ChatID, "message_id", m.MessageID)
	}
	run := &dialogRun{cancel: cancel}
	b.dialogs[k] = run
	b.mu.Unlock()

	b.spawn(func() {
		defer b.endDialog(k, run, cancel)
		b.sleep(dctx, time.Duration(b.cfg.DialogWaitSeconds*float64(time.Second)))
		if dctx.Err() != nil {
			return
		}
		l := b.lock(m.ChatID)
		l.Lock() // the bot may be answering this person right now
		defer l.Unlock()
		if dctx.Err() != nil {
			return
		}
		slog.Info("dialog: evaluating now", "chat_id", m.ChatID, "message_id", m.MessageID)
		b.evaluateLocked(context.WithValue(dctx, commitKey{}, func() bool { return b.commitDialog(run) }), m.ChatID)
	})
}

func (b *Brain) endDialog(k dialogKey, run *dialogRun, cancel context.CancelFunc) {
	b.mu.Lock()
	if b.dialogs[k] == run {
		delete(b.dialogs, k)
	}
	b.mu.Unlock()
	cancel()
}

// commitDialog makes the run uncancellable: it is about to act in the chat. False — it has been
// cancelled. A run that has acted stays so even after a newer one replaces it in b.dialogs.
func (b *Brain) commitDialog(r *dialogRun) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.ended {
		return false
	}
	r.cancel = nil
	return true
}

// commit is called right before the bot does something visible in the chat (a reaction, a
// message). In a dialog evaluation it stops later messages from cancelling the run, so the bot does
// not stop halfway; false — the run was cancelled and must not act. Elsewhere it is always true.
func (b *Brain) commit(ctx context.Context) bool {
	if f, ok := ctx.Value(commitKey{}).(func() bool); ok {
		return f() && ctx.Err() == nil
	}
	return true
}
