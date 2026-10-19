// Package conformance compares this server with real ArcGIS FeatureServer layers. Each layer has
// a GeoParquet copy of its features and the reference server's recorded answers to a fixed
// request set (testdata/conformance). The tests serve the copy, ask the same requests, and fail
// on any difference not in the known gaps list.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// IDVar is the placeholder for the object id field: OBJECTID on the reference, feature_id here.
const IDVar = "{oid}"

// Layer is a reference layer and the values its request templates are filled with.
type Layer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"` // …/FeatureServer/{n}
	License string            `json:"license"`
	Vars    map[string]string `json:"vars"`
}

// Layers lists the reference layers of each tier.
type Layers struct {
	Small []Layer `json:"small"`
	Large []Layer `json:"large"`
}

// Request is one request: a template in requests.json, filled in a recording.
type Request struct {
	Name   string            `json:"name"`
	Op     string            `json:"op,omitempty"`     // "query" (default) or "layer"
	Method string            `json:"method,omitempty"` // GET (default) or POST
	Params map[string]string `json:"params"`
	Rows   string            `json:"rows,omitempty"` // "count": compare how many rows, not which
}

// Response is the reference server's answer to a request.
type Response struct {
	Request
	Status      int             `json:"status"`
	ContentType string          `json:"contentType"`
	Body        json.RawMessage `json:"body,omitempty"`
	PBF         []byte          `json:"pbf,omitempty"`
	Millis      int64           `json:"ms"`
}

// Recording is everything recorded from one reference layer.
type Recording struct {
	Layer     Layer      `json:"layer"`
	Recorded  string     `json:"recorded"`
	Responses []Response `json:"responses"`
}

// Files of a recorded layer under a tier's directory.
func DataPath(dir, name string) string      { return filepath.Join(dir, name, "data.parquet") }
func RecordingPath(dir, name string) string { return filepath.Join(dir, name, "responses.json") }

// ReadJSON decodes a JSON file into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var placeholder = regexp.MustCompile(`\{(lower:)?([a-z0-9_]+)\}`)

// Fill replaces each {var} (or {lower:var}) in the template's parameters. It reports false when
// a var the template uses is not set, and leaves {oid} for Resolve.
func Fill(r Request, vars map[string]string) (Request, bool) {
	ok := true
	out := r
	out.Params = map[string]string{}
	for k, v := range r.Params {
		out.Params[k] = placeholder.ReplaceAllStringFunc(v, func(m string) string {
			if m == IDVar {
				return m
			}
			s := placeholder.FindStringSubmatch(m)
			val, set := vars[s[2]]
			if !set {
				ok = false
			}
			if s[1] != "" {
				val = strings.ToLower(val)
			}
			return val
		})
	}
	return out, ok
}

// Resolve names the object id field in a filled request's parameters.
func Resolve(params map[string]string, idField string) map[string]string {
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = strings.ReplaceAll(v, IDVar, idField)
	}
	return out
}
