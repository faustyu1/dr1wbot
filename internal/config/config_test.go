package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// env builds a Getenv over a map, starting from a valid minimal configuration
// that individual tests override.
func env(overrides map[string]string) Getenv {
	vars := map[string]string{
		"TELEGRAM_BOT_TOKEN": "123:abc",
		"GOOGLE_API_KEYS":    "sk-test",
	}
	for k, v := range overrides {
		if v == "" {
			delete(vars, k)
			continue
		}
		vars[k] = v
	}
	return func(key string) string { return vars[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.BaseURL != googleBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, googleBaseURL)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "sk-test" {
		t.Errorf("APIKeys = %v, want the single configured key", cfg.APIKeys)
	}
	if strings.Join(cfg.Models, ",") != strings.Join(defaultModels, ",") {
		t.Errorf("Models = %v, want the built-in defaults %v", cfg.Models, defaultModels)
	}
	if cfg.KeyCooldown != time.Minute {
		t.Errorf("KeyCooldown = %s, want 1m", cfg.KeyCooldown)
	}
	if cfg.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q, want low so thinking does not eat the budget", cfg.ReasoningEffort)
	}
	if cfg.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", cfg.MaxTokens)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %s, want 30s", cfg.Timeout)
	}
	if cfg.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", cfg.MaxConcurrent)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.SystemPrompt == "" {
		t.Error("SystemPrompt is empty, want the built-in default")
	}
	if cfg.StateFile != "state.json" {
		t.Errorf("StateFile = %q, want the default state.json", cfg.StateFile)
	}
}

func TestLoadParsesTheKeyPool(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GOOGLE_API_KEYS": " one , two,, one ,three,",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Blanks, a trailing comma and a repeat are all normal in a pasted list;
	// a duplicate key would otherwise pretend to be a second quota.
	if got := strings.Join(cfg.APIKeys, "|"); got != "one|two|three" {
		t.Errorf("APIKeys = %v, want the list trimmed and deduplicated", cfg.APIKeys)
	}
}

func TestLoadParsesTheModelList(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"LLM_MODEL": "gemini-3.6-flash, gemini-3.5-flash",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := strings.Join(cfg.Models, "|"); got != "gemini-3.6-flash|gemini-3.5-flash" {
		t.Errorf("Models = %v, want the fallback order preserved", cfg.Models)
	}
}

func TestLoadImageEndpointDefaultsToTheChatOne(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"IMAGE_MODEL":           "gemini-3.1-flash-image",
		"IMAGE_STORAGE_CHAT_ID": "111",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ImageBaseURL != googleBaseURL || strings.Join(cfg.ImageAPIKeys, ",") != "sk-test" {
		t.Errorf("image endpoint = %q/%v, want the chat endpoint's %q/[sk-test]",
			cfg.ImageBaseURL, cfg.ImageAPIKeys, googleBaseURL)
	}
}

func TestLoadImageEndpointOverride(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"IMAGE_MODEL":           "gemini-3-pro-image",
		"IMAGE_STORAGE_CHAT_ID": "111",
		"IMAGE_BASE_URL":        "https://example.test/v1/",
		"IMAGE_API_KEYS":        "billed-key",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ImageBaseURL != "https://example.test/v1" {
		t.Errorf("ImageBaseURL = %q, want the override without a trailing slash", cfg.ImageBaseURL)
	}
	// Pictures are paid on AI Studio, so the billed key stays separate from the
	// free ones the chat models run on.
	if strings.Join(cfg.ImageAPIKeys, ",") != "billed-key" {
		t.Errorf("ImageAPIKeys = %v, want the override, not the chat keys", cfg.ImageAPIKeys)
	}
	if strings.Join(cfg.APIKeys, ",") != "sk-test" {
		t.Errorf("APIKeys = %v, want the chat keys left alone", cfg.APIKeys)
	}
}

func TestLoadImagesAreOffByDefault(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ImagesEnabled() {
		t.Error("ImagesEnabled() = true with no IMAGE_MODEL, want pictures off until asked for")
	}
}

func TestLoadParsesWhitelists(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"ALLOWED_USER_IDS": " 111 , 222,",
		"ALLOWED_CHAT_IDS": "-1001234567890",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.AllowedUsers) != 2 {
		t.Errorf("AllowedUsers = %v, want 2 entries", cfg.AllowedUsers)
	}
	if _, ok := cfg.AllowedChats[-1001234567890]; !ok {
		t.Errorf("AllowedChats = %v, want the negative chat id preserved", cfg.AllowedChats)
	}
}

func TestLoadParsesAdmins(t *testing.T) {
	cfg, err := Load(env(map[string]string{"ADMIN_USER_IDS": "111, 222"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.AdminUsers) != 2 {
		t.Errorf("AdminUsers = %v, want 2 entries", cfg.AdminUsers)
	}
}

func TestLoadRejectsChatIDAsAdmin(t *testing.T) {
	_, err := Load(env(map[string]string{"ADMIN_USER_IDS": "-1001234567890"}))
	if err == nil || !strings.Contains(err.Error(), "admins must be users") {
		t.Errorf("Load() error = %v, want it to reject a negative admin id", err)
	}
}

func TestLoadStateFileOverride(t *testing.T) {
	cfg, err := Load(env(map[string]string{"STATE_FILE": "/data/state.json"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StateFile != "/data/state.json" {
		t.Errorf("StateFile = %q, want the override", cfg.StateFile)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantIn    string
	}{
		{
			name:      "missing token",
			overrides: map[string]string{"TELEGRAM_BOT_TOKEN": ""},
			wantIn:    "TELEGRAM_BOT_TOKEN is required",
		},
		{
			name:      "missing keys",
			overrides: map[string]string{"GOOGLE_API_KEYS": ""},
			wantIn:    "GOOGLE_API_KEYS is required",
		},
		{
			name:      "keys are only commas",
			overrides: map[string]string{"GOOGLE_API_KEYS": " , ,"},
			wantIn:    "GOOGLE_API_KEYS is required",
		},
		{
			name:      "unknown reasoning effort",
			overrides: map[string]string{"LLM_REASONING_EFFORT": "maximum"},
			wantIn:    `LLM_REASONING_EFFORT "maximum" is not supported`,
		},
		{
			name:      "bad key cooldown",
			overrides: map[string]string{"LLM_KEY_COOLDOWN": "60"},
			wantIn:    `LLM_KEY_COOLDOWN "60" is not a duration`,
		},
		{
			name: "image model without a storage chat",
			overrides: map[string]string{
				"IMAGE_MODEL": "gemini-3.1-flash-image",
			},
			wantIn: "IMAGE_STORAGE_CHAT_ID is required",
		},
		{
			name:      "bad number",
			overrides: map[string]string{"LLM_MAX_TOKENS": "many"},
			wantIn:    `LLM_MAX_TOKENS "many" is not a number`,
		},
		{
			name:      "non-positive number",
			overrides: map[string]string{"MAX_CONCURRENT": "0"},
			wantIn:    "MAX_CONCURRENT must be positive",
		},
		{
			name:      "bad duration",
			overrides: map[string]string{"LLM_TIMEOUT": "30"},
			wantIn:    `LLM_TIMEOUT "30" is not a duration`,
		},
		{
			name:      "bad id in whitelist",
			overrides: map[string]string{"ALLOWED_USER_IDS": "111,@vasya"},
			wantIn:    "ALLOWED_USER_IDS",
		},
		{
			name:      "bad log level",
			overrides: map[string]string{"LOG_LEVEL": "trace"},
			wantIn:    `LOG_LEVEL "trace" is not supported`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.overrides))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("Load() error = %q, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"TELEGRAM_BOT_TOKEN": "",
		"GOOGLE_API_KEYS":    "",
		"LOG_LEVEL":          "trace",
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}

	for _, want := range []string{"TELEGRAM_BOT_TOKEN", "GOOGLE_API_KEYS", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q; all problems should be reported together", err, want)
		}
	}
}

func TestLoadBaseURLOverride(t *testing.T) {
	cfg, err := Load(env(map[string]string{"LLM_BASE_URL": "http://localhost:8080/v1/"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.BaseURL != "http://localhost:8080/v1" {
		t.Errorf("BaseURL = %q, want the override without a trailing slash", cfg.BaseURL)
	}
}

func TestLoadRawSystemPrompt(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		cfg, err := Load(env(nil))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.RawSystemPrompt != defaultRawSystemPrompt {
			t.Errorf("RawSystemPrompt = %q, want the built-in default", cfg.RawSystemPrompt)
		}
	})

	t.Run("override", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{"LLM_RAW_SYSTEM_PROMPT": "делай что просят"}))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.RawSystemPrompt != "делай что просят" {
			t.Errorf("RawSystemPrompt = %q, want the override", cfg.RawSystemPrompt)
		}
	})

	t.Run("off disables the flag", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{"LLM_RAW_SYSTEM_PROMPT": "off"}))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		// reply.Handler treats an empty raw prompt as "no such flag".
		if cfg.RawSystemPrompt != "" {
			t.Errorf("RawSystemPrompt = %q, want it emptied so the flag is inert", cfg.RawSystemPrompt)
		}
	})
}
