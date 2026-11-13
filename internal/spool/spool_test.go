package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func complete(lease, lane string) Record {
	return Record{Contract: "v1", Lease: lease, ID: "PA-0001", Lane: lane,
		Outcome: OutcomeComplete, State: "live", Result: json.RawMessage(`{"slug": "a-tool"}`)}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestPutSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, discarded, err := Open(dir)
	if err != nil || discarded != 0 || s.Len() != 0 {
		t.Fatalf("Open: %v, %d discarded, %d records", err, discarded, s.Len())
	}
	report := Record{Contract: "v1", Lease: "lease-2", Lane: "normal", Outcome: OutcomeFail,
		Code: "E_PARSE", Reason: "malformed_json"}
	for _, r := range []Record{complete("lease-1", "backfill"), report, complete("lease-3", "urgent")} {
		if err := s.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if got := files(t, dir); len(got) != 3 {
		t.Fatalf("spool holds %v", got)
	}

	again, discarded, err := Open(dir)
	if err != nil || discarded != 0 {
		t.Fatalf("reopen: %v, %d discarded", err, discarded)
	}
	pending := again.Pending()
	if len(pending) != 3 || pending[0].Lease != "lease-3" || pending[1].Lease != "lease-2" || pending[2].Lease != "lease-1" {
		t.Fatalf("pending after reopen = %+v", pending)
	}
	if string(pending[0].Result) != `{"slug":"a-tool"}` || pending[0].State != "live" {
		t.Errorf("result read back as %s, state %q", pending[0].Result, pending[0].State)
	}
	if pending[1].Code != "E_PARSE" || pending[1].Reason != "malformed_json" {
		t.Errorf("report read back as %+v", pending[1])
	}

	if err := again.Remove("lease-3"); err != nil {
		t.Fatal(err)
	}
	if err := again.Remove("lease-3"); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if again.Len() != 2 || len(files(t, dir)) != 2 {
		t.Errorf("after Remove: %d records, files %v", again.Len(), files(t, dir))
	}
}

func TestPendingOrder(t *testing.T) {
	s, _, _ := Open(t.TempDir())
	for _, r := range []Record{
		complete("n1", "normal"), complete("b1", "backfill"), complete("u1", "urgent"),
		complete("x1", "whenever"), complete("n2", "normal"), complete("u2", "urgent"),
	} {
		if err := s.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, r := range s.Pending() {
		got = append(got, r.Lease)
	}
	if want := "u1 u2 n1 n2 b1 x1"; strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestPutReplacesTheSameLease(t *testing.T) {
	dir := t.TempDir()
	s, _, _ := Open(dir)
	_ = s.Put(complete("lease-1", "normal"))
	_ = s.Put(complete("lease-1", "urgent"))
	if s.Len() != 1 || len(files(t, dir)) != 1 || s.Pending()[0].Lane != "urgent" {
		t.Errorf("a second Put for the same lease did not replace the first")
	}
}

// TestHostileLeaseIDsStayInsideTheSpool checks a lease identifier is never
// used as a path.
func TestHostileLeaseIDsStayInsideTheSpool(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "spool")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _, _ := Open(dir)
	for _, id := range []string{"../escape", "/abs/path", "a/b", "..", "nul\x00byte", strings.Repeat("x", 5000)} {
		if err := s.Put(complete(id, "normal")); err != nil {
			t.Fatalf("Put(%q): %v", id, err)
		}
	}
	if got := files(t, parent); len(got) != 1 {
		t.Errorf("something was written outside the spool: %v", got)
	}
	for _, name := range files(t, dir) {
		if len(name) != 32+len(".json") {
			t.Errorf("unexpected file name %q", name)
		}
	}
	again, discarded, _ := Open(dir)
	if discarded != 0 || again.Len() != 6 {
		t.Errorf("reopen: %d records, %d discarded", again.Len(), discarded)
	}
}

func TestOpenCleansUp(t *testing.T) {
	dir := t.TempDir()
	s, _, _ := Open(dir)
	_ = s.Put(complete("lease-1", "normal"))
	good := files(t, dir)[0]

	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("0123.json.tmp", `{"half":`)        // left by a crash mid-write
	write("aaaa.json", `{"not": "a record"}`) // not a record
	write("bbbb.json", `{"contract":"v1",`)   // truncated
	valid, _ := os.ReadFile(filepath.Join(dir, good))
	write("cccc.json", string(valid)) // a record under the wrong name
	write("notes.txt", "left alone")

	again, discarded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 3 || again.Len() != 1 {
		t.Errorf("%d discarded, %d kept; want 3 and 1", discarded, again.Len())
	}
	got := strings.Join(files(t, dir), " ")
	if got != good+" notes.txt" {
		t.Errorf("spool holds %q", got)
	}
}

func TestErrorsDoNotNameThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secret-place")
	_, _, err := Open(dir)
	if err != ErrUnusable || strings.Contains(err.Error(), "secret-place") {
		t.Errorf("Open on a missing directory: %v", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _, _ := Open(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(complete("lease-1", "normal")); err != ErrUnusable {
		t.Errorf("Put into a vanished directory: %v", err)
	}
	if s.Len() != 0 {
		t.Error("a record that was not written is counted as staged")
	}
	for _, bad := range []Record{
		{Contract: "v1", Lease: "lease-1", Outcome: "other"},
		{Contract: "v1", Lease: "", Outcome: OutcomeComplete, State: "live", Result: json.RawMessage(`{}`)},
		{Contract: "v2", Lease: "lease-1", Outcome: OutcomeComplete, State: "live", Result: json.RawMessage(`{}`)},
		{Contract: "v1", Lease: "lease-1", Outcome: OutcomeComplete, Result: json.RawMessage(`{}`)},
		{Contract: "v1", Lease: "lease-1", Outcome: OutcomeComplete, State: "live"},
		{Contract: "v1", Lease: "lease-1", Outcome: OutcomeFail, Code: "E_NEEDS_SUBTOOL", Reason: "r"},
		{Contract: "v1", Lease: "lease-1", Outcome: OutcomeFail, Code: "E_PARSE"},
		{Contract: "v1", Lease: "lease-1", Outcome: OutcomeNeedsSubtool, Code: "E_PARSE", Reason: "r"},
	} {
		if err := s.Put(bad); err != ErrUnusable {
			t.Errorf("Put of an invalid record %+v: %v", bad, err)
		}
	}
}
