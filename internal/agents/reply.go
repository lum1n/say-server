package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/llm"
	"github.com/vegard/say-v2/internal/store"
)

// historyLimit is how many messages up to the trigger the model sees.
const historyLimit = 40

// reply asks the model for an answer to trigger and sends it.
func (w *worker) reply(ctx context.Context, room core.Room, trigger core.Event) {
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
	if budget := w.cfg.DailyTokenBudget; budget > 0 {
		used, err := w.c.Store.AgentUsage(ctx, w.me.ID, time.Now())
		if err != nil {
			w.warn(ctx, "read usage", err)
			return
		}
		if used.Total() >= budget {
			w.fail(ctx, fmt.Errorf("the daily budget of %d tokens is spent", budget))
			return
		}
	}
	req, err := w.request(ctx, room, trigger)
	if err != nil {
		w.warn(ctx, "read history", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, replyTimeout)
	resp, err := w.llm.Complete(cctx, req)
	cancel()
	if resp.InputTokens+resp.OutputTokens > 0 {
		u := store.Usage{InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens}
		if err := w.c.Store.AddAgentUsage(context.WithoutCancel(ctx), w.me.ID, time.Now(), u); err != nil {
			w.warn(ctx, "record usage", err)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			w.fail(ctx, err)
		}
		return
	}
	if resp.Text == "" {
		return
	}
	// The key makes the reply to a message unique, so two nodes that both
	// think they hold the agent during a lease handover send it once.
	if _, err := w.c.Chat.SendText(ctx, w.me, room.ID, resp.Text, trigger.ID, "builtin:"+trigger.ID); err != nil {
		w.fail(ctx, err)
		return
	}
	w.fail(ctx, nil)
}

// request builds the model input: the agent's instructions and the room's
// recent messages up to trigger, the agent's own as assistant turns.
func (w *worker) request(ctx context.Context, room core.Room, trigger core.Event) (llm.Request, error) {
	events, err := w.c.Store.EventsBefore(ctx, room.ID, trigger.Seq+1, historyLimit)
	if err != nil {
		return llm.Request{}, err
	}
	var msgs []llm.Message
	for _, e := range events {
		if e.Type != core.EventMessage {
			continue
		}
		m := llm.Message{Role: "assistant", Text: e.Text()}
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
		if len(msg) > maxErrLen {
			msg = msg[:maxErrLen] + "…"
		}
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
