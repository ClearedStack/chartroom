// Package decision holds the four catalog rules: publish, link, merge and
// retire. Parse reads one item strictly; Apply works out the change that
// item makes to a catalog. Both are pure functions of their input.
package decision

import (
	"encoding/json"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

// MaxItemBytes is the largest item Chartroom will read.
const MaxItemBytes = 64 << 10

// The four actions.
const (
	ActionPublish = "publish"
	ActionLink    = "link"
	ActionMerge   = "merge"
	ActionRetire  = "retire"
)

// RequiredConditions is how many Shakedown conditions an attestation must
// show as passed. The specification gives the number and not the names, so
// the names are carried as they come and only have to differ.
const RequiredConditions = 5

// Transition is one of Chartroom's rows in Muster's table of queue steps:
// the state an item is held in and the state the step completes it to.
type Transition struct {
	From string
	To   string
}

// Transitions maps each action to its row in Muster contract v1. Only the
// publish step changes the state. Countersign has already moved a linked
// or merged item, and Tidegauge a retired one, before Chartroom lists it.
var Transitions = map[string]Transition{
	ActionPublish: {From: "building", To: catalog.StatusLive},
	ActionLink:    {From: catalog.StatusLinked, To: catalog.StatusLinked},
	ActionMerge:   {From: catalog.StatusMerged, To: catalog.StatusMerged},
	ActionRetire:  {From: catalog.StatusRetired, To: catalog.StatusRetired},
}

// ActionFor returns the action for an item held in the given state.
func ActionFor(state string) (string, bool) {
	for _, action := range []string{ActionPublish, ActionLink, ActionMerge, ActionRetire} {
		if Transitions[action].From == state {
			return action, true
		}
	}
	return "", false
}

// ValidItemID reports whether s is an item identifier as Muster hands them
// out: "PA-" and four digits, or five to nine digits with no leading zero.
func ValidItemID(s string) bool {
	if len(s) < 7 || len(s) > 12 || s[:3] != "PA-" {
		return false
	}
	digits := s[3:]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return len(digits) == 4 || digits[0] != '0'
}

// EntryFields is the text every new entry carries.
type EntryFields struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// Publish is the block that goes with a publish action.
type Publish struct {
	Repository string `json:"repository"`
}

// Condition is one Shakedown condition and whether it passed.
type Condition struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// Attestation is the Shakedown pass attestation.
type Attestation struct {
	Conditions []Condition `json:"conditions"`
}

// Item is one leased item: a decision that is already made upstream and
// that Chartroom only has to publish.
type Item struct {
	Contract    string              `json:"contract"`
	ID          string              `json:"id"`
	State       string              `json:"state"`
	Action      string              `json:"action"`
	Slug        string              `json:"slug"`
	Entry       *EntryFields        `json:"entry,omitempty"`
	Publish     *Publish            `json:"publish,omitempty"`
	Link        *catalog.Link       `json:"link,omitempty"`
	Merge       *catalog.Merge      `json:"merge,omitempty"`
	Retire      *catalog.Retirement `json:"retire,omitempty"`
	Provenance  *catalog.Provenance `json:"provenance,omitempty"`
	Attestation *Attestation        `json:"attestation,omitempty"`
}

// Change is what one item does to the catalog. The command prints it. The
// worker hands Muster its To state and its After entry.
type Change struct {
	Contract string         `json:"contract"`
	ID       string         `json:"id"`
	Action   string         `json:"action"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Slug     string         `json:"slug"`
	Changed  bool           `json:"changed"`
	Before   *catalog.Entry `json:"before"`
	After    *catalog.Entry `json:"after"`
}

// Encode writes the change in canonical form.
func (c *Change) Encode() []byte { return strictjson.Marshal(c) }

// Parse reads one item. The schema version and the action are checked
// before anything else, so an item from a newer contract is reported as
// E_NEEDS_SUBTOOL and not as broken data.
func Parse(data []byte) (*Item, error) {
	if err := strictjson.Check(data, MaxItemBytes); err != nil {
		return nil, jsonError(err)
	}
	var probe struct {
		Contract *string `json:"contract"`
		Action   *string `json:"action"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, cserr.New(cserr.MalformedJSON)
	}
	if probe.Contract == nil || *probe.Contract == "" {
		return nil, cserr.At(cserr.InvalidMetadata, "contract")
	}
	if *probe.Contract != catalog.ContractVersion {
		return nil, cserr.At(cserr.SchemaVersionMismatch, "contract")
	}
	if probe.Action == nil || *probe.Action == "" {
		return nil, cserr.At(cserr.InvalidMetadata, "action")
	}
	if _, ok := Transitions[*probe.Action]; !ok {
		return nil, cserr.At(cserr.UnsupportedEntryType, "action")
	}
	var it Item
	if err := strictjson.Decode(data, MaxItemBytes, &it); err != nil {
		return nil, jsonError(err)
	}
	if err := it.validate(); err != nil {
		return nil, err
	}
	return &it, nil
}

func jsonError(err error) error {
	if err == strictjson.ErrUnknownField {
		return cserr.New(cserr.UnknownField)
	}
	return cserr.New(cserr.MalformedJSON)
}

func invalid(field string) error { return cserr.At(cserr.InvalidMetadata, field) }

// validate checks the item against the contract, in a fixed order so the
// same broken item always reports the same field.
func (it *Item) validate() error {
	if !ValidItemID(it.ID) {
		return invalid("id")
	}
	if it.State == "" {
		return invalid("state")
	}
	if it.State != Transitions[it.Action].From {
		return cserr.At(cserr.UnsupportedTransition, "state")
	}
	if !catalog.ValidSlug(it.Slug) {
		return invalid("slug")
	}

	// Each action carries its own blocks and no others.
	type block struct {
		name    string
		present bool
		wanted  bool
	}
	a := it.Action
	blocks := []block{
		{"entry", it.Entry != nil, a != ActionRetire},
		{"publish", it.Publish != nil, a == ActionPublish},
		{"link", it.Link != nil, a == ActionLink},
		{"merge", it.Merge != nil, a == ActionMerge},
		{"retire", it.Retire != nil, a == ActionRetire},
	}
	for _, b := range blocks {
		if b.present != b.wanted {
			return invalid(b.name)
		}
	}
	if it.Provenance != nil && a == ActionRetire {
		return invalid("provenance")
	}
	if it.Attestation != nil && a != ActionPublish {
		return invalid("attestation")
	}

	if it.Entry != nil {
		if !catalog.ValidTitle(it.Entry.Title) {
			return invalid("entry.title")
		}
		if !catalog.ValidSummary(it.Entry.Summary) {
			return invalid("entry.summary")
		}
	}
	switch a {
	case ActionPublish:
		if !catalog.ValidPublicURL(it.Publish.Repository) {
			return invalid("publish.repository")
		}
	case ActionLink:
		if !catalog.ValidTitle(it.Link.Name) {
			return invalid("link.name")
		}
		if !catalog.ValidPublicURL(it.Link.URL) {
			return invalid("link.url")
		}
	case ActionMerge:
		if !catalog.ValidSlug(it.Merge.Parent) || it.Merge.Parent == it.Slug {
			return invalid("merge.parent")
		}
		if !catalog.ValidNote(it.Merge.Note) {
			return invalid("merge.note")
		}
	case ActionRetire:
		if it.Retire.Reason == "" {
			return invalid("retire.reason")
		}
		if !catalog.ValidRetireReason(it.Retire.Reason) {
			return cserr.At(cserr.UnsupportedValue, "retire.reason")
		}
		if !catalog.ValidNote(it.Retire.Note) {
			return invalid("retire.note")
		}
	}

	if err := it.checkProvenance(); err != nil {
		return err
	}
	if a == ActionPublish {
		return it.checkAttestation()
	}
	return nil
}

// checkProvenance applies the provenance rule: a live or linked entry must
// carry the block, a merged entry may.
func (it *Item) checkProvenance() error {
	p := it.Provenance
	if p == nil {
		if it.Action == ActionPublish || it.Action == ActionLink {
			return cserr.At(cserr.MissingProvenance, "provenance")
		}
		return nil
	}
	if !catalog.ValidProvenanceNote(p.Note) {
		return cserr.At(cserr.MissingProvenance, "provenance.note")
	}
	if len(p.Links) == 0 {
		return cserr.At(cserr.MissingProvenance, "provenance.links")
	}
	if !catalog.ValidProvenanceLinks(p.Links) {
		return invalid("provenance.links")
	}
	return nil
}

// checkAttestation applies the Shakedown rule: exactly five conditions,
// each with its own name, every one passed.
func (it *Item) checkAttestation() error {
	missing := func(field string) error { return cserr.At(cserr.MissingAttestation, field) }
	if it.Attestation == nil {
		return missing("attestation")
	}
	conds := it.Attestation.Conditions
	if len(conds) != RequiredConditions {
		return missing("attestation.conditions")
	}
	names := make(map[string]struct{}, len(conds))
	for _, c := range conds {
		if !catalog.ValidTitle(c.Name) {
			return missing("attestation.conditions.name")
		}
		if _, dup := names[c.Name]; dup {
			return missing("attestation.conditions.name")
		}
		names[c.Name] = struct{}{}
	}
	for _, c := range conds {
		if !c.Passed {
			return missing("attestation.conditions.passed")
		}
	}
	return nil
}

// start returns the change for an item with everything but the entries
// filled in.
func start(it *Item) *Change {
	tr := Transitions[it.Action]
	return &Change{
		Contract: catalog.ContractVersion,
		ID:       it.ID,
		Action:   it.Action,
		From:     tr.From,
		To:       tr.To,
		Slug:     it.Slug,
	}
}

// build makes the entry a publish, link or merge item asks for.
func build(it *Item) *catalog.Entry {
	e := &catalog.Entry{
		Slug:       it.Slug,
		Status:     Transitions[it.Action].To,
		Title:      it.Entry.Title,
		Summary:    it.Entry.Summary,
		Provenance: it.Provenance.Clone(),
	}
	switch it.Action {
	case ActionPublish:
		e.Repository = it.Publish.Repository
	case ActionLink:
		e.Link = &catalog.Link{Name: it.Link.Name, URL: it.Link.URL}
	case ActionMerge:
		e.Merge = &catalog.Merge{Parent: it.Merge.Parent, Note: it.Merge.Note}
	}
	return e
}

// retire archives a live entry, keeping everything it had and adding the
// retirement note. A retirement that is already recorded is left as it is.
func retire(ch *Change, existing *catalog.Entry, it *Item) (*Change, error) {
	switch {
	case existing == nil:
		return nil, cserr.At(cserr.UnsupportedCatalogState, "slug")
	case existing.Status == catalog.StatusLive:
		after := existing.Clone()
		after.Status = catalog.StatusRetired
		after.Retirement = &catalog.Retirement{Reason: it.Retire.Reason, Note: it.Retire.Note}
		ch.After = after
		ch.Changed = true
	case existing.Status == catalog.StatusRetired && *existing.Retirement == *it.Retire:
		ch.After = existing.Clone()
	default:
		return nil, cserr.At(cserr.UnsupportedCatalogState, "slug")
	}
	return ch, nil
}

// Apply works out the change a parsed item makes to a whole catalog. It
// does not alter cat. An item whose result is already in the catalog gives
// a change with Changed false, so a retry is harmless. Any other clash
// between the item and the catalog has no rule and is E_NEEDS_SUBTOOL: the
// entry under that slug may belong to another item.
func Apply(cat *catalog.Catalog, it *Item) (*Change, error) {
	return ApplyCatalog(cat, nil, it)
}

// ApplyCatalog works out the change a leased item makes when the worker can
// read both the whole catalog in Plainforge and the entry Chartroom
// published for this same item before (own, from results.chartroom). It
// enforces the whole-catalog slug collision and live merge-parent rules
// while allowing an item to rebuild and replace its own earlier entry.
func ApplyCatalog(cat *catalog.Catalog, own *catalog.Entry, it *Item) (*Change, error) {
	ch := start(it)
	existing := cat.Find(it.Slug)
	ch.Before = existing
	if own != nil && own.Slug != it.Slug {
		return nil, cserr.At(cserr.UnsupportedCatalogState, "slug")
	}
	if it.Action == ActionRetire {
		return retire(ch, existing, it)
	}
	ch.After = build(it)
	if existing != nil {
		if catalog.Equal(existing, ch.After) {
			return ch, nil
		}
		if own == nil || !catalog.Equal(existing, own) {
			return nil, cserr.At(cserr.UnsupportedCatalogState, "slug")
		}
	}
	if it.Action == ActionMerge {
		parent := cat.Find(it.Merge.Parent)
		if parent == nil || parent.Status != catalog.StatusLive {
			return nil, cserr.At(cserr.UnsupportedCatalogState, "merge.parent")
		}
	}
	ch.Changed = true
	return ch, nil
}

// ApplyOwn works out the change for the worker, which sees one item and
// not the catalog. own is the entry Chartroom published for this same item
// before, or nil. A publish, link or merge replaces it, as Muster replaces
// a worker's earlier result. A retirement needs it: there is nothing to
// retire otherwise.
//
// The worker cannot see other items, so it cannot check that a merge's
// parent is live. Apply can.
func ApplyOwn(own *catalog.Entry, it *Item) (*Change, error) {
	ch := start(it)
	ch.Before = own.Clone()
	if it.Action == ActionRetire {
		if own != nil && own.Slug != it.Slug {
			return nil, cserr.At(cserr.UnsupportedCatalogState, "slug")
		}
		return retire(ch, own.Clone(), it)
	}
	ch.After = build(it)
	ch.Changed = !catalog.Equal(own, ch.After)
	return ch, nil
}

// Run parses an item and a catalog and applies one to the other. The item
// is read first, so a broken item is reported ahead of a broken catalog.
func Run(catalogData []byte, catalogLimit int, itemData []byte) (*Change, *catalog.Catalog, error) {
	it, err := Parse(itemData)
	if err != nil {
		return nil, nil, err
	}
	cat, err := catalog.Parse(catalogData, catalogLimit)
	if err != nil {
		return nil, nil, err
	}
	ch, err := Apply(cat, it)
	if err != nil {
		return nil, nil, err
	}
	if ch.Changed {
		cat.Upsert(ch.After)
	}
	return ch, cat, nil
}
