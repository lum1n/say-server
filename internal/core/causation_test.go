package core

import (
	"errors"
	"testing"
)

func TestNext(t *testing.T) {
	human := &Event{ID: "e1"}
	c, err := Next(human, PrincipalAgent, 2)
	if err != nil || c != (Causation{CausedBy: "e1", HopCount: 1}) {
		t.Fatalf("agent reply to human = %+v, %v", c, err)
	}

	second := &Event{ID: "e2", Causation: c}
	c, err = Next(second, PrincipalAgent, 2)
	if err != nil || c.HopCount != 2 {
		t.Fatalf("second hop = %+v, %v", c, err)
	}

	third := &Event{ID: "e3", Causation: c}
	if _, err := Next(third, PrincipalAgent, 2); !errors.Is(err, ErrHopLimit) {
		t.Fatalf("third hop err = %v, want ErrHopLimit", err)
	}

	c, err = Next(third, PrincipalHuman, 2)
	if err != nil || c != (Causation{CausedBy: "e3"}) {
		t.Fatalf("human reply resets chain = %+v, %v", c, err)
	}

	if c, _ := Next(nil, PrincipalAgent, 0); c != (Causation{}) {
		t.Fatalf("no cause = %+v", c)
	}
}
