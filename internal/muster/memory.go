package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/plainforge"
)

// CatalogPath is where the stand-in keeps the catalog in the hub
// repository.
const CatalogPath = "catalog/tools.json"

// LeaseLength is how long a lease lasts in Muster unless it is configured
// otherwise.
const LeaseLength = 60 * time.Second

// What the stand-in says about where an item stands.
const (
	StatusWaiting   = "waiting"   // a step for Chartroom is in the queue
	StatusLeased    = "leased"    // Chartroom holds the step
	StatusCompleted = "completed" // the step is done
	StatusStopped   = "stopped"   // stopped by E_PARSE until the owner releases it
	StatusPaused    = "paused"    // paused in needs_subtool
)

// rows are Chartroom's rows in Muster's table of queue steps: the state an
// item is held in and the state the step completes it to.
var rows = map[string]string{
	"building": "live",
	"linked":   "linked",
	"merged":   "merged",
	"retired":  "retired",
}

// pausable lists the states Muster's state table lets an item be paused
// from. None of them is a state Chartroom holds an item in.
var pausable = map[string]bool{"recorded": true, "observed": true, "approved": true}

// Seed is an item on record with a step waiting for Chartroom, as Muster
// would hold it: its state, its lane and what earlier workers recorded.
type Seed struct {
	ID      string                     `json:"id"`
	State   string                     `json:"state"`
	Lane    string                     `json:"lane"`
	Found   json.RawMessage            `json:"found,omitempty"`
	Results map[string]json.RawMessage `json:"results,omitempty"`
}

// Outcome is what the stand-in knows about one item.
type Outcome struct {
	ID      string `json:"id"`
	Lane    string `json:"lane"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
	Code    string `json:"code,omitempty"`
}

type finished struct {
	lease, call, arg string
}

type memItem struct {
	seed    Seed
	queued  bool
	stopped bool
	paused  bool
	lease   string
	expires time.Time
	attempt int
	arrival int
	code    string
	done    finished
}

// Memory is an in-memory Muster, as far as Chartroom can see one. It
// offers the steps waiting for Chartroom one at a time, urgent lane first;
// keeps a lease until the step ends or someone else takes it over after it
// ran out; checks a completion against Chartroom's rows; keeps the result;
// and gives the same answer when the call that ended a lease is repeated.
//
// Where the real Muster would keep Chartroom's result for the catalog, the
// stand-in commits it to the hub repository stand-in, so a test or a
// stand-alone run can read the catalog that comes out.
//
// It can also be told to misbehave, to exercise the failure rows of the
// specification.
type Memory struct {
	mu       sync.Mutex
	hub      plainforge.Hub
	now      func() time.Time
	ttl      time.Duration
	items    []*memItem
	arrivals int
	settled  []string
	calls    []string

	down     bool
	stall    int
	dropAcks int
}

// NewMemory returns a stand-in that commits to hub, reads the time from
// now and lets a lease run out after ttl without a heartbeat.
func NewMemory(hub plainforge.Hub, now func() time.Time, ttl time.Duration) *Memory {
	return &Memory{hub: hub, now: now, ttl: ttl}
}

// Seed puts an item on record with a step waiting for Chartroom. The
// stand-in does not check the item: a real Muster never offers Chartroom a
// state it has no row for, and the worker must cope if one ever does.
func (m *Memory) Seed(s Seed) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.arrivals++
	results := make(map[string]json.RawMessage, len(s.Results))
	for k, v := range s.Results {
		results[k] = append(json.RawMessage(nil), v...)
	}
	s.Results = results
	m.items = append(m.items, &memItem{seed: s, queued: true, arrival: m.arrivals})
}

// SetDown makes every call fail as unavailable until it is cleared.
func (m *Memory) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

// StallResults makes the next n result calls fail without being recorded,
// as if the record store behind Muster had been too slow.
func (m *Memory) StallResults(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stall = n
}

// DropAcks makes the next n result calls take effect but report failure,
// as if the acknowledgement had been lost on the way back.
func (m *Memory) DropAcks(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropAcks = n
}

// Calls returns the name of every call received, in order.
func (m *Memory) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.calls...)
}

// Result returns the result kept for a worker on an item, or nil.
func (m *Memory) Result(id, worker string) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		if it.seed.ID == id {
			return append(json.RawMessage(nil), it.seed.Results[worker]...)
		}
	}
	return nil
}

// Outcomes returns every item: those whose step has ended first, in the
// order the steps ended, then the rest in the order they were seeded.
func (m *Memory) Outcomes() []Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Outcome
	seen := make(map[*memItem]bool)
	add := func(it *memItem) {
		if seen[it] {
			return
		}
		seen[it] = true
		status := StatusCompleted
		switch {
		case it.paused:
			status = StatusPaused
		case it.stopped:
			status = StatusStopped
		case it.queued && it.lease != "":
			status = StatusLeased
		case it.queued:
			status = StatusWaiting
		}
		out = append(out, Outcome{
			ID: it.seed.ID, Lane: it.seed.Lane, State: it.seed.State,
			Status: status, Attempt: it.attempt, Code: it.code,
		})
	}
	for _, id := range m.settled {
		for _, it := range m.items {
			if it.seed.ID == id {
				add(it)
			}
		}
	}
	for _, it := range m.items {
		add(it)
	}
	return out
}

// Lease offers the next step: the urgent lane first, then normal, then
// backfill, and within a lane the step that arrived first. A step whose
// lease ran out is offered again.
func (m *Memory) Lease(context.Context) (*Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "lease")
	if m.down {
		return nil, ErrUnavailable
	}
	now := m.now()
	var open []*memItem
	for _, it := range m.items {
		if it.queued && !it.stopped && (it.lease == "" || !now.Before(it.expires)) {
			open = append(open, it)
		}
	}
	if len(open) == 0 {
		return nil, nil
	}
	sort.SliceStable(open, func(i, j int) bool {
		ri, rj := LaneRank(open[i].seed.Lane), LaneRank(open[j].seed.Lane)
		if ri != rj {
			return ri < rj
		}
		return open[i].arrival < open[j].arrival
	})
	it := open[0]
	it.attempt++
	// A lease names the step, its place in the order of arrival and the
	// attempt, as Muster's do. A worker hands it back as it was given.
	it.lease = it.seed.ID + "." + strconv.Itoa(it.arrival) + "." + strconv.Itoa(it.attempt)
	it.expires = now.Add(m.ttl)
	g := &Grant{
		Lease:   it.lease,
		Kind:    KindItem,
		ID:      it.seed.ID,
		State:   it.seed.State,
		Lane:    it.seed.Lane,
		Attempt: it.attempt,
		Expires: it.expires.UTC().Format(time.RFC3339),
		Found:   append(json.RawMessage(nil), it.seed.Found...),
	}
	if len(it.seed.Results) > 0 {
		g.Results = make(map[string]json.RawMessage, len(it.seed.Results))
		for k, v := range it.seed.Results {
			g.Results[k] = append(json.RawMessage(nil), v...)
		}
	}
	return g, nil
}

// held returns the item a lease is held on. A lease that ran out still
// counts until someone else takes the step, so a result that arrives late
// is kept. The caller holds the lock.
func (m *Memory) held(lease string) *memItem {
	if lease == "" {
		return nil
	}
	for _, it := range m.items {
		if it.queued && it.lease == lease {
			return it
		}
	}
	return nil
}

// Heartbeat moves the end of a lease to one lease length from now.
func (m *Memory) Heartbeat(_ context.Context, lease string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "heartbeat")
	if m.down {
		return ErrUnavailable
	}
	it := m.held(lease)
	if it == nil {
		return ErrLeaseNotHeld
	}
	it.expires = m.now().Add(m.ttl)
	return nil
}

// end runs one call that ends a lease, with the stand-in's fault switches
// around it. Repeating the call that ended a lease gives the same answer
// and does nothing twice.
func (m *Memory) end(call, lease, arg string, apply func(it *memItem) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	if m.down {
		return ErrUnavailable
	}
	if m.stall > 0 {
		m.stall--
		return ErrUnavailable
	}
	it := m.held(lease)
	if it == nil {
		for _, done := range m.items {
			if lease != "" && done.done == (finished{lease, call, arg}) {
				return nil
			}
		}
		return ErrLeaseNotHeld
	}
	if err := apply(it); err != nil {
		return err
	}
	it.done = finished{lease, call, arg}
	m.settled = append(m.settled, it.seed.ID)
	if m.dropAcks > 0 {
		m.dropAcks--
		return ErrUnavailable
	}
	return nil
}

// Complete checks the move against Chartroom's rows, records the new state
// and keeps the result. result must be a JSON object or empty.
func (m *Memory) Complete(ctx context.Context, lease, state string, result json.RawMessage) error {
	result = bytes.TrimSpace(result)
	if string(result) == "null" {
		result = nil
	}
	if len(result) > 0 && (result[0] != '{' || !json.Valid(result)) {
		m.note("complete")
		return ErrRefused
	}
	return m.end("complete", lease, state, func(it *memItem) error {
		if to, ok := rows[it.seed.State]; !ok || to != state {
			return ErrMoveNotAllowed
		}
		it.seed.State = state
		it.queued, it.lease = false, ""
		if len(result) > 0 {
			it.seed.Results[Worker] = append(json.RawMessage(nil), result...)
			m.publish(ctx, result)
		}
		return nil
	})
}

// publish commits Chartroom's result to the catalog in the hub repository,
// if it is a catalog entry. The caller holds the lock.
func (m *Memory) publish(ctx context.Context, result json.RawMessage) {
	entry, err := catalog.ParseEntry(result)
	if err != nil {
		return
	}
	cat := catalog.Empty()
	if raw, err := m.hub.ReadFile(ctx, CatalogPath); err == nil {
		if cat, err = catalog.Parse(raw, catalog.MaxBytes); err != nil {
			return
		}
	}
	cat.Upsert(entry)
	_ = m.hub.WriteFile(ctx, entry.Slug, CatalogPath, cat.Encode(), "catalog: "+entry.Status+" "+entry.Slug)
}

func (m *Memory) note(call string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
}

// Fail records E_TIMEOUT or E_PARSE for a step. E_TIMEOUT leaves the step
// in the queue, in its place. E_PARSE stops work on the item until the
// owner releases it. Any other code is refused.
func (m *Memory) Fail(_ context.Context, lease, code string) error {
	if code != "E_TIMEOUT" && code != "E_PARSE" {
		m.note("fail")
		return ErrRefused
	}
	return m.end("fail", lease, code, func(it *memItem) error {
		it.lease, it.code = "", code
		if code == "E_PARSE" {
			it.stopped = true
		}
		return nil
	})
}

// NeedsSubtool pauses an item, where Muster's state table allows it.
func (m *Memory) NeedsSubtool(_ context.Context, lease string) error {
	return m.end("needs_subtool", lease, "", func(it *memItem) error {
		if !pausable[it.seed.State] {
			return ErrMoveNotAllowed
		}
		it.seed.State = "needs_subtool"
		it.queued, it.paused, it.lease, it.code = false, true, "", "E_NEEDS_SUBTOOL"
		return nil
	})
}
