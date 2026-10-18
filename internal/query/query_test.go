package query

import (
	"net/url"
	"strings"
	"testing"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
)

func collection() *Collection {
	return &Collection{
		Meta: &esri.Metadata{Name: "t", IDField: "feature_id", GeometryType: "Polygon",
			Fields: []pq.Column{{Name: "feature_id", Type: "Integer"}, {Name: "kind", Type: "String"}, {Name: "acres", Type: "Double"}}},
		Props: []string{"feature_id", "kind", "acres"},
		Features: []Feature{
			{Props: []any{int64(1), "Lake", 5.0}, Geom: orb.Polygon{{{-112, 40}, {-111, 40}, {-111, 41}, {-112, 40}}}},
			{Props: []any{int64(2), "Pond", nil}, Geom: orb.Point{-100, 30}},
			{Props: []any{int64(3), "Lake", 1.5}, Geom: nil},
		},
	}
}

func run(t *testing.T, q string) string {
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := Normalize(v, nil)
	if err := p.Validate(); err != nil {
		return "error: " + err.Error()
	}
	res, err := Run(collection(), p)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(jsonx.Marshal(res.JSON))
}

func TestRun(t *testing.T) {
	tests := []struct{ q, want string }{
		{"where=kind='Lake'&returnCountOnly=true", `{"count":2}`},
		{"returnIdsOnly=true&orderByFields=acres DESC", `{"objectIdFieldName":"feature_id","objectIds":[1,3,2]}`},
		{"outStatistics=" + url.QueryEscape(`[{"statisticType":"sum","onStatisticField":"acres","outStatisticFieldName":"s"}]`) + "&groupByFieldsForStatistics=kind",
			`{"displayFieldName":"","fields":[{"name":"s","type":"esriFieldTypeDouble","sqlType":"sqlTypeFloat","alias":"s","domain":null,"defaultValue":null},{"name":"kind","type":"esriFieldTypeString","alias":"kind","sqlType":"sqlTypeOther","domain":null,"defaultValue":null,"length":128}],"features":[{"attributes":{"s":6.5,"kind":"Lake"}},{"attributes":{"kind":"Pond"}}]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-112.5,39.5,-110.5,41.5&spatialRel=esriSpatialRelContains&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&spatialRel=esriSpatialRelContains&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&spatialRel=esriSpatialRelWithin&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&spatialRel=esriSpatialRelEnvelopeIntersects&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&spatialRel=esriSpatialRelTouches", "error: 'spatialRel' parameter is invalid"},
		{"resultRecordCount=abc", `error: "resultRecordCount" must be a number`},
		{"f=xml", "error: Invalid format"},
		{"outSR=2263", "error: Unsupported spatial reference: 2263"},
		{"where=nope ===", `error: Invalid "where" parameter: nope ===`},
		{"outFields=kind&returnDistinctValues=true&returnGeometry=false&resultRecordCount=1",
			`{"objectIdFieldName":"feature_id","uniqueIdField":{"name":"feature_id","isSystemMaintained":true},"globalIdFieldName":"","hasZ":false,"hasM":false,"spatialReference":{"wkid":4326,"latestWkid":4326},"fields":[{"name":"kind","type":"esriFieldTypeString","alias":"kind","sqlType":"sqlTypeOther","domain":null,"defaultValue":null,"length":128}],"features":[{"attributes":{"kind":"Lake"}}],"exceededTransferLimit":true,"geometryType":"esriGeometryPolygon"}`},
	}
	for _, tt := range tests {
		if got := run(t, tt.q); got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.q, got, tt.want)
		}
	}
}

func FuzzRun(f *testing.F) {
	for _, s := range []string{"where=1=1", "geometry=" + url.QueryEscape(`{"xmin":-12523442,"ymin":4931105,"xmax":-12445171,"ymax":5009377,"spatialReference":{"wkid":102100}}`),
		"geometry=" + url.QueryEscape(`{"rings":[[[0,0],[0,1],[1,1],[0,0]]]}`) + "&inSR=4326",
		"quantizationParameters=" + url.QueryEscape(`{"mode":"view","originPosition":"upperLeft","tolerance":1,"extent":{"xmin":0,"ymin":0,"xmax":1,"ymax":1}}`),
		"outStatistics=" + url.QueryEscape(`[{"statisticType":"stddev","onStatisticField":"acres"}]`),
		"objectIds=1,2,x&outSR=102100&geometryPrecision=2", "geometry=1,2&distance=5", "orderByFields=kind,acres DESC&resultOffset=1&resultRecordCount=1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		v, err := url.ParseQuery(q)
		if err != nil {
			return
		}
		p, _ := Normalize(v, nil)
		if p.Validate() != nil {
			return
		}
		Run(collection(), p)
	})
}

func TestGeometryPrecisionIsBounded(t *testing.T) {
	for _, q := range []string{"geometryPrecision=100000000", "geometryPrecision=-1"} {
		if got := run(t, q); !strings.HasPrefix(got, "error: ") {
			t.Errorf("%s answered %.80s", q, got)
		}
	}
}
