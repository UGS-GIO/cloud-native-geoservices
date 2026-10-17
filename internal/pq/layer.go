// Package pq reads a layer's GeoParquet over https, fetching only what a request needs.
//
// Row groups are pruned on the bbox covering columns' statistics; inside a row group, the small
// bbox columns pick the matching rows, and where the file has a page index only the pages holding
// them are fetched (the warehouse writes ~100-row pages; vector/sink_archive.py). Decoded values
// stay in a cache per file, column and row group (or page), so a warm request skips zstd and
// parquet decoding.
package pq

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"slices"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/deprecated"
	"github.com/parquet-go/parquet-go/format"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/remote"
)

// Our GeoParquet columns that are not attributes: the covering and the source CRS.
var internal = map[string]bool{"bbox": true, "bbox_xmin": true, "bbox_ymin": true,
	"bbox_xmax": true, "bbox_ymax": true, "target_epsg": true}

// BBox are the covering columns, in xmin, ymin, xmax, ymax order.
var BBox = []string{"bbox_xmin", "bbox_ymin", "bbox_xmax", "bbox_ymax"}

// Column is one attribute: its name and the Esri type the provider declares for it.
type Column struct {
	Name string
	Type string // Integer, Double, Date or String
}

// Group is one row group: its rows and the bounds its statistics give.
type Group struct {
	Start, Rows            int64
	Xmin, Ymin, Xmax, Ymax float64
	IDMin, IDMax           float64
}

type leaf struct {
	index int
	kind  Kind
	conv  func(b *builder, v parquet.Value)
}

// Layer is one opened data file.
type Layer struct {
	URL          string
	file         *remote.File
	pf           *parquet.File
	Geom         string
	HasBBox      bool
	Columns      []Column
	Groups       []Group
	Extent       *[4]float64
	GeometryType string // Point, LineString or Polygon
	Point        bool
	Rows         int64
	IDIsRow      bool // feature_id is row number + 1 everywhere
	Paged        bool // the geometry column has a page index
	leaves       map[string]leaf
	reader       *Reader
}

// ID names the file's version, for cache keys.
func (l *Layer) ID() string { return l.file.ID() }

// Version is the file version the layer was read at.
func (l *Layer) Version() string { return l.file.Version }

// Has reports whether the file has a top-level column called name.
func (l *Layer) Has(name string) bool { _, ok := l.leaves[name]; return ok }

// Open reads the file's footer and works out its schema, geometry type, extent and row groups.
func (r *Reader) Open(ctx context.Context, url string) (*Layer, error) {
	f, err := r.store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	pf, err := parquet.OpenFile(f, f.Size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true),
		parquet.ReadBufferSize(256<<10))
	if err != nil {
		return nil, err
	}
	l := &Layer{URL: url, file: f, pf: pf, leaves: map[string]leaf{}, reader: r}
	md := pf.Metadata()

	var geo struct {
		Primary string `json:"primary_column"`
		Columns map[string]struct {
			Types []string `json:"geometry_types"`
		} `json:"columns"`
	}
	if v, ok := pf.Lookup("geo"); ok {
		_ = json.Unmarshal([]byte(v), &geo)
	}
	l.Geom = geo.Primary
	if l.Geom == "" {
		l.Geom = "geom"
	}

	// The schema elements of the leaf columns, by leaf column index; top-level leaves are fields.
	var elements []*format.SchemaElement
	for i := range md.Schema {
		if md.Schema[i].Type.Valid {
			elements = append(elements, &md.Schema[i])
		}
	}
	for _, c := range pf.Root().Columns() {
		if !c.Leaf() {
			continue
		}
		el := elements[c.Index()]
		kind, conv := converter(el, el.Name == l.Geom)
		l.leaves[el.Name] = leaf{c.Index(), kind, conv}
		if el.Name != l.Geom && !internal[el.Name] {
			l.Columns = append(l.Columns, Column{el.Name, fieldType(el)})
		}
	}
	l.HasBBox = !slices.ContainsFunc(BBox, func(n string) bool { return !l.Has(n) })

	var start int64
	for _, rg := range md.RowGroups {
		g := Group{Start: start, Rows: rg.NumRows,
			Xmin: l.stat(rg, "bbox_xmin", false), Ymin: l.stat(rg, "bbox_ymin", false),
			Xmax: l.stat(rg, "bbox_xmax", true), Ymax: l.stat(rg, "bbox_ymax", true),
			IDMin: l.stat(rg, "feature_id", false), IDMax: l.stat(rg, "feature_id", true)}
		start += rg.NumRows
		l.Groups = append(l.Groups, g)
	}
	l.Rows = start
	if l.HasBBox {
		e := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
		for _, g := range l.Groups {
			e[0], e[1] = math.Min(e[0], g.Xmin), math.Min(e[1], g.Ymin)
			e[2], e[3] = math.Max(e[2], g.Xmax), math.Max(e[3], g.Ymax)
		}
		l.Extent = &e
	}
	t := ""
	if types := geo.Columns[l.Geom].Types; len(types) > 0 {
		t = types[0]
	}
	switch {
	case strings.Contains(t, "Point"):
		l.GeometryType = "Point"
	case strings.Contains(t, "LineString"):
		l.GeometryType = "LineString"
	default:
		l.GeometryType = "Polygon"
	}
	l.Point = l.GeometryType == "Point"
	// feature_id is the row number + 1 in every archive we write; trust it only where stats say so.
	l.IDIsRow = !slices.ContainsFunc(l.Groups, func(g Group) bool {
		return g.IDMin != float64(g.Start+1) || g.IDMax != float64(g.Start+g.Rows)
	})
	if gl, ok := l.leaves[l.Geom]; ok && len(md.RowGroups) > 0 {
		l.Paged = md.RowGroups[0].Columns[gl.index].OffsetIndexOffset != 0
	}
	return l, nil
}

// stat is a row group's min or max statistic for a numeric column, NaN when absent.
func (l *Layer) stat(rg format.RowGroup, name string, isMax bool) float64 {
	lf, ok := l.leaves[name]
	if !ok {
		return math.NaN()
	}
	cm := rg.Columns[lf.index].MetaData
	b := cm.Statistics.MinValue
	if isMax {
		b = cm.Statistics.MaxValue
	}
	if b == nil {
		return math.NaN()
	}
	switch {
	case cm.Type == format.Double && len(b) == 8:
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	case cm.Type == format.Float && len(b) == 4:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
	case cm.Type == format.Int64 && len(b) == 8:
		return float64(int64(binary.LittleEndian.Uint64(b)))
	case cm.Type == format.Int32 && len(b) == 4:
		return float64(int32(binary.LittleEndian.Uint32(b)))
	}
	return math.NaN()
}

func converted(el *format.SchemaElement) (deprecated.ConvertedType, bool) {
	return el.ConvertedType.Get()
}

// fieldType is the Esri type the provider declares for a column.
func fieldType(el *format.SchemaElement) string {
	if el.Name == "feature_id" {
		return "Integer"
	}
	switch el.LogicalType.Value.(type) {
	case *format.TimestampType, *format.DateType:
		return "Date"
	}
	if ct, ok := converted(el); ok {
		switch ct {
		case deprecated.Date, deprecated.TimestampMillis, deprecated.TimestampMicros:
			return "Date"
		}
	}
	t, _ := el.Type.Get()
	switch t {
	case format.Int32, format.Boolean:
		return "Integer"
	case format.Int64, format.Double, format.Float:
		return "Double"
	}
	return "String"
}

// converter decides how a column's parquet values become our values, as hyparquet converts them.
func converter(el *format.SchemaElement, geom bool) (Kind, func(*builder, parquet.Value)) {
	t, _ := el.Type.Get()
	ct, hasCT := converted(el)
	scale := 0
	if s, ok := el.Scale.Get(); ok {
		scale = int(s)
	}
	if d, ok := el.LogicalType.Value.(*format.DecimalType); ok {
		hasCT, ct, scale = true, deprecated.Decimal, int(d.Scale)
	}
	factor := math.Pow(10, float64(-scale))
	if hasCT && ct == deprecated.Decimal {
		return KindFloat, func(b *builder, v parquet.Value) {
			switch v.Kind() {
			case parquet.Int32:
				b.float(float64(v.Int32()) * factor)
			case parquet.Int64:
				b.float(float64(v.Int64()) * factor)
			default:
				b.float(decimalBytes(v.ByteArray()) * factor)
			}
		}
	}
	isDate := hasCT && ct == deprecated.Date
	if _, ok := el.LogicalType.Value.(*format.DateType); ok {
		isDate = true
	}
	var unit int64 // timestamp divisor to milliseconds
	if ts, ok := el.LogicalType.Value.(*format.TimestampType); ok {
		switch ts.Unit.Value.(type) {
		case *format.MilliSeconds:
			unit = 1
		case *format.MicroSeconds:
			unit = 1000
		case *format.NanoSeconds:
			unit = 1000000
		}
	} else if hasCT && ct == deprecated.TimestampMillis {
		unit = 1
	} else if hasCT && ct == deprecated.TimestampMicros {
		unit = 1000
	}
	switch t {
	case format.Boolean:
		return KindBool, func(b *builder, v parquet.Value) { b.bool(v.Boolean()) }
	case format.Int32:
		if isDate {
			return KindInt, func(b *builder, v parquet.Value) { b.int(int64(v.Int32()) * 86400000) }
		}
		if hasCT && ct == deprecated.Uint32 {
			return KindInt, func(b *builder, v parquet.Value) { b.int(int64(uint32(v.Int32()))) }
		}
		return KindInt, func(b *builder, v parquet.Value) { b.int(int64(v.Int32())) }
	case format.Int64:
		if unit > 0 {
			return KindInt, func(b *builder, v parquet.Value) { b.int(v.Int64() / unit) }
		}
		return KindInt, func(b *builder, v parquet.Value) { b.int(v.Int64()) }
	case format.Int96:
		return KindInt, func(b *builder, v parquet.Value) {
			x := v.Int96()
			nanos := int64(uint64(x[1])<<32 | uint64(x[0]))
			days := int64(x[2]) - 2440588
			b.int(days*86400000 + nanos/1000000)
		}
	case format.Float:
		return KindFloat, func(b *builder, v parquet.Value) { b.float(float64(v.Float())) }
	case format.Double:
		return KindFloat, func(b *builder, v parquet.Value) { b.float(v.Double()) }
	}
	if geom {
		return KindBytes, func(b *builder, v parquet.Value) { b.bytes(v.ByteArray()) }
	}
	return KindString, func(b *builder, v parquet.Value) { b.bytes(v.ByteArray()) }
}

func decimalBytes(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	if len(b) < 8 && b[0]&0x80 != 0 {
		v -= 1 << (8 * len(b))
	}
	return float64(v)
}

// GeomBytes estimates the compressed geometry bytes reading the sorted rows fetches and decodes:
// the pages holding them where the chunk's page layout is known, else the whole chunk.
func (l *Layer) GeomBytes(rows []int64) int64 {
	lf, ok := l.leaves[l.Geom]
	if !ok {
		return 0
	}
	var n int64
	spans, _ := l.spans(rows)
	for _, sp := range spans {
		lay, known := l.reader.layouts.Get(chunkKey{l.ID(), lf.index, sp.g})
		if !known {
			start, stop := l.chunkRange(lf, sp.g)
			n += stop - start
			continue
		}
		for _, p := range pagesFor(lay.firsts(), sp.rel) {
			n += lay.pages[p].size
		}
	}
	return n
}
