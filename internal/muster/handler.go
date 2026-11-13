package muster

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Handler serves a Memory stand-in over HTTP, on the same paths the HTTP
// client calls. A call must carry "Authorization: Bearer <token>". It is
// here so the client can be tested against something that follows the
// specification; it is not Muster.
func Handler(m *Memory, token string) http.Handler {
	mux := http.NewServeMux()
	handle := func(path string, fn func(r *http.Request, body []byte) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			out, err := fn(r, body)
			switch {
			case errors.Is(err, ErrLeaseGone):
				w.WriteHeader(http.StatusGone)
			case errors.Is(err, errBadRequest):
				w.WriteHeader(http.StatusBadRequest)
			case err != nil:
				w.WriteHeader(http.StatusServiceUnavailable)
			case out == nil:
				w.WriteHeader(http.StatusNoContent)
			default:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(out)
			}
		})
	}

	handle(PathLease, func(r *http.Request, body []byte) (any, error) {
		var req leaseRequest
		if json.Unmarshal(body, &req) != nil || req.Worker != Worker || req.Max < 1 {
			return nil, errBadRequest
		}
		leases, err := m.Lease(r.Context(), req.Max)
		if err != nil {
			return nil, err
		}
		for i := range leases {
			// An item that is not JSON cannot sit inside a JSON answer
			// as it is, so it travels as a JSON string. The worker then
			// reports it as broken, which it is.
			if !json.Valid(leases[i].Item) {
				leases[i].Item, _ = json.Marshal(string(leases[i].Item))
			}
			if !json.Valid(leases[i].Catalog) {
				leases[i].Catalog, _ = json.Marshal(string(leases[i].Catalog))
			}
		}
		return leaseResponse{Leases: leases}, nil
	})
	handle(PathHeartbeat, func(r *http.Request, body []byte) (any, error) {
		var req heartbeatRequest
		if json.Unmarshal(body, &req) != nil {
			return nil, errBadRequest
		}
		return nil, m.Heartbeat(r.Context(), req.LeaseID)
	})
	handle(PathComplete, func(r *http.Request, body []byte) (any, error) {
		var req completeRequest
		if json.Unmarshal(body, &req) != nil {
			return nil, errBadRequest
		}
		return nil, m.Complete(r.Context(), req.LeaseID, req.Change)
	})
	handle(PathFail, func(r *http.Request, body []byte) (any, error) {
		var req reportRequest
		if json.Unmarshal(body, &req) != nil {
			return nil, errBadRequest
		}
		return nil, m.Fail(r.Context(), req.LeaseID, req.Report)
	})
	handle(PathNeedsSubtool, func(r *http.Request, body []byte) (any, error) {
		var req reportRequest
		if json.Unmarshal(body, &req) != nil {
			return nil, errBadRequest
		}
		return nil, m.NeedsSubtool(r.Context(), req.LeaseID, req.Report)
	})
	return mux
}

var errBadRequest = errors.New("bad request")
