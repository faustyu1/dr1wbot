package memory

import (
	"testing"
	"time"
)

func TestReplyToAnOlderAnswerBranchesOffIt(t *testing.T) {
	s := New(Options{})
	s.Remember(1, nil, "что такое CAP", "CAP — это теорема")
	first := s.Thread(1, "CAP — это теорема")
	s.Remember(1, first, "а подробнее?", "Подробно: согласованность")
	s.Remember(1, nil, "сколько будет 2+2", "Четыре")

	got := s.Thread(1, "CAP — это теорема")
	if len(got) != 2 || got[0].Content != "что такое CAP" {
		t.Fatalf("Thread(first answer) = %+v, want only the exchange that led to it", got)
	}
	got = s.Thread(1, "Подробно: согласованность")
	if len(got) != 4 {
		t.Fatalf("Thread(follow-up) = %+v, want the whole chain", got)
	}
	if latest := s.Latest(1); len(latest) != 2 || latest[1].Content != "Четыре" {
		t.Errorf("Latest() = %+v, want the last answer's thread", latest)
	}
}

func TestThreadIsFoundByTheRenderedText(t *testing.T) {
	s := New(Options{})
	s.Remember(1, nil, "вопрос", "# Заголовок\n\nВот **жирное** и [ссылка](https://example.com) дальше.")

	// What a reply quotes is the message as Telegram shows it, markup gone.
	if got := s.Thread(1, "Заголовок\nВот жирное и ссылка дальше."); len(got) == 0 {
		t.Error("Thread() = empty, want the answer found despite the Markdown")
	}
}

func TestThreadsAreKeptPerChat(t *testing.T) {
	s := New(Options{})
	s.Remember(1, nil, "вопрос", "ответ")
	if got := s.Thread(2, "ответ"); len(got) != 0 {
		t.Errorf("Thread(other chat) = %+v, want nothing", got)
	}
}

func TestThreadsGoCold(t *testing.T) {
	now := time.Unix(0, 0)
	s := New(Options{TTL: time.Minute, Now: func() time.Time { return now }})
	s.Remember(1, nil, "вопрос", "ответ")
	now = now.Add(2 * time.Minute)
	if got := s.Thread(1, "ответ"); len(got) != 0 {
		t.Errorf("Thread() = %+v, want an expired thread gone", got)
	}
	if got := s.Latest(1); len(got) != 0 {
		t.Errorf("Latest() = %+v, want an expired thread gone", got)
	}
}

func TestThreadIsCappedAtMaxTurns(t *testing.T) {
	s := New(Options{})
	var thread []Turn
	for i := 0; i < MaxTurns; i++ {
		answer := "ответ " + string(rune('a'+i))
		s.Remember(1, thread, "вопрос", answer)
		thread = s.Thread(1, answer)
	}
	if len(thread) != MaxTurns {
		t.Errorf("len(thread) = %d, want %d", len(thread), MaxTurns)
	}
}
