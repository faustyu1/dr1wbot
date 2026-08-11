// Package imagegen generates pictures through Google AI Studio's
// OpenAI-compatible images endpoint.
//
// Google's image models have no free tier, so this path only works on a key
// attached to a billed project — which is why it is off by default and why a
// refusal for quota is reported as "no credits" rather than "try later".
//
// The picture comes back as base64 in the response body rather than a URL,
// which is why callers have to hand the decoded bytes to Telegram themselves.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dr1wbot/internal/keyring"
)

// Generator turns a prompt into image bytes.
type Generator interface {
	Generate(ctx context.Context, prompt string) ([]byte, error)
}

// ErrOutOfCredits means no key could pay for the request. It is worth
// distinguishing because the user-facing advice is completely different from a
// refused prompt: enable billing, rather than rephrase.
var ErrOutOfCredits = errors.New("out of credits")

// Client calls the images endpoint.
type Client struct {
	http    *http.Client
	baseURL string
	keys    *keyring.Ring
	model   string
	size    string
}

// Options configures a Client.
type Options struct {
	BaseURL string
	// APIKeys is the rotation pool, same idea as the chat client's.
	APIKeys     []string
	KeyCooldown time.Duration
	Model       string
	// Size is the requested resolution, e.g. "1024x1024". Empty leaves it to
	// the model's default.
	Size       string
	Timeout    time.Duration
	HTTPClient *http.Client // optional; tests inject their own
}

// New builds a Client.
func New(opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	cooldown := opts.KeyCooldown
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	return &Client{
		http:    httpClient,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		keys:    keyring.New(opts.APIKeys, cooldown),
		model:   opts.Model,
		size:    opts.Size,
	}
}

type request struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
	Size           string `json:"size,omitempty"`
}

type response struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// errBodyLimit caps how much of a failed response we read into an error message.
const errBodyLimit = 512

// Generate produces one picture for prompt, walking the key pool if a quota
// refuses.
func (c *Client) Generate(ctx context.Context, prompt string) ([]byte, error) {
	if c.keys.Len() == 0 {
		return nil, fmt.Errorf("no api key configured")
	}

	payload, err := json.Marshal(request{
		Model:          c.model,
		Prompt:         prompt,
		N:              1,
		ResponseFormat: "b64_json",
		Size:           c.size,
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var lastErr error
	for _, lease := range c.keys.Lease() {
		picture, err := c.call(ctx, lease.Key, payload)
		if err == nil {
			c.keys.Works(lease.Index)
			return picture, nil
		}
		lastErr = err
		if !errors.Is(err, ErrOutOfCredits) {
			return nil, err // a refused prompt is refused on every key
		}
		c.keys.Limit(lease.Index)
		if ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// call is one attempt with one key.
func (c *Client) call(ctx context.Context, apiKey string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/images/generations", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		err := fmt.Errorf("image model returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
		switch resp.StatusCode {
		case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
			// On AI Studio all three mean the same thing in practice: this key
			// has nothing to spend on pictures.
			return nil, errors.Join(ErrOutOfCredits, err)
		}
		return nil, err
	}

	var parsed response
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("image model error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		// Image models refuse by answering with nothing, so this is the usual
		// shape of a declined prompt rather than a transport failure.
		return nil, fmt.Errorf("image model returned no picture")
	}

	if b64 := parsed.Data[0].B64JSON; b64 != "" {
		return decodeBase64(b64)
	}
	return decodeDataURI(parsed.Data[0].URL)
}

// decodeDataURI unpacks a "data:image/png;base64,...." value. The endpoint is
// asked for b64_json, but a compatibility layer is free to answer with a url.
func decodeDataURI(uri string) ([]byte, error) {
	if uri == "" {
		return nil, fmt.Errorf("image model returned an empty image url")
	}
	_, encoded, found := strings.Cut(uri, ",")
	if !found || !strings.HasPrefix(uri, "data:") {
		return nil, fmt.Errorf("unsupported image url format")
	}
	return decodeBase64(encoded)
}

func decodeBase64(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("decoded image is empty")
	}
	return data, nil
}
