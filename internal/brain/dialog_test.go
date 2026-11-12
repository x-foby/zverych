package brain

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vvchaschin/communitybot/internal/config"
	"github.com/vvchaschin/communitybot/internal/llm"
)

// dialogEnv: Аня has just talked with the bot (its thread is 1).
func dialogEnv(t *testing.T, wait float64) *env {
	e := setup(t, func(c *config.Config) {
		c.Behavior.BatchIntervalSeconds = 3600
		c.Behavior.DialogWaitSeconds = wait
	})
	e.say(e.msg("бот, где лучше записывать барабаны?"))
	return e
}

// hold makes requests of the role wait until the returned func is called.
func (e *env) hold(role llm.Role) (release func()) {
	gate := make(chan struct{})
	e.llm.set(func(f *fakeLLM) {
		if f.hold == nil {
			f.hold = map[llm.Role]chan struct{}{}
			f.started = make(chan llm.Role, 10)
		}
		f.hold[role] = gate
	})
	return func() { close(gate) }
}

func (e *env) started(role llm.Role) {
	e.t.Helper()
	select {
	case r := <-e.llm.started:
		if r != role {
			e.t.Fatalf("held request: %s, want %s", r, role)
		}
	case <-time.After(3 * time.Second):
		e.t.Fatalf("no %s request", role)
	}
}

func (e *env) deliver(in Incoming) { e.b.OnMessage(context.Background(), in) }

func (e *env) settle() {
	e.t.Helper()
	within(e.t, "dialog handlers", e.b.wait)
}

// The bot waits for the person to fall silent; each new message starts the wait over (2026-11-09).
func TestDialogWaitsForSilence(t *testing.T) {
	e := dialogEnv(t, 15)
	release, waiting := make(chan struct{}), make(chan struct{}, 10)
	e.b.sleep = func(ctx context.Context, d time.Duration) {
		if d != 15*time.Second {
			return // reply delays
		}
		waiting <- struct{}{}
		select {
		case <-ctx.Done():
		case <-release:
		}
	}
	q1, q2 := e.msg("ну"), e.msg("ты про какой кабинет?")
	e.decide(joining(acts(act(q2.MessageID, `"reply":true`)), q2.MessageID, 1))
	e.deliver(q1)
	<-waiting
	e.deliver(q2)
	<-waiting
	if n := len(e.llm.callsOf(llm.RoleFilter)); n != 0 {
		t.Fatalf("evaluated before the person fell silent: %d", n)
	}
	close(release)
	e.settle()
	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) != 1 || !strings.Contains(f[0].user(), "ну") || !strings.Contains(f[0].user(), "какой кабинет") {
		t.Fatalf("one evaluation of both messages expected: %d calls", len(f))
	}
	if got := e.sentTo(q2.MessageID); len(got) != 1 {
		t.Fatalf("reply: %v", got)
	}
}

// A new message cancels the evaluation request under way; its messages are evaluated again.
func TestDialogCancelsTheEvaluation(t *testing.T) {
	e := dialogEnv(t, 0)
	release := e.hold(llm.RoleFilter)
	q1, q2 := e.msg("ну"), e.msg("ты про какой кабинет?")
	e.decide(joining(acts(act(q2.MessageID, `"reply":true`)), q2.MessageID, 1))
	e.deliver(q1)
	e.started(llm.RoleFilter)
	e.deliver(q2) // cancels the first request
	e.started(llm.RoleFilter)
	release()
	e.settle()
	f := e.llm.callsOf(llm.RoleFilter)
	if len(f) != 1 || !strings.Contains(f[0].user(), "[id="+strconv.Itoa(q1.MessageID)+"] Аня: ну") {
		t.Fatalf("the cancelled evaluation's messages are evaluated again: %d calls\n%v", len(f), f)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 2 || e.sentTo(q2.MessageID) == nil {
		t.Fatalf("one dialog reply expected: %v", sent)
	}
}

// A new message cancels the reply being written: the bot does not answer what was said before it.
func TestDialogCancelsTheReply(t *testing.T) {
	e := dialogEnv(t, 0)
	release := e.hold(llm.RoleReply)
	q1, q2 := e.msg("ну"), e.msg("ты про какой кабинет?")
	e.decide(joining(acts(act(q1.MessageID, `"reply":true`)), q1.MessageID, 1))
	e.deliver(q1)
	e.started(llm.RoleReply)
	e.decide(joining(acts(act(q2.MessageID, `"reply":true`)), q2.MessageID, 1))
	e.deliver(q2)
	e.started(llm.RoleReply)
	release()
	e.settle()
	if got := e.sentTo(q1.MessageID); len(got) != 0 {
		t.Fatalf("the cancelled reply was sent: %v", got)
	}
	if got := e.sentTo(q2.MessageID); len(got) != 1 {
		t.Fatalf("reply to the last message: %v", got)
	}
}

// Once the bot has acted in the chat (here: a reaction), it finishes what it started.
func TestDialogIsNotCancelledAfterActing(t *testing.T) {
	e := dialogEnv(t, 0)
	release := e.hold(llm.RoleReply)
	q1, q2 := e.msg("ну"), e.msg("ты про какой кабинет?")
	e.decide(joining(acts(act(q1.MessageID, `"reply":true,"react":"👍"`)), q1.MessageID, 1))
	e.deliver(q1)
	e.started(llm.RoleReply) // the reaction is already there
	e.decide(acts())
	e.deliver(q2)
	release()
	e.settle()
	if got := e.sentTo(q1.MessageID); len(got) != 1 {
		t.Fatalf("a run that has acted is finished: %v", got)
	}
	if f := e.llm.callsOf(llm.RoleFilter); len(f) != 2 || !strings.Contains(f[1].user(), "какой кабинет") {
		t.Fatalf("the new message is evaluated after it: %d calls", len(f))
	}
}
