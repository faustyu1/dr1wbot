// Package tgfile downloads files that arrived in a Telegram message.
package tgfile

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/mymmrac/telego"
)

// MaxBytes caps a download. Base64 inflates a picture by a third before it
// reaches the model, and an oversized attachment is not worth the tokens.
const MaxBytes = 8 << 20

// Downloader fetches file contents by file_id.
type Downloader struct {
	bot  *telego.Bot
	http *http.Client
}

// New builds a Downloader.
func New(bot *telego.Bot, client *http.Client) *Downloader {
	if client == nil {
		client = http.DefaultClient
	}
	return &Downloader{bot: bot, http: client}
}

// Download resolves fileID and returns the bytes behind it.
func (d *Downloader) Download(ctx context.Context, fileID string) ([]byte, error) {
	file, err := d.bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", err)
	}
	if file.FilePath == "" {
		return nil, fmt.Errorf("telegram returned no path for %s", fileID)
	}
	if file.FileSize > MaxBytes {
		return nil, fmt.Errorf("file is %d bytes, over the %d limit", file.FileSize, MaxBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.bot.FileDownloadURL(file.FilePath), nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}

	// Guard against a wrong Content-Length as well as a missing one.
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read download: %w", err)
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("file exceeds the %d byte limit", MaxBytes)
	}
	return data, nil
}
