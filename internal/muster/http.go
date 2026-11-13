package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
)

// The paths of the five worker calls.
const (
	PathLease        = "/v1/lease"
	PathHeartbeat    = "/v1/heartbeat"
	PathComplete     = "/v1/complete"
	PathFail         = "/v1/fail"
	PathNeedsSubtool = "/v1/needs_subtool"
)

// MaxRequestBytes is the largest request body Muster accepts from a worker.
const MaxRequestBytes = 64 << 10

// MaxResponseBytes is the most Chartroom will read from one Muster answer.
const MaxResponseBytes = 2 << 20

// ErrBadAddress means the Muster address given at start-up cannot be used.
// The address itself is never put in the error.
var ErrBadAddress = errors.New("the Muster address is not a usable http or https address")

// The request bodies. Muster refuses a body with a field the call does not
// define, so each has exactly the fields the contract lists, in its order.
type leaseRequest struct {
	Contract string `json:"contract"`
}

type heartbeatRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

type completeRequest struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type failRequest struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	Code     string `json:"code"`
}

// refusal is the body of every refusal: one of the three public codes, a
// fixed word a program can act on and a fixed sentence for a person.
type refusal struct {
	Error  string `json:"error"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// HTTP is the Muster client used in the cluster. It identifies itself with
// the pod's ServiceAccount token, read fresh for every call because the
// token is rotated. Its errors are the fixed errors of this package and
// nothing else: no address, no token, no text from the network.
type HTTP struct {
	base      string
	tokenFile string
	client    *http.Client
}

// NewHTTP returns a client for the Muster at baseURL. timeout bounds each
// call from first byte sent to last byte read.
func NewHTTP(baseURL, tokenFile string, timeout time.Duration) (*HTTP, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, ErrBadAddress
	}
	return &HTTP{
		base:      strings.TrimRight(u.String(), "/"),
		tokenFile: tokenFile,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// Cluster-private traffic: never through a proxy taken
				// from the environment.
				Proxy:               nil,
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				MaxConnsPerHost:     1,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				ForceAttemptHTTP2:   false,
				WriteBufferSize:     4 << 10,
				ReadBufferSize:      4 << 10,
			},
			// A redirect would carry the token to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// encode writes a request body: compact, fields in the contract's order,
// and with no HTML escaping, so a result takes no more room than it needs.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// post sends one call and returns the status and body of the answer.
func (h *HTTP) post(ctx context.Context, path string, in any) (int, []byte, error) {
	token, err := os.ReadFile(h.tokenFile)
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return 0, nil, ErrUnavailable
	}
	body, err := encode(in)
	if err != nil || len(body) > MaxRequestBytes {
		// Muster would refuse it as too large. Do not send it.
		return 0, nil, ErrRefused
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	if len(data) > MaxResponseBytes {
		return 0, nil, ErrBadResponse
	}
	return resp.StatusCode, data, nil
}

// decodeExact decodes one JSON value into v and refuses a field v does not
// define or anything after the value.
func decodeExact(data []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// refused turns an answer that is not a success into one of the fixed
// errors. It goes by the status and the cause, as the contract intends,
// and trusts a refusal only if it has the contract's exact shape.
func refused(status int, body []byte) error {
	var r refusal
	known := decodeExact(body, &r) &&
		(r.Error == "E_TIMEOUT" || r.Error == "E_PARSE" || r.Error == "E_NEEDS_SUBTOOL") && r.Cause != ""
	switch {
	case status >= 200 && status < 300:
		// A success the contract does not define.
		return ErrBadResponse
	case !known:
		return ErrUnavailable
	case status == http.StatusConflict && r.Cause == "lease_not_held":
		return ErrLeaseNotHeld
	case status == http.StatusConflict && r.Cause == "move_not_allowed":
		return ErrMoveNotAllowed
	case status == http.StatusUnprocessableEntity && r.Cause == "unknown_contract":
		return ErrUnknownContract
	case status >= 500 || status == http.StatusTooManyRequests || r.Error == "E_TIMEOUT":
		return ErrUnavailable
	case status >= 400:
		return ErrRefused
	}
	return ErrUnavailable
}

// Lease asks Muster for the next step.
func (h *HTTP) Lease(ctx context.Context) (*Grant, error) {
	status, body, err := h.post(ctx, PathLease, leaseRequest{Contract: catalog.ContractVersion})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var g Grant
		if !decodeExact(body, &g) || g.Lease == "" {
			return nil, ErrBadResponse
		}
		return &g, nil
	}
	return nil, refused(status, body)
}

// result sends a call whose answer only has to be a 200.
func (h *HTTP) result(ctx context.Context, path string, in any) error {
	status, body, err := h.post(ctx, path, in)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	return refused(status, body)
}

// Heartbeat keeps a lease.
func (h *HTTP) Heartbeat(ctx context.Context, lease string) error {
	return h.result(ctx, PathHeartbeat, heartbeatRequest{Contract: catalog.ContractVersion, Lease: lease})
}

// Complete reports a finished step.
func (h *HTTP) Complete(ctx context.Context, lease, state string, result json.RawMessage) error {
	return h.result(ctx, PathComplete, completeRequest{Contract: catalog.ContractVersion, Lease: lease, State: state, Result: result})
}

// Fail reports E_TIMEOUT or E_PARSE for a step.
func (h *HTTP) Fail(ctx context.Context, lease, code string) error {
	return h.result(ctx, PathFail, failRequest{Contract: catalog.ContractVersion, Lease: lease, Code: code})
}

// NeedsSubtool pauses an item no rule covers.
func (h *HTTP) NeedsSubtool(ctx context.Context, lease string) error {
	return h.result(ctx, PathNeedsSubtool, heartbeatRequest{Contract: catalog.ContractVersion, Lease: lease})
}
