// Package transcribe converts voice messages to text via the Groq Whisper API.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Client transcribes audio via Groq's OpenAI-compatible endpoint.
type Client struct {
	apiKey string
	url    string
	model  string
	http   *http.Client
}

// New builds a Client. If apiKey is empty, Transcribe always returns an error.
func New(apiKey, model string) *Client {
	if model == "" {
		model = "whisper-large-v3"
	}
	return &Client{
		apiKey: apiKey,
		url:    "https://api.groq.com/openai/v1/audio/transcriptions",
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// transcriptionResponse is the subset of the Groq JSON we read.
type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe sends audio bytes to Groq and returns the recognized text.
// format is the file extension: "ogg", "mp3", "wav", etc.
func (c *Client) Transcribe(ctx context.Context, audio []byte, format string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("groq api key not configured")
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if err := writer.WriteField("model", c.model); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}
	if err := writer.WriteField("response_format", "json"); err != nil {
		return "", fmt.Errorf("write format field: %w", err)
	}
	if err := writer.WriteField("language", "ru"); err != nil {
		return "", fmt.Errorf("write language field: %w", err)
	}

	filename := "voice." + format
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &buf)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call groq: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("groq returned %s: %s", resp.Status, string(body))
	}

	var result transcriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Text, nil
}
