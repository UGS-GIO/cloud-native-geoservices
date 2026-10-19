package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/planar"
)

type bbox struct {
	Xmin float64 `parquet:"xmin"`
	Ymin float64 `parquet:"ymin"`
	Xmax float64 `parquet:"xmax"`
	Ymax float64 `parquet:"ymax"`
}

// column is one parquet column and how a GeoJSON property fills it.
type column struct {
	name string
	src  string // the Esri field
	typ  reflect.Type
	tag  string
	set  func(v reflect.Value, p json.Number) error
}

func intSetter(bits int) func(reflect.Value, json.Number) error {
	return func(v reflect.Value, p json.Number) error {
		n, err := strconv.ParseInt(p.String(), 10, bits)
		if err != nil {
			return err
		}
		v.Elem().SetInt(n)
		return nil
	}
}

func floatSetter(bits int) func(reflect.Value, json.Number) error {
	return func(v reflect.Value, p json.Number) error {
		f, err := strconv.ParseFloat(p.String(), bits)
		if err != nil {
			return err
		}
		v.Elem().SetFloat(f)
		return nil
	}
}

// columnFor maps an Esri field type to the parquet type that holds it exactly.
func columnFor(f field) column {
	c := column{name: f.Name, src: f.Name, tag: f.Name + ",optional"}
	switch f.Type {
	case "esriFieldTypeOID":
		c.name, c.tag = "feature_id", "feature_id"
		c.typ, c.set = reflect.TypeFor[int64](), intSetter(64)
		return c
	case "esriFieldTypeSmallInteger":
		c.typ, c.set = reflect.TypeFor[*int16](), intSetter(16)
	case "esriFieldTypeInteger":
		c.typ, c.set = reflect.TypeFor[*int32](), intSetter(32)
	case "esriFieldTypeBigInteger":
		c.typ, c.set = reflect.TypeFor[*int64](), intSetter(64)
	case "esriFieldTypeSingle":
		c.typ, c.set = reflect.TypeFor[*float32](), floatSetter(32)
	case "esriFieldTypeDouble":
		c.typ, c.set = reflect.TypeFor[*float64](), floatSetter(64)
	case "esriFieldTypeDate":
		c.typ, c.set, c.tag = reflect.TypeFor[*int64](), intSetter(64), f.Name+",optional,timestamp(millisecond)"
	default:
		c.typ = reflect.TypeFor[*string]()
	}
	return c
}

// extract copies every feature of the layer, in object id order, to a GeoParquet file with the
// bbox covering this server reads. It reads Esri JSON in EPSG:4326: GeoJSON would round
// coordinates to 15 digits and rewrite GlobalIDs.
func extract(ctx context.Context, c *client, u string, m *meta, path string) (int, error) {
	var cols []column
	var fields []reflect.StructField
	for _, f := range m.Fields {
		if f.Type == "esriFieldTypeGeometry" || f.Type == "esriFieldTypeBlob" || f.Type == "esriFieldTypeRaster" {
			continue
		}
		col := columnFor(f)
		cols = append(cols, col)
		fields = append(fields, reflect.StructField{Name: fmt.Sprintf("F%d", len(fields)), Type: col.typ,
			Tag: reflect.StructTag(`parquet:"` + col.tag + `"`)})
	}
	n := len(fields)
	fields = append(fields,
		reflect.StructField{Name: "Geom", Type: reflect.TypeFor[[]byte](), Tag: `parquet:"geom,optional"`},
		reflect.StructField{Name: "Xmin", Type: reflect.TypeFor[*float64](), Tag: `parquet:"bbox_xmin,optional"`},
		reflect.StructField{Name: "Ymin", Type: reflect.TypeFor[*float64](), Tag: `parquet:"bbox_ymin,optional"`},
		reflect.StructField{Name: "Xmax", Type: reflect.TypeFor[*float64](), Tag: `parquet:"bbox_xmax,optional"`},
		reflect.StructField{Name: "Ymax", Type: reflect.TypeFor[*float64](), Tag: `parquet:"bbox_ymax,optional"`},
		reflect.StructField{Name: "Bbox", Type: reflect.TypeFor[*bbox](), Tag: `parquet:"bbox,optional"`})
	rowType := reflect.StructOf(fields)

	file, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	w := parquet.NewWriter(file, parquet.SchemaOf(reflect.New(rowType).Interface()),
		parquet.Compression(&zstd.Codec{}), parquet.MaxRowsPerRowGroup(1000), parquet.PageBufferSize(16<<10))

	extent := orb.Bound{Min: orb.Point{math.Inf(1), math.Inf(1)}, Max: orb.Point{math.Inf(-1), math.Inf(-1)}}
	types := map[string]bool{}
	last, total := int64(-1), 0
	for {
		params := url.Values{"where": {fmt.Sprintf("%s>%d", m.ObjectIDField, last)}, "outFields": {"*"},
			"orderByFields": {m.ObjectIDField}, "resultRecordCount": {strconv.Itoa(m.MaxRecordCount)},
			"returnGeometry": {"true"}, "outSR": {"4326"}, "f": {"json"}}
		var page struct {
			Features []struct {
				Attributes map[string]any `json:"attributes"`
				Geometry   *esriGeometry  `json:"geometry"`
			} `json:"features"`
		}
		if err := c.json(ctx, u+"/query", params, &page); err != nil {
			return total, err
		}
		if len(page.Features) == 0 {
			break
		}
		for _, f := range page.Features {
			row := reflect.New(rowType).Elem()
			for i, col := range cols {
				p, ok := f.Attributes[col.src]
				if !ok || p == nil {
					continue
				}
				v := row.Field(i)
				if col.set == nil {
					s := fmt.Sprint(p)
					v.Set(reflect.ValueOf(&s))
					continue
				}
				num, ok := p.(json.Number)
				if !ok {
					return total, fmt.Errorf("%s: %v is not a number", col.name, p)
				}
				if v.Kind() == reflect.Pointer {
					v.Set(reflect.New(col.typ.Elem()))
					err = col.set(v, num)
				} else {
					err = col.set(v.Addr(), num)
				}
				if err != nil {
					return total, fmt.Errorf("%s: %w", col.name, err)
				}
				if col.name == "feature_id" {
					last = v.Int()
				}
			}
			if g := f.Geometry.orb(); g != nil {
				b := g.Bound()
				extent = extent.Union(b)
				types[g.GeoJSONType()] = true
				raw, err := wkb.Marshal(g)
				if err != nil {
					return total, err
				}
				row.Field(n).SetBytes(raw)
				for j, x := range []float64{b.Min[0], b.Min[1], b.Max[0], b.Max[1]} {
					row.Field(n + 1 + j).Set(reflect.ValueOf(&x))
				}
				row.Field(n + 5).Set(reflect.ValueOf(&bbox{b.Min[0], b.Min[1], b.Max[0], b.Max[1]}))
			}
			if err := w.Write(row.Addr().Interface()); err != nil {
				return total, err
			}
			total++
		}
	}
	geo, _ := json.Marshal(map[string]any{"version": "1.1.0", "primary_column": "geom",
		"columns": map[string]any{"geom": map[string]any{"encoding": "WKB", "geometry_types": sorted(types),
			"bbox": []float64{extent.Min[0], extent.Min[1], extent.Max[0], extent.Max[1]},
			"covering": map[string]any{"bbox": map[string][]string{"xmin": {"bbox", "xmin"}, "ymin": {"bbox", "ymin"},
				"xmax": {"bbox", "xmax"}, "ymax": {"bbox", "ymax"}}}}}})
	w.SetKeyValueMetadata("geo", string(geo))
	if err := w.Close(); err != nil {
		return total, err
	}
	return total, file.Close()
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// esriGeometry is an Esri JSON point, multipoint, polyline or polygon.
type esriGeometry struct {
	X      *float64         `json:"x"`
	Y      *float64         `json:"y"`
	Points []orb.Point      `json:"points"`
	Paths  []orb.LineString `json:"paths"`
	Rings  []orb.Ring       `json:"rings"`
}

// orb converts the geometry as Esri's own GeoJSON does: one part is a single geometry, and each
// counterclockwise ring is a hole of the clockwise ring around it.
func (g *esriGeometry) orb() orb.Geometry {
	switch {
	case g == nil:
		return nil
	case g.X != nil && g.Y != nil:
		return orb.Point{*g.X, *g.Y}
	case len(g.Points) > 0:
		return orb.MultiPoint(g.Points)
	case len(g.Paths) == 1:
		return g.Paths[0]
	case len(g.Paths) > 1:
		return orb.MultiLineString(g.Paths)
	case len(g.Rings) == 0:
		return nil
	}
	var polys orb.MultiPolygon
	var holes []orb.Ring
	for _, r := range g.Rings {
		if r.Orientation() == orb.CW {
			polys = append(polys, orb.Polygon{r})
		} else {
			holes = append(holes, r)
		}
	}
	for _, h := range holes {
		i := slices.IndexFunc(polys, func(p orb.Polygon) bool { return planar.RingContains(p[0], h[0]) })
		if i < 0 {
			polys = append(polys, orb.Polygon{h})
			continue
		}
		polys[i] = append(polys[i], h)
	}
	if len(polys) == 1 {
		return polys[0]
	}
	return polys
}
