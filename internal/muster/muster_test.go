package muster_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/fixture"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/plainforge"
)

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMemory(t *testing.T) (*muster.Memory, *plainforge.Memory, *clock) {
	t.Helper()
	hub := plainforge.NewMemory()
	c := &clock{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	return muster.NewMemory(hub, c.now, muster.LeaseLength), hub, c
}

// seedOf turns a saved lease into the item a Muster would hold to give it.
func seedOf(t *testing.T, name string) muster.Seed {
	t.Helper()
	var g muster.Grant
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(fixture.Root, "lease", name, "lease.json")), &g); err != nil {
		t.Fatal(err)
	}
	return muster.Seed{ID: g.ID, State: g.State, Lane: g.Lane, Found: g.Found, Results: g.Results}
}

// record is the part of a saved staged record these tests use.
type record struct {
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
}

func recordOf(t *testing.T, name string) record {
	t.Helper()
	var r record
	if err := json.Unmarshal(fixture.Read(t, filepath.Join(fixture.Root, "lease", name, "want.record.json")), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMemoryOffersUrgentThenNormalThenBackfill(t *testing.T) {
	m, _, _ := newMemory(t)
	for _, s := range []muster.Seed{
		{ID: "PA-0001", State: "building", Lane: "backfill"},
		{ID: "PA-0002", State: "building", Lane: "normal"},
		{ID: "PA-0003", State: "building", Lane: "urgent"},
		{ID: "PA-0004", State: "building", Lane: "normal"},
		{ID: "PA-0005", State: "building", Lane: "urgent"},
	} {
		m.Seed(s)
	}
	var got []string
	for {
		g, err := m.Lease(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if g == nil {
			break
		}
		if g.Kind != "item" || g.Attempt != 1 || g.Expires != "2026-01-05T09:01:00Z" {
			t.Errorf("grant = %+v", g)
		}
		got = append(got, g.Lease)
	}
	if want := "PA-0003.3.1 PA-0005.5.1 PA-0002.2.1 PA-0004.4.1 PA-0001.1.1"; strings.Join(got, " ") != want {
		t.Errorf("leased %v, want %s", got, want)
	}
}

func TestMemoryCompleteKeepsTheResultAndAnswersRepeats(t *testing.T) {
	m, hub, _ := newMemory(t)
	m.Seed(seedOf(t, "complete-publish"))
	want := recordOf(t, "complete-publish")
	g, _ := m.Lease(ctx)

	m.DropAcks(1)
	if err := m.Complete(ctx, g.Lease, "live", want.Result); err != muster.ErrUnavailable {
		t.Fatalf("first complete: %v, want the acknowledgement lost", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Complete(ctx, g.Lease, "live", want.Result); err != nil {
			t.Fatalf("repeat %d: %v", i+1, err)
		}
	}
	if n := len(hub.Commits()); n != 1 {
		t.Errorf("%d commits to the hub repository, want one", n)
	}
	if o := m.Outcomes()[0]; o.Status != muster.StatusCompleted || o.State != "live" {
		t.Errorf("outcome = %+v", o)
	}
	var kept, sent any
	_ = json.Unmarshal(m.Result("PA-0101", "chartroom"), &kept)
	_ = json.Unmarshal(want.Result, &sent)
	if kept == nil || !equalJSON(kept, sent) {
		t.Errorf("the result kept for chartroom is not the one sent")
	}
	// The same call with another state is not a repeat.
	if err := m.Complete(ctx, g.Lease, "retired", want.Result); err != muster.ErrLeaseNotHeld {
		t.Errorf("another call under an ended lease: %v", err)
	}
	if again, _ := m.Lease(ctx); again != nil {
		t.Errorf("a completed step was offered again")
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// TestMemoryAllowsOnlyChartroomsRows pins the stand-in to Chartroom's four
// rows of the contract's table of queue steps.
func TestMemoryAllowsOnlyChartroomsRows(t *testing.T) {
	allowed := map[[2]string]bool{
		{"building", "live"}: true, {"linked", "linked"}: true, {"merged", "merged"}: true, {"retired", "retired"}: true,
	}
	states := []string{"scored", "building", "live", "linked", "merged", "retired", "parked", "observed"}
	for _, from := range states {
		for _, to := range states {
			m, _, _ := newMemory(t)
			m.Seed(muster.Seed{ID: "PA-0001", State: from, Lane: "normal"})
			g, _ := m.Lease(ctx)
			err := m.Complete(ctx, g.Lease, to, nil)
			if allowed[[2]string{from, to}] {
				if err != nil {
					t.Errorf("%s to %s: %v", from, to, err)
				}
			} else if err != muster.ErrMoveNotAllowed {
				t.Errorf("%s to %s: %v, want the move refused", from, to, err)
			}
		}
	}
}

func TestMemoryRefusesAResultThatIsNotAnObject(t *testing.T) {
	for _, result := range []string{`[1, 2]`, `"text"`, `7`, `{"open": `} {
		m, _, _ := newMemory(t)
		m.Seed(muster.Seed{ID: "PA-0001", State: "building", Lane: "normal"})
		g, _ := m.Lease(ctx)
		if err := m.Complete(ctx, g.Lease, "live", json.RawMessage(result)); err != muster.ErrRefused {
			t.Errorf("result %s: %v", result, err)
		}
	}
	m, _, _ := newMemory(t)
	m.Seed(muster.Seed{ID: "PA-0001", State: "building", Lane: "normal"})
	g, _ := m.Lease(ctx)
	if err := m.Complete(ctx, g.Lease, "live", json.RawMessage(`null`)); err != nil {
		t.Errorf("a null result is no result: %v", err)
	}
}

// TestMemoryLateLeaseCountsUntilTakenOver follows the contract: when a
// lease runs out the step is offered again, and until someone takes it a
// late result is still kept.
func TestMemoryLateLeaseCountsUntilTakenOver(t *testing.T) {
	m, _, c := newMemory(t)
	m.Seed(muster.Seed{ID: "PA-0001", State: "building", Lane: "normal"})
	first, _ := m.Lease(ctx)

	c.t = c.t.Add(45 * time.Second)
	if err := m.Heartbeat(ctx, first.Lease); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	c.t = c.t.Add(50 * time.Second)
	if g, _ := m.Lease(ctx); g != nil {
		t.Fatal("the step was offered again while its lease was kept")
	}
	c.t = c.t.Add(30 * time.Second) // the lease has now run out
	if err := m.Heartbeat(ctx, first.Lease); err != nil {
		t.Errorf("a late lease nobody took over was refused: %v", err)
	}

	c.t = c.t.Add(2 * time.Minute)
	second, _ := m.Lease(ctx)
	if second == nil || second.Lease == first.Lease || second.Attempt != 2 {
		t.Fatalf("the step was not offered again: %+v", second)
	}
	if err := m.Heartbeat(ctx, first.Lease); err != muster.ErrLeaseNotHeld {
		t.Errorf("heartbeat on a lease that was taken over: %v", err)
	}
	if err := m.Complete(ctx, first.Lease, "live", nil); err != muster.ErrLeaseNotHeld {
		t.Errorf("complete on a lease that was taken over: %v", err)
	}
	if err := m.Complete(ctx, second.Lease, "live", nil); err != nil {
		t.Errorf("complete on the new lease: %v", err)
	}
}

func TestMemoryFailAndNeedsSubtool(t *testing.T) {
	m, _, _ := newMemory(t)
	m.Seed(muster.Seed{ID: "PA-0001", State: "building", Lane: "normal"})
	m.Seed(muster.Seed{ID: "PA-0002", State: "building", Lane: "normal"})
	m.Seed(muster.Seed{ID: "PA-0003", State: "approved", Lane: "normal"})

	m.SetDown(true)
	if _, err := m.Lease(ctx); err != muster.ErrUnavailable {
		t.Errorf("lease while down: %v", err)
	}
	m.SetDown(false)

	parse, _ := m.Lease(ctx)
	m.StallResults(1)
	if err := m.Fail(ctx, parse.Lease, "E_PARSE"); err != muster.ErrUnavailable {
		t.Errorf("stalled fail: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Fail(ctx, parse.Lease, "E_PARSE"); err != nil {
			t.Errorf("fail E_PARSE, call %d: %v", i+1, err)
		}
	}

	timeout, _ := m.Lease(ctx)
	if err := m.Fail(ctx, timeout.Lease, "E_NEEDS_SUBTOOL"); err != muster.ErrRefused {
		t.Errorf("fail with E_NEEDS_SUBTOOL: %v, want it refused", err)
	}
	if err := m.Fail(ctx, timeout.Lease, "E_OTHER"); err != muster.ErrRefused {
		t.Errorf("fail with an unknown code: %v", err)
	}
	if err := m.Fail(ctx, timeout.Lease, "E_TIMEOUT"); err != nil {
		t.Errorf("fail E_TIMEOUT: %v", err)
	}
	again, _ := m.Lease(ctx)
	if again == nil || again.ID != "PA-0002" || again.Attempt != 2 {
		t.Fatalf("after E_TIMEOUT the step should be offered again, in its place: %+v", again)
	}
	// Chartroom holds no item in a state Muster lets it pause from.
	if err := m.NeedsSubtool(ctx, again.Lease); err != muster.ErrMoveNotAllowed {
		t.Errorf("needs_subtool from building: %v, want the move refused", err)
	}
	if err := m.Complete(ctx, again.Lease, "live", nil); err != nil {
		t.Errorf("complete after a refused pause: %v", err)
	}

	approved, _ := m.Lease(ctx)
	if err := m.NeedsSubtool(ctx, approved.Lease); err != nil {
		t.Errorf("needs_subtool from approved: %v", err)
	}
	if err := m.NeedsSubtool(ctx, "PA-0009.9.1"); err != muster.ErrLeaseNotHeld {
		t.Errorf("unknown lease: %v", err)
	}
	if g, _ := m.Lease(ctx); g != nil {
		t.Errorf("a stopped or paused item was offered again: %+v", g)
	}

	status := map[string]muster.Outcome{}
	for _, o := range m.Outcomes() {
		status[o.ID] = o
	}
	if o := status["PA-0001"]; o.Status != muster.StatusStopped || o.State != "building" || o.Code != "E_PARSE" {
		t.Errorf("stopped item = %+v", o)
	}
	if o := status["PA-0002"]; o.Status != muster.StatusCompleted || o.State != "live" {
		t.Errorf("completed item = %+v", o)
	}
	if o := status["PA-0003"]; o.Status != muster.StatusPaused || o.State != "needs_subtool" {
		t.Errorf("paused item = %+v", o)
	}
}

// --- the HTTP client ---

const token = "made-up-token-for-tests"

func tokenFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newClient(t *testing.T, url string, timeout time.Duration) *muster.HTTP {
	t.Helper()
	c, err := muster.NewHTTP(url, tokenFile(t, token), timeout)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// exactly fails the test unless err is the wanted fixed error and says
// nothing about where the call went or what came back.
func exactly(t *testing.T, name string, err error, want error) {
	t.Helper()
	if err != want {
		t.Errorf("%s: got %v, want exactly %v", name, err, want)
	}
	if err != nil {
		for _, s := range []string{"127.0.0.1", "localhost", "http", token, "dial", "tcp", "/v1/", "stack trace"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q contains %q", name, err, s)
			}
		}
	}
}

func TestHTTPRoundTripAgainstTheStandIn(t *testing.T) {
	m, hub, _ := newMemory(t)
	publish := seedOf(t, "complete-publish")
	publish.Lane = "urgent"
	m.Seed(publish)
	m.Seed(seedOf(t, "fail-missing-provenance-link"))
	m.Seed(muster.Seed{ID: "PA-0300", State: "scored", Lane: "backfill"})
	srv := httptest.NewServer(muster.Handler(m, token))
	defer srv.Close()
	c := newClient(t, srv.URL+"/", time.Second)

	first, err := c.Lease(ctx)
	if err != nil || first == nil {
		t.Fatalf("lease: %v, %+v", err, first)
	}
	if first.Lease != "PA-0101.1.1" || first.Kind != "item" || first.State != "building" || first.Lane != "urgent" ||
		first.Attempt != 1 || first.Expires != "2026-01-05T09:01:00Z" || len(first.Results) != len(publish.Results) {
		t.Errorf("first grant = %+v", first)
	}
	if err := c.Heartbeat(ctx, first.Lease); err != nil {
		t.Errorf("heartbeat: %v", err)
	}
	want := recordOf(t, "complete-publish")
	exactly(t, "a move that is not Chartroom's", c.Complete(ctx, first.Lease, "retired", want.Result), muster.ErrMoveNotAllowed)
	for i := 0; i < 2; i++ {
		if err := c.Complete(ctx, first.Lease, want.State, want.Result); err != nil {
			t.Errorf("complete, call %d: %v", i+1, err)
		}
	}
	exactly(t, "heartbeat on an ended lease", c.Heartbeat(ctx, first.Lease), muster.ErrLeaseNotHeld)

	second, _ := c.Lease(ctx)
	exactly(t, "fail with a code of its own call", c.Fail(ctx, second.Lease, "E_NEEDS_SUBTOOL"), muster.ErrRefused)
	if err := c.Fail(ctx, second.Lease, "E_PARSE"); err != nil {
		t.Errorf("fail: %v", err)
	}
	third, _ := c.Lease(ctx)
	exactly(t, "needs_subtool from a state that cannot pause", c.NeedsSubtool(ctx, third.Lease), muster.ErrMoveNotAllowed)
	exactly(t, "needs_subtool on an unknown lease", c.NeedsSubtool(ctx, "PA-0009.9.1"), muster.ErrLeaseNotHeld)
	if none, err := c.Lease(ctx); err != nil || none != nil {
		t.Errorf("lease with nothing waiting: %v, %+v", err, none)
	}

	raw, _ := hub.ReadFile(ctx, muster.CatalogPath)
	cat, err := catalog.Parse(raw, catalog.MaxBytes)
	if err != nil || len(cat.Entries) != 1 || cat.Entries[0].Slug != "sample-clause-finder" || cat.Entries[0].Status != "live" {
		t.Errorf("hub catalog after one publication: %v, %s", err, raw)
	}
}

func TestHTTPSendsTheTokenFreshEachCall(t *testing.T) {
	var gotAuth, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	path := tokenFile(t, token)
	c, _ := muster.NewHTTP(srv.URL, path, time.Second)
	if _, err := c.Lease(ctx); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token || gotType != "application/json" {
		t.Errorf("auth %q, type %q", gotAuth, gotType)
	}
	// The token is rotated, so it is read again for every call.
	if err := os.WriteFile(path, []byte("rotated-made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Lease(ctx)
	if gotAuth != "Bearer rotated-made-up-token" {
		t.Errorf("after rotation the call carried %q", gotAuth)
	}
}

func TestHTTPErrorsAreFixedAndSayNothing(t *testing.T) {
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}
	serve := func(h http.HandlerFunc) *muster.HTTP {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return newClient(t, srv.URL, 200*time.Millisecond)
	}
	lease := func(c *muster.HTTP) error {
		_, err := c.Lease(ctx)
		return err
	}

	// Answers that are not the contract's refusals are never trusted,
	// whatever their status says.
	c := serve(status(500, "stack trace naming host 127.0.0.1"))
	exactly(t, "500 that is not a refusal", lease(c), muster.ErrUnavailable)
	c = serve(status(409, `conflict`))
	exactly(t, "409 that is not a refusal", c.Complete(ctx, "l", "live", nil), muster.ErrUnavailable)
	c = serve(status(409, `{"error":"E_PARSE","cause":"lease_not_held","reason":"x","more":1}`))
	exactly(t, "409 with a field the contract does not define", c.Heartbeat(ctx, "l"), muster.ErrUnavailable)
	c = serve(status(409, `{"error":"E_OTHER","cause":"lease_not_held","reason":"x"}`))
	exactly(t, "409 with a code that is not one of the three", c.Heartbeat(ctx, "l"), muster.ErrUnavailable)
	c = serve(status(400, ``))
	exactly(t, "400 with no body", c.Fail(ctx, "l", "E_PARSE"), muster.ErrUnavailable)

	// Answers that claim success without being the contract's.
	c = serve(status(200, `{"leases": []}`))
	exactly(t, "a grant with a field the contract does not define", lease(c), muster.ErrBadResponse)
	c = serve(status(200, `{"lease": "PA-0001.1", "kind": "item"`))
	exactly(t, "a grant cut short", lease(c), muster.ErrBadResponse)
	c = serve(status(200, `{"kind": "item", "id": "PA-0001"}`))
	exactly(t, "a grant with no lease", lease(c), muster.ErrBadResponse)
	c = serve(status(200, `{"lease": "PA-0001.1"} {"lease": "PA-0002.2"}`))
	exactly(t, "two grants in one answer", lease(c), muster.ErrBadResponse)
	c = serve(status(200, `{"lease": "PA-0001.1", "found": "`+strings.Repeat("x", muster.MaxResponseBytes)+`"}`))
	exactly(t, "an oversized answer", lease(c), muster.ErrBadResponse)
	c = serve(status(201, `{}`))
	exactly(t, "a success the contract does not define", lease(c), muster.ErrBadResponse)
	c = serve(status(204, ``))
	exactly(t, "no content where an answer is due", c.Complete(ctx, "l", "live", nil), muster.ErrBadResponse)

	// A result too large for Muster to accept is not sent at all.
	var hits atomic.Int32
	c = serve(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) })
	big := json.RawMessage(`{"note":"` + strings.Repeat("x", muster.MaxRequestBytes) + `"}`)
	exactly(t, "a request over the size limit", c.Complete(ctx, "l", "live", big), muster.ErrRefused)
	if hits.Load() != 0 {
		t.Error("an oversized request was sent")
	}

	// Too slow.
	release := make(chan struct{})
	c = serve(func(w http.ResponseWriter, _ *http.Request) { <-release })
	exactly(t, "timeout", lease(c), muster.ErrUnavailable)
	close(release)

	// Nothing listening.
	gone := httptest.NewServer(status(204, ""))
	url := gone.URL
	gone.Close()
	exactly(t, "connection refused", lease(newClient(t, url, 200*time.Millisecond)), muster.ErrUnavailable)

	// No token to send.
	srv := httptest.NewServer(status(204, ""))
	defer srv.Close()
	noToken, _ := muster.NewHTTP(srv.URL, filepath.Join(t.TempDir(), "absent"), time.Second)
	exactly(t, "missing token file", lease(noToken), muster.ErrUnavailable)
	emptyToken, _ := muster.NewHTTP(srv.URL, tokenFile(t, ""), time.Second)
	exactly(t, "empty token file", lease(emptyToken), muster.ErrUnavailable)
}

// TestHTTPDoesNotFollowRedirects checks the token is never carried to a
// host other than the one configured.
func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL, time.Second).Lease(ctx)
	exactly(t, "redirect", err, muster.ErrUnavailable)
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestHTTPStandInRequiresTheToken(t *testing.T) {
	m, _, _ := newMemory(t)
	srv := httptest.NewServer(muster.Handler(m, token))
	defer srv.Close()
	wrong, _ := muster.NewHTTP(srv.URL, tokenFile(t, "some-other-made-up-token"), time.Second)
	_, err := wrong.Lease(ctx)
	exactly(t, "wrong token", err, muster.ErrRefused)
	if len(m.Calls()) != 0 {
		t.Error("a call with the wrong token reached the stand-in")
	}
}

func TestNewHTTPRefusesBadAddresses(t *testing.T) {
	for _, addr := range []string{"", "muster", "ftp://muster.example", "http://", "http://someone:word@muster.example", "://x"} {
		_, err := muster.NewHTTP(addr, "unused", time.Second)
		if err != muster.ErrBadAddress {
			t.Errorf("NewHTTP(%q): %v", addr, err)
		}
		if err != nil && addr != "" && strings.Contains(err.Error(), addr) {
			t.Errorf("the error repeats the address %q", addr)
		}
	}
}

func TestLaneRank(t *testing.T) {
	if !(muster.LaneRank("urgent") < muster.LaneRank("normal") &&
		muster.LaneRank("normal") < muster.LaneRank("backfill") &&
		muster.LaneRank("backfill") < muster.LaneRank("anything else")) {
		t.Error("lanes are not ordered urgent, normal, backfill")
	}
}

// drain reads a request body for a test server.
func drain(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	return body
}
