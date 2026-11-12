package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func (e *env) notesAnswer(json string) { e.llm.set(func(f *fakeLLM) { f.notes = json }) }

func (e *env) note(userID int64) storage.Note {
	e.t.Helper()
	ns, err := e.st.Notes(context.Background(), chat, []int64{userID})
	if err != nil {
		e.t.Fatal(err)
	}
	return ns[userID]
}

func TestNotesAreUpdatedWithTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.notesAnswer(`{"people": [
		{"person": 1, "chat": "Ане охотно отвечают", "reputation": 2, "own": "любит джем"},
		{"person": 2, "chat": "", "reputation": -1, "own": "шутит"},
		{"person": 7, "chat": "чужой номер", "reputation": 1}]}`)
	e.makeLead(vasyaID, "Вася", "vasya")

	e.say(e.msg("я принесу варенье"))
	v := e.from(vasyaID, "Вася", "vasya", "лучше джем")
	e.say(v)
	r := e.from(vasyaID, "Вася", "vasya", "шучу")
	r.ReplyToID = v.MessageID
	e.say(r, e.msg("джем так джем"))
	e.chatter(3) // the first 4 messages leave the window
	e.tick()

	calls := e.llm.callsOf(llm.RoleNotes)
	if len(calls) != 1 || calls[0].prio != llm.Initiative {
		t.Fatalf("one background notes request expected: %+v", calls)
	}
	u := calls[0].user()
	if !strings.Contains(u, "1. Аня\n   (заметки ещё нет)") || !strings.Contains(u, "2. Вася (@vasya) [админ]\n") ||
		!strings.Contains(u, "Бот (это ты): Бот, @testbot") || !strings.Contains(u, "лучше джем") || strings.Contains(u, "реплика") {
		t.Fatalf("notes prompt must list the chunk's authors and hold its messages:\n%s", u)
	}
	if !strings.Contains(calls[0].system(), "не длиннее 300 символов") {
		t.Fatalf("note limit missing:\n%s", calls[0].system())
	}
	if n := e.note(anyaID); n.Chat != "Ане охотно отвечают" || n.Own != "любит джем" || n.Reputation != 1 {
		t.Fatalf("Аня's note %+v (reputation moves one step at a time)", n)
	}
	if n := e.note(vasyaID); n.Chat != "" || n.Own != "шутит" || n.Reputation != -1 || n.Username != "vasya" {
		t.Fatalf("Вася's note %+v", n)
	}

	// Notes go to replies: the person answered, the people they mention and those around.
	e.say(e.msg("бот, что скажешь про @vasya?"))
	replies := e.llm.callsOf(llm.RoleReply)
	sys := replies[0].system()
	if !strings.Contains(sys, "- Аня [уважают]. В чате: Ане охотно отвечают. Твоё впечатление: любит джем.\n") ||
		!strings.Contains(sys, "- Вася (@vasya) [не в чести]. Твоё впечатление: шутит.\n") ||
		!strings.Contains(sys, "мнение и просьбы уважаемых весят больше") ||
		strings.Index(sys, "- Аня") > strings.Index(sys, "- Вася") {
		t.Fatalf("reply prompt must carry the notes, the person answered first:\n%s", sys)
	}
	if !strings.Contains(replies[0].user(), "Аня [уважают]: бот, что скажешь про @vasya?") {
		t.Fatalf("history lines carry the reputation:\n%s", replies[0].user())
	}

	// …and to the evaluation: the notes of the new messages' authors.
	e.say(e.from(vasyaID, "Вася", "vasya", "ну что, джем?"))
	e.tick()
	evals := e.llm.callsOf(llm.RoleFilter)
	if last := evals[len(evals)-1].system(); !strings.Contains(last, "- Вася (@vasya) [не в чести]") || strings.Contains(last, "любит джем") {
		t.Fatalf("evaluation prompt must carry the notes of the new messages' authors only:\n%s", last)
	}

	// The next chunk starts from the notes as they are.
	e.notesAnswer(`{"people": [{"person": 1, "reputation": 2}]}`)
	e.chatter(4)
	e.tick()
	calls = e.llm.callsOf(llm.RoleNotes)
	if !strings.Contains(calls[len(calls)-1].user(), "Как относятся в чате: Ане охотно отвечают\n   Репутация: 1\n   Твоё впечатление: любит джем") {
		t.Fatalf("the previous note must be in the prompt:\n%s", calls[len(calls)-1].user())
	}
	if n := e.note(anyaID); n.Reputation != 2 || n.Own != "любит джем" {
		t.Fatalf("Аня's note after the second chunk %+v", n)
	}
}

func TestNotesFailureKeepsTheSummary(t *testing.T) {
	e := setup(t, smallWindow)
	e.decide(`{"rules": [], "actions": []}`)
	e.summarizeAnswer("Обсуждали джем.")
	e.notesAnswer("не знаю")
	e.chatter(7)
	e.tick()
	if s := e.summary(0); s.Text != "Обсуждали джем." {
		t.Fatalf("summary %+v", s)
	}
	if n := e.note(anyaID); !n.Empty() || len(e.llm.callsOf(llm.RoleNotes)) != 1 {
		t.Fatalf("no note from an unparsable answer: %+v", n)
	}
}

func TestNoteCommands(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.makeLead(vasyaID, "Вася", "vasya")
	e.makeAuthority(petyaID, "Петя")
	e.say(e.from(anyaID, "Аня", "anya", "всем привет"))
	if _, err := e.st.SetNote(ctx, storage.Note{ChatID: chat, UserID: anyaID, Name: "Аня", Username: "anya",
		Chat: "ей рады", Own: "любит джем", Reputation: 1}, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	sent := func() string {
		s, _ := e.out.snapshot()
		return s[len(s)-1].text
	}
	refusals := func() int { return len(e.llm.callsOf(llm.RoleReply)) }

	e.command(`{"command": "about", "person": 1}`)
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, что думаешь о @anya?"))
	if got := sent(); got != "Аня (@anya)\nРепутация в чате: +1 (уважают)\nКак к нему относятся: ей рады\nМоё впечатление: любит джем" {
		t.Fatalf("the admin sees the note: %q", got)
	}
	// The creator has no say over the group beyond its admins (SPEC 3.1, 2026-11-09).
	e.say(e.from(adminID, "Создатель", "", "бот, что думаешь о @anya?"))
	if r := e.llm.callsOf(llm.RoleReply); refusals() != 1 || !strings.Contains(r[0].user(), "только от админов чата") {
		t.Fatalf("the creator is refused: %+v", r)
	}
	e.say(e.from(petyaID, "Петя", "", "бот, что думаешь о @anya?"))
	if r := e.llm.callsOf(llm.RoleReply); refusals() != 2 || !strings.Contains(r[1].user(), "только с админом чата") {
		t.Fatalf("an authority is refused: %+v", r)
	}
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, что думаешь о @nobody?"))
	if got := sent(); got != "О @nobody у меня пока заметок нет." {
		t.Fatalf("unknown person: %q", got)
	}

	e.command(`{"command": "forget_person", "person": 1}`)
	e.say(e.from(adminID, "Создатель", "", "бот, забудь, что думаешь о @anya"))
	e.say(e.from(petyaID, "Петя", "", "бот, забудь, что думаешь о @anya"))
	if n := e.note(anyaID); n.Empty() || refusals() != 4 {
		t.Fatalf("only the admin erases notes: %+v, %d refusals", n, refusals())
	}
	in := e.from(vasyaID, "Вася", "vasya", "бот, забудь, что думаешь о @anya")
	e.say(in)
	if n := e.note(anyaID); !n.Empty() {
		t.Fatalf("erased: %+v", n)
	}
	if got := e.reactionsTo(in.MessageID); len(got) != 1 || got[0] != "🫡" {
		t.Fatalf("ack expected: %v", got)
	}
	e.say(e.from(vasyaID, "Вася", "vasya", "бот, забудь, что думаешь о @anya"))
	if r := e.llm.callsOf(llm.RoleReply); refusals() != 5 || !strings.Contains(r[4].user(), "заметок о нём у тебя и так нет") {
		t.Fatalf("nothing to erase: %+v", r)
	}
}

func TestApplyNote(t *testing.T) {
	rep := func(n int) *int { return &n }
	old := storage.Note{Chat: "ценят", Own: "басист", Reputation: 1}
	for _, c := range []struct {
		u    noteUpdate
		want storage.Note
	}{
		{noteUpdate{Reputation: rep(-2)}, storage.Note{Chat: "ценят", Own: "басист", Reputation: 0}},
		{noteUpdate{Reputation: rep(5)}, storage.Note{Chat: "ценят", Own: "басист", Reputation: 2}},
		{noteUpdate{Chat: " спорят ", Own: "  "}, storage.Note{Chat: "спорят", Own: "басист", Reputation: 1}},
		{noteUpdate{Own: strings.Repeat("я", 500)}, storage.Note{Chat: "ценят", Own: strings.Repeat("я", 300) + "…", Reputation: 1}},
	} {
		if got := applyNote(old, c.u, 300); got != c.want {
			t.Errorf("applyNote(%+v) = %+v, want %+v", c.u, got, c.want)
		}
	}
	if got := applyNote(storage.Note{Reputation: -2}, noteUpdate{Reputation: rep(-3)}, 300); got.Reputation != -2 {
		t.Errorf("reputation floor: %+v", got)
	}
}
