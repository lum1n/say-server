package core

import "testing"

func TestMentions(t *testing.T) {
	for _, c := range []struct {
		text, name string
		want       bool
	}{
		{"@Ada hi", "Ada", true},
		{"hi @ada", "Ada", true},
		{"hi @Ada, how are you", "Ada", true},
		{"hi @Adam", "Ada", false},
		{"@Adam and @Ada", "Ada", true},
		{"Ada hi", "Ada", false},
		{"email ada@example.com", "example", false},
		{"ask @Deep Thought", "Deep Thought", true},
		{"@Åse!", "åse", true},
		{"anything", "", false},
	} {
		if got := Mentions(c.text, c.name); got != c.want {
			t.Errorf("Mentions(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

func TestEventText(t *testing.T) {
	e := Event{Parts: []Part{
		{Kind: PartText, Data: []byte(`{"text":"hello"}`)},
		{Kind: PartAttachment, Data: []byte(`{}`)},
		{Kind: PartText, Data: []byte(`{"text":"world"}`)},
	}}
	if got := e.Text(); got != "hello\n[attachment]\nworld" {
		t.Fatalf("Text() = %q", got)
	}
}
