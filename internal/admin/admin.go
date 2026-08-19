// Package admin implements the in-chat commands that manage the whitelist and
// the ban list.
//
// Commands are answered directly, without going to the model, so they cost
// nothing and come back instantly.
package admin

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dr1wbot/internal/access"
	"dr1wbot/internal/quota"
)

// Store is the part of access.Store these commands need.
type Store interface {
	IsAdmin(userID int64) bool
	Add(id int64) (bool, error)
	Remove(id int64) (bool, error)
	List() []access.Entry
}

// Bouncer is the ban half of the quota store. A nil one means bans are not
// available at all, which is what a bot with no quota store has.
type Bouncer interface {
	Ban(userID int64, d time.Duration)
	BanName(username string, d time.Duration) (id int64, known bool)
	Pardon(userID int64)
	PardonName(username string) bool
	Lookup(username string) (int64, bool)
	Bans(max int) []quota.Ban
}

// Commands dispatches admin commands.
type Commands struct {
	store   Store
	bouncer Bouncer
	now     func() time.Time
}

// New builds a Commands.
func New(store Store) *Commands {
	return &Commands{store: store, now: time.Now}
}

// WithBouncer adds the ban commands. Without one there is nowhere to record a
// ban, so /ban says so instead of pretending to work.
func (c *Commands) WithBouncer(b Bouncer) *Commands {
	c.bouncer = b
	return c
}

// bansShown caps what /bans prints. A ban list is read in a chat, and a chat
// message has a length limit.
const bansShown = 25

const helpText = "**Команды администратора**\n\n" +
	"*Доступ*\n" +
	"- `/add <id>` — выдать доступ\n" +
	"- `/del <id>` — забрать доступ\n" +
	"- `/list` — показать список доступа\n\n" +
	"*Баны*\n" +
	"- `/ban <id|@username> [срок]` — забанить; без срока — навсегда\n" +
	"- `/unban <id|@username>` — разбанить\n" +
	"- `/bans` — кто забанен сейчас\n\n" +
	"Срок пишется как `30м`, `2ч`, `7д`, `1нед` или `30m`, `2h`, `7d`. " +
	"Голое число — минуты. Слово `навсегда` можно написать явно.\n\n" +
	"Забаненный не получает ответа нигде — ни в группе, ни в личке, " +
	"даже если он есть в списке доступа. Админа забанить нельзя.\n\n" +
	"Положительный `id` — пользователь, отрицательный — группа или канал. " +
	"Свой ID можно узнать у @userinfobot.\n\n" +
	"Всё остальное уходит в модель как обычный вопрос."

// Handle runs text as a command. The second result reports whether text was a
// command at all; when it is false the caller should treat the text as a
// question for the model.
func (c *Commands) Handle(text string, callerID int64) (reply string, handled bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}

	// Telegram renders commands as "/add@botname" when several bots are around.
	name := strings.ToLower(fields[0])
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	args := fields[1:]

	switch name {
	case "/add", "/del", "/ban", "/unban", "/bans", "/list", "/help":
	default:
		return "", false // an unknown slash word is just text
	}

	if !c.store.IsAdmin(callerID) {
		return "⚠️ Команды доступны только администратору.", true
	}

	switch name {
	case "/help":
		return helpText, true
	case "/list":
		return c.list(), true
	case "/add":
		return c.mutate(args, true), true
	case "/ban":
		return c.ban(args), true
	case "/unban":
		return c.unban(args), true
	case "/bans":
		return c.bans(), true
	default:
		return c.mutate(args, false), true
	}
}

// ban sidelines somebody by id or by @username. No duration means forever,
// because that is what an admin who typed none meant.
func (c *Commands) ban(args []string) string {
	if c.bouncer == nil {
		return "⚠️ Баны недоступны: не настроен учёт запросов."
	}
	if len(args) == 0 {
		return "Кого банить?\n\n`/ban 123456789` — навсегда\n" +
			"`/ban @username 2ч` — на два часа\n\nСрок: `30м`, `2ч`, `7д`, `1нед`."
	}

	target := args[0]
	span, err := parseSpan(strings.Join(args[1:], " "))
	if err != nil {
		return fmt.Sprintf("⚠️ %s\n\nПримеры срока: `30м`, `2ч`, `7д`, `1нед`. Без срока — навсегда.", err)
	}

	if id, ok := parseID(target); ok {
		if id <= 0 {
			return "Банятся только пользователи, а их id положительный. Чат просто убери из доступа: `/del " +
				strconv.FormatInt(id, 10) + "`"
		}
		if c.store.IsAdmin(id) {
			return "⚠️ Это админ — забанить его нельзя. Сначала убери id из `ADMIN_USER_IDS`."
		}
		c.bouncer.Ban(id, span)
		return fmt.Sprintf("🚫 `%d` забанен %s.\n\nСнять: `/unban %d`", id, spanText(span), id)
	}

	name := strings.TrimPrefix(target, "@")
	if name == "" || strings.ContainsAny(name, " /") {
		return fmt.Sprintf("⚠️ `%s` — это ни id, ни @username.", target)
	}
	if id, known := c.bouncer.Lookup(name); known && c.store.IsAdmin(id) {
		return "⚠️ Это админ — забанить его нельзя."
	}

	id, known := c.bouncer.BanName(name, span)
	if known {
		return fmt.Sprintf("🚫 `@%s` (id `%d`) забанен %s.\n\nСнять: `/unban @%s`", name, id, spanText(span), name)
	}
	// A name the bot has never seen is still worth banning: Telegram offers no
	// way to look an id up by name, so the alternative is refusing to act until
	// the person shows up — which is exactly when it is too late.
	return fmt.Sprintf("🚫 `@%s` забанен %s.\n\nЭтого ника бот ещё не видел — бан сработает, "+
		"как только он напишет.\nСнять: `/unban @%s`", name, spanText(span), name)
}

// unban lifts a ban, whether it was placed by hand or by the flood detector.
func (c *Commands) unban(args []string) string {
	if c.bouncer == nil {
		return "⚠️ Баны недоступны: не настроен учёт запросов."
	}
	if len(args) != 1 {
		return "Нужен один id или @username: `/unban 123456789` либо `/unban @username`"
	}

	target := args[0]
	if id, ok := parseID(target); ok {
		if id <= 0 {
			return "Банятся только пользователи, а их id положительный."
		}
		c.bouncer.Pardon(id)
		return fmt.Sprintf("✅ Бан с `%d` снят, счётчик нарушений обнулён.", id)
	}

	name := strings.TrimPrefix(target, "@")
	if name == "" || strings.ContainsAny(name, " /") {
		return fmt.Sprintf("⚠️ `%s` — это ни id, ни @username.", target)
	}
	if c.bouncer.PardonName(name) {
		return fmt.Sprintf("✅ Бан с `@%s` снят.", name)
	}
	return fmt.Sprintf("`@%s` и не был забанен.", name)
}

// bans prints who is sidelined right now.
func (c *Commands) bans() string {
	if c.bouncer == nil {
		return "⚠️ Баны недоступны: не настроен учёт запросов."
	}

	list := c.bouncer.Bans(bansShown)
	if len(list) == 0 {
		return "Забаненных нет."
	}

	var b strings.Builder
	b.WriteString("**Забанены сейчас**\n")
	for _, ban := range list {
		b.WriteString("- " + banLine(ban, c.now()) + "\n")
	}
	b.WriteString("\nСнять: `/unban <id>` или `/unban @username`.")
	return b.String()
}

// banLine is one row of /bans: who, for how long, and by whose decision.
func banLine(b quota.Ban, now time.Time) string {
	var who string
	switch {
	case b.ID != 0 && b.Username != "":
		who = fmt.Sprintf("`%d` (@%s)", b.ID, b.Username)
	case b.ID != 0:
		who = fmt.Sprintf("`%d`", b.ID)
	default:
		who = fmt.Sprintf("`@%s` — ещё не появлялся", b.Username)
	}

	when := "навсегда"
	if !b.Forever {
		when = "ещё " + humanDuration(b.Until.Sub(now))
	}

	why := "автобан за флуд"
	if b.Manual {
		why = "вручную"
	}
	if b.Warns > 0 {
		why += fmt.Sprintf(", варнов %d", b.Warns)
	}
	return fmt.Sprintf("%s — %s, %s", who, when, why)
}

// spanText names a duration the way the confirmation should read it.
func spanText(d time.Duration) string {
	if d <= 0 {
		return "навсегда"
	}
	return "на " + humanDuration(d)
}

// humanDuration renders a span the way a person would say it.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч", int(d.Hours()))
	default:
		return fmt.Sprintf("%d д", int(d.Hours()/24))
	}
}

// forever names the durations that mean "no end".
var forever = map[string]bool{
	"": true, "навсегда": true, "насовсем": true, "перманентно": true,
	"forever": true, "perm": true, "permanent": true, "∞": true,
}

// units maps every spelling of a time unit an admin might type. Russian is
// there because the bot is answered in Russian and "2ч" is what somebody
// actually types; the Latin forms are there because the keyboard is often in
// the wrong layout.
var units = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "с": time.Second, "сек": time.Second,
	"m": time.Minute, "min": time.Minute, "м": time.Minute, "мин": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hour": time.Hour, "ч": time.Hour, "час": time.Hour, "часа": time.Hour, "часов": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "д": 24 * time.Hour, "дн": 24 * time.Hour,
	"день": 24 * time.Hour, "дня": 24 * time.Hour, "дней": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "н": 7 * 24 * time.Hour,
	"нед": 7 * 24 * time.Hour, "неделя": 7 * 24 * time.Hour, "недели": 7 * 24 * time.Hour,
	"mo": 30 * 24 * time.Hour, "мес": 30 * 24 * time.Hour, "месяц": 30 * 24 * time.Hour,
	"y": 365 * 24 * time.Hour, "year": 365 * 24 * time.Hour, "г": 365 * 24 * time.Hour, "год": 365 * 24 * time.Hour,
}

// parseSpan reads a ban length. Zero means forever, which is what an empty
// argument means: banning "for a while" is a decision somebody has to make on
// purpose, so the default is the one that does not quietly expire.
func parseSpan(raw string) (time.Duration, error) {
	text := strings.ToLower(strings.Join(strings.Fields(raw), ""))
	if forever[text] {
		return 0, nil
	}

	var total time.Duration
	for rest := text; rest != ""; {
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("`%s` — непонятный срок", raw)
		}
		n, err := strconv.Atoi(rest[:digits])
		if err != nil {
			return 0, fmt.Errorf("`%s` — непонятный срок", raw)
		}

		rest = rest[digits:]
		letters := 0
		for letters < len(rest) && (rest[letters] < '0' || rest[letters] > '9') {
			letters++
		}
		// A bare number is minutes: it is the only unit somebody types without
		// naming, and "/ban 123 30" meaning half an hour is the least surprising
		// reading of it.
		unit := time.Minute
		if letters > 0 {
			known, ok := units[rest[:letters]]
			if !ok {
				return 0, fmt.Errorf("`%s` — непонятная единица времени", rest[:letters])
			}
			unit = known
		}
		rest = rest[letters:]

		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, nil // "0" said forever the long way round
	}
	return total, nil
}

// parseID reads a Telegram id, reporting false for anything that is not one.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

// mutate handles /add and /del, which differ only in the verb.
func (c *Commands) mutate(args []string, adding bool) string {
	usage := "`/del <id>` — например, `/del 123456789`"
	if adding {
		usage = "`/add <id>` — например, `/add 123456789`"
	}

	if len(args) != 1 {
		return "⚠️ Нужен ровно один ID.\n\n" + usage
	}

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Sprintf("⚠️ %q — это не числовой ID.\n\nНужен именно номер, не @username. Узнать: @userinfobot.", args[0])
	}

	var changed bool
	if adding {
		changed, err = c.store.Add(id)
	} else {
		changed, err = c.store.Remove(id)
	}

	switch {
	case errors.Is(err, access.ErrStatic):
		return fmt.Sprintf("⚠️ `%d` задан в переменных окружения — отсюда его не убрать.\n\n"+
			"Удали его из `ALLOWED_USER_IDS`, `ALLOWED_CHAT_IDS` или `ADMIN_USER_IDS` в `.env` и перезапусти бота.", id)
	case err != nil:
		return fmt.Sprintf("⚠️ Не получилось сохранить список: %s", err)
	case !changed && adding:
		return fmt.Sprintf("`%d` уже в списке.", id)
	case !changed:
		return fmt.Sprintf("`%d` в списке и не было.", id)
	case adding:
		return fmt.Sprintf("✅ `%d` добавлен.\n\n%s", id, c.list())
	default:
		return fmt.Sprintf("✅ `%d` удалён.\n\n%s", id, c.list())
	}
}

func (c *Commands) list() string {
	entries := c.store.List()
	if len(entries) == 0 {
		return "Список пуст — значит, бота может звать кто угодно."
	}

	var b strings.Builder
	b.WriteString("**Доступ есть у:**\n")
	for _, e := range entries {
		kind := "пользователь"
		if e.IsChat {
			kind = "чат"
		}
		switch {
		case e.Admin:
			fmt.Fprintf(&b, "- `%d` — админ\n", e.ID)
		case e.Static:
			fmt.Fprintf(&b, "- `%d` — %s, из `.env`\n", e.ID, kind)
		default:
			fmt.Fprintf(&b, "- `%d` — %s\n", e.ID, kind)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
