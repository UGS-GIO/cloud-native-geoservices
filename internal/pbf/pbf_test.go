package pbf

import (
	"testing"

	"github.com/paulmach/orb"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/query"
)

// fields walks one message, returning its fields by number (repeated fields in order).
func fields(t *testing.T, b []byte) map[protowire.Number][][]byte {
	out := map[protowire.Number][][]byte{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Fatal("bad tag")
		}
		b = b[n:]
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			t.Fatal("bad value")
		}
		val := b[:m]
		if typ == protowire.BytesType {
			val, _ = protowire.ConsumeBytes(b)
		}
		out[num] = append(out[num], val)
		b = b[m:]
	}
	return out
}

func TestCount(t *testing.T) {
	q := fields(t, Count(19743))[2][0]
	c := fields(t, fields(t, q)[2][0])
	if v, _ := protowire.ConsumeVarint(c[1][0]); v != 19743 {
		t.Errorf("count %d", v)
	}
}

func TestFeatures(t *testing.T) {
	fs := &query.FeatureSet{
		IDField: "feature_id", GeometryType: "esriGeometryPolyline", SR: query.SpatialRef{Wkid: 102100, Latest: 3857},
		Fields: []esri.Field{{Name: "name", Type: "esriFieldTypeString"}, {Name: "feature_id", Type: "esriFieldTypeOID"}},
		Features: []query.EsriFeature{{
			Attrs: []jsonx.KV{{K: "name", V: nil}, {K: "feature_id", V: int64(7)}},
			// The second point quantizes onto the first and is dropped; y counts down from the top.
			Geom: &geom.Esri{Kind: geom.EsriPolyline, Parts: [][]orb.Point{{{0, 100}, {0.4, 100}, {10, 80}}, {{5, 5}, {5.2, 5}}}},
		}},
		Transform: &geom.Transform{XScale: 1, YScale: 1, YTranslate: 100},
	}
	result := fields(t, fields(t, fields(t, Features(fs))[2][0])[1][0])
	if _, ok := result[9]; ok {
		t.Error("exceededTransferLimit is written only when true")
	}
	if names := result[13]; string(fields(t, names[0])[1][0]) != "feature_id" {
		t.Error("fields are sorted by name")
	}
	feature := fields(t, result[15][0])
	attrs := feature[1]
	if len(attrs) != 2 || len(attrs[1]) != 0 {
		t.Errorf("attributes sorted by name, null as an empty value: %v", attrs)
	}
	g := fields(t, feature[2][0])
	lengths, _ := protowire.ConsumeVarint(g[2][0])
	if len(g[2][0]) != 1 || lengths != 2 {
		t.Errorf("one path of 2 points kept (the other collapses): %v", g[2])
	}
	var coords []int64
	for b := g[3][0]; len(b) > 0; {
		v, n := protowire.ConsumeVarint(b)
		coords = append(coords, protowire.DecodeZigZag(v))
		b = b[n:]
	}
	if want := []int64{0, 0, 10, 20}; len(coords) != 4 || coords[0] != want[0] || coords[2] != want[2] || coords[3] != want[3] {
		t.Errorf("coords %v, want %v", coords, want)
	}
}
