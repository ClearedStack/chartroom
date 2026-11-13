// Package worker is Chartroom's loop: lease items from Muster, work out
// each catalog change, stage the result on disk and hand it to Muster,
// trying again with backoff until Muster acknowledges.
//
// The loop runs in one goroutine and reads the time from a Clock, so the
// same sequence of answers from Muster always gives the same sequence of
// calls.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/spool"
)

// MaxContextBytes is the largest catalog context accepted with one lease.
const MaxContextBytes = 256 << 10

// Clock is the worker's only source of time.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx is done.
	Sleep(ctx context.Context, d time.Duration)
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep waits for d or until ctx is done.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Config sets the worker's pace. The specification gives none of these
// numbers, so they are settings with fixed defaults.
type Config struct {
	// Batch is the most items leased in one call.
	Batch int
	// PollInterval is the wait after Muster had nothing to lease.
	PollInterval time.Duration
	// HeartbeatEvery is how often a lease with a staged result is kept
	// alive while Muster has not acknowledged the result.
	HeartbeatEvery time.Duration
	// CallTimeout bounds one call to Muster.
	CallTimeout time.Duration
	// BackoffBase is the wait after the first unacknowledged call. It
	// doubles after each further one, up to BackoffCap. There is no random
	// part.
	BackoffBase time.Duration
	BackoffCap  time.Duration
	// Once makes Run return when Muster has nothing left to lease and the
	// spool is empty.
	Once bool
}

// DefaultConfig returns the default pace.
func DefaultConfig() Config {
	return Config{
		Batch:          4,
		PollInterval:   5 * time.Second,
		HeartbeatEvery: 20 * time.Second,
		CallTimeout:    10 * time.Second,
		BackoffBase:    time.Second,
		BackoffCap:     time.Minute,
	}
}

// Backoff returns the wait after the given number of unacknowledged calls
// in a row: base, 2×base, 4×base and so on, never more than limit.
func Backoff(base, limit time.Duration, failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	if d > limit {
		return limit
	}
	return d
}

// Worker is one Chartroom worker.
type Worker struct {
	cfg    Config
	muster muster.Client
	spool  *spool.Spool
	clock  Clock
	log    *slog.Logger

	failures  int
	notBefore time.Time
	nextBeat  time.Time
}

// New returns a worker. Results already in the spool are sent before any
// new item is leased.
func New(cfg Config, m muster.Client, s *spool.Spool, clock Clock, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, muster: m, spool: s, clock: clock, log: log}
}

// Run works until ctx is done or, with Config.Once, until there is nothing
// left to do. Results not yet acknowledged stay in the spool.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		progressed, idle := w.step(ctx)
		if idle && w.cfg.Once {
			return
		}
		if !progressed {
			w.clock.Sleep(ctx, w.pause())
		}
	}
}

// step does one round. progressed means there is more to do straight away;
// idle means Muster had nothing to lease and nothing is staged.
func (w *Worker) step(ctx context.Context) (progressed, idle bool) {
	now := w.clock.Now()
	w.beat(ctx, now)
	if now.Before(w.notBefore) {
		return false, false
	}
	if !w.flush(ctx) {
		return false, false
	}
	leases, err := w.lease(ctx)
	if err != nil {
		w.unacknowledged(err)
		return false, false
	}
	if len(leases) == 0 {
		w.failures = 0
		return false, true
	}
	sort.SliceStable(leases, func(i, j int) bool {
		return muster.PriorityRank(leases[i].Priority) < muster.PriorityRank(leases[j].Priority)
	})
	staged := 0
	for _, l := range leases {
		if w.stage(l) {
			staged++
		}
	}
	if staged == 0 {
		// Muster answered, but with nothing that could be staged. Wait
		// before asking again, so a bad answer cannot spin the loop.
		w.unacknowledged(muster.ErrBadResponse)
		return false, false
	}
	w.failures = 0
	return true, false
}

// pause is how long to wait before the next round.
func (w *Worker) pause() time.Duration {
	now := w.clock.Now()
	d := w.cfg.PollInterval
	if w.notBefore.After(now) {
		d = w.notBefore.Sub(now)
	}
	if !w.nextBeat.IsZero() && w.spool.Len() > 0 {
		if until := w.nextBeat.Sub(now); until < d {
			d = until
		}
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

func (w *Worker) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, w.cfg.CallTimeout)
}

func (w *Worker) lease(ctx context.Context) ([]muster.Lease, error) {
	ctx, cancel := w.call(ctx)
	defer cancel()
	return w.muster.Lease(ctx, w.cfg.Batch)
}

// unacknowledged records a call Muster did not acknowledge and sets the
// time before which Muster is not asked again.
func (w *Worker) unacknowledged(err error) {
	w.failures++
	delay := Backoff(w.cfg.BackoffBase, w.cfg.BackoffCap, w.failures)
	w.notBefore = w.clock.Now().Add(delay)
	reason := cserr.UpstreamTimeout
	if errors.Is(err, muster.ErrBadResponse) {
		reason = cserr.MalformedJSON
	}
	w.log.Warn("retry", "code", string(reason.Code), "reason", reason.Name,
		"attempt", w.failures, "delay_ms", delay.Milliseconds(), "staged", w.spool.Len())
}

// flush sends every staged result, most urgent first. It stops at the
// first one Muster does not acknowledge and reports false.
func (w *Worker) flush(ctx context.Context) bool {
	for _, rec := range w.spool.Pending() {
		err := w.send(ctx, rec)
		switch {
		case err == nil:
			w.failures = 0
			w.drop(rec, "acknowledged")
		case errors.Is(err, muster.ErrLeaseGone):
			w.failures = 0
			w.drop(rec, "lease_gone")
		default:
			w.unacknowledged(err)
			return false
		}
	}
	return true
}

func (w *Worker) send(ctx context.Context, rec spool.Record) error {
	ctx, cancel := w.call(ctx)
	defer cancel()
	switch rec.Outcome {
	case spool.OutcomeComplete:
		return w.muster.Complete(ctx, rec.LeaseID, rec.Change)
	case spool.OutcomeFail:
		return w.muster.Fail(ctx, rec.LeaseID, *rec.Report)
	default:
		return w.muster.NeedsSubtool(ctx, rec.LeaseID, *rec.Report)
	}
}

func (w *Worker) drop(rec spool.Record, event string) {
	if err := w.spool.Remove(rec.LeaseID); err != nil {
		w.log.Error("spool_remove_failed", "item_id", rec.ItemID)
		return
	}
	w.log.Info(event, "item_id", rec.ItemID, "outcome", rec.Outcome)
}

// beat keeps alive the leases whose results are staged and not yet
// acknowledged. A lease Muster no longer holds has its result dropped.
func (w *Worker) beat(ctx context.Context, now time.Time) {
	if w.spool.Len() == 0 {
		w.nextBeat = time.Time{}
		return
	}
	if w.nextBeat.IsZero() {
		w.nextBeat = now.Add(w.cfg.HeartbeatEvery)
		return
	}
	if now.Before(w.nextBeat) {
		return
	}
	w.nextBeat = now.Add(w.cfg.HeartbeatEvery)
	for _, rec := range w.spool.Pending() {
		cctx, cancel := w.call(ctx)
		err := w.muster.Heartbeat(cctx, rec.LeaseID)
		cancel()
		if errors.Is(err, muster.ErrLeaseGone) {
			w.drop(rec, "lease_gone")
		}
	}
}

// stage works out the result for one lease and puts it in the spool. It
// reports whether the result was staged.
func (w *Worker) stage(l muster.Lease) bool {
	if l.LeaseID == "" {
		// Without a lease there is nothing to report against.
		w.log.Warn("lease_unusable", "code", string(cserr.Parse), "reason", cserr.InvalidMetadata.Name)
		return false
	}
	rec := Evaluate(l)
	if err := w.spool.Put(rec); err != nil {
		// Not staged means not sent. The lease lapses and Muster hands
		// the item out again.
		w.log.Error("spool_write_failed", "item_id", rec.ItemID)
		return false
	}
	attrs := []any{"item_id", rec.ItemID, "outcome", rec.Outcome}
	if rec.Report != nil {
		attrs = append(attrs, "code", rec.Report.Code, "reason", rec.Report.Reason)
	}
	w.log.Info("staged", attrs...)
	return true
}

// Evaluate turns one lease into the record to stage: the catalog change,
// or the fixed report of why there is none. It is a pure function.
func Evaluate(l muster.Lease) spool.Record {
	rec := spool.Record{
		ContractVersion: catalog.ContractVersion,
		LeaseID:         l.LeaseID,
		Priority:        l.Priority,
	}
	change, itemID, err := evaluate(l)
	rec.ItemID = itemID
	if err == nil {
		rec.Outcome = spool.OutcomeComplete
		rec.Change = change.Encode()
		return rec
	}
	e, ok := cserr.From(err)
	if !ok {
		e = cserr.New(cserr.MalformedJSON)
	}
	rec.Outcome = spool.OutcomeFail
	if e.Reason.Code == cserr.NeedsSubtool {
		rec.Outcome = spool.OutcomeNeedsSubtool
	}
	rec.Report = &muster.Report{
		Code:    string(e.Reason.Code),
		Reason:  e.Reason.Name,
		Field:   e.Field,
		Message: e.Reason.Message,
	}
	return rec
}

// itemID reads the identifier out of an item that may be refused, so the
// record and the log can still say which item it was. It is used only when
// it has the shape of an identifier; anything else is left out.
func itemID(item []byte) string {
	var probe struct {
		ItemID string `json:"item_id"`
	}
	if json.Unmarshal(item, &probe) != nil || !catalog.ValidSlug(probe.ItemID) {
		return ""
	}
	return probe.ItemID
}

func evaluate(l muster.Lease) (*decision.Change, string, error) {
	it, err := decision.Parse(l.Item)
	if err != nil {
		return nil, itemID(l.Item), err
	}
	switch l.Priority {
	case muster.PriorityUrgent, muster.PriorityNormal, muster.PriorityBackfill:
	case "":
		return nil, it.ItemID, cserr.At(cserr.InvalidMetadata, "priority")
	default:
		return nil, it.ItemID, cserr.At(cserr.UnsupportedValue, "priority")
	}
	cat, err := catalog.Parse(l.Catalog, MaxContextBytes)
	if err != nil {
		return nil, it.ItemID, err
	}
	ch, err := decision.Apply(cat, it)
	return ch, it.ItemID, err
}
