package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
)

// A problem is one row of the contract's table of refusals.
type problem struct {
	status int
	refusal
}

// The refusals the stand-in can give, word for word as the contract's
// saved examples have them. A test replays those examples against it.
var (
	notJSON        = &problem{http.StatusBadRequest, refusal{"E_PARSE", "not_json", "the request is not one json value"}}
	unknownField   = &problem{http.StatusBadRequest, refusal{"E_PARSE", "unknown_field", "the request has a field this call does not define"}}
	missingField   = &problem{http.StatusBadRequest, refusal{"E_PARSE", "missing_field", "a required field is missing"}}
	badField       = &problem{http.StatusBadRequest, refusal{"E_PARSE", "bad_field", "a field does not have the form this call requires"}}
	useNeeds       = &problem{http.StatusBadRequest, refusal{"E_PARSE", "bad_field", "report a missing rule with the needs_subtool call"}}
	noCredential   = &problem{http.StatusUnauthorized, refusal{"E_PARSE", "no_credential", "no valid credential was presented"}}
	notYourCall    = &problem{http.StatusForbidden, refusal{"E_PARSE", "not_your_call", "this caller may not make this call"}}
	noSuchCall     = &problem{http.StatusNotFound, refusal{"E_PARSE", "no_such_call", "no such call"}}
	wrongMethod    = &problem{http.StatusMethodNotAllowed, refusal{"E_PARSE", "wrong_method", "this call does not accept that method"}}
	leaseNotHeld   = &problem{http.StatusConflict, refusal{"E_PARSE", "lease_not_held", "the lease is not held"}}
	moveNotAllowed = &problem{http.StatusConflict, refusal{"E_PARSE", "move_not_allowed", "that move is not allowed"}}
	tooLarge       = &problem{http.StatusRequestEntityTooLarge, refusal{"E_PARSE", "too_large", "the request is too large"}}
	wrongType      = &problem{http.StatusUnsupportedMediaType, refusal{"E_PARSE", "wrong_type", "the content type must be application/json"}}
	unknownVersion = &problem{http.StatusUnprocessableEntity, refusal{"E_NEEDS_SUBTOOL", "unknown_contract", "this contract version is not known"}}
	storeSlow      = &problem{http.StatusServiceUnavailable, refusal{"E_TIMEOUT", "store_slow", "the record store took too long"}}
)

func problemOf(err error) *problem {
	switch {
	case errors.Is(err, ErrLeaseNotHeld):
		return leaseNotHeld
	case errors.Is(err, ErrMoveNotAllowed):
		return moveNotAllowed
	case errors.Is(err, ErrRefused):
		return badField
	}
	return storeSlow
}

// The answers to the calls that end or keep a lease.
type beat struct {
	Lease   string `json:"lease"`
	Expires string `json:"expires"`
}

type ack struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
	Held  bool   `json:"held,omitempty"`
}

// Handler serves a Memory stand-in over HTTP the way Muster's contract
// describes: the same paths, the same checks in the same order, the same
// refusals. It knows one caller, Chartroom, by one token. It is here so
// the client can be tested against something that follows the contract; it
// is not Muster.
func Handler(m *Memory, token string) http.Handler {
	ctx := context.Background()
	calls := map[string]func(body []byte) (any, *problem){
		PathLease: func(body []byte) (any, *problem) {
			var req leaseRequest
			if p := read(body, &req, &req.Contract); p != nil {
				return nil, p
			}
			g, err := m.Lease(ctx)
			if err != nil {
				return nil, problemOf(err)
			}
			if g == nil {
				return nil, nil
			}
			return g, nil
		},
		PathHeartbeat: func(body []byte) (any, *problem) {
			var req heartbeatRequest
			if p := read(body, &req, &req.Contract, &req.Lease); p != nil {
				return nil, p
			}
			if err := m.Heartbeat(ctx, req.Lease); err != nil {
				return nil, problemOf(err)
			}
			return beat{Lease: req.Lease, Expires: m.expires(req.Lease)}, nil
		},
		PathComplete: func(body []byte) (any, *problem) {
			var req completeRequest
			if p := read(body, &req, &req.Contract, &req.Lease, &req.State); p != nil {
				return nil, p
			}
			result := bytes.TrimSpace(req.Result)
			switch {
			case len(result) == 0 || string(result) == "null":
				result = nil
			case result[0] != '{':
				return nil, badField
			}
			if err := m.Complete(ctx, req.Lease, req.State, json.RawMessage(result)); err != nil {
				return nil, problemOf(err)
			}
			return m.ack(req.Lease), nil
		},
		PathFail: func(body []byte) (any, *problem) {
			var req failRequest
			if p := read(body, &req, &req.Contract, &req.Lease, &req.Code); p != nil {
				return nil, p
			}
			switch req.Code {
			case "E_TIMEOUT", "E_PARSE":
			case "E_NEEDS_SUBTOOL":
				return nil, useNeeds
			default:
				return nil, badField
			}
			if err := m.Fail(ctx, req.Lease, req.Code); err != nil {
				return nil, problemOf(err)
			}
			return m.ack(req.Lease), nil
		},
		PathNeedsSubtool: func(body []byte) (any, *problem) {
			var req heartbeatRequest
			if p := read(body, &req, &req.Contract, &req.Lease); p != nil {
				return nil, p
			}
			if err := m.NeedsSubtool(ctx, req.Lease); err != nil {
				return nil, problemOf(err)
			}
			return m.ack(req.Lease), nil
		},
	}

	otherCalls := map[string]string{
		"/v1/items":   http.MethodPost,
		"/v1/edits":   http.MethodPost,
		"/v1/feed":    http.MethodGet,
		"/v1/health":  http.MethodPost,
		"/v1/release": http.MethodPost,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				refuse(w, wrongMethod)
				return
			}
			answer(w, http.StatusOK, struct {
				Status string `json:"status"`
			}{Status: "ok"})
			return
		}
		if wantMethod, other := otherCalls[r.URL.Path]; other {
			if r.Method != wantMethod {
				w.Header().Set("Allow", wantMethod)
				refuse(w, wrongMethod)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				refuse(w, noCredential)
				return
			}
			refuse(w, notYourCall)
			return
		}
		call, ok := calls[r.URL.Path]
		switch {
		case !ok:
			refuse(w, noSuchCall)
			return
		case r.Method != http.MethodPost:
			w.Header().Set("Allow", http.MethodPost)
			refuse(w, wrongMethod)
			return
		case r.Header.Get("Authorization") != "Bearer "+token:
			refuse(w, noCredential)
			return
		}
		if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
			refuse(w, wrongType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
		if err != nil {
			refuse(w, tooLarge)
			return
		}
		out, p := call(body)
		switch {
		case p != nil:
			refuse(w, p)
		case out == nil:
			w.WriteHeader(http.StatusNoContent)
		default:
			answer(w, http.StatusOK, out)
		}
	})
}

// read decodes a request body and checks, in the contract's order, that it
// is one JSON value, has no field the call does not define, names a known
// contract version and leaves no required field empty. required lists the
// contract version first.
func read(body []byte, dst any, required ...*string) *problem {
	if !json.Valid(body) {
		return notJSON
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return unknownField
		}
		return badField
	}
	for i, field := range required {
		switch {
		case *field == "":
			return missingField
		case i == 0 && *field != catalog.ContractVersion:
			return unknownVersion
		}
	}
	return nil
}

func refuse(w http.ResponseWriter, p *problem) { answer(w, p.status, p.refusal) }

func answer(w http.ResponseWriter, status int, v any) {
	body, _ := encode(v)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// expires returns the end of a lease as the contract writes a time.
func (m *Memory) expires(lease string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it := m.held(lease); it != nil {
		return it.expires.UTC().Format(time.RFC3339)
	}
	return ""
}

// ack returns the answer to the call that ended a lease.
func (m *Memory) ack(lease string) ack {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.items {
		if it.done.lease == lease {
			a := ack{ID: it.seed.ID, State: it.seed.State}
			switch {
			case it.done.call == "fail" && it.stopped:
				a.Held = true
			case it.done.call == "fail":
				a.Next = Worker
			case it.done.call == "complete" && it.seed.State == "live":
				// The weekly check that follows a publication.
				a.Next = "tidegauge"
			}
			return a
		}
	}
	return ack{}
}
