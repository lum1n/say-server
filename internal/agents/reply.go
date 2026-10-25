package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/llm"
	"github.com/vegard/say-v2/internal/store"
)

// historyLimit is how many messages up to the trigger the model sees.
const historyLimit = 40

// reply asks the model for an answer to trigger and streams it into a
// message. The model sees the room up to seq through, which is past the
// trigger when the trigger's stream ended later.
func (w *worker) reply(ctx context.Context, room core.Room, trigger core.Event, through int64) {
	// Checking the room's limits first saves a model call whose answer
	// the send would refuse. The send checks them again.
	policy := w.c.Chat.AgentPolicy(room)
	if policy.Paused {
		return
	}
	if _, err := core.Next(&trigger, w.me.Kind, policy.MaxHops); errors.Is(err, core.ErrHopLimit) {
		slog.Debug("built-in agent: hop limit reached", "agent", w.me.ID, "room", room.ID)
		return
	}
	req, err := w.request(ctx, room, through)
	if err != nil {
		w.warn(ctx, "read history", err)
		return
	}
	// mctx bounds the model calls; out cancels it when the message cannot
	// be written.
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &output{w: w, room: room.ID, trigger: trigger, cancel: cancel}
	req.OnText = func(text string) { out.show(ctx, text) }
	var tools *toolbox
	var toolErr error
	if len(w.cfg.MCPServers) > 0 {
		tools, toolErr = openTools(ctx, w.c.Version, w.cfg.MCPServers)
		defer tools.close()
		req.Tools = tools.list()
	}
	// abort keeps what was streamed; the failure is on record already.
	abort := func() {
		out.end(ctx, "")
		out.report(ctx)
	}
	// Ask the model, and run the tools it calls, until it answers.
	for round := 1; ; round++ {
		resp, ok := w.complete(mctx, req)
		if !ok {
			abort()
			return
		}
		// Without tools to offer, tool calls are ignored.
		if len(resp.ToolCalls) == 0 || len(req.Tools) == 0 {
			out.end(ctx, resp.Text)
			break
		}
		if round == maxToolRounds {
			w.fail(ctx, fmt.Errorf("the model still called tools after %d rounds", maxToolRounds))
			abort()
			return
		}
		out.next(ctx, resp.Text, resp.ToolCalls)
		req.Messages = append(req.Messages, llm.Message{Role: "assistant", Text: resp.Text, ToolCalls: resp.ToolCalls})
		results := make([]llm.ToolResult, len(resp.ToolCalls))
		for i, c := range resp.ToolCalls {
			results[i] = tools.call(ctx, c)
			out.called(ctx, results[i])
			slog.Info("built-in agent called a tool", "agent", w.me.DisplayName, "tool", c.Name, "failed", results[i].IsError)
		}
		req.Messages = append(req.Messages, llm.Message{Role: "user", ToolResults: results})
		// Tools take time; the room may have been paused meanwhile.
		if room, err := w.c.Store.GetRoom(ctx, room.ID); err != nil || w.c.Chat.AgentPolicy(room).Paused {
			abort()
			return
		}
	}
	if out.report(ctx) {
		return
	}
	// A server that could not be reached stays on record after a reply.
	if toolErr != nil {
		toolErr = fmt.Errorf("replied without some tools: %w", toolErr)
	}
	w.fail(ctx, toolErr)
}

// streamEvery is how often a streaming reply is written at most.
var streamEvery = 250 * time.Millisecond

// errTaken means another node is already writing the reply: during a
// lease handover both holders may answer the same trigger.
var errTaken = errors.New("another node is writing this reply")

// output writes one reply. Its message starts streaming with the first
// text or tool call, so an answer that turns out empty sends nothing. The
// text of every round stays in it, followed by the tools that round called.
type output struct {
	w       *worker
	room    string
	trigger core.Event
	cancel  context.CancelFunc

	done    []core.Part // the parts of finished rounds
	text    string      // the current round's text so far
	id      string      // the message, once started
	written []core.Part
	at      time.Time
	err     error // why the reply cannot be written
}

// maxToolParts bounds the tool calls shown in one reply, which leaves
// room in a message for the text of every round. Later calls still run.
const maxToolParts = 20

// show writes text, the current round's so far, unless it was written
// less than streamEvery ago.
func (o *output) show(ctx context.Context, text string) {
	o.text = text
	if time.Since(o.at) >= streamEvery {
		o.write(ctx)
	}
}

// next ends a round that called tools, and shows them running.
func (o *output) next(ctx context.Context, text string, calls []llm.ToolCall) {
	o.done, o.text = o.parts(text), ""
	shown := 0
	for _, p := range o.done {
		if p.Kind == core.PartToolCall {
			shown++
		}
	}
	for _, c := range calls[:min(len(calls), max(0, maxToolParts-shown))] {
		o.done = append(o.done, core.ToolCall{ID: c.ID, Name: c.Name, Input: toolInput(c.Input), Status: core.ToolRunning}.Part())
	}
	o.write(ctx)
}

// called shows how the tool call r answers went.
func (o *output) called(ctx context.Context, r llm.ToolResult) {
	for i, p := range o.done {
		var c core.ToolCall
		if p.Kind != core.PartToolCall || json.Unmarshal(p.Data, &c) != nil || c.ID != r.CallID || c.Status != core.ToolRunning {
			continue
		}
		c.Status = core.ToolDone
		if r.IsError {
			c.Status, c.Error = core.ToolFailed, cut(r.Content, maxToolError)
		}
		o.done[i] = c.Part()
		o.write(ctx)
		return
	}
}

// parts is the message with text as the current round's.
func (o *output) parts(text string) []core.Part {
	parts := slices.Clone(o.done)
	if text = strings.TrimSpace(text); text != "" {
		parts = append(parts, core.TextPart(text))
	}
	return parts
}

// write writes the message as it is now.
func (o *output) write(ctx context.Context) {
	parts := o.parts(o.text)
	if o.err != nil || len(parts) == 0 || samePartsAs(parts, o.written) {
		return
	}
	if o.id == "" {
		// The key makes the reply to a message unique, so two nodes that
		// both think they hold the agent during a lease handover send it
		// once.
		e, created, err := o.w.c.Chat.SendParts(ctx, o.w.me, o.room, parts, o.trigger.ID, "builtin:"+o.trigger.ID, true)
		if err == nil && !created {
			err = errTaken
		}
		if err != nil {
			o.stop(err)
			return
		}
		o.id = e.ID
	} else if err := o.w.c.Chat.StreamParts(ctx, o.w.me, o.room, o.id, parts); err != nil {
		o.stop(err)
		return
	}
	o.written, o.at = parts, time.Now()
}

// end finishes the message with the last round's text; on a failure, with
// "", it keeps what was written. A reply that never started is sent whole.
func (o *output) end(ctx context.Context, text string) {
	if o.err != nil {
		return
	}
	parts := o.parts(text)
	if o.id == "" {
		if len(parts) == 0 {
			return
		}
		e, created, err := o.w.c.Chat.SendParts(ctx, o.w.me, o.room, parts, o.trigger.ID, "builtin:"+o.trigger.ID, false)
		if err == nil && !created && e.Stream == core.StreamStreaming {
			err = errTaken
		}
		o.err = err
		return
	}
	if text == "" || samePartsAs(parts, o.written) {
		parts = nil
	}
	o.err = o.w.c.Chat.EndParts(ctx, o.w.me, o.room, o.id, parts)
}

func (o *output) stop(err error) {
	o.err = err
	o.cancel()
}

// report records why the reply could not be written, if it could not, and
// says whether it did.
func (o *output) report(ctx context.Context) bool {
	if o.err != nil && !errors.Is(o.err, errTaken) {
		o.w.fail(ctx, o.err)
	}
	return o.err != nil
}

func samePartsAs(a, b []core.Part) bool {
	return slices.EqualFunc(a, b, func(x, y core.Part) bool { return x.Kind == y.Kind && bytes.Equal(x.Data, y.Data) })
}

// maxToolInput and maxToolError bound what a reply shows of a tool call.
const (
	maxToolInput = 4 << 10
	maxToolError = 500
)

// toolInput is a call's arguments as shown in the room: a JSON object of
// at most maxToolInput bytes, or nothing.
func toolInput(in json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if len(in) > maxToolInput || json.Unmarshal(in, &obj) != nil || obj == nil {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, in) != nil {
		return nil
	}
	return b.Bytes()
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// complete makes one model call within the daily budgets and records its
// usage. It reports false, having recorded why, when there is no answer.
func (w *worker) complete(ctx context.Context, req llm.Request) (llm.Response, bool) {
	if tokens, cost := w.cfg.DailyTokenBudget, w.cfg.DailyCostBudget; tokens > 0 || cost > 0 {
		used, err := w.c.Store.AgentUsage(ctx, w.me.ID, time.Now())
		if err != nil {
			w.warn(ctx, "read usage", err)
			return llm.Response{}, false
		}
		if tokens > 0 && used.Total() >= tokens {
			w.fail(ctx, fmt.Errorf("the daily budget of %d tokens is spent", tokens))
			return llm.Response{}, false
		}
		if cost > 0 && used.Cost >= cost {
			w.fail(ctx, fmt.Errorf("the daily budget of $%s is spent", strconv.FormatFloat(float64(cost)/1e6, 'f', -1, 64)))
			return llm.Response{}, false
		}
	}
	cctx, cancel := context.WithTimeout(ctx, replyTimeout)
	resp, err := w.llm.Complete(cctx, req)
	cancel()
	if resp.InputTokens+resp.OutputTokens > 0 {
		u := store.Usage{
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			Cost:         w.cfg.Cost(resp.InputTokens, resp.OutputTokens),
		}
		if err := w.c.Store.AddAgentUsage(context.WithoutCancel(ctx), w.me.ID, time.Now(), u); err != nil {
			w.warn(ctx, "record usage", err)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			w.fail(ctx, err)
		}
		return llm.Response{}, false
	}
	return resp, true
}

// request builds the model input: the agent's instructions and the room's
// recent messages up to seq through, the agent's own as assistant turns.
// Messages show as they are now, so streamed and edited ones are whole.
func (w *worker) request(ctx context.Context, room core.Room, through int64) (llm.Request, error) {
	events, err := w.c.Store.EventsBefore(ctx, room.ID, through+1, historyLimit)
	if err != nil {
		return llm.Request{}, err
	}
	var msgs []llm.Message
	for _, e := range events {
		if e.Type != core.EventMessage {
			continue
		}
		// The agent's own messages leave out its tool calls: shown as
		// [used name], they would teach a model to write calls out.
		m := llm.Message{Role: "assistant", Text: e.Prose()}
		if e.SenderID == w.me.ID && m.Text == "" {
			continue
		}
		if e.SenderID != w.me.ID {
			name, err := w.name(ctx, e.SenderID)
			if err != nil {
				return llm.Request{}, err
			}
			m = llm.Message{Role: "user", Text: name + ": " + e.Text()}
		}
		// Merge turns from the same side, and start with a user turn.
		if n := len(msgs); n > 0 && msgs[n-1].Role == m.Role {
			msgs[n-1].Text += "\n\n" + m.Text
		} else if n > 0 || m.Role == "user" {
			msgs = append(msgs, m)
		}
	}
	return llm.Request{System: w.system(room), Messages: msgs, MaxTokens: w.cfg.MaxTokens}, nil
}

func (w *worker) system(room core.Room) string {
	where := "a chat room"
	if room.Name != "" {
		where = fmt.Sprintf("the chat room %q", room.Name)
	}
	if room.Kind == core.RoomDM {
		where = "a direct conversation"
	}
	s := fmt.Sprintf("You are %s, taking part in %s on say. "+
		"Other people's messages are shown as \"Name: message\"; they mention you as @%s. "+
		"Reply with the text of your next message only, without your name in front. "+
		"Keep replies short and conversational unless you are asked for more.",
		w.me.DisplayName, where, w.me.DisplayName)
	if p := strings.TrimSpace(w.cfg.SystemPrompt); p != "" {
		s += "\n\n" + p
	}
	return s
}

func (w *worker) name(ctx context.Context, id string) (string, error) {
	if n, ok := w.names[id]; ok {
		return n, nil
	}
	p, err := w.c.Store.GetPrincipal(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		p.DisplayName = "someone"
	} else if err != nil {
		return "", err
	}
	w.names[id] = p.DisplayName
	return p.DisplayName, nil
}

// maxErrLen keeps a provider's error page from filling the admin's screen.
const maxErrLen = 500

// fail records why the agent could not reply, for admins to see, or clears
// the record when err is nil.
func (w *worker) fail(ctx context.Context, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
		msg = cut(msg, maxErrLen)
		slog.Warn("built-in agent could not reply", "agent", w.me.DisplayName, "id", w.me.ID, "err", err)
	}
	if msg == w.lastErr {
		return
	}
	if err := w.c.Store.SetBuiltinAgentError(context.WithoutCancel(ctx), w.me.ID, msg); err != nil {
		slog.Warn("built-in agent: record error failed", "agent", w.me.ID, "err", err)
		return
	}
	w.lastErr = msg
}

// warn logs a store failure. The next resync retries.
func (w *worker) warn(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		slog.Warn("built-in agent: "+what+" failed", "agent", w.me.ID, "err", err)
	}
}
