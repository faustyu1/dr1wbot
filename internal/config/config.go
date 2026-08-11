// Package config loads and validates the bot configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// googleBaseURL is Google AI Studio's OpenAI-compatible endpoint. The native
// Gemini API lives one level up and speaks its own dialect; this one takes the
// same /chat/completions the rest of the code is written against.
const googleBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

// defaultModels are tried in order. Both are free-tier on AI Studio, so the
// fallback costs nothing but buys a second daily quota: when 3.6 is spent on
// every key, the bot keeps answering on 3.5.
var defaultModels = []string{"gemini-3.6-flash", "gemini-3.5-flash"}

// defaultSystemPrompt is tuned for a group chat: the bot is a participant in a
// conversation, not a documentation generator. The length rule is the important
// part — an unprompted model answers a one-line question with five paragraphs,
// which in a group chat reads as noise.
const defaultSystemPrompt = `Ты отвечаешь в чате Telegram. Пиши как знающий человек в переписке, а не как справочник.

Длина ответа — под вопрос:
- Простой вопрос, факт, «да или нет» — одна-две фразы. Без предисловий.
- Просят объяснить, сравнить, разобраться, написать код — разворачивайся настолько, насколько нужно, и структурируй: подзаголовки, списки, таблицы, код в блоках с указанием языка.
- Не растягивай короткий ответ до длинного ради солидности и не ужимай сложный до одной строки.

Чего не делать:
- Не повторяй вопрос и не начинай с «отличный вопрос», «конечно», «давай разберёмся».
- Не подводи итог того, что только что написал, и не предлагай помощь в конце.
- Не извиняйся без причины, не оговаривайся, что ты ИИ, не читай нотации и не дописывай дисклеймеры к безобидным вещам.
- Не выдумывай факты, цифры, цитаты и ссылки. Не знаешь — скажи одной фразой.

Отвечай на языке собеседника. Мат и резкость собеседника — не повод для морали, отвечай по делу.`

// defaultRawSystemPrompt is what an admin gets instead of the house style when
// a question starts with "-s". The chat rules above are about fitting into a
// group conversation; sometimes an admin wants the model's own output shape and
// not that, which is the whole point of the flag.
const defaultRawSystemPrompt = `Отвечай так, как считаешь правильным. Ограничений по длине, форме и структуре ответа нет.`

// Config is the fully validated runtime configuration.
type Config struct {
	BotToken string

	BaseURL string
	// APIKeys is the Google AI Studio key pool. Free-tier quota is per key, so
	// several keys are several quotas.
	APIKeys []string
	// KeyCooldown is how long a key that answered 429 is skipped for.
	KeyCooldown time.Duration
	// Models are tried in order; each is attempted with every key first.
	Models       []string
	SystemPrompt string
	// RawSystemPrompt replaces SystemPrompt when an admin prefixes a question
	// with "-s". Empty switches the flag off.
	RawSystemPrompt string
	MaxTokens       int
	ReasoningEffort string
	Timeout         time.Duration

	AdminUsers   map[int64]struct{}
	AllowedUsers map[int64]struct{}
	AllowedChats map[int64]struct{}
	// StateFile stores whitelist changes made through admin commands.
	StateFile string
	// CommandReplyTTL is how long the answer to /add, /del or /list stays in
	// full. Zero leaves it standing.
	CommandReplyTTL time.Duration

	// PublicDailyLimit opens the bot to everybody at this many requests per
	// person per day. Zero keeps it whitelist-only. It, and the knobs below
	// it, only seed the runtime settings: once an admin edits one from the
	// panel, the saved value wins and this is ignored.
	PublicDailyLimit int
	// GlobalDailyLimit caps what the public costs in total per day.
	GlobalDailyLimit int
	// NewAccountThreshold halves the allowance for ids at or above it.
	NewAccountThreshold int64
	PublicBurst         int
	PublicBurstWindow   time.Duration
	PublicBanFor        time.Duration
	PublicMaxTokens     int
	PublicMaxRunes      int
	// WarnTTL is how long a burst violation counts against somebody, and
	// MaxWarns is how many live ones earn a permanent ban. Both are fixed at
	// deploy time: they are about how forgiving the bot is by nature, not a
	// number to tune while watching a flood.
	WarnTTL  time.Duration
	MaxWarns int
	// CacheTTL is how long an answer to an identical question is reused.
	CacheTTL time.Duration
	// AlertCooldown is the minimum gap between two alerts of the same kind.
	AlertCooldown time.Duration
	// QuotaFile holds today's counters across a restart.
	QuotaFile string
	// SettingsFile holds the knobs the admin panel can change.
	SettingsFile string
	// MaxQueue bounds how many summons may wait for a worker.
	MaxQueue int

	// ImageModel is empty when picture generation is switched off, which is the
	// default: no Google image model has a free tier, so drawing needs a key
	// attached to a billed project.
	ImageModel string
	// ImageBaseURL and ImageAPIKeys default to the chat endpoint's. They exist
	// because the picture key need not be the chat key: the text models run on
	// free keys, and only pictures need the billed one.
	ImageBaseURL string
	ImageAPIKeys []string
	// ImageSize is the requested resolution, e.g. 1024x1024. Empty means the
	// model's default.
	ImageSize string
	// ImageStorageChatID is where generated pictures are uploaded to obtain a
	// file_id. Guest mode cannot upload bytes directly, so a picture has to
	// exist in some chat the bot can post to before it can be shown.
	ImageStorageChatID int64
	ImageTimeout       time.Duration

	MemoryTTL time.Duration

	MaxConcurrent int
	MaxReplyRunes int

	LogLevel slog.Level
	Debug    bool
}

// ImagesEnabled reports whether picture generation is fully configured.
func (c *Config) ImagesEnabled() bool {
	return c.ImageModel != "" && c.ImageStorageChatID != 0
}

// Getenv reads one environment variable. os.Getenv satisfies it; tests supply their own.
type Getenv func(string) string

// Load reads the configuration using getenv and validates it. Every problem is
// reported at once so a misconfigured deployment needs a single fix-and-restart
// cycle rather than one per variable.
func Load(getenv Getenv) (*Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	cfg := &Config{}

	cfg.BotToken = strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN"))
	if cfg.BotToken == "" {
		fail("TELEGRAM_BOT_TOKEN is required")
	}

	cfg.BaseURL = googleBaseURL
	if override := strings.TrimSpace(getenv("LLM_BASE_URL")); override != "" {
		cfg.BaseURL = strings.TrimRight(override, "/")
	}

	cfg.APIKeys = list(getenv("GOOGLE_API_KEYS"))
	if len(cfg.APIKeys) == 0 {
		fail("GOOGLE_API_KEYS is required: one or more Google AI Studio keys, comma separated")
	}

	cfg.Models = list(getenv("LLM_MODEL"))
	if len(cfg.Models) == 0 {
		cfg.Models = defaultModels
	}

	cfg.SystemPrompt = strings.TrimSpace(getenv("LLM_SYSTEM_PROMPT"))
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = defaultSystemPrompt
	}

	switch raw := strings.TrimSpace(getenv("LLM_RAW_SYSTEM_PROMPT")); raw {
	case "":
		cfg.RawSystemPrompt = defaultRawSystemPrompt
	case "off":
		// An empty value cannot mean "off" here: unset and blank look the same
		// to getenv, and unset has to keep the default.
		cfg.RawSystemPrompt = ""
	default:
		cfg.RawSystemPrompt = raw
	}

	switch effort := strings.ToLower(strings.TrimSpace(getenv("LLM_REASONING_EFFORT"))); effort {
	case "":
		// Gemini 3 thinks by default and bills it against LLM_MAX_TOKENS, so a
		// group-chat answer can be crowded out by the reasoning it never shows.
		cfg.ReasoningEffort = "low"
	case "none", "low", "medium", "high":
		cfg.ReasoningEffort = effort
	default:
		fail("LLM_REASONING_EFFORT %q is not supported (want none, low, medium or high)", effort)
	}

	// The budget covers thinking as well as the answer, so it is set well above
	// what the reply itself needs.
	cfg.MaxTokens = intVar(getenv, "LLM_MAX_TOKENS", 4096, fail)
	cfg.KeyCooldown = durationVar(getenv, "LLM_KEY_COOLDOWN", time.Minute, fail)
	cfg.Timeout = durationVar(getenv, "LLM_TIMEOUT", 30*time.Second, fail)
	cfg.MaxConcurrent = intVar(getenv, "MAX_CONCURRENT", 8, fail)
	cfg.MaxReplyRunes = intVar(getenv, "MAX_REPLY_RUNES", 3500, fail)

	var err error
	if cfg.AdminUsers, err = idSet(getenv("ADMIN_USER_IDS")); err != nil {
		fail("ADMIN_USER_IDS: %s", err)
	}
	if cfg.AllowedUsers, err = idSet(getenv("ALLOWED_USER_IDS")); err != nil {
		fail("ALLOWED_USER_IDS: %s", err)
	}
	if cfg.AllowedChats, err = idSet(getenv("ALLOWED_CHAT_IDS")); err != nil {
		fail("ALLOWED_CHAT_IDS: %s", err)
	}
	for id := range cfg.AdminUsers {
		if id < 0 {
			fail("ADMIN_USER_IDS: %d is a chat id; admins must be users", id)
		}
	}

	cfg.StateFile = strings.TrimSpace(getenv("STATE_FILE"))
	if cfg.StateFile == "" {
		cfg.StateFile = "state.json"
	}

	cfg.CommandReplyTTL = optionalDurationVar(getenv, "COMMAND_REPLY_TTL", 30*time.Second, fail)

	// Zero is the safe default: an unset limit must not accidentally open a
	// private bot to all of Telegram.
	cfg.PublicDailyLimit = optionalIntVar(getenv, "PUBLIC_DAILY_LIMIT", 0, fail)
	cfg.GlobalDailyLimit = optionalIntVar(getenv, "PUBLIC_GLOBAL_DAILY_LIMIT", 0, fail)
	if raw := strings.TrimSpace(getenv("NEW_ACCOUNT_ID_THRESHOLD")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			fail("NEW_ACCOUNT_ID_THRESHOLD %q is not a positive Telegram id", raw)
		} else {
			cfg.NewAccountThreshold = n
		}
	}
	cfg.PublicBurst = intVar(getenv, "PUBLIC_BURST", 5, fail)
	cfg.WarnTTL = durationVar(getenv, "PUBLIC_WARN_TTL", 30*24*time.Hour, fail)
	cfg.MaxWarns = intVar(getenv, "PUBLIC_MAX_WARNS", 3, fail)
	cfg.CacheTTL = optionalDurationVar(getenv, "ANSWER_CACHE_TTL", 10*time.Minute, fail)
	cfg.AlertCooldown = durationVar(getenv, "ALERT_COOLDOWN", 15*time.Minute, fail)
	cfg.PublicBurstWindow = durationVar(getenv, "PUBLIC_BURST_WINDOW", time.Minute, fail)
	cfg.PublicBanFor = durationVar(getenv, "PUBLIC_BAN_FOR", 15*time.Minute, fail)
	cfg.PublicMaxTokens = intVar(getenv, "PUBLIC_MAX_TOKENS", 1024, fail)
	cfg.PublicMaxRunes = intVar(getenv, "PUBLIC_MAX_RUNES", 2000, fail)
	cfg.MaxQueue = intVar(getenv, "MAX_QUEUE", 32, fail)

	// Both live next to the whitelist, so one volume covers everything that has
	// to survive a restart.
	beside := filepath.Dir(cfg.StateFile)
	cfg.QuotaFile = strings.TrimSpace(getenv("QUOTA_FILE"))
	if cfg.QuotaFile == "" {
		cfg.QuotaFile = filepath.Join(beside, "quota.json")
	}
	cfg.SettingsFile = strings.TrimSpace(getenv("SETTINGS_FILE"))
	if cfg.SettingsFile == "" {
		cfg.SettingsFile = filepath.Join(beside, "settings.json")
	}

	cfg.ImageModel = strings.TrimSpace(getenv("IMAGE_MODEL"))
	cfg.ImageBaseURL = strings.TrimRight(strings.TrimSpace(getenv("IMAGE_BASE_URL")), "/")
	if cfg.ImageBaseURL == "" {
		cfg.ImageBaseURL = cfg.BaseURL
	}
	cfg.ImageAPIKeys = list(getenv("IMAGE_API_KEYS"))
	if len(cfg.ImageAPIKeys) == 0 {
		cfg.ImageAPIKeys = cfg.APIKeys
	}
	cfg.ImageSize = strings.TrimSpace(getenv("IMAGE_SIZE"))
	cfg.ImageTimeout = durationVar(getenv, "IMAGE_TIMEOUT", 2*time.Minute, fail)
	if raw := strings.TrimSpace(getenv("IMAGE_STORAGE_CHAT_ID")); raw != "" {
		if cfg.ImageStorageChatID, err = strconv.ParseInt(raw, 10, 64); err != nil {
			fail("IMAGE_STORAGE_CHAT_ID %q is not a valid chat id", raw)
		}
	}
	if cfg.ImageModel != "" && cfg.ImageStorageChatID == 0 {
		fail("IMAGE_STORAGE_CHAT_ID is required when IMAGE_MODEL is set: " +
			"guest mode cannot upload files, so pictures need a chat to be uploaded to first")
	}

	cfg.MemoryTTL = durationVar(getenv, "MEMORY_TTL", 30*time.Minute, fail)

	switch level := strings.ToLower(strings.TrimSpace(getenv("LOG_LEVEL"))); level {
	case "debug":
		cfg.LogLevel, cfg.Debug = slog.LevelDebug, true
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		fail("LOG_LEVEL %q is not supported (want debug, info, warn or error)", level)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func intVar(getenv Getenv, name string, fallback int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s %q is not a number", name, raw)
		return fallback
	}
	if n <= 0 {
		fail("%s must be positive, got %d", name, n)
		return fallback
	}
	return n
}

func durationVar(getenv Getenv, name string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s %q is not a duration (e.g. 30s, 2m)", name, raw)
		return fallback
	}
	if d <= 0 {
		fail("%s must be positive, got %s", name, d)
		return fallback
	}
	return d
}

// optionalIntVar is intVar for a setting where zero switches a feature off
// rather than being a mistake.
func optionalIntVar(getenv Getenv, name string, fallback int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s %q is not a number", name, raw)
		return fallback
	}
	if n < 0 {
		fail("%s cannot be negative, got %d", name, n)
		return fallback
	}
	return n
}

// optionalDurationVar is durationVar for a setting where zero is a meaningful
// value that switches the feature off rather than a mistake.
func optionalDurationVar(getenv Getenv, name string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s %q is not a duration (e.g. 30s, 2m; 0 disables)", name, raw)
		return fallback
	}
	if d < 0 {
		fail("%s cannot be negative, got %s", name, d)
		return fallback
	}
	return d
}

// list splits a comma-separated setting, dropping blanks and duplicates. Keys
// and model names both arrive this way, and a pasted list tends to carry stray
// spaces, a trailing comma or the same key twice.
func list(raw string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
	}
	return out
}

// idSet parses a comma-separated list of Telegram IDs. Chat IDs are negative,
// so the sign must be preserved.
func idSet(raw string) (map[int64]struct{}, error) {
	set := make(map[int64]struct{})
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, errors.New(strconv.Quote(field) + " is not a valid Telegram ID")
		}
		set[id] = struct{}{}
	}
	return set, nil
}
