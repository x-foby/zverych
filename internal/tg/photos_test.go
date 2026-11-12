package tg

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestPhotoDownload(t *testing.T) {
	data := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "getFile") {
					fmt.Fprint(w, `{"ok":true,"result":{"file_id":"id","file_path":"photos/a.jpg"}}`)
					return
				}
				if r.URL.Path != "/file/bot123:secret/photos/a.jpg" {
					t.Errorf("wrong download path: %s", r.URL.Path)
				}
				if fail {
					w.WriteHeader(500)
					return
				}
				w.Write(data)
			}))
			defer s.Close()
			a, err := New("123:secret", Handlers{}, bot.WithServerURL(s.URL), bot.WithSkipGetMe())
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.Photo(context.Background(), "id")
			if fail {
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("unsafe error: %v", err)
				}
				return
			}
			if err != nil || got != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(data) {
				t.Fatalf("photo: %q %v", got, err)
			}
		})
	}
}

func TestConvertPhotoReferences(t *testing.T) {
	in, ok := convert(&models.Update{Message: &models.Message{ID: 2, Chat: models.Chat{ID: 1, Type: models.ChatTypeSupergroup}, From: &models.User{ID: 3}, Photo: []models.PhotoSize{{FileID: "small"}, {FileID: "large"}}, ReplyToMessage: &models.Message{ID: 1, Photo: []models.PhotoSize{{FileID: "parent"}}}}})
	if !ok || in.PhotoID != "large" || in.ReplyPhotoID != "parent" {
		t.Fatalf("photo references: %+v", in)
	}
}
