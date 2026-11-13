package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/plainforge"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

// CatalogPath is where the stand-in keeps the catalog in the hub
// repository.
const CatalogPath = "catalog/tools.json"

// Stand-in item statuses.
const (
	StatusPending      = "pending"
	StatusLeased       = "leased"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusNeedsSubtool = "needs_subtool"
	StatusRejected     = "rejected"
)

// Outcome is what the stand-in knows about one item.
type Outcome struct {
	ItemID   string `json:"item_id"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
	State    string `json:"state,omitempty"`
	Code     string `json:"code,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Field    string `json:"field,omitempty"`
}

type memItem struct {
	id       string
	priority string
	payload  []byte
	status   string
	state    string
	leaseID  string
	expires  time.Time
	report   Report
	result   []byte
}

// Memory is an in-memory Muster. It hands out leases in priority order,
// expires leases that are not kept alive, commits completed changes to the
// hub repository and remembers results so a repeated call is acknowledged
// again. It can also be told to misbehave, to exercise the failure rows of
// the specification.
type Memory struct {
	mu      sync.Mutex
	hub     plainforge.Hub
	now     func() time.Time
	ttl     time.Duration
	items   []*memItem
	byLease map[string]*memItem
	leases  int
	settled []string
	calls   []string

	down     bool
	stall    int
	dropAcks int
}

// NewMemory returns a stand-in that commits to hub, reads the time from
// now and lets a lease lapse after ttl without a heartbeat.
func NewMemory(hub plainforge.Hub, now func() time.Time, ttl time.Duration) *Memory {
	return &Memory{hub: hub, now: now, ttl: ttl, byLease: make(map[string]*memItem)}
}

// Seed adds an item waiting to be leased. payload is the item exactly as
// it will be handed out; it does not have to be valid.
func (m *Memory) Seed(id, priority string, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, &memItem{id: id, priority: priority, payload: append([]byte{}, payload...), status: StatusPending})
}

// SetDown makes every call fail as unavailable until it is cleared.
func (m *Memory) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

// StallResults makes the next n result calls fail without being recorded,
// as if the catalog sync behind Muster had timed out.
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

// Outcomes returns every item: settled ones first, in the order they were
// settled, then the rest in the order they were seeded.
func (m *Memory) Outcomes() []Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	var out []Outcome
	done := make(map[string]bool)
	add := func(it *memItem) {
		out = append(out, Outcome{
			ItemID: it.id, Priority: it.priority, Status: it.status, State: it.state,
			Code: it.report.Code, Reason: it.report.Reason, Field: it.report.Field,
		})
	}
	for _, id := range m.settled {
		for _, it := range m.items {
			if it.id == id && !done[id] {
				done[id] = true
				add(it)
			}
		}
	}
	for _, it := range m.items {
		if !done[it.id] {
			add(it)
		}
	}
	return out
}

// expire returns lapsed leases to the queue. The caller holds the lock.
func (m *Memory) expire() {
	now := m.now()
	for _, it := range m.items {
		if it.status == StatusLeased && !now.Before(it.expires) {
			it.status = StatusPending
			it.leaseID = ""
		}
	}
}

// Lease hands out up to max pending items, urgent first, then normal, then
// backfill, each group in the order it was seeded.
func (m *Memory) Lease(_ context.Context, max int) ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "lease")
	if m.down {
		return nil, ErrUnavailable
	}
	m.expire()
	var pending []*memItem
	for _, it := range m.items {
		if it.status == StatusPending {
			pending = append(pending, it)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return PriorityRank(pending[i].priority) < PriorityRank(pending[j].priority)
	})
	if len(pending) > max {
		pending = pending[:max]
	}
	out := make([]Lease, 0, len(pending))
	for _, it := range pending {
		m.leases++
		it.status = StatusLeased
		it.leaseID = fmt.Sprintf("lease-%04d", m.leases)
		it.expires = m.now().Add(m.ttl)
		m.byLease[it.leaseID] = it
		out = append(out, Lease{
			LeaseID:  it.leaseID,
			Priority: it.priority,
			Item:     append([]byte{}, it.payload...),
			Catalog:  m.context(it.payload),
		})
	}
	return out, nil
}

// context returns the part of the hub catalog an item can touch. If the
// hub catalog cannot be read as a catalog it is passed on as it is, so the
// worker reports it.
func (m *Memory) context(payload []byte) []byte {
	raw, err := m.hub.ReadFile(context.Background(), CatalogPath)
	if err != nil {
		return catalog.Empty().Encode()
	}
	cat, err := catalog.Parse(raw, catalog.MaxBytes)
	if err != nil {
		return raw
	}
	var probe struct {
		Slug  string `json:"slug"`
		Merge *struct {
			Parent string `json:"parent"`
		} `json:"merge"`
	}
	_ = json.Unmarshal(payload, &probe)
	parent := ""
	if probe.Merge != nil {
		parent = probe.Merge.Parent
	}
	return cat.Subset(probe.Slug, parent).Encode()
}

// held returns the item a live lease belongs to. The caller holds the lock.
func (m *Memory) held(leaseID string) *memItem {
	m.expire()
	it := m.byLease[leaseID]
	if it == nil || it.status != StatusLeased || it.leaseID != leaseID {
		return nil
	}
	return it
}

// Heartbeat pushes a lease's expiry out by one ttl.
func (m *Memory) Heartbeat(_ context.Context, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "heartbeat")
	if m.down {
		return ErrUnavailable
	}
	it := m.held(leaseID)
	if it == nil {
		return ErrLeaseGone
	}
	it.expires = m.now().Add(m.ttl)
	return nil
}

// settle runs one result call with the stand-in's fault switches around it.
func (m *Memory) settle(name, leaseID string, payload []byte, apply func(it *memItem) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// White space is not part of a result: compare results compacted.
	var compact bytes.Buffer
	if json.Compact(&compact, payload) == nil {
		payload = compact.Bytes()
	}
	m.calls = append(m.calls, name)
	if m.down {
		return ErrUnavailable
	}
	if m.stall > 0 {
		m.stall--
		return ErrUnavailable
	}
	if prev := m.byLease[leaseID]; prev != nil && prev.leaseID == leaseID && prev.result != nil {
		// A result for this lease is already recorded. The same result
		// again is acknowledged again; a different one is refused.
		if prev.status != StatusRejected && bytes.Equal(prev.result, payload) {
			return nil
		}
		return ErrLeaseGone
	}
	it := m.held(leaseID)
	if it == nil {
		return ErrLeaseGone
	}
	it.result = append([]byte{}, payload...)
	m.settled = append(m.settled, it.id)
	if !apply(it) {
		it.status = StatusRejected
		return ErrLeaseGone
	}
	if m.dropAcks > 0 {
		m.dropAcks--
		return ErrUnavailable
	}
	return nil
}

// Complete checks the change against the specification's state table and
// against the hub catalog, commits it and records the new state.
func (m *Memory) Complete(ctx context.Context, leaseID string, change json.RawMessage) error {
	return m.settle("complete", leaseID, change, func(it *memItem) bool {
		var ch decision.Change
		if err := strictjson.Decode(change, catalog.MaxBytes, &ch); err != nil {
			return false
		}
		tr, ok := decision.Transitions[ch.Action]
		if !ok || ch.ContractVersion != catalog.ContractVersion || ch.From != tr.From || ch.To != tr.To ||
			ch.After == nil || ch.After.Slug != ch.Slug || ch.After.Status != ch.To {
			return false
		}
		cat := catalog.Empty()
		if raw, err := m.hub.ReadFile(ctx, CatalogPath); err == nil {
			if cat, err = catalog.Parse(raw, catalog.MaxBytes); err != nil {
				return false
			}
		}
		current := cat.Find(ch.Slug)
		if ch.Changed {
			if !catalog.Equal(current, ch.Before) {
				return false
			}
			cat.Upsert(ch.After)
			message := "catalog: " + ch.Action + " " + ch.Slug
			if err := m.hub.WriteFile(ctx, CatalogPath, cat.Encode(), message); err != nil {
				return false
			}
		} else if !catalog.Equal(current, ch.After) {
			return false
		}
		it.status = StatusCompleted
		it.state = ch.To
		return true
	})
}

// Fail records that an item's data was broken.
func (m *Memory) Fail(_ context.Context, leaseID string, r Report) error {
	return m.report("fail", StatusFailed, leaseID, r)
}

// NeedsSubtool pauses an item in needs_subtool.
func (m *Memory) NeedsSubtool(_ context.Context, leaseID string, r Report) error {
	return m.report("needs_subtool", StatusNeedsSubtool, leaseID, r)
}

func (m *Memory) report(name, status, leaseID string, r Report) error {
	return m.settle(name, leaseID, strictjson.Marshal(r), func(it *memItem) bool {
		it.status = status
		it.report = r
		if status == StatusNeedsSubtool {
			it.state = StatusNeedsSubtool
		}
		return true
	})
}
