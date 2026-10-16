// Package query does what Koop's FeatureServer and winnow did with a provider's features:
// validate the request, filter by where, ids and geometry, order, page, compute statistics,
// project, and shape the Esri JSON, GeoJSON or PBF response.
package query

import (
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Params are the request parameters as Koop normalizes them: body then query (query wins),
// empty strings dropped, "true"/"false" as booleans and anything that parses as JSON parsed.
type Params map[string]any

// Raw is the same request as strings, with the same precedence, so the plan, the limits and the
// filters all see one value per parameter.
type Raw map[string]string

// Normalize builds both views of a request.
func Normalize(query, body url.Values) (Params, Raw) {
	raw := Raw{}
	for _, src := range []url.Values{body, query} {
		for k, v := range src {
			if v[0] != "" || raw[k] == "" {
				raw[k] = v[0]
			}
		}
	}
	p := Params{}
	for _, src := range []url.Values{body, query} {
		for k, v := range src {
			if v[0] == "" {
				continue
			}
			p[k] = coerce(v[0])
		}
	}
	return p, raw
}

func coerce(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// True is `=== true`.
func (p Params) True(k string) bool { v, ok := p[k].(bool); return ok && v }

// String is the parameter when it is a string.
func (p Params) String(k string) (string, bool) { s, ok := p[k].(string); return s, ok }

// Number is the parameter when it is a number.
func (p Params) Number(k string) (float64, bool) { f, ok := p[k].(float64); return f, ok }

// Has reports a parameter that is present.
func (p Params) Has(k string) bool { _, ok := p[k]; return ok }

// Format is f, defaulting to json.
func (p Params) Format() string {
	if f, ok := p["f"].(string); ok {
		return f
	}
	return "json"
}

// Error is an Esri error response.
type Error struct {
	Code    int
	Message string
	Details []string
}

func (e *Error) Error() string { return e.Message }

func bad(msg string, details ...string) *Error {
	if details == nil {
		details = []string{}
	}
	return &Error{400, msg, details}
}

func isInt(f float64) bool { return f == math.Trunc(f) && !math.IsInf(f, 0) }

// Validate checks the parameters Koop's query schema checks.
func (p Params) Validate() error {
	if v, ok := p["f"]; ok {
		s, _ := v.(string)
		switch s {
		case "json", "pjson", "pbf", "geojson":
		default:
			return bad("Invalid format", `"f" must be one of [json, pjson, pbf, geojson]`)
		}
	}
	for _, k := range []string{"resultRecordCount", "resultOffset"} {
		if v, ok := p[k]; ok {
			if _, num := v.(float64); !num {
				m := `"` + k + `" must be a number`
				return bad(m, m)
			}
		}
	}
	for _, k := range []string{"returnGeometry", "returnCountOnly"} {
		if v, ok := p[k]; ok {
			if _, b := v.(bool); !b {
				m := `"` + k + `" must be a boolean`
				return bad(m, m)
			}
		}
	}
	for _, k := range []string{"outFields", "orderByFields"} {
		if v, ok := p[k]; ok {
			if _, s := v.(string); !s {
				m := `"` + k + `" must be a string`
				return bad(m, m)
			}
		}
	}
	if v, ok := p["objectIds"]; ok {
		switch x := v.(type) {
		case string:
		case float64:
			if !isInt(x) {
				m := `"objectIds" does not match any of the allowed types`
				return bad(m, m)
			}
		default:
			m := `"objectIds" does not match any of the allowed types`
			return bad(m, m)
		}
	}
	if v, ok := p["outSR"]; ok {
		if _, ok := wkid(v); !ok {
			m := `"outSR" does not match any of the allowed types`
			return bad(m, m)
		}
	}
	if v, ok := p["quantizationParameters"]; ok {
		if _, ok := ParseQuantization(v); !ok {
			return bad("'quantizationParameters' parameter is invalid", `"quantizationParameters" must be of type object`)
		}
	}
	for _, k := range []string{"outSR", "inSR"} {
		if v, ok := p[k]; ok {
			if id, ok := wkid(v); ok && !supported(id) {
				return bad("Unsupported spatial reference: "+strconv.Itoa(id), "Only 4326 and Web Mercator (102100, 3857) are supported")
			}
		}
	}
	return nil
}

func supported(id int) bool {
	switch id {
	case 4326, 4269, 102100, 102113, 3857, 900913, 3785:
		return true
	}
	return false
}

// wkid reads a spatial reference given as a number or {wkid, latestWkid}.
func wkid(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if isInt(x) {
			return int(x), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n, true
		}
	case map[string]any:
		if w, ok := x["wkid"].(float64); ok && isInt(w) {
			return int(w), true
		}
		if w, ok := x["latestWkid"].(float64); ok && isInt(w) {
			return int(w), true
		}
	}
	return 0, false
}

// Quantization is the PBF transform a client asks for.
type Quantization struct {
	Scale                  float64
	XTranslate, YTranslate float64
	OriginPosition         string
	HasOrigin              bool
}

// ParseQuantization reads quantizationParameters.
func ParseQuantization(v any) (Quantization, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return Quantization{}, false
	}
	q := Quantization{Scale: 1}
	if t, ok := m["tolerance"]; ok && t != nil {
		f, ok := t.(float64)
		if !ok {
			return q, false
		}
		q.Scale = f
	}
	if o, ok := m["originPosition"]; ok {
		s, ok := o.(string)
		if !ok {
			return q, false
		}
		q.OriginPosition, q.HasOrigin = s, true
	}
	if e, ok := m["extent"]; ok && e != nil {
		em, ok := e.(map[string]any)
		if !ok {
			return q, false
		}
		for _, k := range []string{"xmin", "ymin", "xmax", "ymax"} {
			if _, ok := em[k].(float64); !ok {
				return q, false
			}
		}
		q.XTranslate, q.YTranslate = em["xmin"].(float64), em["ymax"].(float64)
	}
	return q, true
}
