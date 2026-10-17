package geom

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
)

func mustWKB(t testing.TB, g orb.Geometry) []byte {
	b, err := wkb.Marshal(g, binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFromWKB(t *testing.T) {
	zigzag := orb.LineString{{0, 0}, {1, 0.01}, {2, 0}, {3, 5}, {4, 0}}
	square := orb.Ring{{0, 0}, {0, 1}, {1, 1}, {1, 0}, {0, 0}}
	tiny := orb.Ring{{0, 0}, {0, 0.001}, {0.002, 0.001}, {0.002, 0}, {0, 0}}
	tests := []struct {
		name  string
		g     orb.Geometry
		tol   float64
		want  orb.Geometry
		speck bool
	}{
		{"point", orb.Point{1, 2}, 1, orb.Point{1, 2}, false},
		{"line keeps the far point", zigzag, 0.1, orb.LineString{{0, 0}, {2, 0}, {3, 5}, {4, 0}}, false},
		{"line without tolerance", zigzag, 0, zigzag, false},
		{"polygon survives", orb.Polygon{square}, 0.1, orb.Polygon{square}, false},
		{"small polygon is a speck", orb.Polygon{tiny}, 0.01, orb.Polygon{{{0, 0}, {0.002, 0}, {0.002, 0.001}, {0, 0.001}, {0, 0}}}, true},
		{"hole that simplifies away is dropped", orb.Polygon{square, tiny}, 0.01, orb.Polygon{square}, false},
		{"multipolygon speck", orb.MultiPolygon{{tiny}}, 0.01, orb.MultiPolygon{{{{0, 0}, {0.002, 0}, {0.002, 0.001}, {0, 0.001}, {0, 0}}}}, true},
	}
	for _, tt := range tests {
		got, ok, err := FromWKB(mustWKB(t, tt.g), tt.tol)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", tt.name, ok, err)
		}
		if !orb.Equal(got.G, tt.want) || got.Speck != tt.speck {
			t.Errorf("%s: got %v speck=%v, want %v speck=%v", tt.name, got.G, got.Speck, tt.want, tt.speck)
		}
	}
	if _, ok, err := FromWKB(nil, 1); ok || err != nil {
		t.Error("empty WKB should be no geometry")
	}
}

func TestFromWKBZ(t *testing.T) {
	// ISO WKB LineString Z (1002): x, y, z per point; z is dropped.
	b := []byte{1, 0xea, 3, 0, 0, 2, 0, 0, 0}
	for _, v := range []float64{1, 2, 3, 4, 5, 6} {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	}
	got, _, err := FromWKB(b, 0)
	if err != nil || !orb.Equal(got.G, orb.LineString{{1, 2}, {4, 5}}) {
		t.Fatalf("got %v %v", got.G, err)
	}
}

func TestProjection(t *testing.T) {
	for _, p := range []orb.Point{{-111.95, 40.75}, {0, 0}, {-114.05, 36.99}} {
		back := ToLonLat(ToMercator(p))
		if math.Abs(back[0]-p[0]) > 1e-9 || math.Abs(back[1]-p[1]) > 1e-9 {
			t.Errorf("%v round-tripped to %v", p, back)
		}
	}
	if m := ToMercator(orb.Point{180, 0}); math.Abs(m[0]-20037508.342789244) > 1e-6 {
		t.Errorf("x at 180 = %v", m[0])
	}
}

func TestToEsri(t *testing.T) {
	ccw := orb.Ring{{0, 0}, {1, 0}, {1, 1}, {0, 1}, {0, 0}}
	e, _ := ToEsri(orb.Polygon{ccw})
	if !Clockwise(e.Parts[0]) {
		t.Error("outer ring should be clockwise")
	}
	open := orb.Ring{{0, 0}, {0, 1}, {1, 1}}
	e, _ = ToEsri(orb.Polygon{open})
	if len(e.Parts) != 1 || len(e.Parts[0]) != 4 {
		t.Errorf("ring should be closed: %v", e.Parts)
	}
	e, _ = ToEsri(orb.MultiPolygon{{ccw, orb.Ring{{0.2, 0.2}, {0.2, 0.4}, {0.4, 0.4}, {0.2, 0.2}}}})
	if len(e.Parts) != 2 || Clockwise(e.Parts[0]) || !Clockwise(e.Parts[1]) {
		t.Errorf("multipolygon rings come hole first (as terraformer writes them): %v", e.Parts)
	}
	if got := string(e.AppendJSON(nil)); got[:9] != `{"rings":` {
		t.Errorf("json %s", got)
	}
}

func TestIntersects(t *testing.T) {
	box := orb.Polygon{{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}}
	donut := orb.Polygon{{{-20, -20}, {20, -20}, {20, 20}, {-20, 20}, {-20, -20}}, {{-15, -15}, {-15, 15}, {15, 15}, {15, -15}, {-15, -15}}}
	tests := []struct {
		name string
		g    orb.Geometry
		want bool
	}{
		{"point inside", orb.Point{5, 5}, true},
		{"point on edge", orb.Point{10, 5}, true},
		{"point outside", orb.Point{11, 5}, false},
		{"line crossing", orb.LineString{{-5, 5}, {15, 5}}, true},
		{"line outside", orb.LineString{{-5, -5}, {-1, -1}}, false},
		{"polygon overlapping", orb.Polygon{{{5, 5}, {15, 5}, {15, 15}, {5, 15}, {5, 5}}}, true},
		{"polygon around", orb.Polygon{{{-5, -5}, {15, -5}, {15, 15}, {-5, 15}, {-5, -5}}}, true},
		{"box inside the hole", donut, false},
		{"multipoint one inside", orb.MultiPoint{{20, 20}, {1, 1}}, true},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		if got := Intersects(box, tt.g); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}

func FuzzFromWKB(f *testing.F) {
	f.Add(mustWKB(f, orb.Point{1, 2}), 0.5)
	f.Add(mustWKB(f, orb.MultiPolygon{{{{0, 0}, {0, 1}, {1, 1}, {0, 0}}}}), 0.01)
	f.Add(mustWKB(f, orb.MultiLineString{{{0, 0}, {1, 1}, {2, 0}}}), 0.1)
	f.Add([]byte{1, 0xea, 3, 0, 0, 2, 0, 0, 0}, 0.0)
	f.Fuzz(func(t *testing.T, b []byte, tol float64) {
		FromWKB(b, tol)
	})
}
