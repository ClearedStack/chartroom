// Package muster is the small interface to Muster, the only service
// Chartroom talks to. It holds the interface, an HTTP client for it and an
// in-memory stand-in.
//
// All three follow Muster's contract v1 (contract/v1/CONTRACT.md in the
// Muster repository). The saved examples from that contract are replayed
// by this package's tests.
package muster

import (
	"context"
	"encoding/json"
	"errors"
)

// Worker is Chartroom's name in the naming registry. Muster learns it from
// the ServiceAccount token, so it is never sent in a request.
const Worker = "chartroom"

// KindItem is the kind of step Chartroom works on.
const KindItem = "item"

// The three lanes, in the order Muster empties them.
const (
	LaneUrgent   = "urgent"
	LaneNormal   = "normal"
	LaneBackfill = "backfill"
)

// LaneRank orders lanes: urgent, then normal, then backfill. An unknown
// lane sorts last.
func LaneRank(lane string) int {
	switch lane {
	case LaneUrgent:
		return 0
	case LaneNormal:
		return 1
	case LaneBackfill:
		return 2
	}
	return 3
}

// The errors a call can end in. They are fixed: none carries an address, a
// token or any text that came over the network.
var (
	// ErrUnavailable means Muster did not acknowledge the call: it timed
	// out, could not be reached, was busy or failed. Try again later.
	ErrUnavailable = errors.New("muster did not acknowledge the call")
	// ErrLeaseNotHeld means Muster refused the call because the lease is
	// unknown, has ended or was taken over. The result can be dropped;
	// Muster offers the step again if it still needs doing.
	ErrLeaseNotHeld = errors.New("muster does not hold this lease")
	// ErrMoveNotAllowed means Muster's tables do not allow what the call
	// asked for. Sending it again cannot help.
	ErrMoveNotAllowed = errors.New("muster does not allow this move")
	// ErrUnknownContract means Muster does not speak this contract version.
	ErrUnknownContract = errors.New("muster does not know this contract version")
	// ErrRefused means Muster refused the call for any other reason that
	// sending the same call again cannot cure.
	ErrRefused = errors.New("muster refused the call")
	// ErrBadResponse means Muster answered with data that could not be read.
	ErrBadResponse = errors.New("muster answered with unreadable data")
)

// Grant is the answer to a lease call that found a step: what the step is
// about, the state it is in and what earlier workers recorded. Results
// holds the latest result of each earlier worker, by worker name.
type Grant struct {
	Lease    string                     `json:"lease"`
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	State    string                     `json:"state"`
	Lane     string                     `json:"lane"`
	Attempt  int                        `json:"attempt"`
	Expires  string                     `json:"expires"`
	Found    json.RawMessage            `json:"found,omitempty"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal json.RawMessage            `json:"proposal,omitempty"`
	Incident json.RawMessage            `json:"incident,omitempty"`
}

// Client is the five worker calls of Muster's contract.
type Client interface {
	// Lease asks for the next step. It returns nil when there is none.
	Lease(ctx context.Context) (*Grant, error)
	// Heartbeat keeps a lease.
	Heartbeat(ctx context.Context, lease string) error
	// Complete reports a finished step: the state the item is now in and,
	// optionally, a result, which must be a JSON object.
	Complete(ctx context.Context, lease, state string, result json.RawMessage) error
	// Fail reports E_TIMEOUT or E_PARSE for a step.
	Fail(ctx context.Context, lease, code string) error
	// NeedsSubtool pauses an item no rule covers.
	NeedsSubtool(ctx context.Context, lease string) error
}
