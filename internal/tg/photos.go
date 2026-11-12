package tg

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-telegram/bot"
)

const maxPhotoBytes = 10 << 20

// Photo returns image bytes as a data URL. The Telegram URL (containing the token)
// stays inside this adapter; neither providers nor logs receive it.
func (a *Adapter) Photo(ctx context.Context, id string) (string, error) {
	f, err := a.b.GetFile(ctx, &bot.GetFileParams{FileID: id})
	if err != nil {
		return "", errors.New("get photo metadata failed")
	}
	if f.FileSize > maxPhotoBytes {
		return "", errors.New("photo too large")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.b.FileDownloadLink(f), nil)
	if err != nil {
		return "", errors.New("invalid photo path")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("photo download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("photo download rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil || len(data) > maxPhotoBytes {
		return "", errors.New("photo download incomplete or too large")
	}
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
		return "", errors.New("unsupported photo format")
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
