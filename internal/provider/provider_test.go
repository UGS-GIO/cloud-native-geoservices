package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/catalog"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/query"
)

func fixture(t *testing.T) *Provider {
	root, _ := filepath.Abs("../../testdata")
	items, err := os.ReadFile(filepath.Join(root, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "items.json")
	os.WriteFile(path, []byte(strings.ReplaceAll(string(items), `"testdata/`, `"`+root+"/")), 0o644)
	return &Provider{Catalog: catalog.New(path, time.Minute), Reader: pq.NewReader(16<<20, 16<<20, 64<<20),
		Shapes: geom.NewShapes(16 << 20), MaxRecords: 2000, TileMaxRecords: 5, MaxGeometryRows: 50000}
}

func load(t *testing.T, pr *Provider, topic string, params query.Raw) *query.Collection {
	c, err := pr.Load(context.Background(), topic, "default", true, params)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMetadata(t *testing.T) {
	pr := fixture(t)
	c, err := pr.Load(context.Background(), "wells", "by-purpose", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Meta.Name != "Wells (by-purpose)" || c.Meta.GeometryType != "Point" || c.Meta.MaxRecordCount != 2000 {
		t.Errorf("metadata %+v", c.Meta)
	}
	if _, err := pr.Load(context.Background(), "nope", "default", false, nil); err == nil {
		t.Error("unknown topic")
	}
}

func TestCountAndPage(t *testing.T) {
	pr := fixture(t)
	if c := load(t, pr, "qfaults", query.Raw{"where": "1=1", "returnCountOnly": "true"}); c.Count == nil || *c.Count != 300 {
		t.Errorf("count from the footer: %v", c.Count)
	}
	c := load(t, pr, "qfaults", query.Raw{"resultOffset": "295", "resultRecordCount": "10"})
	if len(c.Features) != 5 || c.Exceeded != query.False || !c.FiltersApplied["offset"] {
		t.Errorf("last page: %d features, exceeded %v", len(c.Features), c.Exceeded)
	}
	c = load(t, pr, "qfaults", query.Raw{"resultRecordCount": "10"})
	if len(c.Features) != 10 || c.Exceeded != query.True {
		t.Errorf("a full page says there is more: %d %v", len(c.Features), c.Exceeded)
	}
	c = load(t, pr, "qfaults", query.Raw{"where": "objectid > 0", "returnGeometry": "false"})
	if c.Exceeded != query.Null || len(c.Features) != 300 || c.Features[0].Geom != nil {
		t.Errorf("unpaged: exceeded %v, %d features", c.Exceeded, len(c.Features))
	}
}

func TestStatisticsCoverTheLayer(t *testing.T) {
	pr := fixture(t)
	pr.MaxGeometryRows = 10
	stats := `[{"statisticType":"count","onStatisticField":"feature_id","outStatisticFieldName":"n"}]`
	c := load(t, pr, "qfaults", query.Raw{"where": "1=1", "outStatistics": stats})
	if len(c.Features) != 300 || c.Features[0].Geom != nil {
		t.Errorf("statistics read %d rows (geometry %v), want all 300 without geometry", len(c.Features), c.Features[0].Geom)
	}
	c = load(t, pr, "qfaults", query.Raw{"outStatistics": stats, "geometry": "-180,-90,180,90"})
	if len(c.Features) != 10 {
		t.Errorf("under a spatial filter statistics read geometry, capped: %d rows", len(c.Features))
	}
}

func TestTile(t *testing.T) {
	pr := fixture(t)
	// A statewide tile at a coarse tolerance: past the tile cap, the largest features are kept and
	// the response is not flagged exceeded.
	params := query.Raw{"geometry": "-115,36,-108,43", "resultType": "tile", "maxAllowableOffset": "0.01"}
	c := load(t, pr, "geolunits", params)
	if len(c.Features) != 5 || c.Exceeded != query.False {
		t.Fatalf("%d features, exceeded %v", len(c.Features), c.Exceeded)
	}
	for i := 1; i < len(c.Features); i++ {
		if spread(c.Features[i].Geom) > spread(c.Features[i-1].Geom) {
			t.Errorf("features are not largest first at %d", i)
		}
	}
	// The same tile again comes from the shape cache with the same answer.
	again := load(t, pr, "geolunits", params)
	for i := range c.Features {
		if c.Features[i].Props[0] != again.Features[i].Props[0] {
			t.Fatal("a cached shape changed the answer")
		}
	}
}

func TestMirror(t *testing.T) {
	pr := &Provider{Mirror: "/data/"}
	if got := pr.dataURL("https://maps-assets.example/warehouse/a/a.parquet"); got != "/data/warehouse/a/a.parquet" {
		t.Errorf("mirror %s", got)
	}
	if got := (&Provider{}).dataURL("https://x/a.parquet"); got != "https://x/a.parquet" {
		t.Errorf("no mirror %s", got)
	}
}
