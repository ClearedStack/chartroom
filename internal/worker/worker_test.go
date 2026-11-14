package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/fixture"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/plainforge"
	"github.com/ClearedStack/chartroom/internal/spool"
	"github.com/ClearedStack/chartroom/internal/worker"
)

// fakeClock is the worker's clock in tests. Sleeping moves the time
// forward at once and is recorded.
type fakeClock struct {
	t       time.Time
	sleeps  []time.Duration
	onSleep func(n int)
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.sleeps))
	}
}

func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.sleeps {
		sum += d
	}
	return sum
}

// rig is one worker wired to the in-memory stand-ins.
type rig struct {
	t      *testing.T
	clock  *fakeClock
	hub    *plainforge.Memory
	muster *muster.Memory
	dir    string
	log    *bytes.Buffer
	cfg    worker.Config
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: &fakeClock{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}, hub: plainforge.NewMemory(), dir: t.TempDir(), log: &bytes.Buffer{}}
	r.muster = muster.NewMemory(r.hub, r.clock.Now, muster.LeaseLength)
	r.cfg = worker.DefaultConfig()
	r.cfg.Once = true
	return r
}

func grantOf(t *testing.T, name string) muster.Grant {
	t.Helper()
	var g muster.Grant
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(fixture.Root, "lease", name, "lease.json")), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func recordOf(t *testing.T, name string) spool.Record {
	t.Helper()
	var rec spool.Record
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(fixture.Root, "lease", name, "want.record.json")), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// fixtureRig seeds the stand-in with the item a Muster would hold to give
// the saved lease.
func fixtureRig(t *testing.T, name string) *rig {
	t.Helper()
	r := newRig(t)
	g := grantOf(t, name)
	r.muster.Seed(muster.Seed{ID: g.ID, State: g.State, Lane: g.Lane, Found: g.Found, Results: g.Results})
	return r
}

// logger writes one JSON line per event, without the time, so a run's log
// is the same every time.
func (r *rig) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(r.log, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// run opens the spool and runs a worker against client until it has
// nothing left to do or ctx is done.
func (r *rig) run(ctx context.Context, client muster.Client) {
	r.t.Helper()
	sp, discarded, err := spool.Open(r.dir)
	if err != nil || discarded != 0 {
		r.t.Fatalf("opening the spool: %v, %d discarded", err, discarded)
	}
	worker.New(r.cfg, client, sp, r.clock, r.logger()).Run(ctx)
}

func (r *rig) staged() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (r *rig) count(call string) int {
	n := 0
	for _, c := range r.muster.Calls() {
		if c == call {
			n++
		}
	}
	return n
}

func compact(raw []byte) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// checkOutcome compares what the stand-ins saw with the record saved for
// the fixture.
func checkOutcome(t *testing.T, r *rig, name string) {
	t.Helper()
	g, want := grantOf(t, name), recordOf(t, name)
	out := r.muster.Outcomes()
	if len(out) != 1 {
		t.Fatalf("%d outcomes", len(out))
	}
	got := out[0]
	if len(r.staged()) != 0 {
		t.Errorf("the spool still holds %v", r.staged())
	}
	switch want.Outcome {
	case spool.OutcomeComplete:
		if got.Status != muster.StatusCompleted || got.State != want.State {
			t.Errorf("outcome = %+v, want completed in state %s", got, want.State)
		}
		if kept := r.muster.Result(g.ID, muster.Worker); compact(kept) != compact(want.Result) {
			t.Errorf("result kept by Muster\n  %s\nwant\n  %s", compact(kept), compact(want.Result))
		}
		// The stand-in passes the result on to the hub repository.
		entry, err := catalog.ParseEntry(want.Result)
		if err != nil {
			t.Fatalf("the saved result is not a catalog entry: %v", err)
		}
		raw, _ := r.hub.ReadFile(context.Background(), muster.CatalogPath)
		cat, err := catalog.Parse(raw, catalog.MaxBytes)
		if err != nil || !catalog.Equal(cat.Find(entry.Slug), entry) {
			t.Errorf("the hub catalog does not hold the entry: %v\n%s", err, raw)
		}
		if n := len(r.hub.Commits()); n != 1 {
			t.Errorf("%d commits to the hub repository, want one", n)
		}
	case spool.OutcomeFail:
		if got.Status != muster.StatusStopped || got.State != g.State || got.Code != want.Code {
			t.Errorf("outcome = %+v, want stopped in state %q with %s", got, g.State, want.Code)
		}
		if len(r.hub.Commits()) != 0 {
			t.Error("something was published for an item that failed")
		}
	case spool.OutcomeNeedsSubtool:
		// Chartroom holds no item in a state Muster lets it pause from,
		// so the pause is refused and the item stays as it was.
		if got.Status != muster.StatusLeased || got.State != g.State {
			t.Errorf("outcome = %+v, want the item left in state %q", got, g.State)
		}
		if r.count("needs_subtool") == 0 || !strings.Contains(r.log.String(), `"msg":"move_not_allowed"`) {
			t.Errorf("needs_subtool was not called and refused:\n%s", r.log)
		}
		if len(r.hub.Commits()) != 0 {
			t.Error("something was published for an item with no rule")
		}
	}
}

// TestLeaseFixtures checks, for each saved lease, the exact record the
// worker stages for it.
func TestLeaseFixtures(t *testing.T) {
	for _, dir := range fixture.Dirs(t, "lease") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			g := grantOf(t, name)
			rec := worker.Evaluate(g)
			wantOutcome := strings.SplitN(name, "-", 2)[0]
			if wantOutcome == "needs" {
				wantOutcome = spool.OutcomeNeedsSubtool
			}
			if rec.Outcome != wantOutcome {
				t.Errorf("outcome = %s (%s %s %s), but the fixture name says %s", rec.Outcome, rec.Code, rec.Reason, rec.Field, wantOutcome)
			}
			fixture.Golden(t, filepath.Join(dir, "want.record.json"), rec.Encode())
			if again := worker.Evaluate(g); !bytes.Equal(again.Encode(), rec.Encode()) {
				t.Error("the same lease gave a different record")
			}
			if rec.Outcome == spool.OutcomeComplete {
				// What is published is a valid catalog entry and nothing
				// else from the lease.
				if _, err := catalog.ParseEntry(rec.Result); err != nil {
					t.Errorf("the result is not a catalog entry: %v", err)
				}
				for _, banned := range []string{"snapshot", "attestation", "conditions", "specification", "made-up job", "score", g.ID} {
					if strings.Contains(string(rec.Result), banned) {
						t.Errorf("the result contains %q", banned)
					}
				}
			}
		})
	}
}

// TestEveryLeaseThroughTheWorker runs each saved lease through the whole
// loop: leased from the Muster stand-in, staged, handed back, kept by
// Muster and passed on to the hub repository stand-in. It does so twice:
// with the stand-in called directly and with the HTTP client in between.
func TestEveryLeaseThroughTheWorker(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, dir := range fixture.Dirs(t, "lease") {
		name := filepath.Base(dir)
		if grantOf(t, name).Kind != muster.KindItem {
			continue // the stand-in offers Chartroom items only
		}
		ran++
		t.Run(name, func(t *testing.T) {
			r := fixtureRig(t, name)
			r.run(context.Background(), r.muster)
			checkOutcome(t, r, name)
		})
		t.Run(name+"/http", func(t *testing.T) {
			r := fixtureRig(t, name)
			srv := httptest.NewServer(muster.Handler(r.muster, "made-up-token"))
			defer srv.Close()
			client, err := muster.NewHTTP(srv.URL, tokenPath, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r.run(context.Background(), client)
			checkOutcome(t, r, name)
		})
	}
	if ran < 25 {
		t.Errorf("only %d lease fixtures ran through the worker", ran)
	}
}

func sleeps(c *fakeClock) string {
	var parts []string
	for _, d := range c.sleeps {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, " ")
}

// TestTimeoutStaysStagedAndRetriesWithBackoff is the E_TIMEOUT row of the
// specification: Muster does not acknowledge, the result stays in the
// spool, and the worker tries again after 1s, 2s and 4s until it is
// acknowledged. The staged file and the log are saved examples.
func TestTimeoutStaysStagedAndRetriesWithBackoff(t *testing.T) {
	r := fixtureRig(t, "complete-publish")
	r.muster.StallResults(3)
	var stagedFile []byte
	r.clock.onSleep = func(n int) {
		names := r.staged()
		if len(names) != 1 {
			t.Errorf("during wait %d the spool holds %v, want one staged result", n, names)
			return
		}
		stagedFile, _ = os.ReadFile(filepath.Join(r.dir, names[0]))
		if len(r.hub.Commits()) != 0 {
			t.Errorf("during wait %d the entry was already published", n)
		}
	}
	r.run(context.Background(), r.muster)

	if got := sleeps(r.clock); got != "1s 2s 4s" {
		t.Errorf("waits = %q, want 1s 2s 4s", got)
	}
	if got := strings.Join(r.muster.Calls(), " "); got != "lease complete complete complete complete lease" {
		t.Errorf("calls = %q", got)
	}
	checkOutcome(t, r, "complete-publish")
	dir := filepath.Join(fixture.Root, "worker", "timeout-then-acknowledged")
	fixture.Golden(t, filepath.Join(dir, "want.staged.json"), stagedFile)
	fixture.Golden(t, filepath.Join(dir, "want.log"), r.log.Bytes())
}

// TestRefusedPauseIsDroppedAndLogged covers what Muster's contract v1 does
// with needs_subtool from Chartroom: none of the states Chartroom holds an
// item in can be paused, so the call is refused. The result is dropped,
// not sent again, and the refusal is logged in Chartroom's own words. The
// log is a saved example.
func TestRefusedPauseIsDroppedAndLogged(t *testing.T) {
	r := fixtureRig(t, "needs-subtool-state-scored")
	r.run(context.Background(), r.muster)
	if got := strings.Join(r.muster.Calls(), " "); got != "lease needs_subtool lease" {
		t.Errorf("calls = %q", got)
	}
	if len(r.clock.sleeps) != 0 {
		t.Errorf("the worker waited %s; a refusal is final and needs no wait", sleeps(r.clock))
	}
	checkOutcome(t, r, "needs-subtool-state-scored")
	fixture.Golden(t, filepath.Join(fixture.Root, "worker", "pause-refused", "want.log"), r.log.Bytes())
}

// TestLostAcknowledgementIsNotPublishedTwice covers a reply that never
// arrives: Muster recorded the step, the worker cannot know, sends again,
// and gets the same answer with nothing done twice.
func TestLostAcknowledgementIsNotPublishedTwice(t *testing.T) {
	for _, name := range []string{"complete-retire", "fail-missing-provenance"} {
		r := fixtureRig(t, name)
		r.muster.DropAcks(1)
		r.run(context.Background(), r.muster)
		if got := sleeps(r.clock); got != "1s" {
			t.Errorf("%s: waits = %q, want 1s", name, got)
		}
		if n := r.count("complete") + r.count("fail"); n != 2 {
			t.Errorf("%s: the result was sent %d times, want 2", name, n)
		}
		checkOutcome(t, r, name)
	}
}

// stopAfterFirstWait runs a worker against a Muster that takes the lease
// call and then stops answering, and stops the worker at its first wait.
// The result is left staged.
func stopAfterFirstWait(t *testing.T, r *rig) {
	t.Helper()
	r.muster.StallResults(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	r.clock.onSleep = func(int) { cancel() }
	r.run(ctx, r.muster)
	r.clock.onSleep = nil
	r.muster.StallResults(0)
	if len(r.staged()) != 1 {
		t.Fatalf("after the first worker stopped the spool holds %v", r.staged())
	}
}

// TestStagedResultSurvivesARestart stops the worker while Muster is not
// answering and starts a new one on the same spool. The new worker sends
// the staged result before it asks for more work.
func TestStagedResultSurvivesARestart(t *testing.T) {
	for _, name := range []string{"complete-link", "fail-missing-provenance", "needs-subtool-state-scored"} {
		r := fixtureRig(t, name)
		stopAfterFirstWait(t, r)
		before := len(r.muster.Calls())
		r.run(context.Background(), r.muster)
		after := r.muster.Calls()[before:]
		if len(after) != 2 || after[0] == "lease" || after[1] != "lease" {
			t.Errorf("%s: calls after restart = %v, want the staged result first and then a lease", name, after)
		}
		checkOutcome(t, r, name)
	}
}

// TestLateResultIsStillKept follows the contract: a lease that ran out
// still counts until someone else takes the step, so a result that waited
// in the spool for longer than a lease is not thrown away.
func TestLateResultIsStillKept(t *testing.T) {
	r := fixtureRig(t, "complete-merge")
	stopAfterFirstWait(t, r)
	r.clock.t = r.clock.t.Add(10 * time.Minute)
	r.run(context.Background(), r.muster)
	if strings.Contains(r.log.String(), "lease_not_held") || r.count("lease") != 2 {
		t.Errorf("the late result was not accepted as it was:\n%s", r.log)
	}
	checkOutcome(t, r, "complete-merge")
}

// TestResultForALeaseTakenOverIsDropped covers the other half: once the
// step has been leased again, the old lease is refused. The staged result
// is dropped, without a wait, and nothing is published by it.
func TestResultForALeaseTakenOverIsDropped(t *testing.T) {
	r := fixtureRig(t, "complete-merge")
	stopAfterFirstWait(t, r)
	r.clock.t = r.clock.t.Add(10 * time.Minute)
	other, err := r.muster.Lease(context.Background())
	if err != nil || other == nil {
		t.Fatalf("the step was not offered again: %v", err)
	}
	r.run(context.Background(), r.muster)
	if !strings.Contains(r.log.String(), `"msg":"lease_not_held"`) || len(r.staged()) != 0 || len(r.clock.sleeps) != 1 {
		t.Errorf("the result for the old lease was not dropped at once:\n%s", r.log)
	}
	if len(r.hub.Commits()) != 0 {
		t.Error("the old lease published something")
	}
	if o := r.muster.Outcomes()[0]; o.Status != muster.StatusLeased || o.Attempt != 2 {
		t.Errorf("outcome = %+v, want the step held under the new lease", o)
	}
}

// TestHeartbeatKeepsALeaseWhileItsResultWaits stalls Muster for longer
// than a lease lasts while something else keeps asking for the step. With
// heartbeats the lease is kept and the result lands under it. Without
// them the step is taken over and the result is refused.
func TestHeartbeatKeepsALeaseWhileItsResultWaits(t *testing.T) {
	const stalls = 8 // 1+2+4+8+16+32+60+60 = 183s, three lease lengths
	rival := func(r *rig) *int {
		taken := 0
		r.clock.onSleep = func(int) {
			if taken == 0 {
				if g, _ := r.muster.Lease(context.Background()); g != nil {
					taken++
				}
			}
		}
		return &taken
	}

	r := fixtureRig(t, "complete-publish")
	r.muster.StallResults(stalls)
	taken := rival(r)
	r.run(context.Background(), r.muster)
	if r.clock.total() != 183*time.Second {
		t.Errorf("waited %s in all, want 183s", r.clock.total())
	}
	if r.count("heartbeat") < 8 {
		t.Errorf("%d heartbeats in 183s at one every 20s", r.count("heartbeat"))
	}
	if *taken != 0 || strings.Contains(r.log.String(), "lease_not_held") || r.count("complete") != stalls+1 {
		t.Errorf("the lease did not hold: taken over %d times, %d completes\n%s", *taken, r.count("complete"), r.log)
	}
	checkOutcome(t, r, "complete-publish")

	quiet := fixtureRig(t, "complete-publish")
	quiet.cfg.HeartbeatEvery = time.Hour
	quiet.muster.StallResults(stalls)
	taken = rival(quiet)
	quiet.run(context.Background(), quiet.muster)
	if quiet.count("heartbeat") != 0 || *taken != 1 || !strings.Contains(quiet.log.String(), `"msg":"lease_not_held"`) {
		t.Errorf("without heartbeats the step should have been taken over (%d times):\n%s", *taken, quiet.log)
	}
}

// TestMusterDownAtStart covers Muster not answering a lease call.
func TestMusterDownAtStart(t *testing.T) {
	r := fixtureRig(t, "complete-publish")
	r.muster.SetDown(true)
	r.clock.onSleep = func(n int) {
		if n == 7 {
			r.muster.SetDown(false)
		}
	}
	r.run(context.Background(), r.muster)
	if got := sleeps(r.clock); got != "1s 2s 4s 8s 16s 32s 1m0s" {
		t.Errorf("waits = %q", got)
	}
	if !strings.Contains(r.log.String(), `"code":"E_TIMEOUT","reason":"upstream_timeout","attempt":7,"delay_ms":60000`) {
		t.Errorf("log:\n%s", r.log)
	}
	checkOutcome(t, r, "complete-publish")
}

// scripted is a Muster that answers from a script: a list of answers to
// lease calls, taken in order, and one answer for every result call. It
// records the order results come back in.
type scripted struct {
	leases  []any // a muster.Grant, an error, or nil for nothing to lease
	result  error
	results []string
}

func (s *scripted) Lease(context.Context) (*muster.Grant, error) {
	if len(s.leases) == 0 {
		return nil, nil
	}
	next := s.leases[0]
	s.leases = s.leases[1:]
	switch v := next.(type) {
	case muster.Grant:
		return &v, nil
	case error:
		return nil, v
	}
	return nil, nil
}
func (s *scripted) Heartbeat(context.Context, string) error { return nil }
func (s *scripted) Complete(_ context.Context, lease, state string, _ json.RawMessage) error {
	s.results = append(s.results, "complete:"+lease+":"+state)
	return s.result
}
func (s *scripted) Fail(_ context.Context, lease, code string) error {
	s.results = append(s.results, "fail:"+lease+":"+code)
	return s.result
}
func (s *scripted) NeedsSubtool(_ context.Context, lease string) error {
	s.results = append(s.results, "needs_subtool:"+lease)
	return s.result
}

// TestStepsAreTakenInTheOrderMusterGivesThem checks the worker leases one
// step at a time and sends each result before it asks for the next, so the
// order Muster chose (urgent, then normal, then backfill) is kept.
func TestStepsAreTakenInTheOrderMusterGivesThem(t *testing.T) {
	relabel := func(name, lease, lane string) muster.Grant {
		g := grantOf(t, name)
		g.Lease, g.Lane = lease, lane
		return g
	}
	s := &scripted{leases: []any{
		relabel("needs-subtool-state-scored", "u1", "urgent"),
		relabel("complete-publish", "u2", "urgent"),
		relabel("fail-missing-provenance", "n1", "normal"),
		relabel("complete-link", "n2", "normal"),
		relabel("complete-retire", "b1", "backfill"),
	}}
	r := newRig(t)
	r.run(context.Background(), s)
	want := "needs_subtool:u1 complete:u2:live fail:n1:E_PARSE complete:n2:linked complete:b1:retired"
	if got := strings.Join(s.results, " "); got != want {
		t.Errorf("results went back as\n  %s\nwant\n  %s", got, want)
	}
	if len(r.clock.sleeps) != 0 {
		t.Errorf("the worker waited between steps: %s", sleeps(r.clock))
	}
}

// TestLeaseCallsThatGoWrong covers every way a lease call can fail to give
// a step. Each is logged with one of the three codes, in Chartroom's own
// words, and asked for again after a wait that doubles.
func TestLeaseCallsThatGoWrong(t *testing.T) {
	noLease := grantOf(t, "complete-publish")
	noLease.Lease = ""
	s := &scripted{leases: []any{
		muster.ErrBadResponse,
		muster.ErrRefused,
		muster.ErrUnknownContract,
		muster.ErrUnavailable,
		noLease,
		grantOf(t, "complete-publish"),
	}}
	r := newRig(t)
	r.run(context.Background(), s)
	if got := sleeps(r.clock); got != "1s 2s 4s 8s 16s" {
		t.Errorf("waits = %q", got)
	}
	for _, line := range []string{
		`"msg":"retry","code":"E_PARSE","reason":"malformed_json","attempt":1`,
		`"msg":"retry","code":"E_PARSE","reason":"upstream_refused","attempt":2`,
		`"msg":"retry","code":"E_NEEDS_SUBTOOL","reason":"schema_version_mismatch","attempt":3`,
		`"msg":"retry","code":"E_TIMEOUT","reason":"upstream_timeout","attempt":4`,
		`"msg":"lease_unusable","code":"E_PARSE","reason":"invalid_metadata"`,
	} {
		if !strings.Contains(r.log.String(), line) {
			t.Errorf("the log lacks %s\n%s", line, r.log)
		}
	}
	if strings.Join(s.results, " ") != "complete:PA-0101.8.1:live" {
		t.Errorf("results = %v", s.results)
	}
}

// TestResultsRefusedForGoodAreDropped covers a result Muster refuses in a
// way that sending it again cannot cure. It leaves the spool at once, so
// one refused result cannot hold up the steps behind it.
func TestResultsRefusedForGoodAreDropped(t *testing.T) {
	for _, refusal := range []error{muster.ErrRefused, muster.ErrUnknownContract, muster.ErrMoveNotAllowed, muster.ErrLeaseNotHeld} {
		s := &scripted{result: refusal, leases: []any{grantOf(t, "complete-publish"), grantOf(t, "complete-link")}}
		r := newRig(t)
		r.run(context.Background(), s)
		if got := strings.Join(s.results, " "); got != "complete:PA-0101.8.1:live complete:PA-0102.3.1:linked" {
			t.Errorf("%v: results = %s", refusal, got)
		}
		if len(r.staged()) != 0 || len(r.clock.sleeps) != 0 {
			t.Errorf("%v: staged %v, waits %s", refusal, r.staged(), sleeps(r.clock))
		}
	}
}

// TestIdleWorkerPollsAndStopsWhenAsked runs the worker as it runs in the
// cluster, without Once: it polls at the poll interval and stops when its
// context is done, leaving nothing behind.
func TestIdleWorkerPollsAndStopsWhenAsked(t *testing.T) {
	r := newRig(t)
	r.cfg.Once = false
	ctx, cancel := context.WithCancel(context.Background())
	r.clock.onSleep = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	r.run(ctx, r.muster)
	if got := sleeps(r.clock); got != "5s 5s 5s" {
		t.Errorf("waits = %q", got)
	}
	if r.count("lease") != 3 {
		t.Errorf("%d lease calls", r.count("lease"))
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{0, 1, 2, 4, 8, 16, 32, 60, 60, 60}
	for failures, w := range want {
		if got := worker.Backoff(time.Second, time.Minute, failures); got != w*time.Second {
			t.Errorf("Backoff after %d = %s, want %s", failures, got, w*time.Second)
		}
	}
	if got := worker.Backoff(time.Second, time.Minute, 1000); got != time.Minute {
		t.Errorf("Backoff after 1000 = %s", got)
	}
	if got := worker.Backoff(2*time.Minute, time.Minute, 1); got != time.Minute {
		t.Errorf("a base above the cap gave %s", got)
	}
}

// TestLargestEntryFitsInOneRequest builds the largest entry the contract
// allows, out of the characters that take the most room once written as
// JSON, and sends it over HTTP to the stand-in, which enforces Muster's
// 64 KiB limit on a request.
func TestLargestEntryFitsInOneRequest(t *testing.T) {
	links := make([]string, catalog.MaxProvLinks)
	for i := range links {
		links[i] = "https://rules.example/" + strings.Repeat(`"`, catalog.MaxURLBytes-30) + strconv.Itoa(i)
	}
	raw := func(v any) json.RawMessage {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	conds := make([]map[string]any, 5)
	for i := range conds {
		conds[i] = map[string]any{"name": "made-up-condition-" + strconv.Itoa(i), "passed": true}
	}
	r := newRig(t)
	r.muster.Seed(muster.Seed{ID: "PA-0001", State: "building", Lane: "normal", Results: map[string]json.RawMessage{
		"waybill": raw(map[string]any{
			"slug":       strings.Repeat("a", catalog.MaxSlugBytes),
			"entry":      map[string]any{"title": strings.Repeat(`"`, catalog.MaxTitleRunes), "summary": strings.Repeat("\U0001F6A2", catalog.MaxSummaryRunes)},
			"provenance": map[string]any{"note": strings.Repeat(`\`, catalog.MaxProvNoteBytes), "links": links},
		}),
		"moldloft":  raw(map[string]any{"publish": map[string]any{"repository": "https://forge.example/" + strings.Repeat(`"`, catalog.MaxURLBytes-30)}}),
		"shakedown": raw(map[string]any{"attestation": map[string]any{"conditions": conds}}),
	}})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(muster.Handler(r.muster, "made-up-token"))
	defer srv.Close()
	client, _ := muster.NewHTTP(srv.URL, tokenPath, time.Second)
	r.run(context.Background(), client)
	o := r.muster.Outcomes()[0]
	if o.Status != muster.StatusCompleted {
		t.Fatalf("outcome = %+v\n%s", o, r.log)
	}
	if n := len(r.muster.Result("PA-0001", muster.Worker)); n > muster.MaxRequestBytes*3/4 {
		t.Errorf("the largest result is %d bytes, too near the %d byte limit", n, muster.MaxRequestBytes)
	} else {
		t.Logf("the largest result is %d bytes of %d allowed", n, muster.MaxRequestBytes)
	}
}

// TestLogsCarryNothingFromTheData runs items full of text that must not be
// repeated and checks none of it reaches the log.
func TestLogsCarryNothingFromTheData(t *testing.T) {
	for _, name := range []string{
		"fail-repository-on-a-private-host",
		"fail-raw-snapshot-in-provenance",
		"fail-id-not-an-item-id",
		"complete-publish-reads-only-its-own-blocks",
		"complete-publish",
	} {
		r := fixtureRig(t, name)
		r.run(context.Background(), r.muster)
		for _, banned := range []string{"svc.cluster.local", "forge.example", "captured page", "EP-0001", "sample-", "made-up", ".8.1", "example.org"} {
			if strings.Contains(r.log.String(), banned) {
				t.Errorf("%s: the log contains %q:\n%s", name, banned, r.log)
			}
		}
	}
}

// TestWorkerHandlesLogbookPostRestartGrant checks the exact grant shape
// Muster's PostgreSQL 17 store returns after a restart, when core.item has
// issued a five-digit identifier (PA-10000+) and Found carries an empty
// source alongside title and address.
func TestWorkerHandlesLogbookPostRestartGrant(t *testing.T) {
	base := grantOf(t, "complete-publish")
	r := newRig(t)
	r.muster.Seed(muster.Seed{
		ID:      "PA-10000",
		State:   "building",
		Lane:    "normal",
		Found:   json.RawMessage(`{"source":"","title":"Sample Clause Finder","address":"https://rules.example/part-900/section-12"}`),
		Results: base.Results,
	})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(muster.Handler(r.muster, "made-up-token"))
	defer srv.Close()
	client, err := muster.NewHTTP(srv.URL, tokenPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.run(context.Background(), client)
	outcomes := r.muster.Outcomes()
	if len(outcomes) != 1 || outcomes[0].ID != "PA-10000" || outcomes[0].Status != muster.StatusCompleted || outcomes[0].State != "live" {
		t.Fatalf("outcomes = %+v\n%s", outcomes, r.log)
	}
}

// TestPackageBoundaries reads the import lists of every internal package.
// Only muster and plainforge may touch net/http, only worker and muster's
// stand-in may import plainforge, and no package may use anything outside
// the Go standard library.
func TestPackageBoundaries(t *testing.T) {
	network := map[string]bool{"net/http": true, "net/rpc": true, "net/smtp": true, "database/sql": true, "os/exec": true}
	for _, pkg := range []string{"worker", "decision", "catalog", "spool", "cserr", "strictjson", "muster", "plainforge"} {
		files, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no files for package %s", pkg)
		}
		for _, file := range files {
			isTest := strings.HasSuffix(file, "_test.go")
			standIn := pkg == "muster" && (strings.HasSuffix(file, "memory.go") || strings.HasSuffix(file, "handler.go"))
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if strings.HasSuffix(path, "/internal/plainforge") && !isTest && !standIn && pkg != "worker" {
					t.Errorf("%s imports Plainforge", file)
				}
				if network[path] && !isTest && pkg != "muster" && pkg != "plainforge" {
					t.Errorf("%s imports %s", file, path)
				}
				if strings.Contains(path, ".") && !strings.HasPrefix(path, "github.com/ClearedStack/chartroom/") {
					t.Errorf("%s imports %s, which is outside the standard library", file, path)
				}
			}
		}
	}
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "require") {
		t.Error("go.mod has a dependency; its license and notice need checking and this test needs updating")
	}
}

func TestWorkerWithPlainforgeHubEnforcesCatalogRulesAndRetriesCleanly(t *testing.T) {
	r := newRig(t)
	workerHub := plainforge.NewMemory()
	r.cfg.Hub = workerHub

	pubGrant := grantOf(t, "complete-publish")
	r.muster.Seed(muster.Seed{ID: "PA-0101", State: "building", Lane: "urgent", Results: pubGrant.Results})

	// Foreign item claiming the same slug ("sample-clause-finder") with a
	// different summary must be paused as E_NEEDS_SUBTOOL once PA-0101 is live.
	clashResults := map[string]json.RawMessage{
		"waybill":   json.RawMessage(`{"slug":"sample-clause-finder","entry":{"title":"Another Tool","summary":"Clashes on slug."},"provenance":{"note":"Credit note.","links":["https://rules.example/part-1"]}}`),
		"moldloft":  pubGrant.Results["moldloft"],
		"shakedown": pubGrant.Results["shakedown"],
	}
	r.muster.Seed(muster.Seed{ID: "PA-0102", State: "building", Lane: "normal", Results: clashResults})

	// Merge whose parent is PA-0101's live slug ("sample-clause-finder") succeeds,
	// while a merge whose parent is missing ("nonexistent-parent") pauses.
	mergeOK := map[string]json.RawMessage{
		"countersign": json.RawMessage(`{"slug":"folded-helper","entry":{"title":"Folded Helper","summary":"Folded into sample-clause-finder."},"merge":{"parent":"sample-clause-finder","note":"Folded into parent calculator."}}`),
	}
	mergeMissingParent := map[string]json.RawMessage{
		"countersign": json.RawMessage(`{"slug":"orphan-merge","entry":{"title":"Orphan Merge","summary":"Parent is not live."},"merge":{"parent":"nonexistent-parent","note":"Missing parent."}}`),
	}
	r.muster.Seed(muster.Seed{ID: "PA-0103", State: "merged", Lane: "normal", Results: mergeOK})
	r.muster.Seed(muster.Seed{ID: "PA-0104", State: "merged", Lane: "backfill", Results: mergeMissingParent})

	// Simulate both a transient Plainforge outage on the first read and a
	// Muster stall after the first Plainforge commit, proving no duplicate
	// catalog commit is made when Complete is retried.
	workerHub.SetDown(true)
	r.muster.StallResults(1)
	r.clock.onSleep = func(n int) {
		if n == 1 {
			workerHub.SetDown(false)
		}
	}

	r.run(context.Background(), r.muster)

	commits := workerHub.Commits()
	if len(commits) != 2 {
		t.Fatalf("workerHub commits = %d, want 2 (publish + valid merge, zero duplicate on Muster retry)", len(commits))
	}
	rawCat, err := workerHub.ReadFile(context.Background(), muster.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := catalog.Parse(rawCat, catalog.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Find("sample-clause-finder") == nil || parsed.Find("folded-helper") == nil {
		t.Fatalf("catalog missing expected entries:\n%s", rawCat)
	}
	if parsed.Find("orphan-merge") != nil {
		t.Fatalf("orphan-merge should not be committed:\n%s", rawCat)
	}
	for _, wantLog := range []string{
		`"msg":"retry","code":"E_TIMEOUT","reason":"upstream_timeout"`,
		`"id":"PA-0102","outcome":"needs_subtool","code":"E_NEEDS_SUBTOOL","reason":"unsupported_catalog_state","field":"slug"`,
		`"id":"PA-0104","outcome":"needs_subtool","code":"E_NEEDS_SUBTOOL","reason":"unsupported_catalog_state","field":"merge.parent"`,
	} {
		if !strings.Contains(r.log.String(), wantLog) {
			t.Errorf("log missing %s\n%s", wantLog, r.log.String())
		}
	}
}
