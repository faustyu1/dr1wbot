// Package admin implements the in-chat commands that manage the whitelist.
//
// Commands are answered directly, without going to the model, so they cost
// nothing and come back instantly.
package admin

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"dr1wbot/internal/access"
)

// Store is the part of access.Store these commands need.
type Store interface {
	IsAdmin(userID int64) bool
	Add(id int64) (bool, error)
	Remove(id int64) (bool, error)
	List() []access.Entry
}

// Pardoner lifts an automatic ban. A nil one means public access is off and
// there is nothing to lift.
type Pardoner interface {
	Pardon(userID int64)
}

// Commands dispatches admin commands.
type Commands struct {
	store  Store
	pardon Pardoner
}

// New builds a Commands.
func New(store Store) *Commands {
	return &Commands{store: store}
}

// WithPardoner adds the /unban command. Bans are handed out automatically, so
// without a way to undo one a false positive would be permanent.
func (c *Commands) WithPardoner(p Pardoner) *Commands {
	c.pardon = p
	return c
}

const helpText = "<b>Команды администратора</b>\n\n" +
	"- <code>/add id</code> — выдать доступ\n" +
	"- <code>/del id</code> — забрать доступ\n" +
	"- <code>/unban id</code> — снять автоматический бан за флуд\n" +
	"- <code>/list</code> — показать текущий список\n" +
	"- <code>/help</code> — эта справка\n\n" +
	"Положительный <code>id</code> — пользователь, отрицательный — группа или канал. " +
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
	case "/add", "/del", "/unban", "/list", "/help":
	default:
		return "", false // an unknown slash word is just text
	}

	if !c.store.IsAdmin(callerID) {
		return "Команды доступны только администратору.", true
	}

	switch name {
	case "/help":
		return helpText, true
	case "/list":
		return c.list(), true
	case "/add":
		return c.mutate(args, true), true
	case "/unban":
		return c.unban(args), true
	default:
		return c.mutate(args, false), true
	}
}

// unban lifts an automatic flood ban.
func (c *Commands) unban(args []string) string {
	if c.pardon == nil {
		return "Публичный доступ выключен — банить некого."
	}
	if len(args) != 1 {
		return "Нужен ровно один id: <code>/unban 123456789</code>"
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Sprintf("<code>%s</code> — это не Telegram ID.", args[0])
	}
	if id <= 0 {
		// Bans are per person; a chat has no allowance to abuse.
		return "Банятся только пользователи, а их id положительный."
	}

	c.pardon.Pardon(id)
	return fmt.Sprintf("Бан с <code>%d</code> снят, счётчик нарушений обнулён.", id)
}

// mutate handles /add and /del, which differ only in the verb.
func (c *Commands) mutate(args []string, adding bool) string {
	usage := "<code>/del id</code> — например, /del 123456789"
	if adding {
		usage = "<code>/add id</code> — например, /add 123456789"
	}

	if len(args) != 1 {
		return "Нужен ровно один ID.\n\n" + usage
	}

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Sprintf("%q — это не числовой ID.\n\nНужен именно номер, не @username. Узнать: @userinfobot.", args[0])
	}

	var changed bool
	if adding {
		changed, err = c.store.Add(id)
	} else {
		changed, err = c.store.Remove(id)
	}

	switch {
	case errors.Is(err, access.ErrStatic):
		return fmt.Sprintf("<code>%d</code> задан в переменных окружения — отсюда его не убрать.\n\n"+
			"Удали его из <code>ALLOWED_USER_IDS</code>, <code>ALLOWED_CHAT_IDS</code> или <code>ADMIN_USER_IDS</code> в .env и перезапусти бота.", id)
	case err != nil:
		return fmt.Sprintf("Не получилось сохранить список: %s", err)
	case !changed && adding:
		return fmt.Sprintf("<code>%d</code> уже в списке.", id)
	case !changed:
		return fmt.Sprintf("<code>%d</code> в списке и не было.", id)
	case adding:
		return fmt.Sprintf("<code>%d</code> добавлен.\n\n%s", id, c.list())
	default:
		return fmt.Sprintf("<code>%d</code> удалён.\n\n%s", id, c.list())
	}
}

func (c *Commands) list() string {
	entries := c.store.List()
	if len(entries) == 0 {
		return "Список пуст — значит, бота может звать кто угодно."
	}

	var b strings.Builder
	b.WriteString("<b>Доступ есть у:</b>\n")
	for _, e := range entries {
		kind := "пользователь"
		if e.IsChat {
			kind = "чат"
		}
		switch {
		case e.Admin:
			fmt.Fprintf(&b, "- <code>%d</code> — админ\n", e.ID)
		case e.Static:
			fmt.Fprintf(&b, "- <code>%d</code> — %s, из .env\n", e.ID, kind)
		default:
			fmt.Fprintf(&b, "- <code>%d</code> — %s\n", e.ID, kind)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
