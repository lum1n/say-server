package core

import "testing"

func TestDescribe(t *testing.T) {
	names := map[string]string{"ada": "Ada", "bob": "Bob", "bot": "Support bot"}
	name := func(id string) string { return names[id] }
	yes, no, three := true, false, 3

	tests := []struct {
		e    Event
		want string
	}{
		{Event{Type: EventAgentSettings, SenderID: "ada", Settings: &AgentSettingsChange{Paused: &yes}}, "Ada paused agents"},
		{Event{Type: EventAgentSettings, SenderID: "ada", Settings: &AgentSettingsChange{Paused: &no}}, "Ada resumed agents"},
		{Event{Type: EventAgentSettings, SenderID: "ada", Settings: &AgentSettingsChange{MaxHopCount: &three}}, "Ada set the agent hop limit to 3"},
		{Event{Type: EventAgentSettings, SenderID: "ada", Settings: &AgentSettingsChange{Paused: &yes, MaxHopCount: &three}}, "Ada paused agents and set the agent hop limit to 3"},
		{Event{Type: EventHandoff, SenderID: "bob", Handoff: &Handoff{From: "bot", To: "bob"}}, "Bob took over from Support bot"},
		{Event{Type: EventHandoff, SenderID: "bob", Handoff: &Handoff{To: "bob"}}, "Bob took over"},
		{Event{Type: EventHandoff, SenderID: "ada", Handoff: &Handoff{From: "bot", To: "bob"}}, "Ada passed the conversation from Support bot to Bob"},
		{Event{Type: EventHandoff, SenderID: "bob", Handoff: &Handoff{From: "bob", To: "bot"}}, "Bob handed over to Support bot"},
		{Event{Type: EventMessage, SenderID: "ada"}, ""},
		{Event{Type: EventAgentSettings, SenderID: "ada"}, ""},
	}
	for _, tt := range tests {
		if got := Describe(tt.e, name); got != tt.want {
			t.Errorf("Describe(%+v) = %q, want %q", tt.e, got, tt.want)
		}
	}
}
