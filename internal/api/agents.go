package api

import (
	"math"
	"net/http"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

// The server's agent policy for rooms that set no limit of their own
// (docs/adr/0005-agent-safety.md).
const (
	DefaultMaxHops    = 8
	DefaultTurnLimit  = 60
	DefaultTurnWindow = 10 * time.Minute
	DefaultRateLimit  = 10
)

// Bounds on what a room may set.
const (
	maxHopCount   = 64
	maxTurnLimit  = 10000
	minTurnWindow = time.Minute
	maxTurnWindow = 24 * time.Hour
	maxRateLimit  = 600
)

var builtinAgentPolicy = core.AgentPolicy{
	MaxHops:    DefaultMaxHops,
	TurnLimit:  DefaultTurnLimit,
	TurnWindow: DefaultTurnWindow,
	RateLimit:  DefaultRateLimit,
}

// AgentPolicy is the policy that applies in room.
func (s *Server) AgentPolicy(room core.Room) core.AgentPolicy {
	return withDefaults(room.Agents, s.defaults)
}

// withDefaults fills the limits p leaves at zero from d.
func withDefaults(p, d core.AgentPolicy) core.AgentPolicy {
	if p.MaxHops == 0 {
		p.MaxHops = d.MaxHops
	}
	if p.TurnLimit == 0 {
		p.TurnLimit = d.TurnLimit
	}
	if p.TurnWindow == 0 {
		p.TurnWindow = d.TurnWindow
	}
	if p.RateLimit == 0 {
		p.RateLimit = d.RateLimit
	}
	return p
}

// budgetErr turns a spent budget into a 429 with Retry-After.
func budgetErr(be *store.BudgetError) *apiError {
	retry := time.Duration(math.Ceil(time.Until(be.RetryAt).Seconds())) * time.Second
	retry = max(retry, time.Second)
	var ae *apiError
	if be.Sender {
		ae = errorf(http.StatusTooManyRequests, "rate_limited",
			"an agent may send %d messages a minute in this room; try again in %s", be.Budget.Max, retry)
	} else {
		ae = errorf(http.StatusTooManyRequests, "agent_budget",
			"agents may send %d messages per %s in this room; try again in %s", be.Budget.Max, be.Budget.Window, retry)
	}
	ae.RetryAfter = retry
	return ae
}

// updateRoom changes a room's agent policy. Only people who own or run the
// room may, so an agent can never lift its own limits or unpause itself.
func (s *Server) updateRoom(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	m, err := s.member(r.Context(), me, id)
	if err != nil {
		writeError(w, err)
		return
	}
	if me.Kind != core.PrincipalHuman || m.Role != core.RoleOwner && m.Role != core.RoleAdmin && !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people who own or run the room change its agent settings"))
		return
	}
	var body struct {
		Agents *wireAgentPatch `json:"agents"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	room, err := s.store.GetRoom(r.Context(), id)
	if err != nil {
		writeError(w, roomErr(err))
		return
	}
	if body.Agents != nil {
		if room.Agents, err = body.Agents.apply(room.Agents); err != nil {
			writeError(w, err)
			return
		}
		if err := s.store.SetAgentPolicy(r.Context(), id, room.Agents); err != nil {
			writeError(w, roomErr(err))
			return
		}
		// Every member's room list shows the room's settings.
		if ms, err := s.store.Members(r.Context(), id); err == nil {
			for _, m := range ms {
				s.notifyRooms(r.Context(), m.PrincipalID)
			}
		}
	}
	writeJSON(w, http.StatusOK, s.toRoom(room))
}

// wireAgentPatch is the agents object of PATCH /rooms/{id}. Absent fields
// stay as they are; a zero limit goes back to the server default.
type wireAgentPatch struct {
	Paused            *bool `json:"paused"`
	MaxHopCount       *int  `json:"max_hop_count"`
	TurnLimit         *int  `json:"turn_limit"`
	TurnWindowSeconds *int  `json:"turn_window_seconds"`
	RateLimit         *int  `json:"rate_limit_per_minute"`
}

func (p wireAgentPatch) apply(pol core.AgentPolicy) (core.AgentPolicy, error) {
	limit := func(name string, v *int, hi int, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > hi {
			return errorf(http.StatusUnprocessableEntity, "invalid", "%s must be from 0 (the server default) to %d", name, hi)
		}
		*dst = *v
		return nil
	}
	if p.Paused != nil {
		pol.Paused = *p.Paused
	}
	if err := limit("max_hop_count", p.MaxHopCount, maxHopCount, &pol.MaxHops); err != nil {
		return pol, err
	}
	if err := limit("turn_limit", p.TurnLimit, maxTurnLimit, &pol.TurnLimit); err != nil {
		return pol, err
	}
	if err := limit("rate_limit_per_minute", p.RateLimit, maxRateLimit, &pol.RateLimit); err != nil {
		return pol, err
	}
	if v := p.TurnWindowSeconds; v != nil {
		window := time.Duration(*v) * time.Second
		if *v != 0 && (window < minTurnWindow || window > maxTurnWindow) {
			return pol, errorf(http.StatusUnprocessableEntity, "invalid",
				"turn_window_seconds must be 0 (the server default) or from %d to %d", int(minTurnWindow.Seconds()), int(maxTurnWindow.Seconds()))
		}
		pol.TurnWindow = window
	}
	return pol, nil
}
