// Package catalog is the model of the tool catalog: its entries, how the
// file is read strictly and how it is written the same way every time.
package catalog

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

// ContractVersion is the one schema version this build understands.
const ContractVersion = "v1"

// MaxBytes is the largest catalog file the command will read.
const MaxBytes = 8 << 20

// The four statuses a catalog entry can have. They are the four target
// states in the specification's transition table.
const (
	StatusLive    = "live"
	StatusLinked  = "linked"
	StatusMerged  = "merged"
	StatusRetired = "retired"
)

// The two reasons a live entry can be retired for.
const (
	ReasonRepealed   = "repealed"
	ReasonSuperseded = "superseded"
)

// Provenance is the provenance block. Note is the note stamped upstream and
// is published byte for byte. The block has exactly these two fields, so a
// raw snapshot cannot ride along.
type Provenance struct {
	Note  string   `json:"note"`
	Links []string `json:"links"`
}

// Link is an external reference to an existing tool.
type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Merge is the note that folds an item's scope into a parent tool.
type Merge struct {
	Parent string `json:"parent"`
	Note   string `json:"note"`
}

// Retirement is the historical note kept when a live entry is archived.
type Retirement struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// Entry is one catalog entry.
type Entry struct {
	Slug       string      `json:"slug"`
	Status     string      `json:"status"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	Repository string      `json:"repository,omitempty"`
	Link       *Link       `json:"link,omitempty"`
	Merge      *Merge      `json:"merge,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`
	Retirement *Retirement `json:"retirement,omitempty"`
}

// Clone returns a deep copy of e.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	c := *e
	if e.Link != nil {
		l := *e.Link
		c.Link = &l
	}
	if e.Merge != nil {
		m := *e.Merge
		c.Merge = &m
	}
	if e.Provenance != nil {
		c.Provenance = e.Provenance.Clone()
	}
	if e.Retirement != nil {
		r := *e.Retirement
		c.Retirement = &r
	}
	return &c
}

// Clone returns a deep copy of p.
func (p *Provenance) Clone() *Provenance {
	if p == nil {
		return nil
	}
	return &Provenance{Note: p.Note, Links: append([]string{}, p.Links...)}
}

// Equal reports whether two entries are the same in every field. Two nil
// entries are equal.
func Equal(a, b *Entry) bool { return reflect.DeepEqual(a, b) }

// Catalog is the catalog file.
type Catalog struct {
	Contract string  `json:"contract"`
	Entries  []Entry `json:"entries"`
}

// Empty returns a catalog with no entries.
func Empty() *Catalog {
	return &Catalog{Contract: ContractVersion, Entries: []Entry{}}
}

// Parse reads a catalog. It returns E_NEEDS_SUBTOOL when the schema
// version or an entry status is one this build does not know, and E_PARSE
// for anything else that is wrong.
func Parse(data []byte, limit int) (*Catalog, error) {
	if err := strictjson.Check(data, limit); err != nil {
		return nil, cserr.New(cserr.InvalidCatalog)
	}
	var probe struct {
		Contract *string `json:"contract"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Contract == nil {
		return nil, cserr.New(cserr.InvalidCatalog)
	}
	if *probe.Contract != ContractVersion {
		return nil, cserr.At(cserr.SchemaVersionMismatch, "catalog.contract")
	}
	var c Catalog
	if err := strictjson.Decode(data, limit, &c); err != nil || c.Entries == nil {
		return nil, cserr.New(cserr.InvalidCatalog)
	}
	seen := make(map[string]struct{}, len(c.Entries))
	for i := range c.Entries {
		e := &c.Entries[i]
		if err := e.check("catalog.entries.status"); err != nil {
			return nil, err
		}
		if _, dup := seen[e.Slug]; dup {
			return nil, cserr.New(cserr.InvalidCatalog)
		}
		seen[e.Slug] = struct{}{}
	}
	c.sort()
	return &c, nil
}

// check reports what is wrong with an entry, if anything: a status this
// build does not know is E_NEEDS_SUBTOOL, any other fault is E_PARSE.
func (e *Entry) check(statusField string) error {
	switch e.Status {
	case StatusLive, StatusLinked, StatusMerged, StatusRetired:
	case "":
		return cserr.New(cserr.InvalidCatalog)
	default:
		return cserr.At(cserr.UnsupportedEntryType, statusField)
	}
	if !e.valid() {
		return cserr.New(cserr.InvalidCatalog)
	}
	return nil
}

// MaxEntryBytes is the largest single entry Chartroom will read.
const MaxEntryBytes = 64 << 10

// ParseEntry reads one catalog entry on its own, as strictly as Parse
// reads a catalog.
func ParseEntry(data []byte) (*Entry, error) {
	var e Entry
	if err := strictjson.Decode(data, MaxEntryBytes, &e); err != nil {
		return nil, cserr.New(cserr.InvalidCatalog)
	}
	if err := e.check("status"); err != nil {
		return nil, err
	}
	return &e, nil
}

func (c *Catalog) sort() {
	sort.Slice(c.Entries, func(i, j int) bool { return c.Entries[i].Slug < c.Entries[j].Slug })
}

// Find returns a copy of the entry with the given slug, or nil.
func (c *Catalog) Find(slug string) *Entry {
	for i := range c.Entries {
		if c.Entries[i].Slug == slug {
			return c.Entries[i].Clone()
		}
	}
	return nil
}

// Upsert adds e, or replaces the entry that has the same slug, and keeps
// the entries in slug order.
func (c *Catalog) Upsert(e *Entry) {
	for i := range c.Entries {
		if c.Entries[i].Slug == e.Slug {
			c.Entries[i] = *e.Clone()
			return
		}
	}
	c.Entries = append(c.Entries, *e.Clone())
	c.sort()
}

// Subset returns a catalog holding only the entries with the given slugs.
func (c *Catalog) Subset(slugs ...string) *Catalog {
	out := Empty()
	for i := range c.Entries {
		for _, s := range slugs {
			if s != "" && c.Entries[i].Slug == s {
				out.Entries = append(out.Entries, *c.Entries[i].Clone())
				break
			}
		}
	}
	return out
}

// Encode writes the catalog in its one canonical form: entries in slug
// order, two-space indent, one trailing newline.
func (c *Catalog) Encode() []byte {
	out := Catalog{Contract: c.Contract, Entries: append([]Entry{}, c.Entries...)}
	out.sort()
	return strictjson.Marshal(out)
}
