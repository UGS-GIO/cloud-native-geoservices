package plan

import (
	"maps"
	"math"
	"testing"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/pq"
)

var layer = Layer{Columns: []pq.Column{{Name: "feature_id"}, {Name: "acres"}, {Name: "wetland_type"}, {Name: "count"}}, BBox: true}

func TestKinds(t *testing.T) {
	tests := []struct {
		params map[string]string
		kind   Kind
	}{
		{map[string]string{"where": "1=1", "returnCountOnly": "true"}, Count},
		{map[string]string{"returnExtentOnly": "true"}, Extent},
		{map[string]string{"where": "acres > 1", "returnCountOnly": "true"}, Features},
		{map[string]string{"geometry": "1,2,3,4", "returnCountOnly": "true"}, Features},
	}
	for _, tt := range tests {
		p, err := New(tt.params, layer, 2000)
		if err != nil || p.Kind != tt.kind {
			t.Errorf("%v: kind %v err %v", tt.params, p.Kind, err)
		}
	}
}

func TestStream(t *testing.T) {
	box := map[string]string{"geometry": "1,2,3,4", "returnCountOnly": "true"}
	with := func(k, v string) map[string]string {
		m := maps.Clone(box)
		m[k] = v
		return m
	}
	tests := []struct {
		params map[string]string
		stream bool
	}{
		{box, true},
		{with("returnIdsOnly", "true"), true},
		{with("spatialRel", "esriSpatialRelWithin"), true},
		{with("returnExtentOnly", "true"), true},
		{with("f", "geojson"), true},
		{with("returnDistinctValues", "true"), false},
		{map[string]string{"returnCountOnly": "true", "where": "acres > 1"}, false},
	}
	for _, tt := range tests {
		if p, _ := New(tt.params, layer, 2000); p.Stream != tt.stream {
			t.Errorf("%v: stream %v", tt.params, p.Stream)
		}
	}
	if p, _ := New(box, layer, 2000); len(p.Columns) != 1 {
		t.Errorf("a count reads %v", p.Columns)
	}
}

func TestPaging(t *testing.T) {
	tests := []struct {
		params map[string]string
		page   *pq.Page
	}{
		{map[string]string{"where": "1=1", "resultOffset": "100", "resultRecordCount": "50"}, &pq.Page{Offset: 100, Limit: 50}},
		{map[string]string{"resultRecordCount": "50000"}, &pq.Page{Limit: 2000}},
		{map[string]string{"orderByFields": "feature_id ASC"}, &pq.Page{Limit: 2000}},
		{map[string]string{"orderByFields": "acres DESC"}, nil},
		{map[string]string{"where": "acres > 1"}, nil},
		{map[string]string{"outStatistics": "[]"}, nil},
		{map[string]string{"returnDistinctValues": "true", "resultRecordCount": "5"}, nil},
		{map[string]string{"geometry": "1,2,3,4"}, &pq.Page{Limit: 2000}},
	}
	for _, tt := range tests {
		p, _ := New(tt.params, layer, 2000)
		if (p.Page == nil) != (tt.page == nil) || p.Page != nil && *p.Page != *tt.page {
			t.Errorf("%v: page %v, want %v", tt.params, p.Page, tt.page)
		}
	}
	if p, _ := New(map[string]string{"geometry": "1,2,3,4"}, Layer{Columns: layer.Columns}, 2000); p.Page != nil {
		t.Error("without a covering, a spatial filter's page comes after the exact test")
	}
}

func TestColumns(t *testing.T) {
	p, _ := New(map[string]string{"outFields": "WETLAND_TYPE", "where": "acres > 1"}, layer, 2000)
	if want := []string{"feature_id", "acres", "wetland_type"}; len(p.Columns) != 3 || p.Columns[1] != want[1] || p.Columns[2] != want[2] {
		t.Errorf("columns %v", p.Columns)
	}
}

func TestFilterBox(t *testing.T) {
	box := FilterBox(map[string]string{"geometry": `{"xmin":-12523442.7,"ymin":4931105.5,"xmax":-12445171.1,"ymax":5009377.0,"spatialReference":{"wkid":102100}}`})
	if box == nil || math.Abs(box[0]-(-112.5)) > 1e-3 || math.Abs(box[3]-40.979) > 1e-2 {
		t.Errorf("mercator box %v", box)
	}
	// 1 km is about 0.009° of latitude and, at 40°N, 0.0117° of longitude.
	for _, g := range []string{"-112,40", `{"x":-112,"y":40}`} {
		b := FilterBox(map[string]string{"geometry": g, "distance": "1", "units": "esriSRUnit_Kilometer"})
		if b == nil || math.Abs(b[3]-b[1]-0.0181) > 2e-4 || math.Abs(b[2]-b[0]-0.0235) > 2e-4 {
			t.Errorf("%s: distance pads the box by latitude: %v", g, b)
		}
	}
	if FilterBox(map[string]string{"geometry": "1,2,3,4", "inSR": "2263"}) != nil {
		t.Error("an SR we do not project has no prefilter")
	}
}

func TestObjectIDs(t *testing.T) {
	if _, err := New(map[string]string{"objectIds": "1,a"}, layer, 2000); err == nil {
		t.Error("non-integer ids")
	}
	p, _ := New(map[string]string{"objectIds": "1, 2,,3"}, layer, 2000)
	if len(p.IDs) != 3 {
		t.Errorf("ids %v", p.IDs)
	}
}

func TestTolerance(t *testing.T) {
	p, _ := New(map[string]string{"maxAllowableOffset": "111.32", "outSR": "102100"}, layer, 2000)
	if math.Abs(p.Tolerance-0.001) > 1e-12 {
		t.Errorf("tolerance %v", p.Tolerance)
	}
	pt, _ := New(map[string]string{"maxAllowableOffset": "111.32"}, Layer{Columns: layer.Columns, Point: true}, 2000)
	if pt.Tolerance != 0 {
		t.Error("points are not simplified")
	}
}
