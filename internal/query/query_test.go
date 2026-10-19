package query

import (
	"net/url"
	"strings"
	"testing"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/pq"
)

func collection() *Collection {
	return &Collection{
		Meta: &esri.Metadata{Name: "t", IDField: "feature_id", GeometryType: "Polygon",
			Fields: []pq.Column{{Name: "feature_id", Type: "Integer"}, {Name: "kind", Type: "String"}, {Name: "acres", Type: "Double"}, {Name: "flag", Type: "Integer"}}},
		Props: []string{"feature_id", "kind", "acres", "flag"},
		Features: []Feature{
			{Props: []any{int64(1), "Lake", 5.0, true}, Geom: orb.Polygon{{{-112, 40}, {-111, 40}, {-111, 41}, {-112, 40}}}},
			{Props: []any{int64(2), "Pond", nil, false}, Geom: orb.Point{-100, 30}},
			{Props: []any{int64(3), "Lake", 1.5, true}, Geom: nil},
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
		{"where=acres+%2B+1+=+6&returnCountOnly=true", `{"count":1}`},
		{"where=1=1&returnIdsOnly=true&orderByFields=acres DESC", `{"objectIdFieldName":"feature_id","objectIds":[1,3,2]}`},
		{"outStatistics=" + url.QueryEscape(`[{"statisticType":"sum","onStatisticField":"acres","outStatisticFieldName":"s"}]`) + "&groupByFieldsForStatistics=kind",
			`{"objectIdFieldName":"feature_id","uniqueIdField":{"name":"feature_id","isSystemMaintained":true},"globalIdFieldName":"","geometryType":"esriGeometryPolygon","spatialReference":{"wkid":4326,"latestWkid":4326},"fields":[{"name":"s","type":"esriFieldTypeDouble","alias":"s","sqlType":"sqlTypeFloat","domain":null,"defaultValue":null},{"name":"kind","type":"esriFieldTypeString","alias":"kind","sqlType":"sqlTypeOther","length":128,"domain":null,"defaultValue":null}],"features":[{"attributes":{"s":6.5,"kind":"Lake"}},{"attributes":{"s":null,"kind":"Pond"}}]}`},
		{"where=1=1&returnIdsOnly=true&orderByFields=flag,feature_id DESC", `{"objectIdFieldName":"feature_id","objectIds":[2,3,1]}`},
		{"outStatistics=" + url.QueryEscape(`[{"statisticType":"min","onStatisticField":"flag","outStatisticFieldName":"lo"},{"statisticType":"max","onStatisticField":"flag","outStatisticFieldName":"hi"}]`),
			`{"objectIdFieldName":"feature_id","uniqueIdField":{"name":"feature_id","isSystemMaintained":true},"globalIdFieldName":"","geometryType":"esriGeometryPolygon","spatialReference":{"wkid":4326,"latestWkid":4326},"fields":[{"name":"lo","type":"esriFieldTypeInteger","alias":"lo","sqlType":"sqlTypeInteger","domain":null,"defaultValue":null},{"name":"hi","type":"esriFieldTypeInteger","alias":"hi","sqlType":"sqlTypeInteger","domain":null,"defaultValue":null}],"features":[{"attributes":{"lo":0,"hi":1}}]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-112.5,39.5,-110.5,41.5&spatialRel=esriSpatialRelContains&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&spatialRel=esriSpatialRelContains&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[]}`},
		{"geometry=-111.5,40.1,-111.4,40.2&spatialRel=esriSpatialRelWithin&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&spatialRel=esriSpatialRelEnvelopeIntersects&returnIdsOnly=true", `{"objectIdFieldName":"feature_id","objectIds":[1]}`},
		{"geometry=-111.9,40.8,-111.8,40.9&spatialRel=esriSpatialRelTouches", "error: 'spatialRel' parameter is invalid"},
		{"resultRecordCount=abc", `error: "resultRecordCount" must be a number`},
		{"f=xml", "error: Invalid format"},
		{"f=geojson&where=1=1&returnCountOnly=true", `{"type":"FeatureCollection","properties":{"count":3},"features":[]}`},
		{"f=geojson&objectIds=3&outFields=kind", `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":null,"properties":{"kind":"Lake"}}]}`},
		{"where=1=1&outStatistics=" + url.QueryEscape(`[{"statisticType":"count","onStatisticField":"kind","outStatisticFieldName":"a b"}]`), "error: "},
		{"where=1=1&resultRecordCount=2.5", "error: Cannot perform query. Invalid query parameters."},
		{"where=1=1&resultOffset=-1", "error: Cannot perform query. Invalid query parameters."},
		{"f=JSON&where=true&returnCountOnly=TRUE", `{"count":3}`},
		{"where=1=1&returnIdsOnly=True", `{"objectIdFieldName":"feature_id","objectIds":[1,2,3]}`},
		{"where=1=1&returnIdsOnly=1", "error: "},
		{"returnCountOnly=true", "error: No where clause specified"},
		{"where=%20&returnCountOnly=true", "error: No where clause specified"},
		{"objectIds=1&returnCountOnly=true", `{"count":1}`},
		{"where=1=1&outSR=2263", "error: Unsupported spatial reference: 2263"},
		{"where=nope ===", `error: Invalid "where" parameter: nope ===`},
		{"where=1=1&outFields=kind&returnDistinctValues=true&returnGeometry=false&resultRecordCount=1",
			`{"objectIdFieldName":"feature_id","uniqueIdField":{"name":"feature_id","isSystemMaintained":true},"globalIdFieldName":"","geometryType":"esriGeometryPolygon","spatialReference":{"wkid":4326,"latestWkid":4326},"fields":[{"name":"kind","type":"esriFieldTypeString","alias":"kind","sqlType":"sqlTypeOther","length":128,"domain":null,"defaultValue":null}],"exceededTransferLimit":true,"features":[{"attributes":{"kind":"Lake"}}]}`},
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

func TestGeometryPrecision(t *testing.T) {
	tests := []struct{ q, want string }{
		{"objectIds=2&geometryPrecision=0", `{"x":-100,"y":30}`},
		{"objectIds=2&geometryPrecision=-3", `{"x":-100,"y":30}`},
		{"objectIds=1&geometryPrecision=100000000", `[-112,40]`},
		{"objectIds=1&geometryPrecision=abc", "error: "},
	}
	for _, tt := range tests {
		if got := run(t, tt.q); !strings.Contains(got, tt.want) {
			t.Errorf("%s answered %.200s", tt.q, got)
		}
	}
}

func TestQuantization(t *testing.T) {
	qp := "&quantizationParameters=" + url.QueryEscape(`{"originPosition":"upperLeft","tolerance":1,"extent":{"xmin":-101,"ymin":0,"xmax":0,"ymax":31}}`)
	tests := []struct{ q, want string }{
		{"objectIds=2" + qp, `"transform":{"originPosition":"upperLeft","scale":[1,1],"translate":[-101,31]}`},
		{"objectIds=2" + qp, `{"x":1,"y":1}`},
		{"objectIds=2&f=geojson" + qp, `"coordinates":[-100,30]`},
		{"objectIds=2&returnGeometry=false" + qp, `"spatialReference":{"wkid":4326,"latestWkid":4326},"fields"`},
	}
	for _, tt := range tests {
		if got := run(t, tt.q); !strings.Contains(got, tt.want) {
			t.Errorf("%s answered %.300s", tt.q, got)
		}
	}
}

func TestRawSpatialQuotedRel(t *testing.T) {
	s, err := Raw{"geometry": "-115,36,-108,43", "spatialRel": `"esriSpatialRelWithin"`}.Spatial()
	if err != nil || s == nil || s.rel != "esriSpatialRelWithin" {
		t.Fatalf("got %+v, %v", s, err)
	}
}
