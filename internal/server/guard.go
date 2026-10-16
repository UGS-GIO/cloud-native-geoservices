package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/plan"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/query"
)

var (
	errScanBusy = errors.New("busy: too many full-layer scans running; retry shortly")
	errWorkBusy = errors.New("busy: too many large queries running; retry shortly")
)

// guard wraps every route: CORS, a cap on requests in flight, a per-request deadline, and panic
// recovery.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE")
			if h := r.Header.Get("Access-Control-Request-Headers"); h != "" {
				w.Header().Set("Access-Control-Allow-Headers", h)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case s.inFlight <- struct{}{}:
			defer func() { <-s.inFlight }()
		default:
			if r.URL.Path == "/healthz" {
				break // health stays green under a flood
			}
			w.Header().Set("Retry-After", "1")
			s.sendStatus(w, http.StatusServiceUnavailable, esriError(503, "busy: too many requests; retry shortly", nil))
			return
		}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "err", v, "url", r.URL.String(), "stack", string(debug.Stack()))
				s.sendStatus(w, http.StatusOK, esriError(500, "Internal error", nil))
			}
		}()
		if s.Limits.Timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), s.Limits.Timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func bad(msg string) error { return &query.Error{Code: 400, Message: msg, Details: []string{}} }

// params reads the query string and a size-limited form or JSON body.
func (s *Server) params(r *http.Request) (query.Params, query.Raw, error) {
	body := url.Values{}
	if r.Method == http.MethodPost && r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, s.Limits.MaxBody+1))
		if err != nil {
			return nil, nil, bad("Could not read the request body")
		}
		if int64(len(b)) > s.Limits.MaxBody {
			return nil, nil, bad(fmt.Sprintf("Request body is larger than %d bytes", s.Limits.MaxBody))
		}
		switch ct := r.Header.Get("Content-Type"); {
		case strings.HasPrefix(ct, "application/json"):
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return nil, nil, bad("Malformed JSON body")
			}
			for k, v := range m {
				if str, ok := v.(string); ok {
					body.Set(k, str)
				} else {
					j, _ := json.Marshal(v)
					body.Set(k, string(j))
				}
			}
		case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
			if body, err = url.ParseQuery(string(b)); err != nil {
				return nil, nil, bad("Malformed form body")
			}
		}
		r.PostForm = body
	}
	p, raw := query.Normalize(r.URL.Query(), body)
	return p, raw, nil
}

var listSep = regexp.MustCompile(`\s*,\s*`)

// checkLimits rejects requests too large to be reasonable.
func (s *Server) checkLimits(raw query.Raw) error {
	if n := len([]rune(raw["where"])); s.Limits.MaxWhere > 0 && n > s.Limits.MaxWhere {
		return bad(fmt.Sprintf("'where' is %d characters; the limit is %d", n, s.Limits.MaxWhere))
	}
	if ids := raw["objectIds"]; ids != "" && s.Limits.MaxObjectIDs > 0 {
		if n := len(listSep.Split(ids, -1)); n > s.Limits.MaxObjectIDs {
			return bad(fmt.Sprintf("'objectIds' has %d ids; the limit is %d", n, s.Limits.MaxObjectIDs))
		}
	}
	return nil
}

// fullScan reports a query that reads every row: no spatial filter, ids or page to narrow it.
func fullScan(raw query.Raw) bool {
	if plan.FilterBox(raw) != nil || strings.TrimSpace(raw["objectIds"]) != "" {
		return false
	}
	order := strings.TrimSpace(raw["orderByFields"])
	return !plan.TrivialWhere(raw) || raw["outStatistics"] != "" || raw["returnDistinctValues"] == "true" ||
		(order != "" && !strings.HasPrefix(strings.ToLower(order), "feature_id"))
}

// light reports a query cheap enough to skip the work queue: a deep-zoom tile, an identify, a
// lookup by a few ids, or a count the footer answers. Everything else competes for a few slots.
func light(raw query.Raw) bool {
	if box := plan.FilterBox(raw); box != nil {
		small := box[2]-box[0] < 0.5 && box[3]-box[1] < 0.5 // a z10 tile or deeper
		return small && plan.TrivialWhere(raw) && raw["outStatistics"] == ""
	}
	if ids := strings.TrimSpace(raw["objectIds"]); ids != "" {
		return strings.Count(ids, ",") < 100
	}
	return plan.TrivialWhere(raw) && raw["returnCountOnly"] == "true"
}

// slot waits a little for one of a few slots, then gives up with busy.
func slot(ctx context.Context, slots chan struct{}, wait time.Duration, busy error) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-t.C:
		return nil, busy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
