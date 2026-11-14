// Package worker is Chartroom's loop: lease a step from Muster, work out
// the catalog entry, stage the result on disk and hand it to Muster,
// trying again with backoff until Muster acknowledges.
//
// Muster hands out one step per lease call and decides the order: urgent
// lane first, then normal, then backfill. The loop takes the steps as they
// come. It runs in one goroutine and reads the time from a Clock, so the
// same sequence of answers from Muster always gives the same sequence of
// calls.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/spool"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

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

// Config sets the worker's pace. Neither specification gives these
// numbers, so they are settings with fixed defaults.
type Config struct {
	// PollInterval is the wait after Muster had nothing to lease.
	PollInterval time.Duration
	// HeartbeatEvery is how often a lease with a staged result is kept
	// while Muster has not acknowledged the result. It has to be well
	// under Muster's lease length, which is 60 seconds by default.
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
// new step is leased.
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

// step does one round: send what is staged, then lease one step and stage
// its result. progressed means there is more to do straight away; idle
// means Muster had nothing to lease and nothing is staged.
func (w *Worker) step(ctx context.Context) (progressed, idle bool) {
	now := w.clock.Now()
	w.beat(ctx, now)
	if now.Before(w.notBefore) {
		return false, false
	}
	if !w.flush(ctx) {
		return false, false
	}
	grant, err := w.lease(ctx)
	if err != nil {
		w.unacknowledged(err)
		return false, false
	}
	if grant == nil {
		w.failures = 0
		return false, true
	}
	if !w.stage(*grant) {
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

func (w *Worker) lease(ctx context.Context) (*muster.Grant, error) {
	ctx, cancel := w.call(ctx)
	defer cancel()
	return w.muster.Lease(ctx)
}

// reasonFor names, in Chartroom's own fixed words, why a call to Muster
// did not go through. Nothing Muster sent is repeated.
func reasonFor(err error) cserr.Reason {
	switch {
	case errors.Is(err, muster.ErrBadResponse):
		return cserr.MalformedJSON
	case errors.Is(err, muster.ErrUnknownContract):
		return cserr.SchemaVersionMismatch
	case errors.Is(err, muster.ErrRefused), errors.Is(err, muster.ErrMoveNotAllowed), errors.Is(err, muster.ErrLeaseNotHeld):
		return cserr.UpstreamRefused
	}
	return cserr.UpstreamTimeout
}

// unacknowledged records a call Muster did not acknowledge and sets the
// time before which Muster is not asked again.
func (w *Worker) unacknowledged(err error) {
	w.failures++
	delay := Backoff(w.cfg.BackoffBase, w.cfg.BackoffCap, w.failures)
	w.notBefore = w.clock.Now().Add(delay)
	reason := reasonFor(err)
	w.log.Warn("retry", "code", string(reason.Code), "reason", reason.Name,
		"attempt", w.failures, "delay_ms", delay.Milliseconds(), "staged", w.spool.Len())
}

// flush sends every staged result, most urgent lane first. A result Muster
// acknowledges or refuses for good leaves the spool. It stops at the first
// one Muster does not answer and reports false.
func (w *Worker) flush(ctx context.Context) bool {
	for _, rec := range w.spool.Pending() {
		err := w.send(ctx, rec)
		switch {
		case err == nil:
			w.failures = 0
			w.drop(rec, "acknowledged", nil)
		case errors.Is(err, muster.ErrLeaseNotHeld):
			// The lease ended or was taken over. Muster offers the step
			// again if it still needs doing.
			w.failures = 0
			w.drop(rec, "lease_not_held", nil)
		case errors.Is(err, muster.ErrMoveNotAllowed):
			w.failures = 0
			w.drop(rec, "move_not_allowed", err)
		case errors.Is(err, muster.ErrRefused), errors.Is(err, muster.ErrUnknownContract):
			// Sending the same result again cannot help.
			w.failures = 0
			w.drop(rec, "refused", err)
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
		return w.muster.Complete(ctx, rec.Lease, rec.State, rec.Result)
	case spool.OutcomeFail:
		return w.muster.Fail(ctx, rec.Lease, rec.Code)
	default:
		return w.muster.NeedsSubtool(ctx, rec.Lease)
	}
}

// drop takes a result out of the spool and logs why. refusal is the error
// Muster's refusal was read as, when the result was refused.
func (w *Worker) drop(rec spool.Record, event string, refusal error) {
	if err := w.spool.Remove(rec.Lease); err != nil {
		w.log.Error("spool_remove_failed", "id", rec.ID)
		return
	}
	if refusal == nil {
		w.log.Info(event, "id", rec.ID, "outcome", rec.Outcome)
		return
	}
	reason := reasonFor(refusal)
	w.log.Warn(event, "id", rec.ID, "outcome", rec.Outcome, "code", string(reason.Code), "reason", reason.Name)
}

// beat keeps the leases whose results are staged and not yet acknowledged.
// A lease Muster no longer holds has its result dropped.
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
		err := w.muster.Heartbeat(cctx, rec.Lease)
		cancel()
		if errors.Is(err, muster.ErrLeaseNotHeld) {
			w.drop(rec, "lease_not_held", nil)
		}
	}
}

// stage works out the result for one leased step and puts it in the spool.
// It reports whether the result was staged.
func (w *Worker) stage(g muster.Grant) bool {
	if g.Lease == "" {
		// Without a lease there is nothing to report against.
		w.log.Warn("lease_unusable", "code", string(cserr.Parse), "reason", cserr.InvalidMetadata.Name)
		return false
	}
	rec := Evaluate(g)
	if err := w.spool.Put(rec); err != nil {
		// Not staged means not sent. The lease runs out and Muster
		// offers the step again.
		w.log.Error("spool_write_failed", "id", rec.ID)
		return false
	}
	attrs := []any{"id", rec.ID, "outcome", rec.Outcome}
	if rec.Code != "" {
		attrs = append(attrs, "code", rec.Code, "reason", rec.Reason)
		if rec.Field != "" {
			attrs = append(attrs, "field", rec.Field)
		}
	}
	w.log.Info("staged", attrs...)
	return true
}

// Evaluate turns one leased step into the record to stage: the state and
// the catalog entry to complete it with, or the fixed code and reason why
// there is none. It is a pure function.
func Evaluate(g muster.Grant) spool.Record {
	rec := spool.Record{
		Contract: catalog.ContractVersion,
		Lease:    g.Lease,
		Lane:     g.Lane,
	}
	if decision.ValidItemID(g.ID) {
		// Only an identifier of the contract's form is kept or logged.
		rec.ID = g.ID
	}
	change, err := evaluate(g)
	if err == nil {
		rec.Outcome = spool.OutcomeComplete
		rec.State = change.To
		rec.Result = strictjson.Marshal(change.After)
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
	rec.Code, rec.Reason, rec.Field = string(e.Reason.Code), e.Reason.Name, e.Field
	return rec
}

func evaluate(g muster.Grant) (*decision.Change, error) {
	switch g.Kind {
	case muster.KindItem:
	case "":
		return nil, cserr.At(cserr.InvalidMetadata, "kind")
	default:
		// A proposal or an incident: valid, and not Chartroom's to do.
		return nil, cserr.At(cserr.UnsupportedEntryType, "kind")
	}
	it, own, err := decision.FromResults(g.ID, g.State, g.Results)
	if err != nil {
		return nil, err
	}
	switch g.Lane {
	case muster.LaneUrgent, muster.LaneNormal, muster.LaneBackfill:
	case "":
		return nil, cserr.At(cserr.InvalidMetadata, "lane")
	default:
		return nil, cserr.At(cserr.UnsupportedValue, "lane")
	}
	return decision.ApplyOwn(own, it)
}
