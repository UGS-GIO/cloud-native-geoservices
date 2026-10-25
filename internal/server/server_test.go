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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/catalog"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/pq"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/provider"
)

// fixture serves the testdata topics from local GeoParquet files.
func fixture(t testing.TB) *httptest.Server {
	root, _ := filepath.Abs("../../testdata")
	return fixtureAt(t, root, "gzip")
}

// fixtureAt serves the testdata topics with their data files under base (a directory or URL), and
// the content codings in compression.
func fixtureAt(t testing.TB, base, compression string) *httptest.Server {
	items, err := os.ReadFile("../../testdata/items.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "items.json")
	os.WriteFile(path, []byte(strings.ReplaceAll(string(items), `"testdata/`, `"`+base+"/")), 0o644)
	p := &provider.Provider{Catalog: catalog.New(catalog.STAC{URL: path}, time.Minute), Reader: pq.NewReader(32<<20, 64<<20, 64<<20),
		Shapes: geom.NewShapes(16 << 20), MaxRecords: 2000, TileMaxRecords: 8000, MaxGeometryRows: 50000, PrecisionMM: 1}
	s := New(p, 300, "", Limits{MaxWhere: 100, MaxBody: 1024, MaxObjectIDs: 5, Timeout: 10 * time.Second,
		MaxScans: 1, ScanWait: time.Second, MaxWork: 2, MaxLight: 4, MaxInFlight: 64, WorkWait: time.Second}, 8<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Compress(compression); err != nil {
		t.Fatal(err)
	}
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
		{"page", "where=1=1&resultRecordCount=10&outFields=*", func(b map[string]any) bool {
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

func TestStream(t *testing.T) {
	defer func(n int) { streamOver = n }(streamOver)
	streamOver = 4 << 10
	ts := fixture(t)
	q := ts.URL + "/ugs/rest/services/geolunits/default/FeatureServer/0/query?where=1=1&"
	for _, tt := range []struct {
		query, coding string
		chunked       bool
	}{{"resultRecordCount=50", "", true}, {"resultRecordCount=50", "gzip", true}, {"returnCountOnly=true", "", false}} {
		res, _ := get(t, q+tt.query, "Accept-Encoding", tt.coding)
		chunked := slices.Contains(res.TransferEncoding, "chunked") && res.ContentLength == -1
		if res.StatusCode != 200 || chunked != tt.chunked {
			t.Errorf("%s %q: status %d, transfer %v, length %d", tt.query, tt.coding, res.StatusCode, res.TransferEncoding, res.ContentLength)
		}
	}
}

func TestDefaultPrecision(t *testing.T) {
	ts := fixture(t)
	q := ts.URL + "/ugs/rest/services/geolunits/default/FeatureServer/0/query?where=1=1&resultRecordCount=5&f=json"
	for _, tt := range []struct {
		query string
		most  int
	}{{"", 8}, {"&outSR=102100", 3}, {"&geometryPrecision=2", 2}, {"&geometryPrecision=21", 15}} {
		res, err := http.Get(q + tt.query)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Features []struct {
				Geometry struct{ Rings [][][]json.Number }
			}
		}
		d := json.NewDecoder(res.Body)
		d.UseNumber()
		d.Decode(&body)
		res.Body.Close()
		most := 0
		for _, f := range body.Features {
			for _, ring := range f.Geometry.Rings {
				for _, pt := range ring {
					for _, n := range pt {
						if _, frac, ok := strings.Cut(n.String(), "."); ok {
							most = max(most, len(frac))
						}
					}
				}
			}
		}
		if most > tt.most || tt.most == 15 && most <= 8 {
			t.Errorf("%q: %d decimals, want at most %d", tt.query, most, tt.most)
		}
	}
}

func TestETag(t *testing.T) {
	ts := fixture(t)
	u := ts.URL + "/ugs/rest/services/geolunits/default/FeatureServer/0/query?f=json&where=1=1&resultRecordCount=3"
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
	defer func(b string) { build = b }(build)
	build = "v9.9.9"
	if res, _ := get(t, u, "If-None-Match", tag); res.StatusCode != http.StatusOK || res.Header.Get("ETag") == tag {
		t.Errorf("a new release revalidated the old ETag: %d", res.StatusCode)
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
	ts := fixtureAt(t, origin.URL, "gzip")
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
// and the filters alike (the query string wins).
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

// spatialRel reads as "query geometry <rel> feature", as ArcGIS Server does: a polygon around
// point features Contains them, and is never Within one.
func TestSpatialRelDirection(t *testing.T) {
	ts := fixture(t)
	polygon := url.QueryEscape(`{"rings":[[[-114,37],[-109,37],[-109,42],[-114,42],[-114,37]]],"spatialReference":{"wkid":4326}}`)
	count := func(rel string) any {
		_, b := get(t, ts.URL+"/ugs/rest/services/wells/by-purpose/FeatureServer/0/query?geometryType=esriGeometryPolygon&inSR=4326"+
			"&returnCountOnly=true&f=json&spatialRel="+rel+"&geometry="+polygon)
		return b["count"]
	}
	intersects := count("esriSpatialRelIntersects")
	if n, _ := intersects.(float64); n == 0 {
		t.Fatalf("no points inside the polygon: %v", intersects)
	}
	if c := count("esriSpatialRelContains"); c != intersects {
		t.Errorf("Contains %v, want every point inside (%v)", c, intersects)
	}
	if w := count("esriSpatialRelWithin"); w != 0.0 {
		t.Errorf("Within %v, want 0: a polygon is not within a point", w)
	}
}

func TestRestInfoHasNoTokenService(t *testing.T) {
	ts := fixture(t)
	_, body := get(t, ts.URL+"/ugs/rest/info?f=json")
	auth, _ := body["authInfo"].(map[string]any)
	if auth["isTokenBasedSecurity"] != false || auth["tokenServicesUrl"] != nil {
		t.Errorf("authInfo %v", auth)
	}
	if res, _ := http.Get(ts.URL + "/ugs/rest/generateToken"); res.StatusCode != http.StatusNotFound {
		t.Errorf("generateToken: %s", res.Status)
	}
}

func getText(t *testing.T, u string) (*http.Response, string) {
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestServicesDirectory(t *testing.T) {
	ts := fixture(t)
	for _, u := range []string{"/ugs/rest/services?f=json", "/ugs/rest/services/?f=pjson"} {
		res, body := get(t, ts.URL+u)
		folders, _ := body["folders"].([]any)
		if res.Header.Get("ETag") == "" || body["currentVersion"] != 12.0 || len(body["services"].([]any)) != 0 ||
			!slices.Equal(folders, []any{"geolunits", "qfaults", "wells"}) {
			t.Errorf("%s: %v", u, body)
		}
	}
	for _, u := range []string{"/ugs/rest/services/wells?f=json", "/ugs/rest/services/wells/?f=json"} {
		_, body := get(t, ts.URL+u)
		var names []string
		for _, s := range body["services"].([]any) {
			m := s.(map[string]any)
			names = append(names, m["name"].(string)+" "+m["type"].(string))
		}
		if len(body["folders"].([]any)) != 0 || !slices.Equal(names, []string{"wells/by-purpose FeatureServer", "wells/by-boxtype FeatureServer"}) {
			t.Errorf("%s: %v", u, body)
		}
	}
	if _, body := get(t, ts.URL+"/ugs/rest/services/nope?f=json"); errorCode(body) != 404 {
		t.Errorf("unknown folder: %v", body)
	}
	for u, want := range map[string]string{"/ugs/rest/services": `href="/ugs/rest/services/qfaults"`,
		"/ugs/rest/services/wells": `href="` + ts.URL + `/ugs/rest/services/wells/by-purpose/FeatureServer"`} {
		res, body := getText(t, ts.URL+u)
		if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, want) {
			t.Errorf("%s: %s %s", u, res.Header.Get("Content-Type"), body)
		}
	}
	_, page := getText(t, ts.URL+"/ugs/rest/services/wells")
	svc := url.QueryEscape(ts.URL + "/ugs/rest/services/wells/by-purpose/FeatureServer")
	if !strings.Contains(page, `data-url="`+ts.URL+"/ugs/rest/services/wells/by-purpose/FeatureServer\"") ||
		!strings.Contains(page, "mapviewer/index.html?url="+svc) {
		t.Errorf("copy URL or Map Viewer link missing: %s", page)
	}
}

func TestNotFoundShowsPath(t *testing.T) {
	ts := fixture(t)
	for path, want := range map[string]string{"/a/b": "Cannot GET /a/b<", "/a/%3Cscript%3E": "Cannot GET /a/&lt;script&gt;<"} {
		res, body := getText(t, ts.URL+path)
		if res.StatusCode != http.StatusNotFound || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s", path, res.StatusCode, body)
		}
	}
}
