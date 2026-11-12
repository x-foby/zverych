package tg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/vvchaschin/communitybot/internal/brain"
	"github.com/vvchaschin/communitybot/internal/storage"
)

func group() models.Chat {
	return models.Chat{ID: -100, Type: models.ChatTypeSupergroup, Title: "Клуб"}
}

func TestConvert(t *testing.T) {
	anya := &models.User{ID: 42, FirstName: "Аня", LastName: "Петрова", Username: "anya"}
	botUser := &models.User{ID: 1, FirstName: "Бот", IsBot: true}

	cases := []struct {
		name string
		msg  *models.Message
		want brain.Incoming
		ok   bool
	}{
		{"no message", nil, brain.Incoming{}, false},
		{"no sender", &models.Message{ID: 1, Chat: group(), Text: "hi"}, brain.Incoming{}, false},
		{"other service message", &models.Message{ID: 1, Chat: group(), From: anya, PinnedMessage: &models.MaybeInaccessibleMessage{}}, brain.Incoming{}, false},
		{"only bots joined", &models.Message{ID: 1, Chat: group(), From: anya, NewChatMembers: []models.User{*botUser}}, brain.Incoming{}, false},
		{"a bot left", &models.Message{ID: 1, Chat: group(), From: anya, LeftChatMember: botUser}, brain.Incoming{}, false},
		{
			"joined by themselves, in a topic",
			&models.Message{ID: 1, Chat: group(), From: anya, NewChatMembers: []models.User{*anya}, IsTopicMessage: true, MessageThreadID: 4},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 1, ThreadID: 4, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "[вступил(а) в чат]", Kind: storage.KindJoin, Date: time.Unix(0, 0)},
			true,
		},
		{
			"several joined by link: bots skipped, the first person is the subject",
			&models.Message{ID: 2, Chat: group(), From: anya, NewChatMembers: []models.User{*anya, *botUser, {ID: 7, FirstName: "Вася"}}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 2, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "[вступили в чат: Аня Петрова (@anya), Вася]", Kind: storage.KindJoin, Date: time.Unix(0, 0)},
			true,
		},
		{
			"added by someone else",
			&models.Message{ID: 3, Chat: group(), From: anya, NewChatMembers: []models.User{{ID: 7, FirstName: "Вася"}}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 3, UserID: 7, UserName: "Вася",
				Text: "[Аня Петрова (@anya) добавил(а) в чат: Вася]", Kind: storage.KindJoin, Date: time.Unix(0, 0)},
			true,
		},
		{
			"left by themselves",
			&models.Message{ID: 4, Chat: group(), From: anya, LeftChatMember: anya},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 4, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "[вышел(а) из чата]", Kind: storage.KindLeave, Date: time.Unix(0, 0)},
			true,
		},
		{
			"removed by someone else",
			&models.Message{ID: 5, Chat: group(), From: anya, LeftChatMember: &models.User{ID: 7, FirstName: "Вася"}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 5, UserID: 7, UserName: "Вася",
				Text: "[удалён(а) из чата, удалил(а) Аня Петрова (@anya)]", Kind: storage.KindLeave, Date: time.Unix(0, 0)},
			true,
		},
		{
			"plain text",
			&models.Message{ID: 5, Chat: group(), From: anya, Date: 1_700_000_000, Text: "привет"},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 5, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "привет", Date: time.Unix(1_700_000_000, 0)},
			true,
		},
		{
			"first name only, reply to the bot in a forum topic",
			&models.Message{ID: 6, Chat: group(), From: &models.User{ID: 7, FirstName: "Вася"}, Text: "да",
				IsTopicMessage: true, MessageThreadID: 3, ReplyToMessage: &models.Message{ID: 4, From: botUser}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 6, ThreadID: 3, UserID: 7, UserName: "Вася",
				Text: "да", ReplyToID: 4, ReplyToUserID: 1, ReplyToUser: storage.Person{ID: 1, Name: "Бот"}, Date: time.Unix(0, 0)},
			true,
		},
		{
			"a plain message in a forum topic points at the topic creation: not a reply",
			&models.Message{ID: 8, Chat: group(), From: anya, Text: "всем привет", IsTopicMessage: true, MessageThreadID: 3,
				ReplyToMessage: &models.Message{ID: 3, From: botUser, ForumTopicCreated: &models.ForumTopicCreated{Name: "Гитары"}}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 8, ThreadID: 3, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "всем привет", TopicName: "Гитары", Date: time.Unix(0, 0)},
			true,
		},
		{
			"the general topic of a group with topics: no thread id",
			&models.Message{ID: 11, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup, Title: "Клуб", IsForum: true}, From: anya, Text: "йо"},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 11, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "йо", IsForum: true, Date: time.Unix(0, 0)},
			true,
		},
		{
			"thread id outside a topic is ignored; reply to a channel post without sender",
			&models.Message{ID: 7, Chat: group(), From: anya, Text: "ну", MessageThreadID: 3,
				ReplyToMessage: &models.Message{ID: 2}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 7, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "ну", ReplyToID: 2, Date: time.Unix(0, 0)},
			true,
		},
		{
			"reply to a service message about a newcomer is about the newcomer, not the one who added them",
			&models.Message{ID: 10, Chat: group(), From: anya, Text: "он главный",
				ReplyToMessage: &models.Message{ID: 9, From: anya, NewChatMembers: []models.User{{ID: 55, FirstName: "Евгений", Username: "evgen_jr"}}}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 10, UserID: 42, UserName: "Аня Петрова", Username: "anya",
				Text: "он главный", ReplyToID: 9, ReplyToUserID: 55, ReplyToUser: storage.Person{ID: 55, Name: "Евгений", Username: "evgen_jr"},
				Date: time.Unix(0, 0)},
			true,
		},
		{
			"bare sticker",
			&models.Message{ID: 9, Chat: group(), From: anya, Sticker: &models.Sticker{Emoji: "😀"}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 9, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "[стикер 😀]", IsMedia: true, Date: time.Unix(0, 0)},
			true,
		},
		{
			"text mentions in text and caption; plain @mentions are left to the brain",
			&models.Message{ID: 8, Chat: group(), From: anya, Photo: []models.PhotoSize{{}}, Caption: "Бот глянь",
				Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention, User: anya}, {Type: models.MessageEntityTypeTextMention}},
				CaptionEntities: []models.MessageEntity{{Type: models.MessageEntityTypeTextMention, User: botUser}}},
			brain.Incoming{ChatID: -100, ChatType: "supergroup", ChatTitle: "Клуб", MessageID: 8, UserID: 42, UserName: "Аня Петрова",
				Username: "anya", Text: "[фото] Бот глянь", IsMedia: true, HasCaption: true, TextMentions: []storage.Person{{ID: 1, Name: "Бот"}}, Date: time.Unix(0, 0)},
			true,
		},
	}
	for _, c := range cases {
		got, ok := convert(&models.Update{Message: c.msg})
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v %v\nwant %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestConvertReaction(t *testing.T) {
	anya := &models.User{ID: 42, FirstName: "Аня"}
	emoji := func(e string) models.ReactionType {
		return models.ReactionType{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: e}}
	}
	got, ok := convertReaction(&models.MessageReactionUpdated{Chat: group(), MessageID: 9, User: anya, Date: 100,
		OldReaction: []models.ReactionType{emoji("👎")},
		NewReaction: []models.ReactionType{emoji("❤"), {Type: models.ReactionTypeTypeCustomEmoji, ReactionTypeCustomEmoji: &models.ReactionTypeCustomEmoji{CustomEmojiID: "1"}},
			{Type: models.ReactionTypeTypePaid, ReactionTypePaid: &models.ReactionTypePaid{}}, {}}})
	want := brain.Reaction{ChatID: -100, ChatType: "supergroup", MessageID: 9, UserID: 42, Date: time.Unix(100, 0),
		Emojis: []string{"❤", "[свой эмодзи]", "⭐"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v %v want %+v", got, ok, want)
	}
	// Removing all reactions is an empty (not nil) set: the brain must clear the user's reactions.
	got, ok = convertReaction(&models.MessageReactionUpdated{Chat: group(), MessageID: 9, User: anya})
	if !ok || got.Emojis == nil || len(got.Emojis) != 0 {
		t.Errorf("removal: %+v %v", got, ok)
	}
	for name, u := range map[string]*models.MessageReactionUpdated{
		"anonymous": {Chat: group(), MessageID: 9, ActorChat: &models.Chat{ID: -100}},
		"bot":       {Chat: group(), MessageID: 9, User: &models.User{ID: 1, IsBot: true}},
	} {
		if _, ok := convertReaction(u); ok {
			t.Errorf("%s reaction must be skipped", name)
		}
	}
}

func TestMessageText(t *testing.T) {
	cases := []struct {
		msg   models.Message
		text  string
		media bool
	}{
		{models.Message{Text: "текст", Photo: []models.PhotoSize{{}}}, "текст", false},
		{models.Message{Sticker: &models.Sticker{Emoji: "😀"}}, "[стикер 😀]", true},
		{models.Message{Photo: []models.PhotoSize{{}}}, "[фото]", true},
		{models.Message{Video: &models.Video{}, Caption: "смотри"}, "[видео] смотри", true},
		{models.Message{Animation: &models.Animation{}}, "[гифка]", true},
		{models.Message{Voice: &models.Voice{}}, "[голосовое]", true},
		{models.Message{VideoNote: &models.VideoNote{}}, "[кружок]", true},
		{models.Message{Document: &models.Document{}, Caption: "отчёт"}, "[файл] отчёт", true},
		{models.Message{Caption: "подпись без медиа"}, "", false},
		{models.Message{}, "", false},
	}
	for i, c := range cases {
		if text, media := messageText(&c.msg); text != c.text || media != c.media {
			t.Errorf("case %d: got %q %v want %q %v", i, text, media, c.text, c.media)
		}
	}
}

// ---- adapter against a fake Bot API ----

type apiCall struct {
	method string
	form   map[string]string
}

type fakeAPI struct {
	mu      sync.Mutex
	calls   []apiCall
	fail    bool
	updates []any
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		form := map[string]string{}
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				form[k] = v[0]
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method, form})
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
			return
		}
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 1, "is_bot": true, "first_name": "Бот", "username": "testbot", "can_read_all_group_messages": true}
		case "getChatMember":
			status := "member"
			if form["user_id"] == "13" {
				status = "left"
			}
			result = map[string]any{"status": status, "user": map[string]any{"id": 12, "is_bot": false, "first_name": "Вася", "username": "vasya"}}
		case "editMessageText":
			result = map[string]any{"message_id": 5, "date": 0, "chat": map[string]any{"id": 42, "type": "private"}}
		case "getChatAdministrators":
			result = []map[string]any{
				{"status": "creator", "user": map[string]any{"id": 10, "is_bot": false, "first_name": "Владелец", "username": "boss"}, "is_anonymous": false},
				{"status": "administrator", "user": map[string]any{"id": 11, "is_bot": false, "first_name": "Модератор"}},
				{"status": "administrator", "user": map[string]any{"id": 1, "is_bot": true, "first_name": "Бот"}},
			}
		case "getUpdates":
			f.mu.Lock()
			pending := f.updates
			f.updates = nil
			f.mu.Unlock()
			if len(pending) == 0 {
				time.Sleep(10 * time.Millisecond) // long polling, but short
			}
			result = pending
		}
		if method == "sendMessage" {
			result = map[string]any{"message_id": 77, "date": 0, "chat": map[string]any{"id": -100, "type": "supergroup"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
}

func newAdapter(t *testing.T, onMessage func(context.Context, brain.Incoming)) (*Adapter, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{}
	s := httptest.NewServer(api.handler())
	t.Cleanup(s.Close)
	a, err := New("123:abc", Handlers{Message: onMessage}, bot.WithServerURL(s.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	return a, api
}

func TestAdapterOutgoing(t *testing.T) {
	a, api := newAdapter(t, func(context.Context, brain.Incoming) {})
	ctx := context.Background()

	id, err := a.Send(ctx, -100, 3, "привет", 0)
	if err != nil || id != 77 {
		t.Fatalf("send: %d %v", id, err)
	}
	if _, err := a.Send(ctx, -100, 0, "ответ", 5); err != nil {
		t.Fatal(err)
	}
	if err := a.React(ctx, -100, 5, "🔥"); err != nil {
		t.Fatal(err)
	}
	if err := a.Typing(ctx, -100, 3); err != nil {
		t.Fatal(err)
	}

	if len(api.calls) != 4 {
		t.Fatalf("calls %+v", api.calls)
	}
	plain, quoted, react, typing := api.calls[0], api.calls[1], api.calls[2], api.calls[3]
	if plain.method != "sendMessage" || plain.form["chat_id"] != "-100" || plain.form["text"] != "привет" ||
		plain.form["message_thread_id"] != "3" || plain.form["reply_parameters"] != "" {
		t.Errorf("plain send: %+v", plain)
	}
	var rp struct {
		MessageID                int  `json:"message_id"`
		AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
	}
	if err := json.Unmarshal([]byte(quoted.form["reply_parameters"]), &rp); err != nil || rp.MessageID != 5 || !rp.AllowSendingWithoutReply {
		t.Errorf("quoted send must reply to 5 and tolerate a deleted target: %+v (%v)", quoted, err)
	}
	if react.method != "setMessageReaction" || react.form["message_id"] != "5" || !strings.Contains(react.form["reaction"], `"emoji":"🔥"`) {
		t.Errorf("react: %+v", react)
	}
	if typing.method != "sendChatAction" || typing.form["action"] != "typing" || typing.form["message_thread_id"] != "3" {
		t.Errorf("typing: %+v", typing)
	}
}

func TestAdapterErrors(t *testing.T) {
	a, api := newAdapter(t, func(context.Context, brain.Incoming) {})
	api.fail = true
	ctx := context.Background()
	if id, err := a.Send(ctx, -100, 0, "x", 0); err == nil || id != 0 {
		t.Errorf("send: %d %v", id, err)
	}
	if err := a.React(ctx, -100, 1, "🔥"); err == nil {
		t.Error("react must report API errors")
	}
	if err := a.Typing(ctx, -100, 0); err == nil {
		t.Error("typing must report API errors")
	}
}

func TestAdapterDeliversConvertedUpdates(t *testing.T) {
	var got []brain.Incoming
	a, _ := newAdapter(t, func(_ context.Context, in brain.Incoming) { got = append(got, in) })
	ctx := context.Background()
	a.b.ProcessUpdate(ctx, &models.Update{Message: &models.Message{ID: 1, Chat: group(), From: &models.User{ID: 42, FirstName: "Аня"}, Text: "привет"}})
	a.b.ProcessUpdate(ctx, &models.Update{Message: &models.Message{ID: 2, Chat: group(), From: &models.User{ID: 42}}}) // service
	if len(got) != 1 || got[0].MessageID != 1 || got[0].Text != "привет" {
		t.Fatalf("delivered %+v", got)
	}
}

func TestNewRejectsBadToken(t *testing.T) {
	if _, err := New("", Handlers{}); err == nil {
		t.Fatal("empty token must be rejected")
	}
}

func TestAdapterMeAndPolling(t *testing.T) {
	got := make(chan brain.Incoming, 1)
	a, api := newAdapter(t, func(_ context.Context, in brain.Incoming) { got <- in })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	me, err := a.Me(ctx)
	if err != nil || me.ID != 1 || me.Username != "testbot" || !me.CanReadAllGroupMessages {
		t.Fatalf("me: %+v %v", me, err)
	}

	api.mu.Lock()
	api.updates = []any{map[string]any{"update_id": 1, "message": map[string]any{
		"message_id": 5, "date": 0, "text": "привет", "chat": map[string]any{"id": -100, "type": "supergroup"},
		"from": map[string]any{"id": 42, "is_bot": false, "first_name": "Аня"}}}}
	api.mu.Unlock()
	stopped := make(chan struct{})
	go func() { a.Start(ctx); close(stopped) }()
	select {
	case in := <-got:
		if in.MessageID != 5 || in.Text != "привет" || in.UserID != 42 {
			t.Fatalf("delivered %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not deliver a polled update")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop on cancel")
	}
}

func member(t models.ChatMemberType) models.ChatMember { return models.ChatMember{Type: t} }

func TestConvertMembership(t *testing.T) {
	admin := models.User{ID: 7, FirstName: "Вова", LastName: "А", Username: "vova"}
	restricted := func(isMember bool) models.ChatMember {
		return models.ChatMember{Type: models.ChatMemberTypeRestricted, Restricted: &models.ChatMemberRestricted{IsMember: isMember}}
	}
	upd := func(old, new models.ChatMember) *models.ChatMemberUpdated {
		return &models.ChatMemberUpdated{Chat: models.Chat{ID: -100, Type: models.ChatTypeGroup, Title: "Клуб"},
			From: admin, OldChatMember: old, NewChatMember: new}
	}
	cases := []struct {
		name             string
		u                *models.ChatMemberUpdated
		ok, joined, gone bool
	}{
		{"added", upd(member(models.ChatMemberTypeLeft), member(models.ChatMemberTypeMember)), true, true, false},
		{"added as admin", upd(member(models.ChatMemberTypeBanned), member(models.ChatMemberTypeAdministrator)), true, true, false},
		{"removed", upd(member(models.ChatMemberTypeMember), member(models.ChatMemberTypeLeft)), true, false, true},
		{"banned owner", upd(member(models.ChatMemberTypeOwner), member(models.ChatMemberTypeBanned)), true, false, true},
		{"promoted", upd(member(models.ChatMemberTypeMember), member(models.ChatMemberTypeAdministrator)), false, false, false},
		{"restricted but member", upd(member(models.ChatMemberTypeMember), restricted(true)), false, false, false},
		{"restricted, not member", upd(restricted(false), member(models.ChatMemberTypeMember)), true, true, false},
		{"restricted without details", upd(models.ChatMember{Type: models.ChatMemberTypeRestricted}, member(models.ChatMemberTypeMember)), true, true, false},
	}
	for _, c := range cases {
		ev, ok := convertMembership(c.u)
		if ok != c.ok || ev.Joined != c.joined || ev.Removed != c.gone {
			t.Errorf("%s: ok=%v %+v", c.name, ok, ev)
			continue
		}
		if ok && (ev.ChatID != -100 || ev.ChatType != "group" || ev.ChatTitle != "Клуб" || ev.ByUserID != 7 || ev.ByUserName != "Вова А (@vova)") {
			t.Errorf("%s: fields %+v", c.name, ev)
		}
	}
	noUsername := upd(member(models.ChatMemberTypeLeft), member(models.ChatMemberTypeMember))
	noUsername.From = models.User{ID: 8, FirstName: "Аня"}
	if ev, _ := convertMembership(noUsername); ev.ByUserName != "Аня" {
		t.Errorf("name without username: %q", ev.ByUserName)
	}
}

func TestDispatch(t *testing.T) {
	var msgs []brain.Incoming
	var evs []brain.Membership
	var migr [][2]int64
	var reacts []brain.Reaction
	var cbs []brain.Callback
	var topics []brain.Topic
	h := Handlers{
		Topic:      func(_ context.Context, tp brain.Topic) { topics = append(topics, tp) },
		Callback:   func(_ context.Context, cb brain.Callback) { cbs = append(cbs, cb) },
		Message:    func(_ context.Context, in brain.Incoming) { msgs = append(msgs, in) },
		Membership: func(_ context.Context, ev brain.Membership) { evs = append(evs, ev) },
		Migrate:    func(_ context.Context, from, to int64) { migr = append(migr, [2]int64{from, to}) },
		Reaction:   func(_ context.Context, r brain.Reaction) { reacts = append(reacts, r) },
	}
	ctx := context.Background()
	anya := &models.User{ID: 42, FirstName: "Аня"}
	h.dispatch(ctx, &models.Update{MyChatMember: &models.ChatMemberUpdated{Chat: group(), From: *anya,
		OldChatMember: member(models.ChatMemberTypeLeft), NewChatMember: member(models.ChatMemberTypeMember)}})
	h.dispatch(ctx, &models.Update{MyChatMember: &models.ChatMemberUpdated{Chat: group(), From: *anya, // promotion: ignored
		OldChatMember: member(models.ChatMemberTypeMember), NewChatMember: member(models.ChatMemberTypeAdministrator)}})
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 1, Chat: models.Chat{ID: -5, Type: models.ChatTypeGroup}, MigrateToChatID: -1005}})
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 1, Chat: models.Chat{ID: -1005, Type: models.ChatTypeSupergroup}, MigrateFromChatID: -5}})
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 2, Chat: group(), From: anya, Text: "привет"}})
	h.dispatch(ctx, &models.Update{MessageReaction: &models.MessageReactionUpdated{Chat: group(), MessageID: 2, User: anya}})
	h.dispatch(ctx, &models.Update{MessageReaction: &models.MessageReactionUpdated{Chat: group(), MessageID: 2}}) // anonymous: skipped
	h.dispatch(ctx, &models.Update{CallbackQuery: &models.CallbackQuery{ID: "q1", From: models.User{ID: 42, FirstName: "Аня", Username: "anya"}, Data: "adm:-100:50:y",
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 9, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}}}}})
	h.dispatch(ctx, &models.Update{CallbackQuery: &models.CallbackQuery{ID: "q2", From: *anya, Data: "x"}}) // the message is gone: skipped
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 30, Chat: group(), From: anya, IsTopicMessage: true, MessageThreadID: 30,
		ForumTopicCreated: &models.ForumTopicCreated{Name: "Гитары"}}})
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 31, Chat: group(), From: anya, IsTopicMessage: true, MessageThreadID: 30,
		ForumTopicEdited: &models.ForumTopicEdited{Name: "Гитары и басы"}}})
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 32, Chat: group(), From: anya, IsTopicMessage: true, MessageThreadID: 30,
		ForumTopicEdited: &models.ForumTopicEdited{IconCustomEmojiID: "5"}}}) // only the icon: skipped
	h.dispatch(ctx, &models.Update{Message: &models.Message{ID: 33, Chat: group(), From: anya,
		ForumTopicEdited: &models.ForumTopicEdited{Name: "Главная"}}}) // General is renamed without a thread id
	wantTopics := []brain.Topic{
		{ChatID: -100, ChatType: "supergroup", ThreadID: 30, Name: "Гитары"},
		{ChatID: -100, ChatType: "supergroup", ThreadID: 30, Name: "Гитары и басы"},
		{ChatID: -100, ChatType: "supergroup", ThreadID: 0, Name: "Главная"},
	}
	if !reflect.DeepEqual(topics, wantTopics) {
		t.Errorf("topics: %+v", topics)
	}
	if len(cbs) != 1 || cbs[0] != (brain.Callback{ID: "q1", UserID: 42, Username: "anya", ChatID: 7, MessageID: 9, Data: "adm:-100:50:y"}) {
		t.Errorf("callbacks: %+v", cbs)
	}
	if len(reacts) != 1 || reacts[0].MessageID != 2 || reacts[0].UserID != 42 {
		t.Errorf("reactions: %+v", reacts)
	}
	if len(evs) != 1 || !evs[0].Joined || evs[0].ByUserID != 42 {
		t.Errorf("membership: %+v", evs)
	}
	if !reflect.DeepEqual(migr, [][2]int64{{-5, -1005}, {-5, -1005}}) {
		t.Errorf("migrate: %v", migr)
	}
	if len(msgs) != 1 || msgs[0].Text != "привет" {
		t.Errorf("messages: %+v", msgs)
	}

	// Missing handlers are skipped, not called.
	var empty Handlers
	empty.dispatch(ctx, &models.Update{MyChatMember: &models.ChatMemberUpdated{Chat: group(), From: *anya,
		OldChatMember: member(models.ChatMemberTypeLeft), NewChatMember: member(models.ChatMemberTypeMember)}})
	empty.dispatch(ctx, &models.Update{Message: &models.Message{Chat: group(), MigrateToChatID: -1}})
	empty.dispatch(ctx, &models.Update{Message: &models.Message{Chat: group(), MigrateFromChatID: -1}})
	empty.dispatch(ctx, &models.Update{Message: &models.Message{ID: 2, Chat: group(), From: anya, Text: "x"}})
	empty.dispatch(ctx, &models.Update{MessageReaction: &models.MessageReactionUpdated{Chat: group(), MessageID: 2, User: anya}})
	empty.dispatch(ctx, &models.Update{Message: &models.Message{ID: 30, Chat: group(), From: anya, ForumTopicCreated: &models.ForumTopicCreated{Name: "x"}}})
	empty.dispatch(ctx, &models.Update{CallbackQuery: &models.CallbackQuery{ID: "q1", From: *anya,
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 9, Chat: models.Chat{ID: 7}}}}})
}

func TestAdapterAllowedUpdates(t *testing.T) {
	a, api := newAdapter(t, func(context.Context, brain.Incoming) {})
	ctx, cancel := context.WithCancel(context.Background())
	go a.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	var updatesForm map[string]string
	for updatesForm == nil && time.Now().Before(deadline) {
		api.mu.Lock()
		for _, c := range api.calls {
			if c.method == "getUpdates" {
				updatesForm = c.form
			}
		}
		api.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	for _, u := range []string{`"message"`, `"my_chat_member"`, `"message_reaction"`, `"callback_query"`} {
		if !strings.Contains(updatesForm["allowed_updates"], u) {
			t.Errorf("must subscribe to %s: %+v", u, updatesForm)
		}
	}
}

func TestAdapterAdmins(t *testing.T) {
	a, api := newAdapter(t, func(context.Context, brain.Incoming) {})
	ps, err := a.Admins(context.Background(), -100)
	if err != nil || !slices.Equal(ps, []storage.Person{{ID: 10, Name: "Владелец", Username: "boss"}, {ID: 11, Name: "Модератор"}, {ID: 1, Name: "Бот"}}) {
		t.Fatalf("admins %v %v", ps, err)
	}
	api.mu.Lock()
	call := api.calls[0]
	api.fail = true
	api.mu.Unlock()
	if call.method != "getChatAdministrators" || call.form["chat_id"] != "-100" {
		t.Errorf("call %+v", call)
	}
	if ps, err := a.Admins(context.Background(), -100); err == nil || ps != nil {
		t.Errorf("errors are reported: %v %v", ps, err)
	}
}

// The private chat's tools (SPEC 3.2): buttons, editing the question, answering the press, membership.
func TestAdapterPrivateTools(t *testing.T) {
	a, api := newAdapter(t, func(context.Context, brain.Incoming) {})
	ctx := context.Background()
	id, err := a.SendButtons(ctx, 42, "Это он?", [][]brain.Button{{{Text: "Да", Data: "adm:-100:12:y"}, {Text: "Нет", Data: "adm:-100:12:n"}}})
	if err != nil || id != 77 {
		t.Fatalf("buttons: %d %v", id, err)
	}
	if err := a.Edit(ctx, 42, 77, "Готово"); err != nil {
		t.Fatal(err)
	}
	if err := a.AnswerCallback(ctx, "q1", ""); err != nil {
		t.Fatal(err)
	}
	p, member, err := a.Member(ctx, -100, 12)
	if err != nil || !member || p != (storage.Person{ID: 12, Name: "Вася", Username: "vasya"}) {
		t.Fatalf("member: %+v %v %v", p, member, err)
	}
	if _, member, err := a.Member(ctx, -100, 13); err != nil || member {
		t.Fatalf("left the group: not a member %v %v", member, err)
	}
	api.mu.Lock()
	calls := slices.Clone(api.calls)
	api.mu.Unlock()
	send, edit, answer, get := calls[0], calls[1], calls[2], calls[3]
	if send.method != "sendMessage" || send.form["chat_id"] != "42" ||
		!strings.Contains(send.form["reply_markup"], `{"text":"Да","callback_data":"adm:-100:12:y"}`) {
		t.Errorf("send with buttons: %+v", send)
	}
	if edit.method != "editMessageText" || edit.form["message_id"] != "77" || edit.form["text"] != "Готово" ||
		!strings.Contains(edit.form["reply_markup"], `"inline_keyboard":[]`) {
		t.Errorf("edit drops the buttons: %+v", edit)
	}
	if answer.method != "answerCallbackQuery" || answer.form["callback_query_id"] != "q1" {
		t.Errorf("answer: %+v", answer)
	}
	if get.method != "getChatMember" || get.form["chat_id"] != "-100" || get.form["user_id"] != "12" {
		t.Errorf("member: %+v", get)
	}

	api.mu.Lock()
	api.fail = true
	api.mu.Unlock()
	if _, err := a.SendButtons(ctx, 42, "x", nil); err == nil {
		t.Error("send errors are reported")
	}
	if err := a.Edit(ctx, 42, 1, "x"); err == nil {
		t.Error("edit errors are reported")
	}
	if err := a.AnswerCallback(ctx, "q", ""); err == nil {
		t.Error("answer errors are reported")
	}
	if _, _, err := a.Member(ctx, -100, 12); err == nil {
		t.Error("member errors are reported")
	}
}
