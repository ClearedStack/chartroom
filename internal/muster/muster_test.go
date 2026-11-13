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
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/fixture"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/plainforge"
)

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMemory(t *testing.T, fixtureName string) (*muster.Memory, *plainforge.Memory, *clock) {
	t.Helper()
	hub := plainforge.NewMemory()
	dir := filepath.Join(fixture.Root, "apply", fixtureName)
	_ = hub.WriteFile(ctx, muster.CatalogPath, fixture.Read(t, filepath.Join(dir, "catalog.json")), "seed")
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	return muster.NewMemory(hub, c.now, time.Minute), hub, c
}

func item(t *testing.T, fixtureName string) []byte {
	t.Helper()
	return fixture.Read(t, filepath.Join(fixture.Root, "apply", fixtureName, "decision.json"))
}

func change(t *testing.T, fixtureName string) []byte {
	t.Helper()
	return fixture.Read(t, filepath.Join(fixture.Root, "apply", fixtureName, "want.change.json"))
}

func TestMemoryLeasesByPriorityThenAge(t *testing.T) {
	m, _, _ := newMemory(t, "ok-publish")
	for _, s := range []struct{ id, priority string }{
		{"b1", "backfill"}, {"n1", "normal"}, {"u1", "urgent"}, {"x1", "whenever"}, {"n2", "normal"}, {"u2", "urgent"},
	} {
		m.Seed(s.id, s.priority, []byte(`{}`))
	}
	var got []string
	for {
		leases, err := m.Lease(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(leases) == 0 {
			break
		}
		for _, l := range leases {
			got = append(got, l.Priority)
		}
	}
	if want := "urgent urgent normal normal backfill whenever"; strings.Join(got, " ") != want {
		t.Errorf("leased in order %v, want %s", got, want)
	}
}

func TestMemoryLeaseContextIsTheTouchedEntries(t *testing.T) {
	m, _, _ := newMemory(t, "ok-merge")
	m.Seed("itm-0103", "normal", item(t, "ok-merge"))
	leases, _ := m.Lease(ctx, 1)
	cat, err := catalog.Parse(leases[0].Catalog, catalog.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].Slug != "sample-ledger-check" {
		t.Errorf("context = %+v, want only the merge parent", cat.Entries)
	}
}

func TestMemoryCompleteCommitsOnceAndAcknowledgesRepeats(t *testing.T) {
	m, hub, _ := newMemory(t, "ok-publish")
	m.Seed("itm-0101", "normal", item(t, "ok-publish"))
	leases, _ := m.Lease(ctx, 1)
	ch := change(t, "ok-publish")

	m.DropAcks(1)
	if err := m.Complete(ctx, leases[0].LeaseID, ch); err != muster.ErrUnavailable {
		t.Fatalf("first complete: %v, want the acknowledgement lost", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Complete(ctx, leases[0].LeaseID, ch); err != nil {
			t.Fatalf("repeat %d: %v", i+1, err)
		}
	}
	if n := len(hub.Commits()); n != 2 {
		t.Errorf("%d commits, want the seed and one publish", n)
	}
	got, _ := hub.ReadFile(ctx, muster.CatalogPath)
	fixture.Golden(t, filepath.Join(fixture.Root, "apply", "ok-publish", "want.catalog.json"), got)
	if o := m.Outcomes()[0]; o.Status != muster.StatusCompleted || o.State != "live" {
		t.Errorf("outcome = %+v", o)
	}
	// A different result for a settled lease is refused.
	if err := m.Complete(ctx, leases[0].LeaseID, change(t, "ok-link")); err != muster.ErrLeaseGone {
		t.Errorf("different result for a settled lease: %v", err)
	}
}

func TestMemoryRefusesChangesOutsideTheSpecification(t *testing.T) {
	alter := func(fn func(ch *decision.Change)) []byte {
		var ch decision.Change
		if err := json.Unmarshal(change(t, "ok-publish"), &ch); err != nil {
			t.Fatal(err)
		}
		fn(&ch)
		return ch.Encode()
	}
	cases := map[string][]byte{
		"not a change":              []byte(`{"slug": "sample-clause-finder"}`),
		"unknown action":            alter(func(ch *decision.Change) { ch.Action = "feature" }),
		"transition not in table":   alter(func(ch *decision.Change) { ch.From = "scored" }),
		"target state not in table": alter(func(ch *decision.Change) { ch.To = "linked" }),
		"entry status differs":      alter(func(ch *decision.Change) { ch.After.Status = "retired" }),
		"entry for another slug":    alter(func(ch *decision.Change) { ch.After.Slug = "sample-other-tool" }),
		"before does not match hub": alter(func(ch *decision.Change) { ch.Before = ch.After.Clone() }),
		"unchanged but not in hub":  alter(func(ch *decision.Change) { ch.Changed = false }),
		"other contract version":    alter(func(ch *decision.Change) { ch.ContractVersion = "v2" }),
	}
	for name, payload := range cases {
		m, hub, _ := newMemory(t, "ok-publish")
		m.Seed("itm-0101", "normal", item(t, "ok-publish"))
		leases, _ := m.Lease(ctx, 1)
		if err := m.Complete(ctx, leases[0].LeaseID, payload); err != muster.ErrLeaseGone {
			t.Errorf("%s: got %v, want the change refused", name, err)
		}
		if len(hub.Commits()) != 1 {
			t.Errorf("%s: the hub repository was written", name)
		}
		if o := m.Outcomes()[0]; o.Status != muster.StatusRejected {
			t.Errorf("%s: outcome = %+v", name, o)
		}
	}
}

func TestMemoryLeaseLapsesWithoutHeartbeat(t *testing.T) {
	m, _, c := newMemory(t, "ok-publish")
	m.Seed("itm-0101", "normal", item(t, "ok-publish"))
	first, _ := m.Lease(ctx, 1)

	c.t = c.t.Add(50 * time.Second)
	if err := m.Heartbeat(ctx, first[0].LeaseID); err != nil {
		t.Fatalf("heartbeat inside the lease: %v", err)
	}
	c.t = c.t.Add(50 * time.Second)
	if none, _ := m.Lease(ctx, 1); len(none) != 0 {
		t.Fatal("the item was leased twice while the first lease was alive")
	}
	c.t = c.t.Add(11 * time.Second)
	if err := m.Heartbeat(ctx, first[0].LeaseID); err != muster.ErrLeaseGone {
		t.Errorf("heartbeat after the lease lapsed: %v", err)
	}
	if err := m.Complete(ctx, first[0].LeaseID, change(t, "ok-publish")); err != muster.ErrLeaseGone {
		t.Errorf("complete after the lease lapsed: %v", err)
	}
	second, _ := m.Lease(ctx, 1)
	if len(second) != 1 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("the item was not leased again: %+v", second)
	}
	if err := m.Complete(ctx, second[0].LeaseID, change(t, "ok-publish")); err != nil {
		t.Errorf("complete on the new lease: %v", err)
	}
}

func TestMemoryReportsAndFaults(t *testing.T) {
	m, _, _ := newMemory(t, "ok-publish")
	m.Seed("a", "normal", []byte(`{}`))
	m.Seed("b", "normal", []byte(`{}`))
	m.SetDown(true)
	if _, err := m.Lease(ctx, 2); err != muster.ErrUnavailable {
		t.Errorf("lease while down: %v", err)
	}
	m.SetDown(false)
	leases, _ := m.Lease(ctx, 2)
	parse := muster.Report{Code: "E_PARSE", Reason: "malformed_json", Message: "m"}
	pause := muster.Report{Code: "E_NEEDS_SUBTOOL", Reason: "unsupported_entry_type", Field: "action", Message: "m"}

	m.StallResults(1)
	if err := m.Fail(ctx, leases[0].LeaseID, parse); err != muster.ErrUnavailable {
		t.Errorf("stalled fail: %v", err)
	}
	if err := m.Fail(ctx, leases[0].LeaseID, parse); err != nil {
		t.Errorf("fail: %v", err)
	}
	if err := m.Fail(ctx, leases[0].LeaseID, parse); err != nil {
		t.Errorf("repeated fail: %v", err)
	}
	if err := m.NeedsSubtool(ctx, leases[1].LeaseID, pause); err != nil {
		t.Errorf("needs_subtool: %v", err)
	}
	if err := m.NeedsSubtool(ctx, "lease-9999", pause); err != muster.ErrLeaseGone {
		t.Errorf("unknown lease: %v", err)
	}
	out := m.Outcomes()
	if out[0].Status != muster.StatusFailed || out[0].Code != "E_PARSE" || out[0].State != "" {
		t.Errorf("failed item = %+v", out[0])
	}
	if out[1].Status != muster.StatusNeedsSubtool || out[1].State != "needs_subtool" || out[1].Field != "action" {
		t.Errorf("paused item = %+v", out[1])
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

// leak fails the test if an error says anything beyond the three fixed
// errors of the package.
func leak(t *testing.T, name string, err error, want error) {
	t.Helper()
	if err != want {
		t.Errorf("%s: got %v, want exactly %v", name, err, want)
	}
	if err != nil {
		for _, s := range []string{"127.0.0.1", "localhost", "http", token, "dial", "tcp", "/v1/"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q contains %q", name, err, s)
			}
		}
	}
}

func TestHTTPRoundTripAgainstTheStandIn(t *testing.T) {
	m, hub, _ := newMemory(t, "ok-publish")
	m.Seed("itm-0101", "urgent", item(t, "ok-publish"))
	m.Seed("broken", "normal", []byte(`{"contract_version": "v1"`))
	srv := httptest.NewServer(muster.Handler(m, token))
	defer srv.Close()
	c := newClient(t, srv.URL+"/", time.Second)

	leases, err := c.Lease(ctx, 4)
	if err != nil || len(leases) != 2 {
		t.Fatalf("lease: %v, %d leases", err, len(leases))
	}
	if leases[0].Priority != "urgent" || !json.Valid(leases[0].Item) || !json.Valid(leases[0].Catalog) {
		t.Errorf("first lease = %+v", leases[0])
	}
	if _, err := decision.Parse(leases[1].Item); err == nil {
		t.Error("the broken item arrived as a usable item")
	}
	if err := c.Heartbeat(ctx, leases[0].LeaseID); err != nil {
		t.Errorf("heartbeat: %v", err)
	}
	if err := c.Complete(ctx, leases[0].LeaseID, change(t, "ok-publish")); err != nil {
		t.Errorf("complete: %v", err)
	}
	if err := c.Complete(ctx, leases[0].LeaseID, change(t, "ok-publish")); err != nil {
		t.Errorf("repeated complete: %v", err)
	}
	if err := c.Fail(ctx, leases[1].LeaseID, muster.Report{Code: "E_PARSE", Reason: "malformed_json", Message: "m"}); err != nil {
		t.Errorf("fail: %v", err)
	}
	leak(t, "heartbeat on a settled lease", c.Heartbeat(ctx, leases[0].LeaseID), muster.ErrLeaseGone)
	leak(t, "needs_subtool on an unknown lease", c.NeedsSubtool(ctx, "lease-9999", muster.Report{}), muster.ErrLeaseGone)
	if again, err := c.Lease(ctx, 4); err != nil || len(again) != 0 {
		t.Errorf("second lease: %v, %d leases", err, len(again))
	}
	got, _ := hub.ReadFile(ctx, muster.CatalogPath)
	fixture.Golden(t, filepath.Join(fixture.Root, "apply", "ok-publish", "want.catalog.json"), got)
}

func TestHTTPRequestShape(t *testing.T) {
	var gotAuth, gotType, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAuth, gotType, gotPath, gotBody = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, string(body)
		_, _ = w.Write([]byte(`{"leases": []}`))
	}))
	defer srv.Close()
	path := tokenFile(t, token)
	c, _ := muster.NewHTTP(srv.URL, path, time.Second)
	if _, err := c.Lease(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token || gotType != "application/json" || gotPath != "/v1/lease" {
		t.Errorf("auth %q, type %q, path %q", gotAuth, gotType, gotPath)
	}
	if gotBody != `{"worker":"chartroom","contract_version":"v1","max":3}` {
		t.Errorf("body = %s", gotBody)
	}
	// The token is read again for every call, because it is rotated.
	if err := os.WriteFile(path, []byte("rotated-made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Lease(ctx, 1)
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

	c := serve(status(500, "stack trace naming host 127.0.0.1"))
	_, err := c.Lease(ctx, 1)
	leak(t, "500 on lease", err, muster.ErrUnavailable)
	leak(t, "500 on complete", c.Complete(ctx, "l", json.RawMessage(`{}`)), muster.ErrUnavailable)

	c = serve(status(401, ""))
	leak(t, "401", c.Heartbeat(ctx, "l"), muster.ErrUnavailable)
	c = serve(status(429, ""))
	leak(t, "429", c.Fail(ctx, "l", muster.Report{}), muster.ErrUnavailable)
	c = serve(status(400, ""))
	leak(t, "400", c.NeedsSubtool(ctx, "l", muster.Report{}), muster.ErrUnavailable)
	for _, code := range []int{404, 409, 410} {
		c = serve(status(code, ""))
		leak(t, "lease gone", c.Complete(ctx, "l", json.RawMessage(`{}`)), muster.ErrLeaseGone)
		_, err = c.Lease(ctx, 1)
		leak(t, "lease gone on lease", err, muster.ErrUnavailable)
	}

	c = serve(status(200, `{"leases": [`))
	_, err = c.Lease(ctx, 1)
	leak(t, "unreadable answer", err, muster.ErrBadResponse)
	c = serve(status(200, `{"leases": [{"lease_id":"a"},{"lease_id":"b"}]}`))
	_, err = c.Lease(ctx, 1)
	leak(t, "more leases than asked for", err, muster.ErrBadResponse)
	c = serve(status(200, `{"leases": ["`+strings.Repeat("x", muster.MaxResponseBytes)+`"]}`))
	_, err = c.Lease(ctx, 1)
	leak(t, "oversized answer", err, muster.ErrBadResponse)
	c = serve(status(204, ""))
	if leases, err := c.Lease(ctx, 1); err != nil || len(leases) != 0 {
		t.Errorf("204 on lease: %v, %d leases", err, len(leases))
	}

	// Too slow.
	release := make(chan struct{})
	c = serve(func(w http.ResponseWriter, _ *http.Request) { <-release })
	_, err = c.Lease(ctx, 1)
	leak(t, "timeout", err, muster.ErrUnavailable)
	close(release)

	// Nothing listening.
	gone := httptest.NewServer(status(200, ""))
	url := gone.URL
	gone.Close()
	_, err = newClient(t, url, 200*time.Millisecond).Lease(ctx, 1)
	leak(t, "connection refused", err, muster.ErrUnavailable)

	// No token to send.
	srv := httptest.NewServer(status(200, `{"leases": []}`))
	defer srv.Close()
	noToken, _ := muster.NewHTTP(srv.URL, filepath.Join(t.TempDir(), "absent"), time.Second)
	_, err = noToken.Lease(ctx, 1)
	leak(t, "missing token file", err, muster.ErrUnavailable)
	emptyToken, _ := muster.NewHTTP(srv.URL, tokenFile(t, ""), time.Second)
	_, err = emptyToken.Lease(ctx, 1)
	leak(t, "empty token file", err, muster.ErrUnavailable)
}

// TestHTTPDoesNotFollowRedirects checks the token is never carried to a
// host other than the one configured.
func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"leases": []}`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL, time.Second).Lease(ctx, 1)
	leak(t, "redirect", err, muster.ErrUnavailable)
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestHTTPStandInRequiresTheToken(t *testing.T) {
	m, _, _ := newMemory(t, "ok-publish")
	srv := httptest.NewServer(muster.Handler(m, token))
	defer srv.Close()
	wrong, _ := muster.NewHTTP(srv.URL, tokenFile(t, "some-other-made-up-token"), time.Second)
	_, err := wrong.Lease(ctx, 1)
	leak(t, "wrong token", err, muster.ErrUnavailable)
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

func TestPriorityRank(t *testing.T) {
	if !(muster.PriorityRank("urgent") < muster.PriorityRank("normal") &&
		muster.PriorityRank("normal") < muster.PriorityRank("backfill") &&
		muster.PriorityRank("backfill") < muster.PriorityRank("anything else")) {
		t.Error("priorities are not ordered urgent, normal, backfill")
	}
}
