package cserr

import (
	"fmt"
	"strings"
	"testing"
)

// TestThreePublicCodes pins the rule that there are three public error
// codes and no others.
func TestThreePublicCodes(t *testing.T) {
	allowed := map[Code]bool{"E_TIMEOUT": true, "E_PARSE": true, "E_NEEDS_SUBTOOL": true}
	seenCode := map[Code]bool{}
	seenName := map[string]bool{}
	for _, r := range Reasons() {
		if !allowed[r.Code] {
			t.Errorf("reason %s has code %s", r.Name, r.Code)
		}
		if seenName[r.Name] {
			t.Errorf("reason %s is listed twice", r.Name)
		}
		if r.Message == "" || strings.ContainsAny(r.Message, "\n:/@") {
			t.Errorf("reason %s has an unsuitable message %q", r.Name, r.Message)
		}
		seenCode[r.Code] = true
		seenName[r.Name] = true
	}
	if len(seenCode) != 3 {
		t.Errorf("reasons cover %d codes, want 3", len(seenCode))
	}
}

func TestErrorFormat(t *testing.T) {
	if got := New(MalformedJSON).Error(); got != "E_PARSE malformed_json: the data is not well-formed JSON" {
		t.Errorf("got %q", got)
	}
	if got := At(InvalidMetadata, "entry.title").Error(); got != "E_PARSE invalid_metadata entry.title: the catalog metadata is not valid" {
		t.Errorf("got %q", got)
	}
	wrapped := fmt.Errorf("wrapped: %w", New(UpstreamTimeout))
	if e, ok := From(wrapped); !ok || e.Reason != UpstreamTimeout {
		t.Error("From did not find the wrapped error")
	}
	if _, ok := From(fmt.Errorf("other")); ok {
		t.Error("From found a public error where there is none")
	}
}
