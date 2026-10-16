// Package plan turns GeoServices query params into what to read from a layer's GeoParquet.
//
// Ours: the columns a request needs, a bbox prefilter on the GeoParquet covering columns,
// objectIds, simplification, and paging when `where` is trivial. The query package applies
// everything else (exact geometry tests, where, statistics, projection, PBF) to what comes back.
package plan

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
)

// Kind is what a request needs from the file.
type Kind int

const (
	Features Kind = iota
	Count         // the row count from the footer
	Extent        // the extent from the footer
)

// Layer is what planning needs to know about a file.
type Layer struct {
	Columns []pq.Column
	Point   bool
}

// Plan is what to read.
type Plan struct {
	Kind           Kind
	Box            *[4]float64 // lon/lat prefilter
	IDs            []int64
	OutSR          int // 0 when not given
	Columns        []string
	Geometry       bool
	Present        bool    // only rows with a geometry count, and the geometry itself is not read
	Tolerance      float64 // degrees; 0 means none
	Page           *pq.Page
	FiltersApplied map[string]bool
}

// Error is a request the plan rejects.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func parseJSON(s string) any {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

var digits = regexp.MustCompile(`^\d+$`)

// WkidOf reads a spatial reference: a number, digits, or {latestWkid, wkid}; 0 when absent.
func WkidOf(v any) int {
	if s, ok := v.(string); ok {
		v = parseJSON(s)
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		if digits.MatchString(x) {
			n, _ := strconv.Atoi(x)
			return n
		}
	case map[string]any:
		if w, ok := x["latestWkid"].(float64); ok {
			return int(w)
		}
		if w, ok := x["wkid"].(float64); ok {
			return int(w)
		}
	}
	return 0
}

func isTrue(v string) bool { return v == "true" }

// bboxOf is any Esri geometry (or "xmin,ymin,xmax,ymax") as [xmin, ymin, xmax, ymax] in its SR.
func bboxOf(geometry string) ([4]float64, map[string]any, bool) {
	g := parseJSON(geometry)
	switch x := g.(type) {
	case string:
		parts := strings.Split(x, ",")
		if len(parts) != 4 {
			return [4]float64{}, nil, false
		}
		var b [4]float64
		for i, p := range parts {
			b[i] = js.ParseNumber(p)
			if math.IsNaN(b[i]) || math.IsInf(b[i], 0) {
				return b, nil, false
			}
		}
		return b, nil, true
	case map[string]any:
		if _, ok := x["xmin"]; ok {
			return [4]float64{js.Number(x["xmin"]), js.Number(x["ymin"]), js.Number(x["xmax"]), js.Number(x["ymax"])}, x, true
		}
		if _, ok := x["x"]; ok {
			return [4]float64{js.Number(x["x"]), js.Number(x["y"]), js.Number(x["x"]), js.Number(x["y"])}, x, true
		}
		b := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
		n := 0
		add := func(v any) {
			for _, p := range asList(v) {
				xy := asList(p)
				if len(xy) < 2 {
					continue
				}
				px, py := js.Number(xy[0]), js.Number(xy[1])
				b[0], b[1] = math.Min(b[0], px), math.Min(b[1], py)
				b[2], b[3] = math.Max(b[2], px), math.Max(b[3], py)
				n++
			}
		}
		add(x["points"])
		for _, k := range []string{"paths", "rings"} {
			for _, part := range asList(x[k]) {
				add(part)
			}
		}
		return b, x, n > 0
	}
	return [4]float64{}, nil, false
}

func asList(v any) []any { l, _ := v.([]any); return l }

var meters = map[string]float64{"esriSRUnit_Meter": 1, "esriSRUnit_Kilometer": 1000, "esriSRUnit_Foot": 0.3048,
	"esriSRUnit_StatuteMile": 1609.344, "esriSRUnit_NauticalMile": 1852}

// FilterBox is the request's spatial filter as a lon/lat box, grown by `distance` when given.
func FilterBox(params map[string]string) *[4]float64 {
	g, ok := params["geometry"]
	if !ok || g == "" {
		return nil
	}
	box, obj, ok := bboxOf(g)
	if !ok {
		return nil
	}
	for _, v := range box {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	}
	wkid := 0
	if s, ok := params["inSR"]; ok {
		wkid = WkidOf(s)
	}
	if wkid == 0 && obj != nil {
		wkid = WkidOf(obj["spatialReference"])
	}
	if wkid == 0 {
		wkid = 4326
	}
	if geom.Mercator[wkid] {
		box[0], box[1] = geom.BoxToLonLat(box[0], box[1])
		box[2], box[3] = geom.BoxToLonLat(box[2], box[3])
	} else if wkid != 4326 && wkid != 4269 {
		return nil // an SR we don't project: no prefilter, the exact test still filters
	}
	unit, ok := meters[params["units"]]
	if !ok {
		unit = 1
	}
	pad := 0.0
	if d := params["distance"]; d != "" {
		pad = js.ParseNumber(d) * unit / 111320
	}
	return &[4]float64{box[0] - pad, box[1] - pad, box[2] + pad, box[3] + pad}
}

var trivial = regexp.MustCompile(`(?i)^\s*(1\s*=\s*1)?\s*$`)
var featureIDOrder = regexp.MustCompile(`(?i)^\s*feature_id(\s+asc)?\s*$`)
var nonWord = regexp.MustCompile(`[^\w]`)

// TrivialWhere reports a where that selects everything.
func TrivialWhere(params map[string]string) bool {
	w, ok := params["where"]
	return !ok || trivial.MatchString(w)
}

// New plans a request. maxRecords caps a page.
func New(params map[string]string, layer Layer, maxRecords int64) (*Plan, error) {
	p := &Plan{Box: FilterBox(params), FiltersApplied: map[string]bool{}}
	for _, s := range strings.Split(params["objectIds"], ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n := js.ParseNumber(s)
		if n != math.Trunc(n) || math.IsInf(n, 0) {
			return nil, &Error{"objectIds must be integers"}
		}
		p.IDs = append(p.IDs, int64(n))
	}
	simple := TrivialWhere(params)
	if s, ok := params["outSR"]; ok {
		p.OutSR = WkidOf(s)
	}
	countOnly, extentOnly := isTrue(params["returnCountOnly"]), isTrue(params["returnExtentOnly"])
	if simple && p.Box == nil && len(p.IDs) == 0 && countOnly && !extentOnly {
		p.Kind = Count
		return p, nil
	}
	if simple && p.Box == nil && len(p.IDs) == 0 && extentOnly {
		p.Kind = Extent
		return p, nil
	}

	// Columns: the requested outFields plus anything where, ordering or statistics read.
	outFields, ok := params["outFields"]
	if !ok {
		outFields = "*"
	}
	var out []string
	for _, f := range strings.Split(outFields, ",") {
		out = append(out, strings.ToLower(strings.TrimSpace(f)))
	}
	var mentions []string
	for _, k := range []string{"where", "orderByFields", "groupByFieldsForStatistics", "outStatistics"} {
		if v := params[k]; v != "" {
			mentions = append(mentions, v)
		}
	}
	m := strings.Join(mentions, " ")
	for _, c := range layer.Columns {
		keep := c.Name == "feature_id" || slices.Contains(out, "*") || slices.Contains(out, strings.ToLower(c.Name))
		if !keep && m != "" {
			re, err := regexp.Compile(`(?i)\b` + nonWord.ReplaceAllString(c.Name, "") + `\b`)
			keep = err == nil && re.MatchString(m)
		}
		if keep {
			p.Columns = append(p.Columns, c.Name)
		}
	}

	// Anything under a spatial filter still needs geometry, for the exact test; the response
	// leaves it out when returnGeometry is false. Statistics return none, so without a filter they
	// read none and cover the whole layer, not the rows a geometry read is capped at.
	stats := params["outStatistics"] != ""
	if isTrue(params["returnIdsOnly"]) || countOnly || stats || params["returnGeometry"] == "false" {
		p.Geometry = p.Box != nil
	} else {
		p.Geometry = true
	}
	p.Present = stats && !p.Geometry
	offset := math.NaN()
	if s, ok := params["maxAllowableOffset"]; ok {
		offset = js.ParseNumber(s)
	}
	if p.Geometry && !layer.Point && !math.IsNaN(offset) && !math.IsInf(offset, 0) && offset > 0 {
		out := p.OutSR
		if out == 0 {
			out = 4326
		}
		if geom.Mercator[out] {
			p.Tolerance = offset / 111320
		} else {
			p.Tolerance = offset
		}
	}

	// Paging is ours when there is no where left to filter by. Rows are stored in feature_id
	// order, so a page of rows is a page of features. Like ArcGIS Server, a page is at most
	// maxRecords, which also bounds what one request holds in memory.
	limit := maxRecords
	if s, ok := params["resultRecordCount"]; ok {
		if asked := js.ParseNumber(s); asked == math.Trunc(asked) && asked > 0 && !math.IsInf(asked, 0) {
			limit = min(int64(asked), maxRecords)
		}
	}
	order, hasOrder := params["orderByFields"]
	ordered := !hasOrder || order == "" || featureIDOrder.MatchString(order)
	if simple && ordered && !countOnly && !isTrue(params["returnIdsOnly"]) && params["outStatistics"] == "" {
		off := js.ParseNumber(params["resultOffset"])
		if math.IsNaN(off) || off < 0 {
			off = 0
		}
		p.Page = &pq.Page{Offset: int64(off), Limit: limit}
		for _, k := range []string{"offset", "limit", "resultRecordCount", "orderByFields"} {
			p.FiltersApplied[k] = true
		}
	}
	return p, nil
}
