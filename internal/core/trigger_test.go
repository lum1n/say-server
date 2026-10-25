package core

import (
	"testing"
	"time"
)

func TestTriggers(t *testing.T) {
	bot := Principal{ID: "bot", Kind: PrincipalAgent, DisplayName: "Echo"}
	now := time.Now()
	msg := func(sender, text string) Event {
		return Event{SenderID: sender, Type: EventMessage, Parts: []Part{TextPart(text)}}
	}
	group := Room{Kind: RoomGroup}
	member := Membership{Role: RoleMember}
	cases := []struct {
		name string
		room Room
		m    Membership
		msg  Event
		at   time.Time
		want Trigger
	}{
		{"mention", group, member, msg("ada", "hi @echo"), now, TriggerMention},
		{"no mention", group, member, msg("ada", "hi echo"), now, ""},
		{"dm", Room{Kind: RoomDM}, member, msg("ada", "hi"), now, TriggerDM},
		{"responder", group, Membership{Role: RoleMember, Scopes: []string{ScopeResponder}}, msg("ada", "hi"), now, TriggerResponder},
		{"observer", Room{Kind: RoomDM}, Membership{Role: RoleObserver}, msg("ada", "@echo"), now, ""},
		{"own message", Room{Kind: RoomDM}, member, msg("bot", "@echo"), now, ""},
		{"old", Room{Kind: RoomDM}, member, msg("ada", "hi"), now.Add(-MaxTriggerAge - time.Second), ""},
	}
	for _, c := range cases {
		if got := Triggers(bot, c.room, c.m, c.msg, c.at, now); got != c.want {
			t.Errorf("%s: Triggers = %q, want %q", c.name, got, c.want)
		}
	}
}
