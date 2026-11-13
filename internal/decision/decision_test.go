package decision_test

import (
	"path/filepath"
	"testing"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/fixture"
)

// TestTransitionsMatchTheSpecification pins the state table from SPEC.md.
func TestTransitionsMatchTheSpecification(t *testing.T) {
	want := map[string]decision.Transition{
		"publish": {From: "building", To: "live"},
		"link":    {From: "scored", To: "linked"},
		"merge":   {From: "scored", To: "merged"},
		"retire":  {From: "live", To: "retired"},
	}
	if len(decision.Transitions) != len(want) {
		t.Fatalf("there are %d transitions, the specification has %d", len(decision.Transitions), len(want))
	}
	for action, tr := range want {
		if decision.Transitions[action] != tr {
			t.Errorf("%s = %+v, want %+v", action, decision.Transitions[action], tr)
		}
	}
	if decision.RequiredConditions != 5 {
		t.Errorf("RequiredConditions = %d, the specification says 5", decision.RequiredConditions)
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
	for _, banned := range []string{"attestation", "conditions", "item_id", "itm-0101", "building", "snapshot"} {
		if contains(got, banned) {
			t.Errorf("published catalog contains %q", banned)
		}
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
