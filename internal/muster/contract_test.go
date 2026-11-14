package muster_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/fixture"
	"github.com/ClearedStack/chartroom/internal/muster"
)

// example is one of Muster's saved contract examples. Its format is
// described beside the originals; see testdata/contract/SOURCE.md.
type example struct {
	About string `json:"about"`
	Given *struct {
		Items     json.RawMessage `json:"items"`
		Calls     json.RawMessage `json:"calls"`
		Wait      string          `json:"wait"`
		Condition string          `json:"condition"`
	} `json:"given"`
	Request struct {
		As          string          `json:"as"`
		Method      string          `json:"method"`
		Path        string          `json:"path"`
		Body        json.RawMessage `json:"body"`
		Raw         *string         `json:"raw"`
		ContentType string          `json:"content_type"`
	} `json:"request"`
	Expect struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Stream json.RawMessage `json:"stream"`
	} `json:"expect"`
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return buf.String()
}

func loadExamples(t *testing.T) map[string]example {
	t.Helper()
	dir := filepath.Join(fixture.Root, "contract")
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no contract examples under %s", dir)
	}
	out := make(map[string]example, len(names))
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var ex example
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ex); err != nil {
			t.Fatalf("%s: the example has a shape this test does not know: %v", filepath.Base(name), err)
		}
		out[filepath.Base(name)] = ex
	}
	return out
}

// workerCalls are the five calls a worker makes.
var workerCalls = map[string]bool{
	muster.PathLease: true, muster.PathHeartbeat: true, muster.PathComplete: true,
	muster.PathFail: true, muster.PathNeedsSubtool: true,
}

// notChartrooms are the calls in the contract that Chartroom never makes.
// A path in an example that is in neither list fails the test, so a new
// call cannot go unnoticed.
var notChartrooms = map[string]bool{
	"/v1/items": true, "/v1/edits": true, "/v1/feed": true, "/v1/health": true,
	"/v1/release": true, "/healthz": true, "/v1/made-up": true,
}

// meaning says how Chartroom must read each cause in the contract's table
// of refusals. It is written out here, apart from the client, so the two
// have to agree.
var meaning = map[string]error{
	"lease_not_held":   muster.ErrLeaseNotHeld,
	"move_not_allowed": muster.ErrMoveNotAllowed,
	"unknown_contract": muster.ErrUnknownContract,

	// Nothing to gain by sending the same call again.
	"not_json": muster.ErrRefused, "unknown_field": muster.ErrRefused, "missing_field": muster.ErrRefused,
	"bad_field": muster.ErrRefused, "no_credential": muster.ErrRefused, "not_your_call": muster.ErrRefused,
	"no_such_call": muster.ErrRefused, "wrong_method": muster.ErrRefused, "too_large": muster.ErrRefused,
	"wrong_type": muster.ErrRefused, "duplicate": muster.ErrRefused, "unknown_value": muster.ErrRefused,

	// Worth trying again later.
	"rate_limited": muster.ErrUnavailable, "store_slow": muster.ErrUnavailable, "busy": muster.ErrUnavailable,
	"bad_record": muster.ErrUnavailable, "unexpected": muster.ErrUnavailable,
}

// call makes the client call that an example's request stands for, with
// the arguments taken from the example's body. ok is false when the body
// is one a correct client never sends.
func call(c *muster.HTTP, path string, body json.RawMessage) (grant *muster.Grant, err error, ok bool) {
	var b struct {
		Contract string          `json:"contract"`
		Lease    string          `json:"lease"`
		State    string          `json:"state"`
		Result   json.RawMessage `json:"result"`
		Code     string          `json:"code"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if len(body) == 0 || dec.Decode(&b) != nil || b.Contract != "v1" {
		return nil, nil, false
	}
	has := func(fields ...string) bool {
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(body, &keys)
		if len(keys) != len(fields) {
			return false
		}
		for _, f := range fields {
			if _, ok := keys[f]; !ok {
				return false
			}
		}
		return true
	}
	switch path {
	case muster.PathLease:
		if !has("contract") {
			return nil, nil, false
		}
		grant, err = c.Lease(ctx)
	case muster.PathHeartbeat:
		if !has("contract", "lease") || b.Lease == "" {
			return nil, nil, false
		}
		err = c.Heartbeat(ctx, b.Lease)
	case muster.PathNeedsSubtool:
		if !has("contract", "lease") || b.Lease == "" {
			return nil, nil, false
		}
		err = c.NeedsSubtool(ctx, b.Lease)
	case muster.PathFail:
		if !has("contract", "lease", "code") || b.Lease == "" || (b.Code != "E_TIMEOUT" && b.Code != "E_PARSE") {
			return nil, nil, false
		}
		err = c.Fail(ctx, b.Lease, b.Code)
	case muster.PathComplete:
		withResult := has("contract", "lease", "state", "result")
		if !(withResult || has("contract", "lease", "state")) || b.Lease == "" || b.State == "" {
			return nil, nil, false
		}
		if withResult && !bytes.HasPrefix(bytes.TrimSpace(b.Result), []byte("{")) {
			return nil, nil, false
		}
		err = c.Complete(ctx, b.Lease, b.State, b.Result)
	default:
		return nil, nil, false
	}
	return grant, err, true
}

// TestContractExamples replays Muster's saved examples from Chartroom's
// side of the contract.
//
// For every example of a worker call, Chartroom's client is made to send
// the same call, and the body it sends must equal the example's, field for
// field and in the same order. The example's answer is then served back,
// byte for byte, and the client must read it as the contract means it.
// Every refusal in the contract is served to the client too, whichever
// call the example happened to use.
func TestContractExamples(t *testing.T) {
	examples := loadExamples(t)
	if len(examples) != 52 {
		t.Fatalf("loaded %d contract examples, want all 52 from contract/v1/examples", len(examples))
	}
	names := make([]string, 0, len(examples))
	for name := range examples {
		names = append(names, name)
	}
	sort.Strings(names)

	replayed := map[string]int{}
	causes := map[string]bool{}
	checked := 0
	for _, name := range names {
		ex := examples[name]
		path := ex.Request.Path
		if !workerCalls[path] && !notChartrooms[path] {
			t.Errorf("%s: %s is a call this test does not know", name, path)
			continue
		}
		refusal := ex.Expect.Status >= 400
		t.Run(name, func(t *testing.T) {
			checked++
			if !workerCalls[path] && !refusal {
				// Non-worker calls (/v1/items, /v1/edits, /v1/feed, /v1/health,
				// /v1/release, /healthz) must never be sendable by Chartroom's
				// client, and if sent with a worker token to the stand-in (other
				// than public GET /healthz) must be refused as not_your_call.
				c := newClient(t, "http://127.0.0.1:1", time.Millisecond)
				if _, _, sendable := call(c, path, ex.Request.Body); sendable {
					t.Fatalf("non-worker call %s was accepted as sendable by Chartroom's client", path)
				}
				if path != "/healthz" {
					m, _, _ := newMemory(t)
					srv := httptest.NewServer(muster.Handler(m, token))
					defer srv.Close()
					req, err := http.NewRequest(ex.Request.Method, srv.URL+path, bytes.NewReader([]byte(compact(t, ex.Request.Body))))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+token)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					got := drainResponse(resp)
					if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(got), `"cause":"not_your_call"`) {
						t.Errorf("worker calling %s got %d %s, want 403 not_your_call", path, resp.StatusCode, got)
					}
				}
				return
			}

			var sent []byte
			var sentType, sentAuth, sentPath, sentMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent, sentType, sentAuth = drain(r), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
				sentPath, sentMethod = r.URL.Path, r.Method
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ex.Expect.Status)
				_, _ = w.Write([]byte(compact(t, ex.Expect.Body) + "\n"))
			}))
			defer srv.Close()
			c := newClient(t, srv.URL, time.Second)

			// The request. Where the example's own request is one a
			// correct client never sends, any call will do to receive
			// the refusal.
			grant, err, sendable := call(c, path, ex.Request.Body)
			if !workerCalls[path] || ex.Request.Method != http.MethodPost || ex.Request.Raw != nil || ex.Request.ContentType != "" {
				sendable = false
			}
			if sendable {
				replayed[path]++
				if got, want := string(sent), compact(t, ex.Request.Body); got != want {
					t.Errorf("request body\n  sent %s\n  want %s", got, want)
				}
				if sentPath != path || sentMethod != http.MethodPost || sentType != "application/json" || sentAuth != "Bearer "+token {
					t.Errorf("request was %s %s, type %q, auth %q", sentMethod, sentPath, sentType, sentAuth)
				}
			} else {
				if !refusal {
					t.Fatalf("the example is answered %d, but its request is one Chartroom's client cannot send", ex.Expect.Status)
				}
				grant, err = c.Lease(ctx)
			}

			// The answer.
			switch {
			case refusal:
				var body struct{ Error, Cause, Reason string }
				if json.Unmarshal(ex.Expect.Body, &body) != nil || body.Cause == "" {
					t.Fatalf("the refusal has no cause: %s", ex.Expect.Body)
				}
				causes[body.Cause] = true
				want, known := meaning[body.Cause]
				if !known {
					t.Fatalf("cause %q is new: decide how Chartroom reads it and add it to this test", body.Cause)
				}
				exactly(t, "refusal "+body.Cause, err, want)
				if grant != nil {
					t.Error("a refusal was read as a grant")
				}
			case path == muster.PathLease && ex.Expect.Status == http.StatusNoContent:
				if err != nil || grant != nil {
					t.Errorf("nothing to lease was read as %+v, %v", grant, err)
				}
			case path == muster.PathLease:
				if err != nil || grant == nil {
					t.Fatalf("grant not read: %v", err)
				}
				// Reading the grant and writing it back gives the saved
				// body, so no field was dropped, renamed or reordered.
				back, _ := json.Marshal(grant)
				if got, want := string(back), compact(t, ex.Expect.Body); got != want {
					t.Errorf("grant\n  read as %s\n  saved as %s", got, want)
				}
			default:
				if err != nil {
					t.Errorf("an acknowledgement was read as %v", err)
				}
			}
		})
	}

	if checked != len(examples) {
		t.Errorf("checked %d of %d examples", checked, len(examples))
	}
	for path := range workerCalls {
		if replayed[path] == 0 {
			t.Errorf("no example of %s was replayed as a request", path)
		}
	}
	for cause := range meaning {
		if !causes[cause] {
			t.Errorf("the contract's examples no longer show cause %q", cause)
		}
	}
}

// TestStandInAnswersLikeTheContract sends the examples that need nothing
// set up first, exactly as saved, to Chartroom's in-memory stand-in for
// Muster, and checks the status and the body it gives against the saved
// ones. The stand-in knows one caller, so an example made as a caller that
// is not a worker is left out.
func TestStandInAnswersLikeTheContract(t *testing.T) {
	workers := map[string]bool{
		"": true, "trawl": true, "sextant": true, "assay": true, "countersign": true, "waybill": true,
		"moldloft": true, "shakedown": true, "chartroom": true, "tidegauge": true, "bellbuoy": true,
	}
	ran := 0
	for name, ex := range loadExamples(t) {
		path := ex.Request.Path
		if ex.Given != nil || !(workerCalls[path] || path == "/v1/made-up" || path == "/healthz") || !workers[ex.Request.As] {
			continue
		}
		// Chartroom IS a worker, so refused-403-not_your_call (conn calling /v1/lease)
		// is not Chartroom's caller identity; non-worker paths with Chartroom's token
		// are checked in TestContractExamples above.
		ran++
		m, _, _ := newMemory(t)
		srv := httptest.NewServer(muster.Handler(m, token))
		body := []byte(compact(t, ex.Request.Body))
		if ex.Request.Raw != nil {
			body = []byte(*ex.Request.Raw)
		}
		req, err := http.NewRequest(ex.Request.Method, srv.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		contentType := ex.Request.ContentType
		if contentType == "" && ex.Request.Method == http.MethodPost {
			contentType = "application/json"
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if ex.Request.As != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := drainResponse(resp)
		srv.Close()
		if resp.StatusCode != ex.Expect.Status {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, ex.Expect.Status)
		}
		if want := compact(t, ex.Expect.Body); strings.TrimSpace(string(got)) != want {
			t.Errorf("%s: body\n  got  %s\n  want %s", name, got, want)
		}
	}
	if ran < 9 {
		t.Errorf("only %d examples could be sent to the stand-in; the examples have changed shape", ran)
	}
}

func drainResponse(resp *http.Response) []byte {
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes()
}
