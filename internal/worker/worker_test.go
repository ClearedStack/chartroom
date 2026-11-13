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

func newRig(t *testing.T, catalogData []byte) *rig {
	t.Helper()
	r := &rig{t: t, clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}, hub: plainforge.NewMemory(), dir: t.TempDir(), log: &bytes.Buffer{}}
	_ = r.hub.WriteFile(context.Background(), muster.CatalogPath, catalogData, "seed")
	r.muster = muster.NewMemory(r.hub, r.clock.Now, time.Minute)
	r.cfg = worker.DefaultConfig()
	r.cfg.Once = true
	return r
}

func fixtureRig(t *testing.T, name string) *rig {
	t.Helper()
	dir := filepath.Join(fixture.Root, "apply", name)
	r := newRig(t, fixture.Read(t, filepath.Join(dir, "catalog.json")))
	r.muster.Seed("item", "normal", fixture.Read(t, filepath.Join(dir, "decision.json")))
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

func (r *rig) catalog() []byte {
	data, _ := r.hub.ReadFile(context.Background(), muster.CatalogPath)
	return data
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

// checkFixtureOutcome compares what the stand-ins saw with the fixture's
// saved outputs.
func checkFixtureOutcome(t *testing.T, r *rig, name string) {
	t.Helper()
	dir := filepath.Join(fixture.Root, "apply", name)
	out := r.muster.Outcomes()
	if len(out) != 1 {
		t.Fatalf("%d outcomes", len(out))
	}
	got := out[0]
	if len(r.staged()) != 0 {
		t.Errorf("the spool still holds %v", r.staged())
	}
	if errPath := filepath.Join(dir, "want.error.txt"); fixture.Exists(errPath) {
		code, reason, field := fixture.ErrorLine(t, fixture.Read(t, errPath))
		wantStatus := muster.StatusFailed
		if code == "E_NEEDS_SUBTOOL" {
			wantStatus = muster.StatusNeedsSubtool
		}
		if got.Status != wantStatus || got.Code != code || got.Reason != reason || got.Field != field {
			t.Errorf("outcome = %+v, want %s %s %s %q", got, wantStatus, code, reason, field)
		}
		if !bytes.Equal(r.catalog(), fixture.Read(t, filepath.Join(dir, "catalog.json"))) || len(r.hub.Commits()) != 1 {
			t.Errorf("the hub catalog changed although the item was not published")
		}
		return
	}
	var change struct {
		To      string `json:"to"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(dir, "want.change.json")), &change); err != nil {
		t.Fatal(err)
	}
	if got.Status != muster.StatusCompleted || got.State != change.To {
		t.Errorf("outcome = %+v, want completed in state %s", got, change.To)
	}
	if change.Changed {
		fixture.Golden(t, filepath.Join(dir, "want.catalog.json"), r.catalog())
		if len(r.hub.Commits()) != 2 {
			t.Errorf("%d commits, want the seed and one", len(r.hub.Commits()))
		}
	} else if len(r.hub.Commits()) != 1 {
		t.Errorf("an unchanged entry was committed again")
	}
}

// TestEveryFixtureThroughTheWorker runs each saved example through the
// whole loop: leased from the Muster stand-in, staged, handed back and
// committed to the hub repository stand-in.
func TestEveryFixtureThroughTheWorker(t *testing.T) {
	for _, dir := range fixture.Dirs(t, "apply") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			r := fixtureRig(t, name)
			r.run(context.Background(), r.muster)
			checkFixtureOutcome(t, r, name)
		})
	}
}

// TestEveryFixtureOverHTTP does the same with the HTTP client in between.
func TestEveryFixtureOverHTTP(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range fixture.Dirs(t, "apply") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			r := fixtureRig(t, name)
			srv := httptest.NewServer(muster.Handler(r.muster, "made-up-token"))
			defer srv.Close()
			client, err := muster.NewHTTP(srv.URL, tokenPath, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			r.run(context.Background(), client)
			checkFixtureOutcome(t, r, name)
		})
	}
}

// TestLeaseFixtures checks, for each saved lease, the exact record the
// worker stages for it.
func TestLeaseFixtures(t *testing.T) {
	for _, dir := range fixture.Dirs(t, "lease") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			var l muster.Lease
			if err := json.Unmarshal(fixture.Read(t, filepath.Join(dir, "lease.json")), &l); err != nil {
				t.Fatal(err)
			}
			rec := worker.Evaluate(l)
			wantOutcome := strings.SplitN(name, "-", 2)[0]
			if wantOutcome == "needs" {
				wantOutcome = spool.OutcomeNeedsSubtool
			}
			if rec.Outcome != wantOutcome {
				t.Errorf("outcome = %s, but the fixture name says %s", rec.Outcome, wantOutcome)
			}
			fixture.Golden(t, filepath.Join(dir, "want.record.json"), rec.Encode())
			if again := worker.Evaluate(l); !bytes.Equal(again.Encode(), rec.Encode()) {
				t.Error("the same lease gave a different record")
			}
		})
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
	r := fixtureRig(t, "ok-publish")
	r.muster.StallResults(3)
	var stagedFile []byte
	r.clock.onSleep = func(n int) {
		names := r.staged()
		if len(names) != 1 {
			t.Errorf("during wait %d the spool holds %v, want one staged result", n, names)
			return
		}
		stagedFile, _ = os.ReadFile(filepath.Join(r.dir, names[0]))
		if len(r.hub.Commits()) != 1 {
			t.Errorf("during wait %d the catalog was already committed", n)
		}
	}
	r.run(context.Background(), r.muster)

	if got := sleeps(r.clock); got != "1s 2s 4s" {
		t.Errorf("waits = %q, want 1s 2s 4s", got)
	}
	if got := strings.Join(r.muster.Calls(), " "); got != "lease complete complete complete complete lease" {
		t.Errorf("calls = %q", got)
	}
	checkFixtureOutcome(t, r, "ok-publish")
	dir := filepath.Join(fixture.Root, "worker", "timeout-then-acknowledged")
	fixture.Golden(t, filepath.Join(dir, "want.staged.json"), stagedFile)
	fixture.Golden(t, filepath.Join(dir, "want.log"), r.log.Bytes())
}

// TestLostAcknowledgementIsNotPublishedTwice covers a reply that never
// arrives: Muster committed, the worker cannot know, sends again, and the
// catalog is committed once.
func TestLostAcknowledgementIsNotPublishedTwice(t *testing.T) {
	r := fixtureRig(t, "ok-retire")
	r.muster.DropAcks(1)
	r.run(context.Background(), r.muster)
	if got := sleeps(r.clock); got != "1s" {
		t.Errorf("waits = %q, want 1s", got)
	}
	if r.count("complete") != 2 {
		t.Errorf("complete was called %d times, want 2", r.count("complete"))
	}
	checkFixtureOutcome(t, r, "ok-retire")
}

// TestStagedResultSurvivesARestart stops the worker while Muster is not
// answering and starts a new one on the same spool. The new worker sends
// the staged result before it asks for more work.
func TestStagedResultSurvivesARestart(t *testing.T) {
	for _, name := range []string{"ok-link", "parse-missing-provenance-publish", "subtool-unsupported-entry-type"} {
		r := fixtureRig(t, name)
		r.muster.SetDown(true)
		// Lease while Muster still answers, then lose it.
		r.muster.SetDown(false)
		r.muster.StallResults(1 << 20)
		ctx, cancel := context.WithCancel(context.Background())
		r.clock.onSleep = func(int) { cancel() }
		r.run(ctx, r.muster)
		if len(r.staged()) != 1 {
			t.Fatalf("%s: after the first worker stopped the spool holds %v", name, r.staged())
		}
		before := len(r.muster.Calls())

		r.muster.StallResults(0)
		r.clock.onSleep = nil
		r.run(context.Background(), r.muster)
		after := r.muster.Calls()[before:]
		if len(after) != 2 || after[0] == "lease" || after[1] != "lease" {
			t.Errorf("%s: calls after restart = %v, want the staged result first and then a lease", name, after)
		}
		checkFixtureOutcome(t, r, name)
	}
}

// TestLapsedLeaseIsDroppedAndDoneAgain covers a result that waited too
// long: Muster has given the lease up, so the staged result is dropped,
// the item is leased again and the catalog is still committed once.
func TestLapsedLeaseIsDroppedAndDoneAgain(t *testing.T) {
	r := fixtureRig(t, "ok-merge")
	r.muster.StallResults(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	r.clock.onSleep = func(int) { cancel() }
	r.run(ctx, r.muster)

	r.clock.t = r.clock.t.Add(2 * time.Minute)
	r.muster.StallResults(0)
	r.clock.onSleep = nil
	r.run(context.Background(), r.muster)
	if !strings.Contains(r.log.String(), `"msg":"lease_gone"`) {
		t.Errorf("the log does not show the lapsed lease being dropped:\n%s", r.log)
	}
	if r.count("complete") != 3 {
		t.Errorf("complete was called %d times, want 3 (stalled, refused, accepted)", r.count("complete"))
	}
	checkFixtureOutcome(t, r, "ok-merge")
}

// TestHeartbeatKeepsALeaseWhileItsResultWaits stalls Muster for longer
// than a lease lasts. With heartbeats the first lease is still good when
// Muster answers; without them it lapses.
func TestHeartbeatKeepsALeaseWhileItsResultWaits(t *testing.T) {
	const stalls = 8 // 1+2+4+8+16+32+60+60 = 183s, three lease lengths

	r := fixtureRig(t, "ok-publish")
	r.muster.StallResults(stalls)
	r.run(context.Background(), r.muster)
	if r.clock.total() != 183*time.Second {
		t.Errorf("waited %s in all, want 183s", r.clock.total())
	}
	if r.count("heartbeat") < 8 {
		t.Errorf("%d heartbeats in 183s at one every 20s", r.count("heartbeat"))
	}
	if strings.Contains(r.log.String(), "lease_gone") || r.count("complete") != stalls+1 {
		t.Errorf("the lease did not hold: %d completes\n%s", r.count("complete"), r.log)
	}
	checkFixtureOutcome(t, r, "ok-publish")

	quiet := fixtureRig(t, "ok-publish")
	quiet.cfg.HeartbeatEvery = time.Hour
	quiet.muster.StallResults(stalls)
	quiet.run(context.Background(), quiet.muster)
	if quiet.count("heartbeat") != 0 || !strings.Contains(quiet.log.String(), "lease_gone") {
		t.Errorf("without heartbeats the lease should have lapsed:\n%s", quiet.log)
	}
	checkFixtureOutcome(t, quiet, "ok-publish")
}

// TestMusterDownAtStart covers Muster not answering a lease call.
func TestMusterDownAtStart(t *testing.T) {
	r := fixtureRig(t, "ok-publish")
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
	checkFixtureOutcome(t, r, "ok-publish")
}

// scripted is a Muster that hands out a fixed list of leases once, in the
// order given, and records the order results come back in.
type scripted struct {
	leases  []muster.Lease
	results []string
	bad     int
}

func (s *scripted) Lease(context.Context, int) ([]muster.Lease, error) {
	if s.bad > 0 {
		s.bad--
		return nil, muster.ErrBadResponse
	}
	out := s.leases
	s.leases = nil
	return out, nil
}
func (s *scripted) Heartbeat(context.Context, string) error { return nil }
func (s *scripted) Complete(_ context.Context, id string, _ json.RawMessage) error {
	s.results = append(s.results, "complete:"+id)
	return nil
}
func (s *scripted) Fail(_ context.Context, id string, _ muster.Report) error {
	s.results = append(s.results, "fail:"+id)
	return nil
}
func (s *scripted) NeedsSubtool(_ context.Context, id string, _ muster.Report) error {
	s.results = append(s.results, "needs_subtool:"+id)
	return nil
}

func leaseFixture(t *testing.T, name string) muster.Lease {
	t.Helper()
	var l muster.Lease
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(fixture.Root, "lease", name, "lease.json")), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestUrgentBeforeNormalBeforeBackfill hands the worker a batch in the
// wrong order and checks the results go back urgent first, then normal,
// then backfill, keeping the given order inside each.
func TestUrgentBeforeNormalBeforeBackfill(t *testing.T) {
	relabel := func(l muster.Lease, id, priority string) muster.Lease {
		l.LeaseID, l.Priority = id, priority
		return l
	}
	publish := leaseFixture(t, "complete-publish")
	broken := leaseFixture(t, "fail-missing-provenance")
	unknown := leaseFixture(t, "needs-subtool-unknown-action")
	s := &scripted{leases: []muster.Lease{
		relabel(publish, "b1", "backfill"),
		relabel(broken, "n1", "normal"),
		relabel(unknown, "u1", "urgent"),
		relabel(publish, "x1", "whenever"),
		relabel(publish, "n2", "normal"),
		relabel(publish, "u2", "urgent"),
		relabel(publish, "", "urgent"), // no lease to report against: skipped
	}}
	r := newRig(t, nil)
	r.run(context.Background(), s)
	want := "needs_subtool:u1 complete:u2 fail:n1 complete:n2 complete:b1 needs_subtool:x1"
	if got := strings.Join(s.results, " "); got != want {
		t.Errorf("results went back as\n  %s\nwant\n  %s", got, want)
	}
	if !strings.Contains(r.log.String(), `"msg":"lease_unusable"`) {
		t.Error("the lease without an identifier was not logged")
	}
}

// TestNothingUsableLeasedDoesNotSpin covers a Muster that keeps answering
// with leases that cannot be reported against. The worker waits between
// calls instead of asking again at once.
func TestNothingUsableLeasedDoesNotSpin(t *testing.T) {
	r := newRig(t, nil)
	rounds := 0
	client := &scripted{}
	refill := func() { client.leases = []muster.Lease{{Priority: "urgent", Item: json.RawMessage(`{}`)}} }
	refill()
	r.clock.onSleep = func(n int) {
		rounds = n
		if n < 3 {
			refill()
		}
	}
	r.run(context.Background(), client)
	if got := sleeps(r.clock); got != "1s 2s 4s" || rounds != 3 {
		t.Errorf("waits = %q", got)
	}
	if len(client.results) != 0 || len(r.staged()) != 0 {
		t.Errorf("something was staged or sent: %v %v", client.results, r.staged())
	}
}

// TestUnreadableAnswerFromMuster covers a lease answer that cannot be
// read: it is logged as E_PARSE and asked for again after a wait.
func TestUnreadableAnswerFromMuster(t *testing.T) {
	s := &scripted{bad: 2, leases: []muster.Lease{leaseFixture(t, "complete-publish")}}
	r := newRig(t, nil)
	r.run(context.Background(), s)
	if got := sleeps(r.clock); got != "1s 2s" {
		t.Errorf("waits = %q", got)
	}
	if !strings.Contains(r.log.String(), `"code":"E_PARSE","reason":"malformed_json","attempt":2`) {
		t.Errorf("log:\n%s", r.log)
	}
	if strings.Join(s.results, " ") != "complete:lease-0001" {
		t.Errorf("results = %v", s.results)
	}
}

// TestIdleWorkerPollsAndStopsWhenAsked runs the worker as it runs in the
// cluster, without Once: it polls at the poll interval and stops when its
// context is done, leaving nothing behind.
func TestIdleWorkerPollsAndStopsWhenAsked(t *testing.T) {
	r := newRig(t, nil)
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

// TestLogsCarryNothingFromTheData runs items full of text that must not be
// repeated and checks none of it reaches the log.
func TestLogsCarryNothingFromTheData(t *testing.T) {
	for _, name := range []string{
		"parse-invalid-metadata-repository-with-credentials",
		"parse-invalid-metadata-repository-private-host",
		"parse-unknown-field-raw-snapshot",
		"parse-invalid-metadata-item-id",
		"ok-publish",
	} {
		r := fixtureRig(t, name)
		r.run(context.Background(), r.muster)
		for _, banned := range []string{"made-up-word", "someone", "svc.cluster.local", "forge.example", "captured page", "ITM 0101", "lease-"} {
			if strings.Contains(r.log.String(), banned) {
				t.Errorf("%s: the log contains %q:\n%s", name, banned, r.log)
			}
		}
	}
}

// TestPackageBoundaries reads the import lists of the packages that run in
// the worker. None may reach Plainforge, and only the Muster client may
// touch the network: Chartroom talks to Muster and to nothing else.
func TestPackageBoundaries(t *testing.T) {
	network := map[string]bool{"net/http": true, "net/rpc": true, "net/smtp": true, "database/sql": true, "os/exec": true}
	for _, pkg := range []string{"worker", "decision", "catalog", "spool", "cserr", "strictjson", "muster"} {
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
				if strings.HasSuffix(path, "/internal/plainforge") && !isTest && !standIn {
					t.Errorf("%s imports Plainforge", file)
				}
				if network[path] && !isTest && pkg != "muster" {
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
