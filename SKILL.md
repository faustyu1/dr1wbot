# SKILL.md — dr1wbot

> Карта возможностей бота: что он умеет, какие API использует и как настроить каждое умение.
> Рассчитан на самостоятельное чтение — можно открыть любой раздел и настроить фичу без README.

---

## Бэкенд: OpenAI-совместимый API

Бот работает через стандартный протокол OpenAI Chat Completions. По умолчанию — **OpenRouter** (`https://openrouter.ai/api/v1`), но поддерживает любой совместимый эндпоинт.

### Запрос к модели

```
POST {baseURL}/chat/completions
Authorization: Bearer {apiKey}
Content-Type: application/json
```

Тело запроса соответствует [официальной спецификации OpenAI](https://platform.openai.com/docs/api-reference/chat):

```jsonc
{
  "model": "openai/gpt-5.6-terra-pro",       // первая модель из списка
  "messages": [                               // система + история + вопрос
    {"role": "system",  "content": "..."},
    {"role": "user",    "content": "вопрос"},
    {"role": "assistant","content": "ответ"},
    {"role": "user",    "content": "новый вопрос"}
  ],
  "max_completion_tokens": 4096,              // бюджет на ответ + размышления
  "reasoning_effort": "low",                  // сколько модель думает
  "stream": false                             // всегда false: бот правит одно сообщение
}
```

### Ключевые соответствия спецификации

| Поле | Назначение | Наша конфиг-переменная |
|---|---|---|
| `model` | ID модели (можно список через запятую для фолбэка) | `LLM_MODEL` |
| `max_completion_tokens` | Верхняя граница токенов ответа + reasoning | `LLM_MAX_TOKENS` (4096) |
| `reasoning_effort` | Усилие размышления модели | `LLM_REASONING_EFFORT` (low) |
| `stream` | Потоковая передача | Всегда `false` |
| `messages[].role` | `system`, `user`, `assistant` | `LLM_SYSTEM_PROMPT` |

> **Важно:** поле `max_tokens` устарело с выходом reasoning-моделей. Бот использует `max_completion_tokens` — актуальное поле из [спецификации OpenAI](https://platform.openai.com/docs/api-reference/chat/create).

> **Обработка отказа:** если модель отказывается отвечать, в ответе `choices[0].message.refusal` содержится текст отказа, а `content` пуст. Бот парсит `refusal` и возвращает ошибку `"model refused: {текст}"` вместо безликого «empty answer».

### reasoning_effort

Допустимые значения (по спецификации OpenAI):

| Значение | Когда ставить |
|---|---|
| `none` | Модель не рассуждает вообще |
| `minimal` | Минимум — быстрые ответы |
| `low` | **По умолчанию** — чат, где размышления невидимы и не должны съедать бюджет |
| `medium` | Сложные вопросы |
| `high` | Глубокий анализ |
| `xhigh` | Максимум размышлений |
| `max` | Абсолютный максимум |

Reasoning-модели списывают токены размышлений из того же `max_completion_tokens`. Если бюджет мал, он тратится на невидимое «обдумывание» — ответ приходит пустым. В этом случае бот прямо сообщает: «raise LLM_MAX_TOKENS».

### Фолбэк моделей и ротация ключей

Модели перебираются по порядку. На каждой — весь пул ключей:

```
модель 1 × ключ 1 → 429 → ключ 2 → 429 → ... 
модель 2 × ключ 1 → 200 ✅
```

| Поведение | Реакция |
|---|---|
| `429 Too Many Requests` или `402 Payment Required` | Ключ откладывается на `LLM_KEY_COOLDOWN`, пробуется следующий |
| `5xx` | Считается временным — пробуется следующий ключ, но не называется лимитом |
| `400` | Сломанный запрос — возвращается сразу, ключи не тратятся |
| Все ключи × все модели исчерпаны | `ErrRateLimited` — пользователю: «Лимиты исчерпаны» |

---

## Генерация картинок

Через [OpenAI Images API](https://platform.openai.com/docs/api-reference/images) (`/images/generations`):

```
POST {baseURL}/images/generations
Authorization: Bearer {apiKey}
```

```jsonc
{
  "model": "openai/gpt-image-1",
  "prompt": "кот в скафандре",
  "n": 1,
  "response_format": "b64_json",   // только для dall-e-*; gpt-image-* всегда возвращает base64
  "size": "1024x1024"
}
```

Выключена по умолчанию. Для включения нужны:

| Переменная | Описание |
|---|---|
| `IMAGE_MODEL` | Модель генерации (`gpt-image-1`, `dall-e-3`, ...) |
| `IMAGE_STORAGE_CHAT_ID` | Чат для загрузки картинки (guest mode не умеет заливать файлы) |
| `IMAGE_API_KEYS` | Ключ с биллингом, если основной не подходит (по умолчанию = `OPENAI_API_KEYS`) |
| `IMAGE_SIZE` | Разрешение (`1024x1024`) |
| `IMAGE_TIMEOUT` | Таймаут (2m) |

`401`, `402`, `403`, `429` от image API считаются «нет кредитов» (`ErrOutOfCredits`) — ключ паркуется, пробуется следующий; пользователь видит «Нечем оплатить картинку», а не «попробуйте позже». `5xx` — transient, ретраится на следующем ключе.

---

## Проверка ключей

```
GET {baseURL}/models
Authorization: Bearer {apiKey}
```

Бесплатный запрос: возвращает список доступных моделей. Используется в `/admin → Лимиты → Проверить ключи` для проверки живости каждого ключа. Не тратит квоту генерации.

---

## Конфигурация: полный список переменных

### Обязательные

| Переменная | Что делает |
|---|---|
| `TELEGRAM_BOT_TOKEN` | Токен от @BotFather |
| `OPENAI_API_KEYS` | Ключи API через запятую (пул для ротации) |

### Провайдер и модель

| Переменная | По умолчанию | Что делает |
|---|---|---|
| `LLM_BASE_URL` | `https://openrouter.ai/api/v1` | OpenAI-совместимый эндпоинт |
| `LLM_MODEL` | `openai/gpt-5.6-terra-pro,openai/gpt-5.6-terra` | Модели (фолбэк по порядку) |
| `LLM_SYSTEM_PROMPT` | встроенный промпт | Системный промпт для чата |
| `LLM_RAW_SYSTEM_PROMPT` | встроенный | Замена промпта по флагу `-s`; `off` выключает |
| `LLM_MAX_TOKENS` | `4096` | Бюджет на ответ + reasoning |
| `LLM_REASONING_EFFORT` | `low` | none \| minimal \| low \| medium \| high \| xhigh \| max |
| `LLM_TIMEOUT` | `30s` | Таймаут запроса к модели |
| `LLM_KEY_COOLDOWN` | `1m` | Сколько ключ отдыхает после 429 |

### Доступ

| Переменная | Что делает |
|---|---|
| `ADMIN_USER_IDS` | Админы (всегда могут звать, управляют /add /del /list) |
| `ALLOWED_USER_IDS` | Люди, которые могут звать бота где угодно |
| `ALLOWED_CHAT_IDS` | Чаты, где бот отвечает всем (ID групп отрицательные) |
| `PUBLIC_DAILY_LIMIT` | 0 = приватный; >0 = публичный с дневным лимитом на человека |

### Публичный доступ (засевают настройки при первом запуске)

| Переменная | По умолчанию | Что делает |
|---|---|---|
| `PUBLIC_GLOBAL_DAILY_LIMIT` | `0` (без потолка) | Потолок на весь бот в сутки |
| `NEW_ACCOUNT_ID_THRESHOLD` | | Свежим аккаунтам — половина лимита |
| `PUBLIC_BURST` | `5` | Запросов за окно до варна |
| `PUBLIC_BURST_WINDOW` | `1m` | Окно всплеска |
| `PUBLIC_BAN_FOR` | `15m` | Таймаут после варна |
| `PUBLIC_MAX_TOKENS` | `1024` | Потолок ответа публике |
| `PUBLIC_MAX_RUNES` | `2000` | Потолок длины вопроса от публики |
| `MAX_QUEUE` | `32` | Лимит очереди ожидающих |
| `PUBLIC_WARN_TTL` | `720h` (30 дней) | Сколько варн висит |
| `PUBLIC_MAX_WARNS` | `3` | Живых варнов → бан навсегда |

### Картинки

| Переменная | Что делает |
|---|---|
| `IMAGE_MODEL` | Модель генерации (пусто = выключено) |
| `IMAGE_BASE_URL` | Эндпоинт image API (по умолчанию = `LLM_BASE_URL`) |
| `IMAGE_API_KEYS` | Ключи для картинок (по умолчанию = `OPENAI_API_KEYS`) |
| `IMAGE_STORAGE_CHAT_ID` | Чат для загрузки (обязателен при `IMAGE_MODEL`) |
| `IMAGE_SIZE` | Разрешение |
| `IMAGE_TIMEOUT` | `2m` |

### Прочее

| Переменная | По умолчанию | Что делает |
|---|---|---|
| `ANSWER_CACHE_TTL` | `10m` | Кэш одинаковых вопросов; 0 = выкл |
| `ALERT_COOLDOWN` | `15m` | Минимальный интервал между алертами одного вида |
| `MAX_CONCURRENT` | `8` | Параллельных запросов к модели |
| `MAX_REPLY_RUNES` | `3500` | Максимальная длина ответа |
| `MEMORY_TTL` | `30m` | Сколько бот помнит контекст диалога |
| `COMMAND_REPLY_TTL` | `30s` | Через сколько ответ команды сжимается до ✓ |
| `LOG_LEVEL` | `info` | debug \| info \| warn \| error |
| `STATE_FILE` | `state.json` | Вайтлист (/add /del) |
| `QUOTA_FILE` | рядом со state | Счётчики и баны публики |
| `SETTINGS_FILE` | рядом со state | Настройки из панели |

---

## Архитектура запроса

```
Пользователь зовёт @botname в чате
        │
        ▼
┌─ reply.Handler ──────────────────────────────┐
│  1. Проверка доступа (access)                 │
│  2. Команда? → admin.Handle (без модели)     │
│  3. Картинка? → intent.Detect → imagegen      │
│  4. Текст:                                   │
│     a. Заглушка ●●● (AnswerGuestQuery)        │
│     b. Кэш? (answers.Cache)                   │
│     c. llm.Complete → POST /chat/completions   │
│        ├─ keyring.Lease (ротация ключей)     │
│        ├─ модель 1 → 429 → модель 2          │
│        └─ 200 → ответ                         │
│     d. memory.Remember (контекст)             │
│     e. editText → готовый ответ в Markdown    │
└───────────────────────────────────────────────┘
```

---

## Команды админа

Все разбираются до похода в модель — отвечают мгновенно и не тратят квоту.

| Команда | Действие |
|---|---|
| `/add <id>` | Выдать доступ (id>0 — пользователь, id<0 — группа) |
| `/del <id>` | Забрать доступ (записи из .env нельзя удалить) |
| `/unban <id>` | Снять бан за флуд |
| `/list` | Показать текущий список |
| `/help` | Справка |

Флаг `-s` перед вопросом заменяет системный промпт на `LLM_RAW_SYSTEM_PROMPT` — без правил про длину и тон:
```
@dr0wbot -s распиши архитектуру целиком
```

---

## Структура пакетов

```
cmd/bot/           точка входа: long polling, graceful shutdown
internal/
├── config/        env → Config (все ошибки разом)
├── access/        вайтлист + сохранение на диск
├── admin/         /add /del /list /unban
├── keyring/       пул ключей: ротация, остывание после 429
├── llm/           клиент OpenAI Chat Completions: модели × ключи
├── imagegen/      клиент Images API (выключен по умолчанию)
├── quota/         лимиты публики, всплески, варны, баны
├── answers/       кэш одинаковых вопросов
├── alert/         алерты админам (с антиспамом)
├── settings/      настройки из панели, на диске
├── menu/          личка: /start, /admin, инлайн-кнопки
├── tgemoji/       премиум-эмодзи + фолбэк
├── reply/         заглушка → модель → правка
├── mdtext/        вырезание @mention, сборка промпта, обрезка
├── intent/        детектор «нарисуй» → imagegen
├── memory/        контекст диалога в памяти процесса
└── tgfile/        загрузка вложений по file_id
```

---

## Ссылки

- [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat)
- [OpenAI Images API](https://platform.openai.com/docs/api-reference/images)
- [OpenRouter](https://openrouter.ai) — агрегатор моделей с единым API
- [telego](https://github.com/mymmrac/telego) — Telegram Bot API для Go
