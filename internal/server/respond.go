package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/catalog"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/pbf"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/provider"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/query"
)

const jsonType = "application/json; charset=utf-8"

// encoded is a response body ready to send, and what the encoded-response cache holds.
type encoded struct {
	body        []byte // nil when only the compressed form is kept
	packed      []byte
	coding      *coding // of packed
	contentType string
	pbf         bool
}

// size is what the response cache counts for it.
func (e *encoded) size() int64 { return int64(len(e.body) + len(e.packed) + 256) }

var callbackChars = regexp.MustCompile(`[^\w\d\.\(\)\[\]]`)

// encode writes v as JSON, JSONP or pretty JSON, as the request asks.
func encode(p query.Params, v any) *encoded {
	if cb, ok := p.String("callback"); ok {
		buf := []byte(callbackChars.ReplaceAllString(cb, "") + "(")
		buf = jsonx.Append(buf, v)
		return &encoded{body: append(buf, ')'), contentType: "application/javascript"}
	}
	body := jsonx.Marshal(v)
	if p.Format() == "pjson" {
		var buf bytes.Buffer
		if json.Indent(&buf, body, "", "  ") == nil {
			body = buf.Bytes()
		}
	}
	return &encoded{body: body, contentType: jsonType}
}

// encodeQuery writes a query result in the requested format.
func encodeQuery(p query.Params, res *query.Response) (*encoded, error) {
	if _, cb := p.String("callback"); cb || p.Format() != "pbf" {
		return encode(p, res.JSON), nil
	}
	var body []byte
	switch {
	case p.True("returnExtentOnly") && res.Features != nil:
		body = pbf.Features(res.Features)
	case p.True("returnCountOnly"):
		var n int64
		if res.Count != nil {
			n = *res.Count
		}
		body = pbf.Count(n)
	case p.True("returnIdsOnly") && res.IDs != nil:
		body = pbf.IDs(res.IDs)
	case res.Features != nil:
		body = pbf.Features(res.Features)
	default:
		return nil, query.Bad("Bad Request")
	}
	return &encoded{body: body, contentType: "application/x-protobuf", pbf: true}, nil
}

// etag is a strong validator for a response: the data version plus the normalized request.
func etag(version string, r *http.Request) string {
	h := sha256.New()
	fmt.Fprint(h, version, "\x00", r.URL.Path, "\x00", r.URL.Query().Encode(), "\x00")
	if r.PostForm != nil {
		h.Write([]byte(r.PostForm.Encode()))
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
}

// notModified returns the If-None-Match tag that matches tag, ignoring the suffix compressed ETags
// carry, so a 304 repeats the ETag of the client's representation; "" when none matches.
func notModified(r *http.Request, tag string) string {
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		t = strings.TrimSpace(t)
		if b := baseTag(t); b == tag {
			return t
		} else if t == "*" {
			return tag
		}
	}
	return ""
}

// send writes a successful response; nil out means 304 Not Modified.
func (s *Server) send(w http.ResponseWriter, r *http.Request, out *encoded, tag string) {
	h := w.Header()
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d", s.CacheTTL, s.CacheTTL))
	}
	if tag != "" {
		h.Set("ETag", tag)
	}
	h.Set("Vary", "Accept-Encoding")
	if out == nil {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", out.contentType)
	if out.pbf {
		h.Set("Content-Disposition", "inline;filename=results.pbf")
	}
	body, c := s.represent(out, negotiate(r.Header.Get("Accept-Encoding"), s.codings), tag)
	if c != nil {
		h.Set("Content-Encoding", c.name)
		if tag != "" {
			h.Set("ETag", codedTag(tag, c))
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// sendStatus writes JSON with a status and no caching.
func (s *Server) sendStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(status)
	w.Write(jsonx.Marshal(v))
}

func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>Cannot %s %s</pre>\n</body>\n</html>\n", r.Method, url.PathEscape(r.URL.Path))
}

// fail writes an error: an Esri error body with HTTP 200 (as ArcGIS does), or 503 when shedding.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client left (Map Viewer cancels tiles it panned away from)
	}
	code, msg, details := 500, err.Error(), []string{}
	var qe *query.Error
	var ce *catalog.Error
	switch {
	case errors.Is(err, provider.ErrBusy) || errors.Is(err, errScanBusy) || errors.Is(err, errWorkBusy):
		w.Header().Set("Retry-After", "5")
		s.sendStatus(w, http.StatusServiceUnavailable, esriError(503, err.Error(), nil))
		return
	case errors.Is(err, context.DeadlineExceeded):
		code, msg = 504, "Request timed out"
	case errors.As(err, &qe):
		code, details = qe.Code, qe.Details
	case errors.As(err, &ce):
		code = ce.Code
	default:
		s.Log.Error("request failed", "err", err)
	}
	if code == 403 {
		s.sendStatus(w, http.StatusOK, jsonx.Obj{{K: "error", V: jsonx.Obj{{K: "code", V: 400}, {K: "messageCode", V: "CONT_0001"},
			{K: "message", V: "Item does not exist or is inaccessible."}, {K: "details", V: []string{}}}}})
		return
	}
	s.sendStatus(w, http.StatusOK, esriError(code, msg, details))
}

func esriError(code int, msg string, details []string) jsonx.Obj {
	if details == nil {
		details = []string{}
	}
	return jsonx.Obj{{K: "error", V: jsonx.Obj{{K: "code", V: code}, {K: "message", V: msg}, {K: "details", V: details}}}}
}
