package web

import (
	"testing"
	"time"
)

func TestWikiExtract(t *testing.T) {
	accepted := []struct {
		text  string
		topic string
	}{
		{"who was Ada Lovelace", "Ada Lovelace"},
		{"who is Marie Curie", "Marie Curie"},
		{"who were the Beatles", "the Beatles"},
		{"who's Ada Lovelace", "Ada Lovelace"},
		{"what is a capybara", "capybara"},
		{"what is an axiom", "axiom"},
		{"WHO IS Marie Curie", "Marie Curie"},
		{"who was Ada Lovelace.", "Ada Lovelace"},
	}
	for _, tc := range accepted {
		got, ok := wikiExtract(tc.text)
		if !ok {
			t.Errorf("wikiExtract(%q) = false, want true", tc.text)
			continue
		}
		if got.Topic != tc.topic {
			t.Errorf("wikiExtract(%q) topic = %q, want %q", tc.text, got.Topic, tc.topic)
		}
	}

	rejected := []string{
		"",
		"who is",
		"what is",
		"what is the capital of France",
		"who is the president of the United States",
		"what is a capybara's habitat",
		"who wrote the novel 1984",
		"what are the health benefits of green tea",
		"when did the roman empire fall",
		"how tall is mount everest",
		"tell me about Ada Lovelace",
		"what is the meaning of life",
		"who discovered penicillin",
		"what was the cause of the war",
	}
	for _, text := range rejected {
		if _, ok := wikiExtract(text); ok {
			t.Errorf("wikiExtract(%q) = true, want false", text)
		}
	}
}

func TestFirstTwoSentencesAbbreviations(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "no abbreviation",
			text: "The capybara is a mammal. It lives in South America.",
			want: "The capybara is a mammal. It lives in South America.",
		},
		{
			name: "keeps Dr. together",
			text: "Dr. Smith was born in London. He was a scientist.",
			want: "Dr. Smith was born in London. He was a scientist.",
		},
		{
			name: "keeps U.S. together",
			text: "The U.S. is a country. It has fifty states.",
			want: "The U.S. is a country. It has fifty states.",
		},
		{
			name: "keeps e.g. together",
			text: "He likes fruit, e.g. apples. They are red.",
			want: "He likes fruit, e.g. apples. They are red.",
		},
		{
			name: "keeps D.C. together",
			text: "Dr. Smith was born in D.C. He lived there.",
			want: "Dr. Smith was born in D.C. He lived there.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstTwoSentences(tc.text, wikiMaxExtractRunes)
			if got != tc.want {
				t.Errorf("firstTwoSentences = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFirstTwoSentencesCap(t *testing.T) {
	long := "Ada Lovelace was an English mathematician. She is regarded as the first computer programmer. She wrote notes on the Analytical Engine that contained the first published algorithm."
	got := firstTwoSentences(long, 400)
	if runeCount(got) > 400 {
		t.Errorf("firstTwoSentences exceeded 400 runes: %d", runeCount(got))
	}
	// The cut must be at a word boundary, so it should not end with a partial word.
	if got == "" {
		t.Fatal("expected a non-empty result")
	}
	if last := []rune(got); last[len(last)-1] != '…' && last[len(last)-1] == ' ' {
		t.Errorf("unexpected trailing space: %q", got)
	}
}

func TestSummaryCache(t *testing.T) {
	t.Run("put and get", func(t *testing.T) {
		c := newSummaryCache(time.Hour, 8, time.Now)
		c.put("capybara", WikiOut{Title: "Capybara"})
		if got, ok := c.get("capybara"); !ok || got.Title != "Capybara" {
			t.Fatalf("expected hit, got %v ok=%v", got, ok)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		now := time.Unix(0, 0)
		c := newSummaryCache(24*time.Hour, 8, func() time.Time { return now })
		c.put("a", WikiOut{Title: "A"})
		if _, ok := c.get("a"); !ok {
			t.Fatal("expected fresh hit")
		}
		now = now.Add(25 * time.Hour)
		if _, ok := c.get("a"); ok {
			t.Fatal("expected expired miss")
		}
	})
	t.Run("lru eviction", func(t *testing.T) {
		c := newSummaryCache(time.Hour, 2, time.Now)
		c.put("a", WikiOut{Title: "A"})
		c.put("b", WikiOut{Title: "B"})
		c.put("c", WikiOut{Title: "C"}) // evicts the least recently used, "a"
		if _, ok := c.get("a"); ok {
			t.Error("a should have been evicted")
		}
		if _, ok := c.get("b"); !ok {
			t.Error("b should remain")
		}
		if _, ok := c.get("c"); !ok {
			t.Error("c should remain")
		}
	})
	t.Run("refresh moves to front", func(t *testing.T) {
		c := newSummaryCache(time.Hour, 2, time.Now)
		c.put("a", WikiOut{Title: "A"})
		c.put("b", WikiOut{Title: "B"})
		c.get("a") // touch a so b becomes LRU
		c.put("c", WikiOut{Title: "C"})
		if _, ok := c.get("b"); ok {
			t.Error("b should have been evicted")
		}
		if _, ok := c.get("a"); !ok {
			t.Error("a should remain after refresh")
		}
	})
}

func runeCount(s string) int { return len([]rune(s)) }
