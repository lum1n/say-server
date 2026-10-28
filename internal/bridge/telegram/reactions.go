package telegram

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gotd/td/tg"

	"github.com/lum1n/say-server/internal/bridge"
)

// Telegram tells of reactions as a message's whole list, not as who added
// or took back what, so the receiver keeps each message's last list and
// sends on the difference. The list names people only in small chats and
// for the latest few reactions (RecentReactions); someone falling off it
// is taken as a removal only when that emoji's count went down too, and
// reactions it never names are not bridged. The account's own come from
// ChosenOrder, which a "min" update leaves out, so they are kept as they
// were then.

// maxReactionMemory bounds the messages whose reactions are remembered.
const maxReactionMemory = 2000

// selfPeer stands for the account in a reactionState.
const selfPeer = "self"

// reactionState is one message's reactions as last seen.
type reactionState struct {
	counts map[string]int   // emoji → how many
	by     map[reactor]bool // who reacted with what, as far as Telegram says
}

type reactor struct {
	peer  string // a chat id, or selfPeer
	emoji string
}

// reactionChange is someone adding or taking back an emoji.
type reactionChange struct {
	reactor
	removed bool
}

// readReactions makes a state of the list Telegram sent; prev supplies the
// account's own reactions when the list is "min".
func readReactions(r tg.MessageReactions, self int64, prev *reactionState) reactionState {
	s := reactionState{counts: map[string]int{}, by: map[reactor]bool{}}
	for _, c := range r.Results {
		e, ok := c.Reaction.(*tg.ReactionEmoji)
		if !ok {
			continue
		}
		s.counts[e.Emoticon] = c.Count
		if _, chosen := c.GetChosenOrder(); chosen && !r.Min {
			s.by[reactor{selfPeer, e.Emoticon}] = true
		}
	}
	if r.Min && prev != nil {
		for who := range prev.by {
			if who.peer == selfPeer && s.counts[who.emoji] > 0 {
				s.by[who] = true
			}
		}
	}
	for _, p := range r.RecentReactions {
		e, ok := p.Reaction.(*tg.ReactionEmoji)
		if !ok {
			continue
		}
		peer, ok := peerID(p.PeerID)
		if u, isUser := p.PeerID.(*tg.PeerUser); p.My || isUser && u.UserID == self {
			peer, ok = selfPeer, true
		}
		if ok {
			s.by[reactor{peer, e.Emoticon}] = true
		}
	}
	return s
}

// diffReactions returns what changed from prev to now. With no prev, the
// message is new to the receiver and only additions are known.
func diffReactions(prev *reactionState, now reactionState) []reactionChange {
	var out []reactionChange
	if prev != nil {
		for who := range prev.by {
			if now.by[who] {
				continue
			}
			if who.peer == selfPeer || now.counts[who.emoji] < prev.counts[who.emoji] {
				out = append(out, reactionChange{who, true})
			}
		}
	}
	for who := range now.by {
		if prev == nil || !prev.by[who] {
			out = append(out, reactionChange{who, false})
		}
	}
	return out
}

// reactions queues the changes in a message's reactions.
func (r *receiver) reactions(peer tg.PeerClass, msgID int, list tg.MessageReactions) {
	chat, ok := remoteChat(r.st, peer)
	if !ok {
		return
	}
	key := chat.ID + "/" + strconv.Itoa(msgID)
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]reactionState{}
	}
	var prev *reactionState
	if p, ok := r.seen[key]; ok {
		prev = &p
	}
	now := readReactions(list, r.st.self(), prev)
	if len(r.seen) >= maxReactionMemory && prev == nil {
		for k := range r.seen {
			delete(r.seen, k)
			break
		}
	}
	r.seen[key] = now
	r.mu.Unlock()
	for _, c := range diffReactions(prev, now) {
		a := bridge.Author{Self: true}
		if c.peer != selfPeer {
			a = bridge.Author{ID: c.peer, Name: r.st.name(c.peer)}
			if a.Name == "" {
				a.Name = c.peer
			}
		}
		r.q.push(bridge.Incoming{
			Chat:     chat,
			Author:   a,
			Time:     time.Now(),
			Reaction: &bridge.IncomingReaction{Target: strconv.Itoa(msgID), Key: c.emoji, Removed: c.removed},
		})
	}
}

// React sets the owner's reaction on a message: Telegram replaces the
// account's reactions with the list sent, so an addition sends the one
// emoji and a removal none. An emoji the chat does not allow is refused.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	api := c.client()
	if api == nil {
		return "", fmt.Errorf("telegram: not connected")
	}
	id, err := strconv.Atoi(r.Target)
	if err != nil {
		return "", bridge.Rejected(fmt.Errorf("telegram: bad message id %q", r.Target))
	}
	peer, err := c.inputPeer(ctx, api, r.Chat)
	if err != nil {
		return "", bridge.Rejected(err)
	}
	req := &tg.MessagesSendReactionRequest{Peer: peer, MsgID: id}
	if !r.Removed {
		req.Reaction = []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: r.Key}}
	}
	if _, err := api.MessagesSendReaction(ctx, req); err != nil {
		return "", callErr(err)
	}
	return "", nil
}
