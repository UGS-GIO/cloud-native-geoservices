package conformance

import (
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
	"github.com/UGS-GIO/cloud-native-geoservices/internal/server"
)

// recorded is a layer's recording and what its layer description says.
type recorded struct {
	Recording
	title             string
	idField           string
	maxRecords, tiles int64
}

func load(t testing.TB, dir string, layers []Layer) []recorded {
	var out []recorded
	for _, l := range layers {
		var r recorded
		if err := ReadJSON(RecordingPath(dir, l.Name), &r.Recording); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(r.Responses, func(x Response) bool { return x.Op == "layer" })
		if i < 0 {
			t.Fatalf("%s: no layer description recorded", l.Name)
		}
		var meta struct {
			Name               string `json:"name"`
			ObjectIDField      string `json:"objectIdField"`
			MaxRecordCount     int64  `json:"maxRecordCount"`
			TileMaxRecordCount int64  `json:"tileMaxRecordCount"`
		}
		if err := json.Unmarshal(r.Responses[i].Body, &meta); err != nil {
			t.Fatal(err)
		}
		r.title, r.idField, r.maxRecords, r.tiles = meta.Name, meta.ObjectIDField, meta.MaxRecordCount, meta.TileMaxRecordCount
		out = append(out, r)
	}
	return out
}

// serve serves a layer's copy with main's default limits and the reference's page sizes.
func serve(t testing.TB, dir string, r recorded) *httptest.Server {
	type layer struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	var cfg struct {
		Layers []layer `json:"layers"`
	}
	abs, err := filepath.Abs(DataPath(dir, r.Layer.Name))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Layers = append(cfg.Layers, layer{r.Layer.Name, r.title, abs})
	b, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "layers.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &provider.Provider{Catalog: catalog.New(catalog.Config{URL: path}, time.Hour),
		Reader: pq.NewReader(64<<20, 256<<20, 256<<20), Shapes: geom.NewShapes(64 << 20),
		MaxRecords: r.maxRecords, TileMaxRecords: r.tiles, MaxGeometryRows: 50000, PrecisionMM: 1}
	s := server.New(p, 300, "", server.Limits{MaxWhere: 4096, MaxBody: 1 << 20, MaxObjectIDs: 10000,
		Timeout: 5 * time.Minute, MaxScans: 2, ScanWait: time.Minute, MaxWork: 8, MaxLight: 8, MaxInFlight: 64,
		WorkWait: time.Minute}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ask sends a recorded request to our server.
func ask(t testing.TB, base string, layer string, req Request) (int, []byte, bool, time.Duration) {
	u := base + "/ugs/rest/services/" + url.PathEscape(layer) + "/default/FeatureServer/0"
	if req.Op != "layer" {
		u += "/query"
	}
	params := url.Values{}
	for k, v := range Resolve(req.Params, "feature_id") {
		params.Set(k, v)
	}
	var res *http.Response
	var err error
	start := time.Now()
	if req.Method == http.MethodPost {
		res, err = http.PostForm(u, params)
	} else {
		res, err = http.Get(u + "?" + params.Encode())
	}
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	pbf := req.Params["f"] == "pbf" && !strings.HasPrefix(string(body), "{")
	return res.StatusCode, body, pbf, time.Since(start)
}

// timing is how long the reference and we took to answer one request.
type timing struct {
	name       string
	esri, ours time.Duration
}

// check asks every recorded request of every layer and fails on each difference the gaps file
// does not list, and on each known gap that no longer happens.
func check(t *testing.T, dir, gapsPath string, layers []Layer) []timing {
	gaps, err := LoadGaps(gapsPath)
	if err != nil {
		t.Fatal(err)
	}
	recs := load(t, dir, layers)
	type pattern struct {
		gap *Gap
		i   int
	}
	used := map[pattern]bool{}
	var times []timing
	for _, rec := range recs {
		ts := serve(t, dir, rec)
		for _, r := range rec.Responses {
			status, body, pbf, took := ask(t, ts.URL, rec.Layer.Name, r.Request)
			times = append(times, timing{rec.Layer.Name + "/" + r.Name, time.Duration(r.Millis) * time.Millisecond, took})
			refBody := []byte(r.Body)
			if r.PBF != nil {
				refBody = r.PBF
			}
			ref, err := Normalize(r.Request, r.Status, refBody, r.PBF != nil, rec.idField)
			if err != nil {
				t.Fatalf("%s reference: %v", rec.Layer.Name, err)
			}
			ours, err := Normalize(r.Request, status, body, pbf, "feature_id")
			if err != nil {
				t.Errorf("%s/%s: %v: %.200s", rec.Layer.Name, r.Name, err, body)
				continue
			}
			_, ordered := r.Params["orderByFields"]
			for _, d := range Compare(ref, ours, ordered) {
				key := Key(rec.Layer.Name, r.Name, d)
				if slices.ContainsFunc(gaps.Ignore, func(g *Gap) bool { return len(g.Matches(key)) > 0 }) {
					continue
				}
				known := false
				for _, g := range gaps.Known {
					for _, i := range g.Matches(key) {
						used[pattern{g, i}], known = true, true
					}
				}
				if !known {
					t.Errorf("%s ×%d: %s", key, d.Count, d.Example)
				}
			}
		}
	}
	for _, g := range gaps.Known {
		for i, m := range g.Match {
			if !used[pattern{g, i}] {
				t.Errorf("known gap no longer happens, remove it from %s: %s", gapsPath, m)
			}
		}
	}
	return times
}
