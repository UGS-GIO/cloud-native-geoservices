package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/catalog"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/provider"
)

// fixture serves the testdata topics from local GeoParquet files.
func fixture(t *testing.T) *httptest.Server {
	root, _ := filepath.Abs("../../testdata")
	return fixtureAt(t, root)
}

// fixtureAt serves the testdata topics with their data files under base (a directory or URL).
func fixtureAt(t *testing.T, base string) *httptest.Server {
	items, err := os.ReadFile("../../testdata/items.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "items.json")
	os.WriteFile(path, []byte(strings.ReplaceAll(string(items), `"testdata/`, `"`+base+"/")), 0o644)
	p := &provider.Provider{Catalog: catalog.New(path, time.Minute), Reader: pq.NewReader(32<<20, 64<<20, 64<<20),
		Shapes: geom.NewShapes(16 << 20), MaxRecords: 2000, TileMaxRecords: 8000, MaxGeometryRows: 50000}
	s := New(p, 300, "", Limits{MaxWhere: 100, MaxBody: 1024, MaxObjectIDs: 5, Timeout: 10 * time.Second,
		MaxScans: 1, ScanWait: time.Second, MaxWork: 2, MaxLight: 4, MaxInFlight: 64, WorkWait: time.Second}, 8<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, u string, header ...string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	b, _ := io.ReadAll(res.Body)
	json.Unmarshal(b, &body)
	return res, body
}

func errorCode(body map[string]any) float64 {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return code
}

func TestQueries(t *testing.T) {
	ts := fixture(t)
	q := ts.URL + "/ugs/rest/services/qfaults/default/FeatureServer/0/query?"
	tests := []struct {
		name, query string
		check       func(map[string]any) bool
	}{
		{"count", "where=1=1&returnCountOnly=true", func(b map[string]any) bool { return b["count"] == 300.0 }},
		{"page", "resultRecordCount=10&outFields=*", func(b map[string]any) bool {
			return len(b["features"].([]any)) == 10 && b["exceededTransferLimit"] == true
		}},
		{"box without geometry", "geometry=-114,36,-109,43&returnGeometry=false&resultRecordCount=5", func(b map[string]any) bool {
			f := b["features"].([]any)
			_, hasGeom := f[0].(map[string]any)["geometry"]
			return len(f) == 5 && !hasGeom
		}},
		{"where", "where=" + url.QueryEscape("feature_id IN (1,2,3)") + "&returnIdsOnly=true", func(b map[string]any) bool {
			return len(b["objectIds"].([]any)) == 3
		}},
		{"bad where", "where=" + url.QueryEscape("x ==="), func(b map[string]any) bool { return errorCode(b) == 400 }},
		{"where too long", "where=" + strings.Repeat("a", 101), func(b map[string]any) bool { return errorCode(b) == 400 }},
		{"too many ids", "objectIds=1,2,3,4,5,6", func(b map[string]any) bool { return errorCode(b) == 400 }},
	}
	for _, tt := range tests {
		res, body := get(t, q+tt.query)
		if res.StatusCode != 200 || !tt.check(body) {
			t.Errorf("%s: status %d body %v", tt.name, res.StatusCode, body)
		}
	}
	res, body := get(t, ts.URL+"/ugs/rest/services/nope/default/FeatureServer/0?f=json")
	if errorCode(body) != 404 || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("unknown topic: %v %q", body, res.Header.Get("Cache-Control"))
	}
	_, body = get(t, ts.URL+"/ugs/rest/services/wells/nope/FeatureServer/0?f=json")
	if e, _ := body["error"].(map[string]any); e["message"] != "unknown render 'nope' for wells (have: by-purpose, by-boxtype)" {
		t.Errorf("unknown render: %v", body)
	}
}

func TestETag(t *testing.T) {
	ts := fixture(t)
	u := ts.URL + "/ugs/rest/services/geolunits/default/FeatureServer/0/query?f=json&resultRecordCount=3"
	res, _ := get(t, u)
	tag := res.Header.Get("ETag")
	if !strings.HasPrefix(tag, `"`) || !strings.Contains(res.Header.Get("Cache-Control"), "stale-while-revalidate=300") {
		t.Fatalf("headers %v", res.Header)
	}
	again, _ := get(t, u)
	if again.Header.Get("ETag") != tag {
		t.Error("same URL and data, different ETag")
	}
	if res, _ := get(t, u, "If-None-Match", tag); res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", res.StatusCode)
	}
	if other, _ := get(t, u+"&outSR=102100"); other.Header.Get("ETag") == tag {
		t.Error("different query, same ETag")
	}
}

func TestBodyLimit(t *testing.T) {
	ts := fixture(t)
	res, err := http.Post(ts.URL+"/ugs/rest/services/qfaults/default/FeatureServer/0/query", "application/x-www-form-urlencoded",
		strings.NewReader("where="+strings.Repeat("x", 2000)))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	if errorCode(body) != 400 {
		t.Errorf("large body: %v", body)
	}
	res, _ = http.Post(ts.URL+"/ugs/rest/services/qfaults/default/FeatureServer/0/query", "application/x-www-form-urlencoded",
		strings.NewReader("where=1%3D1&returnCountOnly=true&f=json"))
	json.NewDecoder(res.Body).Decode(&body)
	if body["count"] != 300.0 {
		t.Errorf("form POST: %v", body)
	}
}

// A request that gives up while a layer opens must not fail the requests waiting on the same open.
func TestCancelledOpenDoesNotFailWaiters(t *testing.T) {
	root, _ := filepath.Abs("../../testdata")
	files := http.FileServer(http.Dir(root))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	ts := fixtureAt(t, origin.URL)
	u := ts.URL + "/ugs/rest/services/qfaults/default/FeatureServer/0/query?where=1=1&returnCountOnly=true&f=json"

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()
	time.Sleep(10 * time.Millisecond)
	res, body := get(t, u)
	if res.StatusCode != 200 || body["count"] != 300.0 {
		t.Fatalf("waiter got %d %v", res.StatusCode, body)
	}
}

// A POST's query string and form body resolve to one value per parameter for the plan, the limits
// and the filters alike (the query string wins, as in Koop).
func TestPostParameterPrecedence(t *testing.T) {
	ts := fixture(t)
	post := func(query, form string) map[string]any {
		res, err := http.Post(ts.URL+"/ugs/rest/services/qfaults/default/FeatureServer/0/query?"+query,
			"application/x-www-form-urlencoded", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]any
		json.NewDecoder(res.Body).Decode(&body)
		return body
	}
	if b := post("where="+url.QueryEscape("feature_id < 10")+"&returnCountOnly=true&f=json", "where=1%3D1"); b["count"] != 9.0 {
		t.Errorf("count %v, want 9", b)
	}
	long := url.QueryEscape("feature_id IN (" + strings.Repeat("1,", 60) + "1)")
	if b := post("where="+long+"&f=json", "where=1%3D1"); errorCode(b) != 400 {
		t.Errorf("a long where in the query string passed the limit: %v", b)
	}
}
