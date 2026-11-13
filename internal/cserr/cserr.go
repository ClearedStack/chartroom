// Package cserr holds the three public error codes and the fixed list of
// reasons Chartroom can give for each one.
//
// Every message is a constant. Nothing read from an item, a catalog, the
// network or the operating system is ever copied into an error, so an error
// cannot name a machine, an address or a credential.
package cserr

import "errors"

// Code is one of the three public error codes.
type Code string

const (
	// Timeout means something outside Chartroom took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse means the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool means the data was valid and no rule covers it.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Reason is one fixed cause under a code.
type Reason struct {
	Code    Code
	Name    string
	Message string
}

// The complete list of reasons. Each one has at least one saved example
// under testdata.
var (
	MalformedJSON      = Reason{Parse, "malformed_json", "the data is not well-formed JSON"}
	UnknownField       = Reason{Parse, "unknown_field", "the data has a field that is not in the contract"}
	InvalidMetadata    = Reason{Parse, "invalid_metadata", "the catalog metadata is not valid"}
	MissingProvenance  = Reason{Parse, "missing_provenance", "the provenance block is missing or incomplete"}
	MissingAttestation = Reason{Parse, "missing_attestation", "the Shakedown pass attestation is missing or does not show five passed conditions"}
	InvalidCatalog     = Reason{Parse, "invalid_catalog", "the catalog is not valid"}

	UnsupportedEntryType    = Reason{NeedsSubtool, "unsupported_entry_type", "the catalog entry type is not supported"}
	SchemaVersionMismatch   = Reason{NeedsSubtool, "schema_version_mismatch", "the schema version is not supported"}
	UnsupportedTransition   = Reason{NeedsSubtool, "unsupported_transition", "no rule covers this state transition"}
	UnsupportedCatalogState = Reason{NeedsSubtool, "unsupported_catalog_state", "no rule covers the current catalog state of this entry"}
	UnsupportedValue        = Reason{NeedsSubtool, "unsupported_value", "no rule covers this value"}

	UpstreamTimeout = Reason{Timeout, "upstream_timeout", "an upstream service did not answer in time"}
)

// Reasons returns every reason, in a fixed order.
func Reasons() []Reason {
	return []Reason{
		MalformedJSON, UnknownField, InvalidMetadata, MissingProvenance,
		MissingAttestation, InvalidCatalog,
		UnsupportedEntryType, SchemaVersionMismatch, UnsupportedTransition,
		UnsupportedCatalogState, UnsupportedValue,
		UpstreamTimeout,
	}
}

// Error is a public error: a reason and, when it helps, the name of the
// contract field at fault. Field is always a name from the contract, never
// a value taken from the data.
type Error struct {
	Reason Reason
	Field  string
}

// New makes an error for a reason.
func New(r Reason) *Error { return &Error{Reason: r} }

// At makes an error for a reason, pointing at a contract field.
func At(r Reason, field string) *Error { return &Error{Reason: r, Field: field} }

// Error formats as "CODE reason [field]: message".
func (e *Error) Error() string {
	s := string(e.Reason.Code) + " " + e.Reason.Name
	if e.Field != "" {
		s += " " + e.Field
	}
	return s + ": " + e.Reason.Message
}

// From returns the public error inside err, if there is one.
func From(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
