package catalog

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ClearedStack/chartroom/internal/cserr"
)

func TestValidSlug(t *testing.T) {
	good := []string{"a", "sample-tool", "tool-2", "a1-b2-c3", strings.Repeat("a", 64)}
	bad := []string{"", "-a", "a-", "a--b", "A", "sample_tool", "sample tool", "../a", "a/b", "a.b", "é", strings.Repeat("a", 65)}
	for _, s := range good {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false", s)
		}
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true", s)
		}
	}
}

func TestValidPublicURL(t *testing.T) {
	good := []string{
		"https://forge.example/tools/sample",
		"https://rules.example/part-900/section-12?view=text#top",
		"https://a.b.example:8443/x",
		"https://localized.example/x",
	}
	bad := []string{
		"",
		"forge.example/tools",
		"http://forge.example/tools",
		"ftp://forge.example/tools",
		"https:forge.example",
		"https:///tools",
		"https://forge/tools",
		"https://forge.example./tools",
		"https://Forge.Example/tools",
		"https://someone@forge.example/tools",
		"https://someone:word@forge.example/tools",
		"https://192.0.2.10/tools",
		"https://[2001:db8::1]/tools",
		"https://localhost/tools",
		"https://forge.localhost/tools",
		"https://forge.cs-workers.svc/tools",
		"https://forge.cs-workers.svc.cluster.local/tools",
		"https://forge.internal/tools",
		"https://forge_one.example/tools",
		"https://-forge.example/tools",
		"https://forge.example/a b",
		"https://forge.example/a\nb",
		"https://forge.example/" + strings.Repeat("a", MaxURLBytes),
	}
	for _, s := range good {
		if !ValidPublicURL(s) {
			t.Errorf("ValidPublicURL(%q) = false", s)
		}
	}
	for _, s := range bad {
		if ValidPublicURL(s) {
			t.Errorf("ValidPublicURL(%q) = true", s)
		}
	}
}

func TestTextLimits(t *testing.T) {
	if !ValidTitle(strings.Repeat("é", MaxTitleRunes)) || ValidTitle(strings.Repeat("é", MaxTitleRunes+1)) {
		t.Error("title limit is not counted in runes")
	}
	if !ValidSummary(strings.Repeat("a", MaxSummaryRunes)) || ValidSummary(strings.Repeat("a", MaxSummaryRunes+1)) {
		t.Error("summary limit is wrong")
	}
	if !ValidNote("line one\nline two") || ValidNote("tab\there") || ValidNote("") || ValidNote("padded ") {
		t.Error("note rule is wrong")
	}
	if ValidTitle("two\nlines") || ValidSummary("two\nlines") {
		t.Error("a title or summary may not hold a line break")
	}
	if !ValidProvenanceNote(" kept\tas is\n") || ValidProvenanceNote(" \n\t") || ValidProvenanceNote("bell\a") ||
		ValidProvenanceNote(strings.Repeat("a", MaxProvNoteBytes+1)) {
		t.Error("provenance note rule is wrong")
	}
}

func entry(slug string) Entry {
	return Entry{
		Slug: slug, Status: StatusLive, Title: "Sample", Summary: "A made-up tool.",
		Repository: "https://forge.example/tools/" + slug,
		Provenance: &Provenance{Note: "Made up.", Links: []string{"https://rules.example/a"}},
	}
}

func TestEncodeIsStableAndSorted(t *testing.T) {
	c := Empty()
	for _, s := range []string{"c-tool", "a-tool", "b-tool"} {
		e := entry(s)
		c.Upsert(&e)
	}
	first := c.Encode()
	parsed, err := Parse(first, MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.Encode(), first) {
		t.Error("Parse then Encode does not give the same bytes")
	}
	if a, b, cc := strings.Index(string(first), "a-tool"), strings.Index(string(first), "b-tool"), strings.Index(string(first), "c-tool"); !(a < b && b < cc) {
		t.Error("entries are not in slug order")
	}
	if got := string(Empty().Encode()); got != "{\n  \"contract\": \"v1\",\n  \"entries\": []\n}\n" {
		t.Errorf("empty catalog encodes as %q", got)
	}
}

func TestFindAndUpsertCopy(t *testing.T) {
	c := Empty()
	e := entry("a-tool")
	c.Upsert(&e)
	e.Provenance.Links[0] = "https://rules.example/changed"
	got := c.Find("a-tool")
	if got.Provenance.Links[0] != "https://rules.example/a" {
		t.Error("Upsert kept a reference to its argument")
	}
	got.Title = "changed"
	if c.Find("a-tool").Title != "Sample" {
		t.Error("Find returned a reference into the catalog")
	}
	if c.Find("missing") != nil {
		t.Error("Find invented an entry")
	}
	replacement := entry("a-tool")
	replacement.Title = "Replaced"
	c.Upsert(&replacement)
	if len(c.Entries) != 1 || c.Find("a-tool").Title != "Replaced" {
		t.Error("Upsert did not replace the entry with the same slug")
	}
}

func TestSubset(t *testing.T) {
	c := Empty()
	for _, s := range []string{"a-tool", "b-tool", "c-tool"} {
		e := entry(s)
		c.Upsert(&e)
	}
	sub := c.Subset("c-tool", "", "a-tool", "missing")
	if len(sub.Entries) != 2 || sub.Entries[0].Slug != "a-tool" || sub.Entries[1].Slug != "c-tool" {
		t.Errorf("Subset gave %+v", sub.Entries)
	}
}

func TestParseOversized(t *testing.T) {
	data := Empty().Encode()
	if _, err := Parse(data, len(data)-1); err == nil {
		t.Fatal("an oversized catalog was accepted")
	} else if e, _ := cserr.From(err); e == nil || e.Reason != cserr.InvalidCatalog {
		t.Errorf("got %v", err)
	}
}

func TestParseEntry(t *testing.T) {
	e := entry("a-tool")
	good := Empty()
	good.Upsert(&e)
	raw := []byte(`{"slug":"a-tool","status":"live","title":"Sample","summary":"A made-up tool.","repository":"https://forge.example/tools/a-tool","provenance":{"note":"Made up.","links":["https://rules.example/a"]}}`)
	got, err := ParseEntry(raw)
	if err != nil || !Equal(got, &e) {
		t.Fatalf("ParseEntry: %v, %+v", err, got)
	}
	cases := map[string]cserr.Reason{
		`{"published":true}`: cserr.InvalidCatalog,
		`[]`:                 cserr.InvalidCatalog,
		`{"slug":"a-tool","status":"featured","title":"Sample","summary":"A made-up tool."}`: cserr.UnsupportedEntryType,
		`{"slug":"a-tool","status":"live","title":"Sample","summary":"A made-up tool."}`:     cserr.InvalidCatalog,
		`{"slug":"a-tool","title":"Sample","summary":"A made-up tool."}`:                     cserr.InvalidCatalog,
	}
	for in, want := range cases {
		_, err := ParseEntry([]byte(in))
		if e, _ := cserr.From(err); e == nil || e.Reason != want {
			t.Errorf("ParseEntry(%s) = %v, want %s", in, err, want.Name)
		}
	}
}
