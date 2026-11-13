// Package muster is the small interface to Muster, the only service
// Chartroom talks to. It holds the interface, an HTTP client for it and an
// in-memory stand-in that follows SPEC.md.
package muster

import (
	"context"
	"encoding/json"
	"errors"
)

// Worker is the name Chartroom gives when it asks for work.
const Worker = "chartroom"

// The three priorities, in the order they are processed.
const (
	PriorityUrgent   = "urgent"
	PriorityNormal   = "normal"
	PriorityBackfill = "backfill"
)

// PriorityRank orders priorities: urgent, then normal, then backfill. An
// unknown priority sorts last.
func PriorityRank(p string) int {
	switch p {
	case PriorityUrgent:
		return 0
	case PriorityNormal:
		return 1
	case PriorityBackfill:
		return 2
	}
	return 3
}

var (
	// ErrUnavailable means Muster did not acknowledge the call: it timed
	// out, could not be reached or answered with an error. The call can be
	// tried again.
	ErrUnavailable = errors.New("muster did not acknowledge the call")
	// ErrLeaseGone means Muster no longer holds this lease. The result for
	// it can be dropped; Muster will lease the item again if it needs to.
	ErrLeaseGone = errors.New("muster no longer holds this lease")
	// ErrBadResponse means Muster answered with data that could not be read.
	ErrBadResponse = errors.New("muster answered with unreadable data")
)

// Lease is one leased item. Item is the decision to publish. Catalog is
// the part of the catalog the decision can touch: at least the entry with
// the same slug and, for a merge, the parent entry.
type Lease struct {
	LeaseID  string          `json:"lease_id"`
	Priority string          `json:"priority"`
	Item     json.RawMessage `json:"item"`
	Catalog  json.RawMessage `json:"catalog"`
}

// Report is what Chartroom tells Muster when an item fails or is paused.
// Every field is a constant from package cserr.
type Report struct {
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Client is the five calls the specification lists.
type Client interface {
	// Lease asks for up to max items, highest priority first.
	Lease(ctx context.Context, max int) ([]Lease, error)
	// Heartbeat keeps a lease alive.
	Heartbeat(ctx context.Context, leaseID string) error
	// Complete hands over the catalog change for a lease.
	Complete(ctx context.Context, leaseID string, change json.RawMessage) error
	// Fail reports that the item's data was broken.
	Fail(ctx context.Context, leaseID string, r Report) error
	// NeedsSubtool pauses an item no rule covers.
	NeedsSubtool(ctx context.Context, leaseID string, r Report) error
}
