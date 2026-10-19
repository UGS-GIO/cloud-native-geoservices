package query

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/js"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/pq"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/where"
)

// Response is a query result: what to write as JSON, and what PBF encodes when f=pbf.
type Response struct {
	JSON     any
	Count    *int64
	IDs      *IDs
	Features *FeatureSet
	Extent   jsonx.Obj // a returnExtentOnly envelope
	geoJSON  bool      // JSON is already the f=geojson answer
}

// IDs is a returnIdsOnly result.
type IDs struct {
	IDField  string
	IDs      []any
	Exceeded bool // a page that has more after it
}

// SpatialRef is an output spatial reference.
type SpatialRef struct {
	Wkid, Latest int
}

// AppendJSON implements jsonx.Appender.
func (s SpatialRef) AppendJSON(buf []byte) []byte {
	return jsonx.Append(buf, jsonx.Obj{{K: "wkid", V: s.Wkid}, {K: "latestWkid", V: s.Latest}})
}

// outputSRs are the spatial references a response can be in, labeled as ArcGIS Online labels them.
var outputSRs = map[int]SpatialRef{0: {4326, 4326}, 4326: {4326, 4326}, 4269: {4269, 4269},
	102100: {102100, 3857}, 3857: {102100, 3857}, 102113: {102113, 3785}, 3785: {102113, 3785}}

// project is how a lon/lat point becomes a point in s, nil when it stays as it is.
func (s SpatialRef) project() orb.Projection {
	switch s.Wkid {
	case 102100, 102113:
		return geom.ToMercator
	case 4269:
		return geom.ToNAD83
	}
	return nil
}

// Envelope is a lon/lat bounding box (empty when inverted) in s. Web Mercator clamps latitude to
// ±85.0511.
func (s SpatialRef) Envelope(b [4]float64) jsonx.Obj {
	switch {
	case b[0] > b[2]:
	case s.Wkid == 102100 || s.Wkid == 102113:
		b[0], b[1] = geom.ExtentToMercator(b[0], b[1])
		b[2], b[3] = geom.ExtentToMercator(b[2], b[3])
	case s.Wkid == 4269:
		ring := orb.Ring{{b[0], b[1]}, {b[0], b[3]}, {b[2], b[3]}, {b[2], b[1]}}
		e := geom.Projected(ring, geom.ToNAD83).Bound()
		b = [4]float64{e.Min[0], e.Min[1], e.Max[0], e.Max[1]}
	}
	return jsonx.Obj{{K: "xmin", V: b[0]}, {K: "ymin", V: b[1]}, {K: "xmax", V: b[2]}, {K: "ymax", V: b[3]},
		{K: "spatialReference", V: s}}
}

// OutputSR is the spatial reference a response to outSR wkid (0 when not given) is in.
func OutputSR(wkid int) SpatialRef { return outputSRs[wkid] }

// selectAll is a where that is only 1=1, which needs no evaluating.
var selectAll = regexp.MustCompile(`^\s*1\s*=\s*1\s*$`)

// Run answers a query over the provider's collection.
func Run(c *Collection, p Params) (*Response, error) {
	res, err := answer(c, p)
	if err != nil || p.Format() != "geojson" || res.geoJSON {
		return res, err
	}
	res.JSON = res.featureCollection(p)
	return res, nil
}

func answer(c *Collection, p Params) (*Response, error) {
	// Counts and extents the provider worked out from the file metadata.
	if c.Count != nil && p.True("returnCountOnly") && !p.True("returnExtentOnly") {
		return &Response{JSON: jsonx.Obj{{K: "count", V: *c.Count}}, Count: c.Count}, nil
	}
	if c.Extent != nil && p.True("returnExtentOnly") {
		var n *int64
		if p.True("returnCountOnly") {
			n = c.Count
		}
		sr, _ := c.Extent.Get("spatialReference")
		return extentResult(c.Meta, sr.(SpatialRef), c.Extent, n), nil
	}

	w, err := newWinnow(c, p)
	if err != nil {
		return nil, err
	}
	if p.True("returnCountOnly") || p.True("returnExtentOnly") {
		w.limit, w.offset = 0, 0 // AGOL counts and covers every match, not a page
	}
	idx, err := w.filter()
	if err != nil {
		return nil, err
	}
	if c.Next != nil {
		// An unpaged count needs no rows kept, only tallied.
		tally := p.True("returnCountOnly") && !p.True("returnExtentOnly")
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
	if p.Format() == "geojson" && !p.True("returnCountOnly") && !p.True("returnExtentOnly") && !p.True("returnIdsOnly") {
		feats, exceeded := w.geojson(idx)
		o := jsonx.Obj{{K: "type", V: "FeatureCollection"}}
		if _, ok := p["outSR"]; ok {
			o = append(o, jsonx.KV{K: "crs", V: crs(w.outSR)})
		}
		if exceeded || c.Exceeded {
			o = append(o, jsonx.KV{K: "properties", V: jsonx.Obj{{K: "exceededTransferLimit", V: true}}})
		}
		return &Response{JSON: append(o, jsonx.KV{K: "features", V: feats}), geoJSON: true}, nil
	}
	feats, exceeded := w.esriFeatures(idx)
	if p.True("returnExtentOnly") {
		var n *int64
		if p.True("returnCountOnly") {
			n = new(int64(len(feats)))
		}
		return extentResult(c.Meta, w.outSR, w.extentOf(idx), n), nil
	}
	if p.True("returnCountOnly") {
		n := int64(len(feats))
		return &Response{JSON: jsonx.Obj{{K: "count", V: n}}, Count: &n}, nil
	}
	if p.True("returnIdsOnly") {
		ids := &IDs{IDField: c.Meta.IDField, Exceeded: exceeded}
		ids.IDs = make([]any, len(feats))
		for i, ft := range feats {
			ids.IDs[i] = nil
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
	fs.Exceeded = fs.Exceeded || exceeded
	if v, ok := p["quantizationParameters"]; ok && w.geometry {
		if q, err := ParseQuantization(v); err == nil {
			fs.Transform = q.Transform(w.outSR, c.Meta.Extent)
		}
	}
	return &Response{JSON: fs, Features: fs}, nil
}

// extentResult is an extent (null when nil), with a count when n is set. As f=pbf, AGOL sends it as a polygon
// feature result whose one feature is the extent's ring, with the count as an attribute.
func extentResult(m *esri.Metadata, sr SpatialRef, ext jsonx.Obj, n *int64) *Response {
	fs := &FeatureSet{IDField: m.IDField, GeometryType: "esriGeometryPolygon", SR: sr, Features: []EsriFeature{{}}}
	o := jsonx.Obj{}
	if n != nil {
		o = append(o, jsonx.KV{K: "count", V: *n})
		fs.Fields = []esri.Field{{Name: "count", Type: "esriFieldTypeInteger"}}
		fs.Features[0].Attrs = []jsonx.KV{{K: "count", V: *n}}
	}
	if b, ok := box(ext); ok {
		ring := []orb.Point{{b[0], b[1]}, {b[0], b[3]}, {b[2], b[3]}, {b[2], b[1]}, {b[0], b[1]}}
		fs.Features[0].Geom = &geom.Esri{Kind: geom.EsriPolygon, Parts: [][]orb.Point{ring}}
	}
	var v any
	if ext != nil {
		v = ext
	}
	return &Response{JSON: append(o, jsonx.KV{K: "extent", V: v}), Count: n, Extent: ext, Features: fs}
}

// box is an envelope's corners, false when it is empty.
func box(ext jsonx.Obj) ([4]float64, bool) {
	var b [4]float64
	if ext == nil {
		return b, false
	}
	for i, k := range []string{"xmin", "ymin", "xmax", "ymax"} {
		v, _ := ext.Get(k)
		b[i], _ = v.(float64)
	}
	ok := b[0] <= b[2] && b[1] <= b[3] && !math.IsInf(b[0], 0) && !math.IsInf(b[2], 0)
	return b, ok
}

// crs names a GeoJSON answer's spatial reference, as AGOL does.
func crs(sr SpatialRef) jsonx.Obj {
	return jsonx.Obj{{K: "type", V: "name"}, {K: "properties", V: jsonx.Obj{{K: "name", V: "EPSG:" + strconv.Itoa(sr.Latest)}}}}
}

// featureCollection is the f=geojson form of a count, ids, extent or statistics answer.
func (res *Response) featureCollection(p Params) jsonx.Obj {
	o := jsonx.Obj{{K: "type", V: "FeatureCollection"}}
	id, _ := geom.Wkid(p["outSR"])
	if _, ok := p["outSR"]; ok || res.Extent != nil {
		o = append(o, jsonx.KV{K: "crs", V: crs(OutputSR(id))})
	}
	if b, ok := box(res.Extent); ok {
		o = append(o, jsonx.KV{K: "bbox", V: b[:]})
	}
	props := jsonx.Obj{}
	if res.Count != nil {
		props = append(props, jsonx.KV{K: "count", V: *res.Count})
	}
	if res.IDs != nil {
		props = append(props, jsonx.KV{K: "objectIdFieldName", V: res.IDs.IDField}, jsonx.KV{K: "objectIds", V: res.IDs.IDs})
	}
	if len(props) > 0 {
		o = append(o, jsonx.KV{K: "properties", V: props})
	}
	feats := []any{}
	if res.Features != nil && res.Extent == nil {
		for _, f := range res.Features.Features {
			feats = append(feats, jsonx.Obj{{K: "type", V: "Feature"}, {K: "geometry", V: nil}, {K: "properties", V: jsonx.Obj(f.Attrs)}})
		}
	}
	return append(o, jsonx.KV{K: "features", V: feats})
}

// winnow is one query's normalized options over a collection.
type winnow struct {
	c          *Collection
	p          Params
	where      *where.Clause
	ids        []any
	idIndex    int
	spatial    *Spatial
	fields     []int // property indexes to output; nil means all
	order      []orderKey
	limit      int // resultRecordCount + 1, or 0
	offset     int
	distinct   bool
	outSR      SpatialRef
	toSR       orb.Projection // outSR's projection, nil for lon/lat
	geometry   bool           // returnGeometry
	precision  *int
	scale      float64                   // without geometryPrecision, coordinates round to 1/scale; 0 keeps them
	project    func(orb.Point) orb.Point // outGeometry's, once worked out
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
		units, _ := p.String("units")
		if w.spatial, err = newSpatial(g, p["inSR"], rel, p["distance"], units); err != nil {
			return nil, err
		}
	}

	if p.True("returnIdsOnly") {
		w.fields = []int{w.idIndex}
	} else if s, ok := p.String("outFields"); ok && s != "*" {
		w.fields = []int{}
		for _, name := range strings.Split(s, ",") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			_, i, ok := c.field(name)
			if !ok {
				return nil, Bad("'outFields' parameter is invalid")
			}
			if i >= 0 {
				w.fields = append(w.fields, i)
			}
		}
	}

	if err := w.parseStatistics(); err != nil {
		return nil, err
	}
	if s, ok := p.String("orderByFields"); ok && !applied["orderByFields"] {
		for _, part := range strings.Split(s, ",") {
			words := strings.Fields(part)
			if len(words) == 0 {
				continue
			}
			k := orderKey{prop: -1, name: words[0]}
			if len(words) > 2 || len(words) == 2 && !strings.EqualFold(words[1], "ASC") && !strings.EqualFold(words[1], "DESC") {
				return nil, Bad("'" + strings.TrimSpace(part) + "' parameter is invalid")
			}
			k.desc = len(words) == 2 && strings.EqualFold(words[1], "DESC")
			if f, i, ok := c.field(k.name); ok {
				k.name, k.prop = f.Name, i
			} else if !w.outputName(k.name) {
				return nil, Bad("'" + k.name + "' parameter is invalid")
			}
			w.order = append(w.order, k)
		}
	}
	if n, ok := p.Number("resultRecordCount"); ok && !applied["resultRecordCount"] && !applied["limit"] && isInt(n) && n != 0 {
		w.limit = int(n) + 1
	}
	// As on AGOL, a page of features is at most maxRecordCount, with or without geometry.
	features := !p.True("returnCountOnly") && !p.True("returnIdsOnly") && !p.True("returnExtentOnly") && w.aggregates == nil
	maxRecords := c.Meta.MaxRecordCount
	if s, _ := p.String("resultType"); s == "tile" {
		maxRecords = c.Meta.TileMaxRecordCount
	}
	if features && maxRecords > 0 && !applied["limit"] && (w.limit == 0 || w.limit > maxRecords+1) {
		w.limit = maxRecords + 1
	}
	if n, ok := p.Number("resultOffset"); ok && w.limit != 0 && !applied["offset"] && n > 0 {
		w.offset = int(n)
	}
	w.distinct = p.True("returnDistinctValues")

	outID := 4326
	if v, ok := p["outSR"]; ok {
		outID, _ = geom.Wkid(v)
	}
	w.outSR = outputSRs[outID]
	w.toSR = w.outSR.project()
	if b, ok := p["returnGeometry"].(bool); ok && !b {
		w.geometry = false
	}
	// AGOL rounds a negative precision as 0, and past 20 digits keeps full precision.
	n, set := p.Number("geometryPrecision")
	if set && n <= 20 {
		v := max(int(n), 0)
		w.precision = &v
	} else if _, q := p["quantizationParameters"]; !set && c.PrecisionMM > 0 && p.Format() != "pbf" && !q {
		perMM := 1e3 / c.PrecisionMM // a millimeter is 1e-3 m, or about 1e-8 degrees
		if !geom.Mercator[w.outSR.Wkid] {
			perMM *= 111320
		}
		w.scale = math.Pow(10, math.Round(math.Log10(perMM)))
	}
	return w, nil
}

func (w *winnow) resolve(name string) (int, string, bool) {
	f, i, ok := w.c.field(name)
	return i, f.Type, ok && i >= 0
}

// filter returns the indexes of the features that pass where, objectIds and the geometry
// filter, ordered and paged (after grouping, statistics apply their own ordering).
func (w *winnow) filter() ([]int, error) {
	var idx []int
	for i := range w.c.Features {
		ft := &w.c.Features[i]
		if w.where != nil && !w.where.Match(func(f int) any { return ft.Props[f] }) {
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
	if w.where != nil && w.where.Err() != nil {
		return nil, Bad(w.where.Err().Error())
	}
	return idx, nil
}

// drain filters the provider's further chunks too, keeping what passes without its geometry (an
// extent keeps its bounds), or with tally set only counting it.
func (w *winnow) drain(idx []int, tally bool) ([]int, int64, error) {
	var kept []Feature
	n := int64(0)
	for {
		n += int64(len(idx))
		for _, i := range idx {
			if !tally {
				ft := Feature{Props: w.c.Features[i].Props}
				if g := w.c.Features[i].Geom; g != nil && w.p.True("returnExtentOnly") {
					ft.Geom = geom.Bounds(g).ToRing()
				}
				kept = append(kept, ft)
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
		if idx, err = w.filter(); err != nil {
			return nil, 0, err
		}
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
	if w.project == nil {
		w.project = w.outPoint
		if w.precision == nil && w.scale == 0 {
			w.project = w.toSR
		}
		if w.project == nil {
			return g
		}
	}
	// g may be a cached shape, so it is projected into a copy.
	return geom.Projected(g, w.project)
}

func (w *winnow) outPoint(p orb.Point) orb.Point {
	if w.toSR != nil {
		p = w.toSR(p)
	}
	if w.precision != nil {
		p = orb.Point{toFixed(p[0], *w.precision), toFixed(p[1], *w.precision)}
	} else if w.scale > 0 {
		p = orb.Point{math.Round(p[0]*w.scale) / w.scale, math.Round(p[1]*w.scale) / w.scale}
	}
	return p
}

func toFixed(f float64, d int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', d, 64), 64)
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

// geojson is the f=geojson feature list: each feature has an id when the id field is output, and
// a null geometry when returnGeometry is false.
func (w *winnow) geojson(idx []int) ([]any, bool) {
	idx = w.sorted(idx)
	withID := w.idIndex >= 0 && (w.fields == nil || slices.Contains(w.fields, w.idIndex))
	out := make([]any, 0, len(idx))
	for _, i := range idx {
		ft := &w.c.Features[i]
		o := jsonx.Obj{{K: "type", V: "Feature"}}
		if withID {
			o = append(o, jsonx.KV{K: "id", V: ft.Props[w.idIndex]})
		}
		var g any
		if w.geometry {
			if og := w.outGeometry(ft.Geom); og != nil {
				g = geom.GeoJSON{G: og}
			}
		}
		out = append(out, append(o, jsonx.KV{K: "geometry", V: g}, jsonx.KV{K: "properties", V: jsonx.Obj(w.attrs(ft))}))
	}
	if w.distinct {
		out = distinct(out, func(o any, b []byte) []byte { return jsonx.Append(b, o) })
	}
	return page(w, out)
}

// extentOf covers the selected features: from the bbox covering columns when the provider read
// them, else from their geometry. It is null when nothing has a geometry.
func (w *winnow) extentOf(idx []int) jsonx.Obj {
	b := geom.Bounds(nil)
	cols := make([]int, len(pq.BBox))
	for k, name := range pq.BBox {
		cols[k] = w.c.propIndex(name)
	}
	for _, i := range idx {
		ft := &w.c.Features[i]
		if cols[0] < 0 {
			if ft.Geom != nil {
				b = b.Union(geom.Bounds(ft.Geom))
			}
			continue
		}
		var v [4]float64
		ok := true
		for k, c := range cols {
			if v[k], ok = ft.Props[c].(float64); !ok {
				break
			}
		}
		if ok {
			b = b.Union(orb.Bound{Min: orb.Point{v[0], v[1]}, Max: orb.Point{v[2], v[3]}})
		}
	}
	if b.Min[0] > b.Max[0] {
		return nil
	}
	return w.outSR.Envelope([4]float64{b.Min[0], b.Min[1], b.Max[0], b.Max[1]})
}
