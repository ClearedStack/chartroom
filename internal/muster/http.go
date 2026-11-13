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

// The paths of the five calls. They are provisional: Muster does not exist
// yet and its own specification will fix them.
const (
	PathLease        = "/v1/lease"
	PathHeartbeat    = "/v1/heartbeat"
	PathComplete     = "/v1/complete"
	PathFail         = "/v1/fail"
	PathNeedsSubtool = "/v1/needs_subtool"
)

// MaxResponseBytes is the most Chartroom will read from one Muster answer.
const MaxResponseBytes = 2 << 20

// ErrBadAddress means the Muster address given at start-up cannot be used.
// The address itself is never put in the error.
var ErrBadAddress = errors.New("the Muster address is not a usable http or https address")

type leaseRequest struct {
	Worker          string `json:"worker"`
	ContractVersion string `json:"contract_version"`
	Max             int    `json:"max"`
}

type leaseResponse struct {
	Leases []Lease `json:"leases"`
}

type heartbeatRequest struct {
	LeaseID string `json:"lease_id"`
}

type completeRequest struct {
	LeaseID string          `json:"lease_id"`
	Change  json.RawMessage `json:"change"`
}

type reportRequest struct {
	LeaseID string `json:"lease_id"`
	Report
}

// HTTP is the Muster client used in the cluster. It identifies itself with
// the pod's ServiceAccount token, read fresh for every call because the
// token is rotated. Its errors are the three fixed errors of this package
// and nothing else: no address, no token, no text from the network stack.
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

// post sends one call. When out is not nil a 200 answer is decoded into it.
func (h *HTTP) post(ctx context.Context, path string, in, out any) error {
	token, err := os.ReadFile(h.tokenFile)
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return ErrUnavailable
	}
	body, err := json.Marshal(in)
	if err != nil {
		return ErrBadResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	resp, err := h.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return ErrUnavailable
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if out == nil {
			return nil
		}
		if len(data) > MaxResponseBytes || json.Unmarshal(data, out) != nil {
			return ErrBadResponse
		}
		return nil
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound, http.StatusConflict, http.StatusGone:
		if path == PathLease {
			return ErrUnavailable
		}
		return ErrLeaseGone
	}
	return ErrUnavailable
}

// Lease asks Muster for up to max items.
func (h *HTTP) Lease(ctx context.Context, max int) ([]Lease, error) {
	var resp leaseResponse
	req := leaseRequest{Worker: Worker, ContractVersion: catalog.ContractVersion, Max: max}
	if err := h.post(ctx, PathLease, req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Leases) > max {
		return nil, ErrBadResponse
	}
	return resp.Leases, nil
}

// Heartbeat keeps a lease alive.
func (h *HTTP) Heartbeat(ctx context.Context, leaseID string) error {
	return h.post(ctx, PathHeartbeat, heartbeatRequest{LeaseID: leaseID}, nil)
}

// Complete hands over the catalog change for a lease.
func (h *HTTP) Complete(ctx context.Context, leaseID string, change json.RawMessage) error {
	return h.post(ctx, PathComplete, completeRequest{LeaseID: leaseID, Change: change}, nil)
}

// Fail reports that the item's data was broken.
func (h *HTTP) Fail(ctx context.Context, leaseID string, r Report) error {
	return h.post(ctx, PathFail, reportRequest{LeaseID: leaseID, Report: r}, nil)
}

// NeedsSubtool pauses an item no rule covers.
func (h *HTTP) NeedsSubtool(ctx context.Context, leaseID string, r Report) error {
	return h.post(ctx, PathNeedsSubtool, reportRequest{LeaseID: leaseID, Report: r}, nil)
}
