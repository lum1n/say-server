package core

import "errors"

// Causation links an event to the event that triggered it. HopCount is the
// number of automated hops since the last human event; it is how the core
// stops agent loops (docs/adr/0005-agent-safety.md).
type Causation struct {
	CausedBy string
	HopCount int
}

var ErrHopLimit = errors.New("core: max hop count reached")

// Next returns the causation for an event sent by a principal of kind in
// reply to cause. Non-automated senders reset the chain. It fails when an
// automated reply would exceed maxHops.
func Next(cause *Event, kind PrincipalKind, maxHops int) (Causation, error) {
	if cause == nil {
		return Causation{}, nil
	}
	if !kind.Automated() {
		return Causation{CausedBy: cause.ID}, nil
	}
	hops := cause.Causation.HopCount + 1
	if hops > maxHops {
		return Causation{}, ErrHopLimit
	}
	return Causation{CausedBy: cause.ID, HopCount: hops}, nil
}
