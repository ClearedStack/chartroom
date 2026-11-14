package decision_test

import (
	"path/filepath"
	"testing"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/fixture"
)

// TestTransitionsMatchMustersContract pins Chartroom's four rows in the
// table of queue steps in Muster's contract v1.
func TestTransitionsMatchMustersContract(t *testing.T) {
	want := map[string]decision.Transition{
		"publish": {From: "building", To: "live"},
		"link":    {From: "linked", To: "linked"},
		"merge":   {From: "merged", To: "merged"},
		"retire":  {From: "retired", To: "retired"},
	}
	if len(decision.Transitions) != len(want) {
		t.Fatalf("there are %d transitions, the contract has %d rows for Chartroom", len(decision.Transitions), len(want))
	}
	for action, tr := range want {
		if decision.Transitions[action] != tr {
			t.Errorf("%s = %+v, want %+v", action, decision.Transitions[action], tr)
		}
		if got, ok := decision.ActionFor(tr.From); !ok || got != action {
			t.Errorf("ActionFor(%q) = %q, %v", tr.From, got, ok)
		}
	}
	for _, state := range []string{"", "scored", "live", "approved", "needs_subtool", "screened_out"} {
		if action, ok := decision.ActionFor(state); ok {
			t.Errorf("ActionFor(%q) = %q; Chartroom holds no item in that state", state, action)
		}
	}
	if decision.RequiredConditions != 5 {
		t.Errorf("RequiredConditions = %d, the specification says 5", decision.RequiredConditions)
	}
}

// TestValidItemID pins the identifier form from Muster's contract:
// ^PA-([0-9]{4}|[1-9][0-9]{4,8})$.
func TestValidItemID(t *testing.T) {
	good := []string{"PA-0001", "PA-0417", "PA-9999", "PA-10000", "PA-123456789"}
	bad := []string{"", "PA-", "PA-001", "PA-01234", "PA-1234567890", "pa-0001", "EP-0001", "PA-00a1", "PA-0001 ", " PA-0001", "PA-0001.1", "incident:PA-0001"}
	for _, s := range good {
		if !decision.ValidItemID(s) {
			t.Errorf("ValidItemID(%q) = false", s)
		}
	}
	for _, s := range bad {
		if decision.ValidItemID(s) {
			t.Errorf("ValidItemID(%q) = true", s)
		}
	}
}

// TestApplyDoesNotAlterItsInput checks Apply is pure: the catalog it is
// given is the same afterwards.
func TestApplyDoesNotAlterItsInput(t *testing.T) {
	for _, name := range []string{"ok-publish", "ok-link", "ok-merge", "ok-retire"} {
		dir := filepath.Join(fixture.Root, "apply", name)
		cat, err := catalog.Parse(fixture.Read(t, filepath.Join(dir, "catalog.json")), catalog.MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		it, err := decision.Parse(fixture.Read(t, filepath.Join(dir, "decision.json")))
		if err != nil {
			t.Fatal(err)
		}
		before := string(cat.Encode())
		ch, err := decision.Apply(cat, it)
		if err != nil {
			t.Fatal(err)
		}
		// Altering what came back must not reach the catalog either.
		ch.After.Title = "altered"
		if ch.Before != nil {
			ch.Before.Title = "altered"
		}
		if string(cat.Encode()) != before {
			t.Errorf("%s: Apply altered the catalog it was given", name)
		}
	}
}

// TestPublishedEntryCarriesOnlyContractFields checks that nothing from the
// item beyond the catalog fields reaches a published entry: no attestation,
// no item identifier, no state.
func TestPublishedEntryCarriesOnlyContractFields(t *testing.T) {
	got := string(fixture.Read(t, filepath.Join(fixture.Root, "apply", "ok-publish", "want.catalog.json")))
	for _, banned := range []string{"attestation", "conditions", "\"id\"", "PA-0101", "building", "snapshot"} {
		if contains(got, banned) {
			t.Errorf("published catalog contains %q", banned)
		}
	}
}

func TestApplyCatalogRebuildsOwnEntryAndRejectsForeignCollisions(t *testing.T) {
	dir := filepath.Join(fixture.Root, "apply", "ok-publish")
	cat, err := catalog.Parse(fixture.Read(t, filepath.Join(dir, "want.catalog.json")), catalog.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	it, err := decision.Parse(fixture.Read(t, filepath.Join(dir, "decision.json")))
	if err != nil {
		t.Fatal(err)
	}
	own := cat.Find(it.Slug)
	if own == nil {
		t.Fatalf("missing %s in fixture catalog", it.Slug)
	}

	// Rebuilding the same item with updated summary replaces its own entry.
	rebuilt := *it
	rebuiltEntry := *it.Entry
	rebuiltEntry.Summary = "Updated summary on clean-room rebuild."
	rebuilt.Entry = &rebuiltEntry
	ch, err := decision.ApplyCatalog(cat, own, &rebuilt)
	if err != nil || !ch.Changed || ch.After.Summary != rebuiltEntry.Summary {
		t.Fatalf("own rebuild = %+v, %v", ch, err)
	}

	// Another item (own == nil) attempting to claim the same slug is E_NEEDS_SUBTOOL.
	if _, err := decision.ApplyCatalog(cat, nil, &rebuilt); err == nil {
		t.Fatal("foreign slug collision succeeded, want E_NEEDS_SUBTOOL")
	} else if e, ok := cserr.From(err); !ok || e.Reason != cserr.UnsupportedCatalogState || e.Field != "slug" {
		t.Fatalf("foreign slug collision = %v, want UnsupportedCatalogState slug", err)
	}

	// Changing an item's slug on rebuild has no rule to remove the old slug.
	renamed := rebuilt
	renamed.Slug = "renamed-tool"
	if _, err := decision.ApplyCatalog(cat, own, &renamed); err == nil {
		t.Fatal("slug rename on rebuild succeeded, want E_NEEDS_SUBTOOL")
	} else if e, ok := cserr.From(err); !ok || e.Reason != cserr.UnsupportedCatalogState || e.Field != "slug" {
		t.Fatalf("slug rename = %v, want UnsupportedCatalogState slug", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
