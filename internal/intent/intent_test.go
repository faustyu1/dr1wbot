package intent

import "testing"

func TestDetectImage(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantPrompt string
	}{
		{name: "нарисуй", text: "нарисуй кота в скафандре", wantPrompt: "кота в скафандре"},
		{name: "капитализация не важна", text: "Нарисуй Кота", wantPrompt: "Кота"},
		{name: "нарисуй мне", text: "нарисуй мне закат", wantPrompt: "закат"},
		{name: "изобрази", text: "изобрази дракона", wantPrompt: "дракона"},
		{name: "рисуй", text: "рисуй схему сети", wantPrompt: "схему сети"},
		{name: "сгенерируй картинку", text: "сгенерируй картинку леса", wantPrompt: "леса"},
		{name: "сгенерируй фото", text: "сгенерируй фото машины", wantPrompt: "машины"},
		{name: "создай изображение", text: "создай изображение горы", wantPrompt: "горы"},
		{name: "двоеточие после триггера", text: "нарисуй: рыжий кот", wantPrompt: "рыжий кот"},
		{name: "запятая после триггера", text: "нарисуй, пожалуйста, кота", wantPrompt: "пожалуйста, кота"},
		{name: "английский draw", text: "draw a red cube", wantPrompt: "a red cube"},
		{name: "generate an image", text: "generate an image of a cat", wantPrompt: "of a cat"},
		{name: "явная команда", text: "/img кот на луне", wantPrompt: "кот на луне"},
		{name: "лишние пробелы", text: "   нарисуй   кота  ", wantPrompt: "кота"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, kind := Detect(tt.text)
			if kind != Image {
				t.Fatalf("Detect(%q) kind = Text, want Image", tt.text)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tt.wantPrompt)
			}
		})
	}
}

func TestDetectText(t *testing.T) {
	// A picture costs money and takes far longer than an answer, so anything
	// that only mentions drawing must stay on the text path.
	tests := []struct {
		name string
		text string
	}{
		{name: "пустая строка", text: ""},
		{name: "обычный вопрос", text: "что такое кворум?"},
		{name: "триггер не в начале", text: "как правильно нарисуй или нарисуйте?"},
		{name: "спрашивают про слово", text: "объясни слово нарисуй"},
		{name: "триггер как часть слова", text: "drawing board best practices"},
		{name: "нарисуйка не нарисуй", text: "нарисуйка это что"},
		{name: "просят текст про рисование", text: "расскажи как художники рисуют портреты"},
		{name: "команда админа", text: "/add 123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, kind := Detect(tt.text)
			if kind != Text {
				t.Errorf("Detect(%q) kind = Image, want Text (prompt %q)", tt.text, prompt)
			}
		})
	}
}

func TestDetectTextReturnsTrimmedInput(t *testing.T) {
	prompt, kind := Detect("  что такое кворум?  ")
	if kind != Text {
		t.Fatal("kind = Image, want Text")
	}
	if prompt != "что такое кворум?" {
		t.Errorf("prompt = %q, want the trimmed question", prompt)
	}
}

func TestDetectImageWithoutSubject(t *testing.T) {
	// The caller needs to tell "draw" apart from "draw a cat" so it can ask
	// what to draw instead of sending an empty prompt to the model.
	for _, text := range []string{"нарисуй", "/img", "draw"} {
		prompt, kind := Detect(text)
		if kind != Image {
			t.Errorf("Detect(%q) kind = Text, want Image", text)
		}
		if prompt != "" {
			t.Errorf("Detect(%q) prompt = %q, want empty", text, prompt)
		}
	}
}

func TestCommandWorksWithBotSuffix(t *testing.T) {
	// Telegram writes "/img@botname" when several bots are in the chat. The
	// suffix must not leak into the prompt, or the model draws the username.
	for _, text := range []string{"/img@dr0wbot кот на луне", "/img@dr0wbot  кот на луне"} {
		prompt, kind := Detect(text)
		if kind != Image {
			t.Errorf("Detect(%q) kind = Text, want Image", text)
		}
		if prompt != "кот на луне" {
			t.Errorf("Detect(%q) prompt = %q, want %q", text, prompt, "кот на луне")
		}
	}

	if prompt, kind := Detect("/img@dr0wbot"); kind != Image || prompt != "" {
		t.Errorf(`Detect("/img@dr0wbot") = (%q, %v), want ("", Image)`, prompt, kind)
	}
}
