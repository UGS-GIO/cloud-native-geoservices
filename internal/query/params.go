// Package query does what Koop's FeatureServer and winnow did with a provider's features:
// validate the request, filter by where, ids and geometry, order, page, compute statistics,
// project, and shape the Esri JSON, GeoJSON or PBF response.
package query

import (
	"encoding/json"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
)

// Params are the request parameters: body then query (query wins), empty strings dropped,
// booleans and f in any case, text kept as text, and anything else that parses as JSON parsed.
type Params map[string]any

// Raw is the same request as strings, with the same precedence, so the plan, the limits and the
// filters all see one value per parameter.
type Raw map[string]string

// Normalize builds both views of a request.
func Normalize(query, body url.Values) (Params, Raw) {
	p, raw := Params{}, Raw{}
	for _, src := range []url.Values{body, query} {
		for k, v := range src {
			if v[0] != "" {
				raw[k], p[k] = read(k, v[0])
			} else if raw[k] == "" {
				raw[k] = v[0]
			}
		}
	}
	return p, raw
}

// booleans are read as true or false in any case; any other value is a 400.
var booleans = []string{"returnGeometry", "returnCountOnly", "returnIdsOnly", "returnExtentOnly", "returnDistinctValues"}

// text parameters are never parsed as JSON.
var text = []string{"where", "outFields", "orderByFields", "groupByFieldsForStatistics"}

func read(k, s string) (string, any) {
	switch {
	case k == "f":
		s = strings.ToLower(s)
		return s, s
	case slices.Contains(text, k):
		return s, s
	case slices.Contains(booleans, k):
		switch b := strings.ToLower(strings.TrimSpace(s)); b {
		case "true", "false":
			return b, b == "true"
		}
		return s, s
	}
	return s, coerce(s)
}

func coerce(s string) any {
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

func (p Params) has(keys ...string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { _, ok := p[k]; return ok })
}

// Number is the parameter when it is a number.
func (p Params) Number(k string) (float64, bool) { f, ok := p[k].(float64); return f, ok }

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

// Bad is a 400 error; details default to none.
func Bad(msg string, details ...string) *Error {
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
			return Bad("Invalid format", `"f" must be one of [json, pjson, pbf, geojson]`)
		}
	}
	for _, k := range booleans {
		if v, ok := p[k]; ok && typeOf(v) != "boolean" {
			return Bad("", "'"+k+"' parameter is invalid")
		}
	}
	for _, k := range []string{"resultRecordCount", "resultOffset"} {
		if v, ok := p[k]; ok && typeOf(v) != "number" {
			m := `"` + k + `" must be a number`
			return Bad(m, m)
		}
		if n, ok := p.Number(k); ok && (!isInt(n) || n < 0) {
			return Bad("Cannot perform query. Invalid query parameters.", "'"+k+"' parameter is invalid")
		}
	}
	if v, ok := p["geometryPrecision"]; ok && typeOf(v) != "number" {
		return Bad("", "'geometryPrecision' parameter is invalid")
	}
	if w, _ := p.String("where"); strings.TrimSpace(w) == "" && !p.has("objectIds", "geometry", "outStatistics", "time") {
		return Bad("No where clause specified")
	}
	if v, ok := p["objectIds"]; ok {
		if f, num := v.(float64); typeOf(v) != "string" && !(num && isInt(f)) {
			m := `"objectIds" does not match any of the allowed types`
			return Bad(m, m)
		}
	}
	if v, ok := p["outSR"]; ok {
		if _, ok := geom.Wkid(v); !ok {
			m := `"outSR" does not match any of the allowed types`
			return Bad(m, m)
		}
	}
	if v, ok := p["quantizationParameters"]; ok {
		if _, ok := ParseQuantization(v); !ok {
			return Bad("'quantizationParameters' parameter is invalid", `"quantizationParameters" must be of type object`)
		}
	}
	if v, ok := p["spatialRel"]; ok {
		if s, _ := v.(string); !slices.Contains(spatialRels, s) {
			return Bad("'spatialRel' parameter is invalid", `"spatialRel" must be one of [`+strings.Join(spatialRels, ", ")+`]`)
		}
	}
	if v, ok := p["units"]; ok {
		if s, _ := v.(string); typeOf(v) != "number" && !geom.Unit(s) {
			return Bad("'units' parameter is invalid")
		}
	}
	if v, ok := p["outSR"]; ok {
		if id, _ := geom.Wkid(v); outputSRs[id].Wkid == 0 {
			return Bad("Unsupported spatial reference: "+strconv.Itoa(id), "Only 4326, 4269 and Web Mercator (102100, 3857, 102113, 3785) are supported")
		}
	}
	if v, ok := p["inSR"]; ok {
		if id, ok := geom.Wkid(v); ok && !supported(id) {
			return Bad("Unsupported spatial reference: "+strconv.Itoa(id), "Only 4326, 4269 and Web Mercator (102100, 3857, 102113, 3785, 900913) are supported")
		}
	}
	return nil
}

// spatialRels are the relationships the geometry filter honours, as the layer advertises.
var spatialRels = []string{"esriSpatialRelIntersects", "esriSpatialRelContains", "esriSpatialRelEnvelopeIntersects", "esriSpatialRelWithin"}

func typeOf(v any) string {
	switch v.(type) {
	case float64:
		return "number"
	case bool:
		return "boolean"
	case string:
		return "string"
	}
	return ""
}

func supported(id int) bool {
	switch id {
	case 4326, 4269, 102100, 102113, 3857, 900913, 3785:
		return true
	}
	return false
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
