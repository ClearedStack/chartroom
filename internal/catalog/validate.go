package catalog

import (
	"net"
	"net/url"
	"strings"
	"unicode"
)

// Limits on catalog text. They keep one entry small enough that a batch of
// items fits comfortably inside the memory budget.
const (
	MaxSlugBytes     = 64
	MaxTitleRunes    = 120
	MaxSummaryRunes  = 600
	MaxNoteRunes     = 2000
	MaxProvNoteBytes = 4000
	MaxProvLinks     = 16
	MaxURLBytes      = 512
)

// ValidSlug reports whether s is a catalog slug: lower-case letters and
// digits in groups joined by single hyphens, at most 64 bytes. Item
// identifiers follow the same rule.
func ValidSlug(s string) bool {
	if len(s) == 0 || len(s) > MaxSlugBytes {
		return false
	}
	prevHyphen := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevHyphen = false
		case c == '-':
			if prevHyphen {
				return false
			}
			prevHyphen = true
		default:
			return false
		}
	}
	return !prevHyphen
}

// validText reports whether s is non-empty, has no leading or trailing
// white space, no control characters (a line feed is allowed when newline
// is true) and at most max runes.
func validText(s string, max int, newline bool) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	n := 0
	for _, r := range s {
		n++
		if r == '\n' && newline {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return n <= max
}

// ValidTitle reports whether s can be an entry title or a link name.
func ValidTitle(s string) bool { return validText(s, MaxTitleRunes, false) }

// ValidSummary reports whether s can be an entry summary.
func ValidSummary(s string) bool { return validText(s, MaxSummaryRunes, false) }

// ValidNote reports whether s can be a merge or retirement note.
func ValidNote(s string) bool { return validText(s, MaxNoteRunes, true) }

// ValidProvenanceNote reports whether s can be a provenance note. The note
// is published byte for byte, so white space at its ends is allowed; it
// only has to say something, stay within the size limit and hold no control
// characters other than line feed and tab.
func ValidProvenanceNote(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > MaxProvNoteBytes {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// internalSuffixes are host name endings that only resolve inside a private
// network. A public catalog must not point at them.
var internalSuffixes = []string{".local", ".internal", ".localhost", ".svc"}

// ValidPublicURL reports whether s is a link the public catalog may carry:
// an absolute https URL with no user name or password, whose host is a
// dotted DNS name, not an IP address and not a private-network name.
func ValidPublicURL(s string) bool {
	if s == "" || len(s) > MaxURLBytes {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil {
		return false
	}
	host := u.Hostname()
	if host == "" || host != strings.ToLower(host) || net.ParseIP(host) != nil {
		return false
	}
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !validHostLabel(label) {
			return false
		}
	}
	for _, suffix := range internalSuffixes {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

func validHostLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

// ValidProvenanceLinks reports whether links is a usable list of
// provenance links.
func ValidProvenanceLinks(links []string) bool {
	if len(links) == 0 || len(links) > MaxProvLinks {
		return false
	}
	for _, l := range links {
		if !ValidPublicURL(l) {
			return false
		}
	}
	return true
}

// ValidRetireReason reports whether r is a retirement reason the
// specification names.
func ValidRetireReason(r string) bool {
	return r == ReasonRepealed || r == ReasonSuperseded
}

func validProvenance(p *Provenance) bool {
	return p != nil && ValidProvenanceNote(p.Note) && ValidProvenanceLinks(p.Links)
}

// valid reports whether e has exactly the shape its status calls for.
func (e *Entry) valid() bool {
	if !ValidSlug(e.Slug) || !ValidTitle(e.Title) || !ValidSummary(e.Summary) {
		return false
	}
	switch e.Status {
	case StatusLive, StatusRetired:
		if !ValidPublicURL(e.Repository) || !validProvenance(e.Provenance) || e.Link != nil || e.Merge != nil {
			return false
		}
		if e.Status == StatusLive {
			return e.Retirement == nil
		}
		return e.Retirement != nil && ValidRetireReason(e.Retirement.Reason) && ValidNote(e.Retirement.Note)
	case StatusLinked:
		return e.Repository == "" && e.Merge == nil && e.Retirement == nil &&
			e.Link != nil && ValidTitle(e.Link.Name) && ValidPublicURL(e.Link.URL) &&
			validProvenance(e.Provenance)
	case StatusMerged:
		return e.Repository == "" && e.Link == nil && e.Retirement == nil &&
			e.Merge != nil && ValidSlug(e.Merge.Parent) && e.Merge.Parent != e.Slug && ValidNote(e.Merge.Note) &&
			(e.Provenance == nil || validProvenance(e.Provenance))
	}
	return false
}
