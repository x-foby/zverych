package llm

import (
	"encoding/json"
	"testing"
)

func TestImageContent(t *testing.T) {
	for _, m := range []Message{{Role: "user", Content: "text"}, {Role: "user", Content: "photo", Images: []string{"data:image/jpeg;base64,YQ=="}}} {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Role    string
			Content json.RawMessage
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if len(m.Images) == 0 {
			var text string
			if err := json.Unmarshal(wire.Content, &text); err != nil || text != m.Content {
				t.Fatalf("text changed: %s", raw)
			}
		} else {
			var parts []struct {
				Type     string
				Text     string
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if err := json.Unmarshal(wire.Content, &parts); err != nil {
				t.Fatal(err)
			}
			if len(parts) != 2 || parts[0].Text != m.Content || parts[1].Type != "image_url" || parts[1].ImageURL.URL != m.Images[0] {
				t.Fatalf("bad image parts: %s", raw)
			}
		}
	}
}
