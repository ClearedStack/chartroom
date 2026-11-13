package decision

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

// Self is the name Chartroom's own result is kept under.
const Self = "chartroom"

// wanted lists, for each action, the blocks Chartroom reads from the
// results of earlier workers. A retirement takes its slug and everything
// else from the entry Chartroom published before.
var wanted = map[string][]string{
	ActionPublish: {"slug", "entry", "publish", "provenance", "attestation"},
	ActionLink:    {"slug", "entry", "link", "provenance"},
	ActionMerge:   {"slug", "entry", "merge", "provenance"},
	ActionRetire:  {"retire"},
}

// FromResults builds the item for a leased step from what Muster hands
// over: the item's identifier, the state it is held in and the latest
// result of each earlier worker.
//
// Muster's contract does not say what a worker's result holds, so
// Chartroom looks for its blocks by name. Each block must be at the top
// level of exactly one earlier worker's result. A block in none is
// missing; a block in two has no rule to choose between them and is
// E_NEEDS_SUBTOOL. Everything else in a result is another worker's
// business and is not read, copied or published.
//
// It also returns the entry Chartroom published for this item before, if
// there is one.
func FromResults(id, state string, results map[string]json.RawMessage) (*Item, *catalog.Entry, error) {
	if !ValidItemID(id) {
		return nil, nil, invalid("id")
	}
	if state == "" {
		return nil, nil, invalid("state")
	}
	action, ok := ActionFor(state)
	if !ok {
		return nil, nil, cserr.At(cserr.UnsupportedTransition, "state")
	}

	var own *catalog.Entry
	if raw, ok := results[Self]; ok {
		e, err := catalog.ParseEntry(raw)
		if err != nil {
			return nil, nil, err
		}
		own = e
	}
	if action == ActionRetire && own == nil {
		return nil, nil, cserr.At(cserr.UnsupportedCatalogState, "results."+Self)
	}

	workers := make([]string, 0, len(results))
	for w := range results {
		if w != Self {
			workers = append(workers, w)
		}
	}
	sort.Strings(workers)
	blocks := make(map[string]json.RawMessage)
	for _, w := range workers {
		top, err := strictjson.TopLevel(results[w], MaxItemBytes)
		if err != nil {
			return nil, nil, cserr.At(cserr.MalformedJSON, "results")
		}
		for _, name := range wanted[action] {
			raw, ok := top[name]
			if !ok || string(raw) == "null" {
				continue
			}
			if _, twice := blocks[name]; twice {
				return nil, nil, cserr.At(cserr.UnsupportedValue, "results."+name)
			}
			blocks[name] = raw
		}
	}
	if action == ActionRetire {
		blocks["slug"], _ = json.Marshal(own.Slug)
	}

	// Put the item together as one document and read it as strictly as
	// any other item.
	var doc bytes.Buffer
	doc.WriteString(`{"contract":"` + catalog.ContractVersion + `"`)
	field := func(name string, raw []byte) {
		doc.WriteString(`,"` + name + `":`)
		doc.Write(raw)
	}
	for _, kv := range [][2]string{{"id", id}, {"state", state}, {"action", action}} {
		quoted, _ := json.Marshal(kv[1])
		field(kv[0], quoted)
	}
	names := wanted[action]
	if action == ActionRetire {
		names = []string{"slug", "retire"}
	}
	for _, name := range names {
		if raw, ok := blocks[name]; ok {
			field(name, raw)
		}
	}
	doc.WriteString("}")
	it, err := Parse(doc.Bytes())
	if err != nil {
		return nil, nil, err
	}
	return it, own, nil
}
