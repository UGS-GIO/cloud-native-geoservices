package query

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/project"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/where"
)

// Response is a query result: what to write as JSON, and what PBF encodes when f=pbf.
type Response struct {
	JSON     any
	Count    *int64
	IDs      *IDs
	Features *FeatureSet
}

// IDs is a returnIdsOnly result.
type IDs struct {
	IDField string
	IDs     []any
}

// SpatialRef is an output spatial reference.
type SpatialRef struct {
	Wkid, Latest int
}

// AppendJSON implements jsonx.Appender.
func (s SpatialRef) AppendJSON(buf []byte) []byte {
	return jsonx.Append(buf, jsonx.Obj{{K: "wkid", V: s.Wkid}, {K: "latestWkid", V: s.Latest}})
}

// outputSRs is Koop's normalization of the response spatial reference; anything else is 4326.
var outputSRs = map[int]SpatialRef{102100: {102100, 3857}, 3857: {3857, 3857}, 102113: {102113, 3785},
	3785: {3785, 3785}, 4269: {4269, 4269}}

var selectAll = regexp.MustCompile(`1\s*=\s*1`)

// plusAsSpace turns + outside quotes into spaces, as Koop does for form-encoded where clauses.
func plusAsSpace(w string) string {
	b := []byte(strings.ReplaceAll(w, "''", "\x00\x00"))
	single, double := false, false
	for i, c := range b {
		switch {
		case c == '\'' && !double:
			single = !single
		case c == '"' && !single:
			double = !double
		case c == '+' && !single && !double:
			b[i] = ' '
		}
	}
	return strings.ReplaceAll(string(b), "\x00\x00", "''")
}

// Run answers a query over the provider's collection, as Koop's queryJson did.
func Run(c *Collection, p Params) (*Response, error) {
	f := p.Format()
	// Counts and extents the provider worked out from the file metadata.
	if c.Count != nil && p.True("returnCountOnly") {
		o := jsonx.Obj{{K: "count", V: *c.Count}}
		if p.True("returnExtentOnly") {
			o.Set("extent", c.Extent)
		}
		return &Response{JSON: o, Count: c.Count}, nil
	}
	if c.Extent != nil && p.True("returnExtentOnly") && !p.True("returnCountOnly") {
		return &Response{JSON: jsonx.Obj{{K: "extent", V: c.Extent}}}, nil
	}
	if f == "pbf" && p.True("returnExtentOnly") {
		return nil, Bad("Bad Request")
	}

	w, err := newWinnow(c, p)
	if err != nil {
		return nil, err
	}
	idx := w.filter()
	if c.Next != nil {
		// An unpaged count needs no rows kept, only tallied.
		tally := p.True("returnCountOnly") && w.limit == 0
		var n int64
		if idx, n, err = w.drain(idx, tally); err != nil {
			return nil, err
		}
		if tally {
			return &Response{JSON: jsonx.Obj{{K: "count", V: n}}, Count: &n}, nil
		}
	}

	if stats := w.aggregates; stats != nil {
		return w.statistics(idx)
	}
	if f == "geojson" {
		return &Response{JSON: jsonx.Obj{{K: "type", V: "FeatureCollection"}, {K: "features", V: w.geojson(idx)}}}, nil
	}
	feats, exceeded := w.esriFeatures(idx)
	if p.True("returnCountOnly") || p.True("returnExtentOnly") {
		n := int64(len(feats))
		o := jsonx.Obj{}
		if p.True("returnCountOnly") {
			o.Set("count", n)
		}
		if p.True("returnExtentOnly") {
			o.Set("extent", w.extentOf(feats))
		}
		return &Response{JSON: o, Count: &n}, nil
	}
	if p.True("returnIdsOnly") {
		ids := &IDs{IDField: c.Meta.IDField}
		ids.IDs = make([]any, len(feats))
		for i, ft := range feats {
			ids.IDs[i] = jsonx.Undefined
			for _, kv := range ft.Attrs {
				if kv.K == c.Meta.IDField {
					ids.IDs[i] = kv.V
				}
			}
		}
		return &Response{JSON: ids, IDs: ids}, nil
	}
	outFields, _ := p.String("outFields")
	fs := &FeatureSet{
		IDField:      c.Meta.IDField,
		GeometryType: esri.GeometryType(c.Meta.GeometryType),
		SR:           w.outSR,
		Fields:       esri.QueryFields(c.Meta, outFields),
		Features:     feats,
		Exceeded:     c.Exceeded,
	}
	if exceeded {
		fs.Exceeded = True
	}
	return &Response{JSON: fs, Features: fs}, nil
}

// winnow is one query's normalized options over a collection.
type winnow struct {
	c          *Collection
	p          Params
	where      where.Expr
	ids        []any
	idIndex    int
	spatial    *Spatial
	fields     []int // property indexes to output; nil means all
	order      []orderKey
	limit      int // resultRecordCount + 1, or 0
	offset     int
	distinct   bool
	toMercator bool
	outSR      SpatialRef
	geometry   bool // returnGeometry
	precision  *int
	aggregates []aggregate
	groupBy    []string
}

type orderKey struct {
	prop int
	desc bool
	name string
}

func newWinnow(c *Collection, p Params) (*winnow, error) {
	w := &winnow{c: c, p: p, idIndex: c.propIndex(c.Meta.IDField), geometry: true}
	applied := c.FiltersApplied

	whereStr := "1=1"
	if v, ok := p["where"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, Bad((&where.Error{Where: "must be a string if defined"}).Error())
		}
		whereStr = s
	}
	whereStr = plusAsSpace(whereStr)
	if !selectAll.MatchString(whereStr) && strings.TrimSpace(whereStr) != "" {
		e, err := where.Parse(whereStr, w.resolve)
		if err != nil {
			return nil, Bad(err.Error())
		}
		w.where = e
	}

	switch v := p["objectIds"].(type) {
	case string:
		for _, s := range strings.Split(v, ",") {
			if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && isInt(n) && strings.TrimSpace(s) != "" {
				w.ids = append(w.ids, n)
			} else {
				w.ids = append(w.ids, s)
			}
		}
	case float64:
		w.ids = []any{v}
	}

	if g, ok := p["geometry"]; ok {
		rel, _ := p.String("spatialRel")
		var err error
		if w.spatial, err = newSpatial(g, p["inSR"], rel); err != nil {
			return nil, err
		}
	}

	if p.True("returnIdsOnly") {
		w.fields = []int{w.idIndex}
	} else if s, ok := p.String("outFields"); ok && s != "*" {
		for _, name := range strings.Split(s, ",") {
			if i := c.propIndex(strings.TrimSpace(name)); i >= 0 {
				w.fields = append(w.fields, i)
			}
		}
		if w.fields == nil {
			w.fields = []int{}
		}
	}

	if s, ok := p.String("orderByFields"); ok && !applied["orderByFields"] {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			name, dir, _ := strings.Cut(part, " ")
			w.order = append(w.order, orderKey{c.propIndex(name), strings.EqualFold(dir, "DESC"), name})
		}
	}
	if n, ok := p.Number("resultRecordCount"); ok && !applied["resultRecordCount"] && !applied["limit"] && isInt(n) && n != 0 {
		w.limit = int(n) + 1
	}
	if n, ok := p.Number("resultOffset"); ok && w.limit != 0 && !applied["offset"] && n > 0 {
		w.offset = int(n)
	}
	w.distinct = p.True("returnDistinctValues")

	outID := 4326
	if v, ok := p["outSR"]; ok {
		outID, _ = wkid(v)
	}
	w.outSR = outputSRs[outID]
	if w.outSR.Wkid == 0 {
		w.outSR = SpatialRef{4326, 4326}
	}
	w.toMercator = geom.Mercator[outID] && outID != 0
	if b, ok := p["returnGeometry"].(bool); ok && !b {
		w.geometry = false
	}
	if n, ok := p.Number("geometryPrecision"); ok && n != 0 {
		// Koop's Number.prototype.toFixed throws outside 0..100; past it, formatting is unbounded work.
		if n < 0 || n > 100 {
			return nil, Bad("geometryPrecision must be between 0 and 100")
		}
		v := int(n)
		w.precision = &v
	}
	if err := w.parseStatistics(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *winnow) resolve(name string) (int, bool) {
	for i, prop := range w.c.Props {
		if strings.EqualFold(prop, name) {
			return i, true
		}
	}
	return 0, false
}

// filter returns the indexes of the features that pass where, objectIds and the geometry
// filter, ordered and paged (after grouping, statistics apply their own ordering).
func (w *winnow) filter() []int {
	var idx []int
	for i := range w.c.Features {
		ft := &w.c.Features[i]
		if w.where != nil && !where.Match(w.where, func(f int) any { return ft.Props[f] }) {
			continue
		}
		if w.ids != nil && !w.idMatch(ft) {
			continue
		}
		if w.spatial != nil && !ft.Known && !w.spatial.Match(ft.Geom) {
			continue
		}
		idx = append(idx, i)
	}
	return idx
}

// drain filters the provider's further chunks too, keeping what passes without its geometry, or
// with tally set only counting it.
func (w *winnow) drain(idx []int, tally bool) ([]int, int64, error) {
	var kept []Feature
	n := int64(0)
	for {
		n += int64(len(idx))
		for _, i := range idx {
			if !tally {
				kept = append(kept, Feature{Props: w.c.Features[i].Props})
			}
		}
		chunk, err := w.c.Next()
		if err != nil {
			return nil, 0, err
		}
		if chunk == nil {
			w.c.Features = kept
			idx = make([]int, len(kept))
			for i := range idx {
				idx[i] = i
			}
			return idx, n, nil
		}
		w.c.Features = chunk
		idx = w.filter()
	}
}

func (w *winnow) idMatch(ft *Feature) bool {
	if w.idIndex < 0 {
		return false
	}
	f, ok := js.AsNumber(ft.Props[w.idIndex])
	if !ok {
		return false
	}
	for _, id := range w.ids {
		if n, ok := id.(float64); ok && n == f {
			return true
		}
	}
	return false
}

// outGeometry projects and rounds a feature's geometry for output.
func (w *winnow) outGeometry(g orb.Geometry) orb.Geometry {
	if g == nil {
		return nil
	}
	// g may be a cached shape: project.Geometry works in place, so it gets one copy.
	if w.toMercator || w.precision != nil {
		g = orb.Clone(g)
	}
	if w.toMercator {
		g = project.Geometry(g, geom.ToMercator)
	}
	if w.precision != nil {
		d := *w.precision
		g = project.Geometry(g, func(p orb.Point) orb.Point { return orb.Point{toFixed(p[0], d), toFixed(p[1], d)} })
	}
	return g
}

func toFixed(f float64, d int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', max(d, 0), 64), 64)
	return v
}

// attrs are a feature's output attributes, in property order (or outFields order).
func (w *winnow) attrs(ft *Feature) []jsonx.KV {
	if w.fields == nil {
		out := make([]jsonx.KV, len(w.c.Props))
		for i, name := range w.c.Props {
			out[i] = jsonx.KV{K: name, V: ft.Props[i]}
		}
		return out
	}
	out := make([]jsonx.KV, len(w.fields))
	for i, f := range w.fields {
		out[i] = jsonx.KV{K: w.c.Props[f], V: ft.Props[f]}
	}
	return out
}

// esriFeatures shapes the selected features as Esri JSON features: distinct, ordered, paged.
func (w *winnow) esriFeatures(idx []int) ([]EsriFeature, bool) {
	idx = w.sorted(idx)
	out := make([]EsriFeature, 0, len(idx))
	for _, i := range idx {
		ft := &w.c.Features[i]
		ef := EsriFeature{Attrs: w.attrs(ft), HasGeom: w.geometry}
		if w.geometry && ft.Geom != nil {
			if e, ok := geom.ToEsri(w.outGeometry(ft.Geom)); ok {
				ef.Geom = &e
			}
		}
		out = append(out, ef)
	}
	if w.distinct {
		out = distinct(out, EsriFeature.AppendJSON)
	}
	return page(w, out)
}

func page[T any](w *winnow, out []T) ([]T, bool) {
	if w.limit == 0 {
		return out, false
	}
	from := min(len(out), w.offset)
	to := min(len(out), w.offset+w.limit)
	out = out[from:to]
	if len(out) == w.limit {
		return out[:len(out)-1], true
	}
	return out, false
}

// distinct drops the items whose JSON an earlier item already has.
func distinct[T any](xs []T, appendJSON func(T, []byte) []byte) []T {
	seen := map[string]bool{}
	var buf []byte
	return slices.DeleteFunc(xs, func(x T) bool {
		buf = appendJSON(x, buf[:0])
		if seen[string(buf)] {
			return true
		}
		seen[string(buf)] = true
		return false
	})
}

// geojson is the f=geojson feature list.
func (w *winnow) geojson(idx []int) []any {
	idx = w.sorted(idx)
	out := make([]any, 0, len(idx))
	for _, i := range idx {
		ft := &w.c.Features[i]
		props := jsonx.Obj(w.attrs(ft))
		o := jsonx.Obj{}
		if w.fields == nil {
			o = append(o, jsonx.KV{K: "type", V: "Feature"})
		}
		o = append(o, jsonx.KV{K: "properties", V: props})
		if w.geometry {
			if g := w.outGeometry(ft.Geom); g != nil {
				o = append(o, jsonx.KV{K: "geometry", V: geom.GeoJSON{G: g}})
			} else {
				o = append(o, jsonx.KV{K: "geometry", V: nil})
			}
		}
		out = append(out, o)
	}
	if w.distinct {
		out = distinct(out, func(o any, b []byte) []byte { return jsonx.Append(b, o) })
	}
	out, _ = page(w, out)
	return out
}

// extentOf is esri-extent over output features.
func (w *winnow) extentOf(fs []EsriFeature) jsonx.Obj {
	b := geom.Bounds(nil)
	for _, f := range fs {
		if f.Geom == nil {
			continue
		}
		if f.Geom.Kind == geom.EsriPoint {
			b = b.Extend(orb.Point{f.Geom.X, f.Geom.Y})
		}
		for _, part := range f.Geom.Parts {
			for _, pt := range part {
				b = b.Extend(pt)
			}
		}
	}
	return Extent(jsonx.Obj{{K: "wkid", V: w.outSR.Wkid}}, b.Min[0], b.Min[1], b.Max[0], b.Max[1])
}

// Extent is an Esri envelope in spatial reference sr.
func Extent(sr jsonx.Obj, xmin, ymin, xmax, ymax float64) jsonx.Obj {
	return jsonx.Obj{{K: "xmin", V: xmin}, {K: "ymin", V: ymin}, {K: "xmax", V: xmax}, {K: "ymax", V: ymax},
		{K: "spatialReference", V: sr}}
}
