package brain

import (
	"context"
	"testing"

	"github.com/vvchaschin/communitybot/internal/storage"
)

const newChat = int64(-200)

func (e *env) membership(ev Membership) {
	e.t.Helper()
	within(e.t, "membership", func() {
		e.b.OnMembership(context.Background(), ev)
		e.b.wait()
	})
}

// group returns the stored record of the group (ok: the bot knows it).
func (e *env) group(id int64) (storage.Chat, bool) {
	e.t.Helper()
	c, ok, err := e.st.Chat(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, ok
}

// setGroup puts a group into a state and mode, as the creator or an admin would.
func (e *env) setGroup(id int64, state, mode string) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.st.RegisterChat(ctx, id, "", adminID, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.SetChatState(ctx, id, state, mode); err != nil {
		e.t.Fatal(err)
	}
	e.b.dropChat(id)
}

func inChat(in Incoming, id int64) Incoming { in.ChatID = id; return in }

// stored reports how many messages of the group the bot keeps.
func (e *env) stored(id int64) int {
	e.t.Helper()
	ms, err := e.st.Recent(context.Background(), id, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(ms)
}

// Anyone may add the bot; a new group is off: nothing is stored, nothing is done (SPEC 3.1).
func TestAnyoneAddsTheBotAndItStaysOff(t *testing.T) {
	e := setup(t)
	e.membership(Membership{ChatID: newChat, ChatType: "supergroup", ChatTitle: "Новая", ByUserID: anyaID, ByUserName: "Аня", Joined: true})
	c, ok := e.group(newChat)
	if !ok || c.State != storage.ChatOff || c.Title != "Новая" || c.AddedBy != anyaID {
		t.Fatalf("a group added by anyone is known and off: %+v", c)
	}
	e.say(inChat(e.msg("@testbot привет"), newChat), inChat(e.msg("как дела"), newChat))
	e.b.OnReaction(context.Background(), Reaction{ChatID: newChat, ChatType: "supergroup", MessageID: 1, UserID: anyaID, Emojis: []string{"🔥"}})
	e.tick()
	if sent, reacts := e.out.snapshot(); len(sent) != 0 || len(reacts) != 0 {
		t.Fatalf("the bot does nothing in an off group: %+v %+v", sent, reacts)
	}
	if n := e.stored(newChat); n != 0 {
		t.Fatalf("an off group keeps no messages, got %d", n)
	}
	if len(e.llm.calls) != 0 {
		t.Fatalf("an off group costs no tokens: %+v", e.llm.calls)
	}
	// Added by the creator: off just the same — turning on is a separate step.
	e.membership(Membership{ChatID: -300, ChatType: "group", ByUserID: adminID, Joined: true})
	if c, _ := e.group(-300); c.State != storage.ChatOff {
		t.Fatalf("added by the creator: still off %+v", c)
	}
}

// A message from a group the bot was added to while it was down: the group is recorded, off.
func TestUnknownGroupIsRecordedOff(t *testing.T) {
	e := setup(t)
	in := inChat(e.msg("кто-нибудь тут есть вообще?"), newChat)
	in.ChatTitle = "Подвал"
	e.say(in)
	if c, ok := e.group(newChat); !ok || c.State != storage.ChatOff || c.Title != "Подвал" {
		t.Fatalf("unknown group: %+v %v", c, ok)
	}
	if sent, _ := e.out.snapshot(); len(sent) != 0 || e.stored(newChat) != 0 {
		t.Fatal("nothing is said or stored")
	}
}

// Removing the bot forgets everything about the group; adding it again starts from scratch, off.
func TestRemovalForgetsTheGroup(t *testing.T) {
	e := setup(t)
	e.makeLead(vasyaID, "Вася", "vasya")
	e.say(e.msg("привет"))
	e.membership(Membership{ChatID: chat, ChatType: "supergroup", ByUserID: anyaID, Removed: true})
	if _, ok := e.group(chat); ok {
		t.Fatal("removal forgets the group")
	}
	if e.stored(chat) != 0 || len(e.roles()) != 0 {
		t.Fatal("removal wipes the group's data")
	}
	e.membership(Membership{ChatID: chat, ChatType: "supergroup", ByUserID: adminID, Joined: true})
	if c, _ := e.group(chat); c.State != storage.ChatOff {
		t.Fatalf("re-added: off %+v", c)
	}
}

// A "joined" status echo (e.g. after a migration) keeps an active group active.
func TestJoinEchoKeepsTheState(t *testing.T) {
	e := setup(t)
	e.membership(Membership{ChatID: chat, ChatType: "supergroup", ByUserID: anyaID, Joined: true})
	if c, _ := e.group(chat); c.State != storage.ChatActive || c.Mode != storage.ModeTalk {
		t.Fatalf("kept %+v", c)
	}
	e.say(e.msg("@testbot ты тут?"))
	if sent, _ := e.out.snapshot(); len(sent) != 1 {
		t.Fatal("still works")
	}
}

func TestPrivateMembershipIgnored(t *testing.T) {
	e := setup(t)
	e.membership(Membership{ChatID: anyaID, ChatType: "private", ByUserID: anyaID, Joined: true})
	if _, ok := e.group(anyaID); ok {
		t.Fatal("private chats are not groups")
	}
}

func TestMigrateCarriesTheGroup(t *testing.T) {
	e := setup(t)
	e.say(e.msg("привет")) // caches the old id
	within(t, "migrate", func() { e.b.OnMigrate(context.Background(), chat, newChat) })
	if _, ok := e.group(chat); ok {
		t.Fatal("the old id is gone")
	}
	if c, ok := e.group(newChat); !ok || c.State != storage.ChatActive {
		t.Fatalf("the state moves to the supergroup id: %+v", c)
	}
	e.say(inChat(e.msg("@testbot ты тут?"), newChat))
	if sent, _ := e.out.snapshot(); len(sent) != 1 || e.out.sentTo[0] != newChat {
		t.Fatal("the bot keeps working after migration")
	}
	// Migration of a group the bot does not know does nothing.
	within(t, "migrate unknown", func() { e.b.OnMigrate(context.Background(), -300, -301) })
	if _, ok := e.group(-301); ok {
		t.Fatal("unknown groups do not appear through migration")
	}
}
