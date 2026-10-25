package core

import "encoding/json"

// ToolCall is the data of a tool-call part: a tool the sender used while
// writing the message, shown so people can see what an agent did. It
// carries the outcome but not the result, which may hold what the tool
// read from places the room's members cannot see.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is the call's arguments, a JSON object; absent when they were
	// too long or not valid JSON.
	Input  json.RawMessage `json:"input,omitempty"`
	Status ToolStatus      `json:"status"`
	// Error says why a failed call failed.
	Error string `json:"error,omitempty"`
}

type ToolStatus string

const (
	ToolRunning ToolStatus = "running"
	ToolDone    ToolStatus = "done"
	ToolFailed  ToolStatus = "failed"
)

// Part is c as a tool-call part.
func (c ToolCall) Part() Part {
	data, _ := json.Marshal(c)
	return Part{Kind: PartToolCall, Data: data}
}

// TextPart is a text part, for non-empty text.
func TextPart(text string) Part {
	data, _ := json.Marshal(map[string]string{"text": text})
	return Part{Kind: PartText, Data: data}
}
