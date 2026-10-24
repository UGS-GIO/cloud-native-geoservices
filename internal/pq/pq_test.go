package pq

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/deprecated"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

func open(t *testing.T, name string) *Layer {
	path, _ := filepath.Abs("../../testdata/" + name)
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestOpen(t *testing.T) {
	l := open(t, "qfaults.parquet")
	if l.Rows != 300 || len(l.Groups) != 3 || l.GeometryType != "LineString" || !l.IDIsRow || !l.HasBBox || l.Extent == nil {
		t.Fatalf("%+v", l)
	}
	for _, c := range l.Columns {
		if internal[c.Name] || c.Name == l.Geom {
			t.Errorf("%s is not an attribute", c.Name)
		}
	}
}

func TestSelectAndRead(t *testing.T) {
	ctx := context.Background()
	l := open(t, "wells.parquet")
	all, _ := l.Select(ctx, Selection{Box: l.Extent})
	if len(all) != 300 {
		t.Errorf("the extent selects every row: %d", len(all))
	}
	page, _ := l.Select(ctx, Selection{Page: &Page{Offset: 290, Limit: 20}})
	if len(page) != 10 || page[0] != 290 {
		t.Errorf("last page %v", page)
	}
	ids, _ := l.Select(ctx, Selection{IDs: []int64{300, 1, 1, 999}})
	if len(ids) != 2 || ids[0] != 0 || ids[1] != 299 {
		t.Errorf("ids %v", ids)
	}
	rows, err := l.Read(ctx, []string{"feature_id", l.Geom}, []int64{0, 150, 299})
	if err != nil {
		t.Fatal(err)
	}
	if rows.Get(0, 1) != int64(151) || len(rows.Bytes(1, 2)) == 0 {
		t.Errorf("read %v", rows.Get(0, 1))
	}
	if _, err := l.Read(ctx, []string{"feature_id"}, []int64{300}); err == nil {
		t.Error("a row past the end should fail")
	}
}

func TestIDsWhenIDsAreNotRowNumbers(t *testing.T) {
	ctx := context.Background()
	l := open(t, "qfaults.parquet")
	l.IDIsRow = false
	ids, err := l.Select(ctx, Selection{IDs: []int64{300, 1, 150, 999}})
	if err != nil || !slices.Equal(ids, []int64{0, 149, 299}) {
		t.Errorf("ids %v %v", ids, err)
	}
	l.Groups[1].IDMin, l.Groups[1].IDMax = math.NaN(), math.NaN() // no statistics: read the group
	if ids, _ := l.Select(ctx, Selection{IDs: []int64{150}}); !slices.Equal(ids, []int64{149}) {
		t.Errorf("group without statistics %v", ids)
	}
	if ids, _ := l.Select(ctx, Selection{IDs: []int64{999}}); len(ids) != 0 {
		t.Errorf("an id outside every group %v", ids)
	}
}

func TestUnsignedIDStatistics(t *testing.T) {
	type row struct {
		ID uint32 `parquet:"feature_id"`
	}
	path := filepath.Join(t.TempDir(), "uint.parquet")
	f, _ := os.Create(path)
	w := parquet.NewGenericWriter[row](f)
	w.Write([]row{{5}, {3_000_000_000}})
	w.Close()
	f.Close()
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := l.Select(context.Background(), Selection{IDs: []int64{3_000_000_000}}); !slices.Equal(ids, []int64{1}) {
		t.Errorf("an id above int32 in a uint32 column %v", ids)
	}
}

func TestInt64FieldTypes(t *testing.T) {
	type row struct {
		ID    int64  `parquet:"feature_id"`
		Count int64  `parquet:"count"`
		Key   int64  `parquet:"key"`
		Empty *int64 `parquet:"empty,optional"`
		Big   uint64 `parquet:"big"`
	}
	path := filepath.Join(t.TempDir(), "ints.parquet")
	f, _ := os.Create(path)
	w := parquet.NewGenericWriter[row](f)
	w.Write([]row{{ID: 1, Count: -5, Key: 8489717270951711}, {ID: 2, Count: 19, Key: 160384871698236}})
	w.Close()
	f.Close()
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range l.Columns {
		got[c.Name] = c.Type
	}
	// The type comes from the schema, as in ArcGIS Online, whatever the values.
	if got["count"] != "BigInteger" || got["key"] != "BigInteger" || got["empty"] != "BigInteger" || got["big"] != "Double" {
		t.Errorf("types %v", got)
	}
}

func TestAppendKey(t *testing.T) {
	key := func(v *Values, i int) (string, bool) {
		b, ok := v.AppendKey(nil, i, '|')
		return string(b), ok
	}
	floats := &Values{Kind: KindFloat, N: 5, Floats: []float64{0, math.Copysign(0, -1), math.NaN(), 1.5, 0},
		Valid: []bool{true, true, true, true, false}}
	if a, _ := key(floats, 0); a != must(key(floats, 1)) {
		t.Error("-0 prints as 0")
	}
	if a, _ := key(floats, 3); a == must(key(floats, 0)) || a == must(key(floats, 2)) {
		t.Error("distinct numbers")
	}
	text := &Values{Kind: KindString, N: 3, Data: []byte("a|b"), Offs: []uint32{0, 0, 3, 3}, Valid: []bool{true, true, false}}
	if a, _ := key(text, 0); a != must(key(text, 2)) || a != must(key(floats, 4)) {
		t.Error(`"" and null print alike`)
	}
	if _, ok := key(text, 1); ok {
		t.Error("text holding the separator")
	}
}

func must(s string, _ bool) string { return s }

// A file with small pages and some null bboxes: the page index prunes, and the rows stay those
// every row group's statistics and a test of each row pick.
func TestInBoxPages(t *testing.T) {
	type row struct {
		ID   int64    `parquet:"feature_id"`
		Xmin *float64 `parquet:"bbox_xmin,optional"`
		Ymin *float64 `parquet:"bbox_ymin,optional"`
		Xmax *float64 `parquet:"bbox_xmax,optional"`
		Ymax *float64 `parquet:"bbox_ymax,optional"`
		Geom []byte   `parquet:"geom"`
	}
	rng := rand.New(rand.NewPCG(1, 2))
	rows := make([]row, 5000)
	for i := range rows {
		rows[i].ID = int64(i + 1)
		if rng.IntN(20) == 0 {
			continue
		}
		// Sorted along x, as a Hilbert order keeps neighbours together.
		x, y := -10+20*float64(i)/5000, -10+20*rng.Float64()
		rows[i].Xmin, rows[i].Ymin = &x, &y
		x2, y2 := x+rng.Float64()/10, y+rng.Float64()/10
		rows[i].Xmax, rows[i].Ymax = &x2, &y2
	}
	path := filepath.Join(t.TempDir(), "paged.parquet")
	f, _ := os.Create(path)
	w := parquet.NewGenericWriter[row](f, parquet.PageBufferSize(512), parquet.MaxRowsPerRowGroup(1500))
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	v := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	pruned, sure, covers := false, false, false
	for _, box := range [][4]float64{{-3, -2, -2.5, 4}, {-1, -1, 1, 1}, {9.9, -10, 11, 10}, {-20, -20, 20, 20}, {5, 20, 6, 30}, {9, -10.5, 11, 10.5}} {
		got, err := l.Select(context.Background(), Selection{Box: &box})
		if err != nil {
			t.Fatal(err)
		}
		var want []int64
		for g, gr := range l.Groups {
			for i := gr.Start; overlaps(gr, &box) && i < gr.Start+gr.Rows; i++ {
				r := rows[i]
				if v(r.Xmax) >= box[0] && v(r.Xmin) <= box[2] && v(r.Ymax) >= box[1] && v(r.Ymin) <= box[3] {
					want = append(want, i)
				}
			}
			segs, _ := l.segments(context.Background(), g)
			for _, sg := range segs {
				pruned, sure = pruned || !sg.meets(&box), sure || sg.within(&box)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("box %v: %d rows, want %d", box, len(got), len(want))
		}
		// A covered row's bbox lies in the box.
		all := l.AllRows()
		in := func(b [4]float64) bool { return b[0] >= box[0] && b[1] >= box[1] && b[2] <= box[2] && b[3] <= box[3] }
		covered, err := l.Covered(context.Background(), all, in)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range covered {
			r := rows[i]
			if c && (r.Xmin == nil || !in([4]float64{*r.Xmin, *r.Ymin, *r.Xmax, *r.Ymax})) {
				t.Fatalf("box %v: row %d covered", box, i)
			}
			covers = covers || c
		}
	}
	if pages, _, _ := l.want(context.Background(), l.leaves["bbox_xmin"], 0, []int64{}); pages == nil {
		t.Error("no rows read the whole chunk")
	}
	if !pruned || !sure || !covers {
		t.Error("the page index pruned nothing", pruned, sure, covers)
	}
}

func TestFieldTypeMatchesConverter(t *testing.T) {
	el := func(typ format.Type, ct *deprecated.ConvertedType, lt format.LogicalTypeValue) *format.SchemaElement {
		e := &format.SchemaElement{Name: "c", Type: thrift.New(typ), LogicalType: format.LogicalType{Value: lt}}
		if ct != nil {
			e.ConvertedType = thrift.New(*ct)
		}
		return e
	}
	dec, date := deprecated.Decimal, deprecated.Date
	for _, tt := range []struct {
		el   *format.SchemaElement
		want string
	}{
		{el(format.FixedLenByteArray, &dec, &format.DecimalType{Scale: 2, Precision: 38}), "Double"},
		{el(format.ByteArray, &dec, nil), "Double"},
		{el(format.Int32, nil, &format.DecimalType{Scale: 2, Precision: 9}), "Double"},
		{el(format.Int96, nil, nil), "Date"},
		{el(format.Int32, &date, nil), "Date"},
		{el(format.Boolean, nil, nil), "Integer"},
		{el(format.ByteArray, nil, &format.StringType{}), "String"},
		{el(format.FixedLenByteArray, nil, nil), "String"},
	} {
		kind, _ := converter(tt.el, false)
		if got := fieldType(tt.el); got != tt.want || (got == "String") != (kind == KindString) {
			t.Errorf("%v %v: %s, kind %v", tt.el.Type, tt.el.LogicalType.Value, got, kind)
		}
	}
}

// A GeoParquet 2.0 file: native geometry under another name, no covering and no geo metadata.
// The geospatial statistics prune row groups, and the rows in the box stay those of a full scan.
func TestNativeGeomColumn(t *testing.T) {
	type both struct {
		Label []byte `parquet:"label,geometry"`
		Geom  []byte `parquet:"geom,geometry"`
	}
	type other struct {
		Label []byte `parquet:"label,geometry"`
		Shape []byte `parquet:"shape,geometry"`
	}
	pt := binary.LittleEndian.AppendUint32([]byte{1}, 1)
	pt = append(pt, make([]byte, 16)...)
	open := func(write func(*os.File)) *Layer {
		path := filepath.Join(t.TempDir(), "two.parquet")
		f, _ := os.Create(path)
		write(f)
		f.Close()
		l, err := NewReader(16<<20, 16<<20, 64<<20).Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := open(func(f *os.File) {
		w := parquet.NewGenericWriter[both](f)
		w.Write([]both{{pt, pt}})
		w.Close()
	})
	if l.Geom != "geom" {
		t.Errorf("with a geom column: %q", l.Geom)
	}
	l = open(func(f *os.File) {
		w := parquet.NewGenericWriter[other](f)
		w.Write([]other{{pt, pt}})
		w.Close()
	})
	if l.Geom != "label" {
		t.Errorf("without one, the first native column: %q", l.Geom)
	}
}

func TestNativeGeometry(t *testing.T) {
	type row struct {
		ID    int64  `parquet:"feature_id"`
		Shape []byte `parquet:"shape,geometry"`
	}
	point := func(x, y float64) []byte {
		b := binary.LittleEndian.AppendUint32([]byte{1}, 1)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(x))
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(y))
	}
	rng := rand.New(rand.NewPCG(1, 2))
	rows := make([]row, 3000)
	for i := range rows {
		rows[i] = row{int64(i + 1), point(-10+20*float64(i)/3000, -10+20*rng.Float64())}
	}
	path := filepath.Join(t.TempDir(), "native.parquet")
	f, _ := os.Create(path)
	w := parquet.NewGenericWriter[row](f, parquet.MaxRowsPerRowGroup(500))
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()
	ctx := context.Background()
	l, err := NewReader(16<<20, 16<<20, 64<<20).Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Geom != "shape" || l.GeometryType != "Point" || l.HasBBox {
		t.Fatalf("%+v", l)
	}
	nan := math.NaN()
	g := &l.Groups[0] // no statistics: read the group
	g.Xmin, g.Ymin, g.Xmax, g.Ymax = nan, nan, nan, nan
	box := [4]float64{-1, -1, 1, 1}
	got, err := l.Select(ctx, Selection{Box: &box})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[int]bool{}
	for _, r := range got {
		groups[l.groupOf(r)] = true
	}
	if len(groups) != 3 || !groups[0] {
		t.Errorf("row groups read %v", groups)
	}
	in := func(rows []int64) (out []int64) {
		v, _ := l.Read(ctx, []string{"shape"}, rows)
		for i, r := range rows {
			b := v.Bytes(0, i)
			x, y := math.Float64frombits(binary.LittleEndian.Uint64(b[5:])), math.Float64frombits(binary.LittleEndian.Uint64(b[13:]))
			if x >= box[0] && x <= box[2] && y >= box[1] && y <= box[3] {
				out = append(out, r)
			}
		}
		return out
	}
	if want := in(l.AllRows()); len(want) == 0 || !slices.Equal(in(got), want) {
		t.Errorf("%d rows in the box, want %d", len(in(got)), len(want))
	}
	// Ids and a box: an id in a row group far from the box is dropped.
	if ids, _ := l.Select(ctx, Selection{IDs: []int64{1, 3000}, Box: &box}); !slices.Equal(ids, []int64{0}) {
		t.Errorf("ids in the box %v", ids)
	}
}

// Geospatial statistics bound a row group; absent ones and a GEOGRAPHY box across the
// antimeridian leave what they cannot bound unknown.
func TestGeoBounds(t *testing.T) {
	l := &Layer{Geom: "geom", leaves: map[string]leaf{"geom": {}}}
	rg := func(b format.BoundingBox) format.RowGroup {
		return format.RowGroup{Columns: []format.ColumnChunk{{MetaData: format.ColumnMetaData{
			GeospatialStatistics: format.GeospatialStatistics{BBox: b}}}}}
	}
	nan := math.NaN()
	for _, tt := range []struct {
		b    format.BoundingBox
		want [4]float64
	}{
		{format.BoundingBox{XMin: 1, XMax: 2, YMin: 3, YMax: 4}, [4]float64{1, 3, 2, 4}},
		{format.BoundingBox{}, [4]float64{nan, nan, nan, nan}},
		{format.BoundingBox{XMin: 170, XMax: -170, YMin: -5, YMax: 5}, [4]float64{nan, -5, nan, 5}},
	} {
		g := l.geoBounds(rg(tt.b))
		got := [4]float64{g.Xmin, g.Ymin, g.Xmax, g.Ymax}
		for i := range got {
			if got[i] != tt.want[i] && !(math.IsNaN(got[i]) && math.IsNaN(tt.want[i])) {
				t.Errorf("%+v: %v", tt.b, got)
				break
			}
		}
	}
}
