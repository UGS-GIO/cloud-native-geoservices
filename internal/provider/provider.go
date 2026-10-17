// Package provider loads `/ugs/rest/services/{topic}/{render}/FeatureServer` data: the topic's
// metadata and, for a query, the features its plan selects from the GeoParquet.
package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/paulmach/orb"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/catalog"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/esri"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/jsonx"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/plan"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/pq"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/query"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/remote"
)

const unlimited = int64(1) << 50

// ErrBusy is returned when the geometry a request would decode does not fit in the budget soon.
var ErrBusy = errors.New("busy: too much geometry being read; retry shortly")

// Provider ties the catalog, the parquet reader and the shape cache together.
type Provider struct {
	Catalog *catalog.Catalog
	Reader  *pq.Reader
	Shapes  *geom.Shapes
	// MaxRecords is a page, like ArcGIS Server's maxRecordCount (2000).
	MaxRecords int64
	// TileMaxRecords: Map Viewer splits a tile whose response exceeds its limit into four and asks
	// again, so a tile request gets a larger page (8000); a dense tile then answers once.
	TileMaxRecords int64
	// MaxGeometryRows caps the geometries one request reads before where filtering, so one
	// query cannot exhaust memory; past it the response says the transfer limit was exceeded.
	MaxGeometryRows int64
	// GeometryBudget bounds the compressed geometry bytes being fetched and decoded at once,
	// across requests; a request that cannot get its share within BudgetWait gets ErrBusy.
	GeometryBudget *semaphore.Weighted
	BudgetBytes    int64
	BudgetWait     time.Duration
	// Heavy is a request whose geometry estimate passes HeavyBytes: only len(HeavySlots) of them
	// decode at once, so light requests (deep tiles, identify) keep a CPU.
	HeavyBytes int64
	HeavySlots chan struct{}
	// Mirror, when set, replaces the scheme and host of data URLs: a local directory or another
	// range-capable server holding the same paths (to measure the server without the CDN).
	Mirror string
}

// dataURL applies the mirror to a data URL.
func (pr *Provider) dataURL(href string) string {
	if pr.Mirror == "" {
		return href
	}
	u, err := url.Parse(href)
	if err != nil || u.Host == "" {
		return href
	}
	return strings.TrimSuffix(pr.Mirror, "/") + u.Path
}

// Version names everything a render's responses depend on: its catalog entry, data file and
// Esri renderer.
func (pr *Provider) Version(ctx context.Context, topicID, render string) (string, error) {
	item, err := pr.Catalog.Topic(ctx, topicID)
	if err != nil {
		return "", err
	}
	layer, err := pr.Open(ctx, item.Data())
	if err != nil {
		return "", err
	}
	d, err := pr.Catalog.Drawing(ctx, item, render)
	if err != nil {
		return "", err
	}
	return item.Version + "|" + layer.ID() + "|" + d.Version, nil
}

// Open opens a data file, cached like the catalog.
func (pr *Provider) Open(ctx context.Context, href string) (*pq.Layer, error) {
	u := pr.dataURL(href)
	key := "layer:" + u
	v, err := pr.Catalog.Cache.Get(key, func() (any, error) {
		// Shared with every request waiting on this key, so one that is cancelled cannot fail the rest.
		ctx := context.WithoutCancel(ctx)
		// Once per CACHE_TTL: keep the open layer when a HEAD says the file has not changed.
		if last, ok := pr.Catalog.Cache.Last(key); ok {
			if v, err := pr.Reader.Version(ctx, u); err == nil && v == last.(*pq.Layer).Version() {
				return last, nil
			}
		}
		return pr.Reader.Open(ctx, u)
	})
	if err != nil {
		return nil, err
	}
	return v.(*pq.Layer), nil
}

// Load returns the collection for a request. Service and layer descriptors (isQuery false) only
// need the metadata. A read refused because the file was re-ingested since it was opened reopens
// the topic's files once, rather than failing until the next version check.
func (pr *Provider) Load(ctx context.Context, topicID, render string, isQuery bool, params query.Raw) (*query.Collection, error) {
	item, err := pr.Catalog.Topic(ctx, topicID)
	if err != nil {
		return nil, err
	}
	c, err := pr.load(ctx, item, topicID, render, isQuery, params)
	if errors.Is(err, remote.ErrChanged) {
		for _, a := range item.Assets {
			pr.Catalog.Cache.Forget("layer:" + pr.dataURL(a.Href))
		}
		c, err = pr.load(ctx, item, topicID, render, isQuery, params)
	}
	return c, err
}

func (pr *Provider) load(ctx context.Context, item *catalog.Item, topicID, render string, isQuery bool, params query.Raw) (*query.Collection, error) {
	var layer *pq.Layer
	var drawing *catalog.Drawing
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { layer, err = pr.Open(gctx, item.Data()); return })
	g.Go(func() (err error) { drawing, err = pr.Catalog.Drawing(gctx, item, render); return })
	if err := g.Wait(); err != nil {
		return nil, err
	}
	style := drawing.Info

	name := topicID
	if item.Title != nil {
		name = *item.Title
	}
	if render != "default" {
		name += " (" + render + ")"
	}
	meta := &esri.Metadata{Name: name, Description: item.Description, IDField: "feature_id",
		GeometryType: layer.GeometryType, Fields: layer.Columns, Extent: layer.Extent,
		MaxRecordCount: int(pr.MaxRecords), Renderer: style["renderer"], LabelingInfo: style["labelingInfo"],
		MinScale: style["minScale"]}
	c := &query.Collection{Meta: meta}
	if !isQuery {
		return c, nil
	}

	maxRecords := pr.MaxRecords
	if params["resultType"] == "tile" {
		maxRecords = pr.TileMaxRecords
	}
	p, err := plan.New(params, plan.Layer{Columns: layer.Columns, Point: layer.Point}, maxRecords)
	if err != nil {
		return nil, query.Bad(err.Error())
	}
	// Small-scale requests on a heavy layer read its coarsest generalized copy that is still finer
	// than the request (vector/sink_archive.py).
	source := layer
	if ov := overview(item, p.Tolerance); ov != "" && p.Kind == plan.Features && p.Geometry && len(p.IDs) == 0 {
		if source, err = pr.Open(ctx, ov); err != nil {
			return nil, err
		}
		if p, err = plan.New(params, plan.Layer{Columns: source.Columns, Point: source.Point}, maxRecords); err != nil {
			return nil, query.Bad(err.Error())
		}
	}
	if p.Kind != plan.Features {
		n := layer.Rows
		c.Count = &n
		if p.Kind == plan.Extent && layer.Extent != nil {
			c.Extent = projectExtent(layer.Extent, p.OutSR)
		}
		return c, nil
	}
	c.FiltersApplied = p.FiltersApplied
	c.Props = p.Columns
	// Which rows have a geometry comes from the covering column; a file without one reads geometry.
	if p.Present && !source.HasBBox {
		p.Geometry, p.Present = true, false
	}

	// At a drawing scale, read past the page: specks are thinned below, as the vector tiles do,
	// so a dense small-scale tile still fits in one page instead of splitting into hundreds.
	thin := p.Page != nil && p.Tolerance > 0
	// An overview's rows are small, so a drawing tile reads all of them rather than cutting the
	// Hilbert-ordered rows off partway across the tile.
	readLimit := unlimited
	if source == layer {
		readLimit = pr.MaxGeometryRows
	}
	sel := pq.Selection{IDs: p.IDs, Box: p.Box, Page: p.Page}
	if thin {
		sel.Page = &pq.Page{Offset: p.Page.Offset, Limit: readLimit}
	}
	rows, err := source.Select(ctx, sel)
	if err != nil {
		return nil, err
	}
	limit := unlimited
	switch {
	case p.Page != nil:
		limit = p.Page.Limit
	case p.Geometry:
		limit = pr.MaxGeometryRows
	}
	readRows := rows
	if thin {
		readRows = rows[:min(int64(len(rows)), readLimit)]
	} else if int64(len(rows)) > limit {
		readRows = rows[:limit+1]
	}
	feats, err := pr.features(ctx, source, p, readRows, thin)
	if err != nil {
		return nil, err
	}
	// A drawing tile over the limit keeps its largest features, as the vector tiles drop the
	// smallest: flagging it exceeded makes Map Viewer split it into four and ask again, repeatedly.
	tile := params["resultType"] == "tile" && thin
	if tile && int64(len(feats)) > limit {
		for i := range feats {
			if !feats[i].speck {
				feats[i].size = spread(feats[i].f.Geom)
			}
		}
		slices.SortStableFunc(feats, func(a, b shaped) int { return cmp.Compare(b.size, a.size) })
	}
	// Mirrors JavaScript's `!tile && (over || (thin && read past))`, where thin is null without a page.
	switch {
	case tile:
		c.Exceeded = query.False
	case int64(len(feats)) > limit:
		c.Exceeded = query.True
	case p.Page == nil:
		c.Exceeded = query.Null
	case thin && int64(len(rows)) > readLimit:
		c.Exceeded = query.True
	}
	if int64(len(feats)) > limit {
		feats = feats[:limit]
	}
	c.Features = make([]query.Feature, len(feats))
	for i := range feats {
		c.Features[i] = feats[i].f
	}
	return c, nil
}

// cell is a tolerance-sized cell and a symbol class (the feature's other attributes joined, as
// the JavaScript provider keys it): one speck is kept per cell.
type cell struct {
	x, y  float64
	class string
}

type shaped struct {
	f     query.Feature
	speck bool
	size  float64 // spread, when ranking a tile's features
}

// features reads the plan's columns (and geometry) at rows and builds the features, thinning
// specks to one per tolerance-sized cell per symbol class when drawing.
func (pr *Provider) features(ctx context.Context, source *pq.Layer, p *plan.Plan, rows []int64, thin bool) ([]shaped, error) {
	values, err := source.Read(ctx, p.Columns, rows)
	if err != nil {
		return nil, err
	}
	shapes, has, err := pr.shapes(ctx, source, p, values, rows)
	if err != nil {
		return nil, err
	}
	if p.Present {
		bbox, err := source.Read(ctx, pq.BBox[:1], rows)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			has[i] = bbox.Get(0, i) != nil
		}
	}
	out := make([]shaped, 0, len(rows))
	cells := map[cell]bool{}
	var class strings.Builder
	for i := range rows {
		props := make([]any, len(p.Columns))
		for c := range p.Columns {
			props[c] = values.Get(c, i)
		}
		var g orb.Geometry
		if (p.Geometry || p.Present) && !has[i] {
			continue
		}
		if p.Geometry {
			g = shapes[i].G
		}
		if thin && shapes[i].Speck {
			if pt, ok := firstPoint(g); ok {
				class.Reset()
				first := true
				for c, name := range p.Columns {
					if name == "feature_id" {
						continue
					}
					if !first {
						class.WriteByte('|')
					}
					first = false
					class.WriteString(js.Join(props[c]))
				}
				k := cell{math.Floor(pt[0] / p.Tolerance), math.Floor(pt[1] / p.Tolerance), class.String()}
				if cells[k] {
					continue
				}
				cells[k] = true
			}
		}
		out = append(out, shaped{f: query.Feature{Props: props, Geom: g}, speck: shapes[i].Speck})
	}
	return out, nil
}

// shapes are the rows' geometries at the plan's tolerance: from the shape cache, reading and
// simplifying WKB (in parallel) only for the rows it does not hold.
func (pr *Provider) shapes(ctx context.Context, source *pq.Layer, p *plan.Plan, values *pq.Rows, rows []int64) ([]geom.Shape, []bool, error) {
	shapes := make([]geom.Shape, len(rows))
	has := make([]bool, len(rows))
	if !p.Geometry {
		return shapes, has, nil
	}
	idCol := slices.Index(p.Columns, "feature_id")
	ids := make([]int64, len(rows))
	var missing []int
	for i := range rows {
		if idCol >= 0 {
			ids[i], _ = values.Get(idCol, i).(int64)
		}
		if p.Tolerance > 0 && idCol >= 0 {
			if sh, ok, found := pr.Shapes.Lookup(source.ID(), ids[i], p.Tolerance); found {
				shapes[i], has[i] = sh, ok
				continue
			}
		}
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return shapes, has, nil
	}
	missingRows := make([]int64, len(missing))
	for k, i := range missing {
		missingRows[k] = rows[i]
	}
	release, err := pr.admit(ctx, source.GeomBytes(missingRows))
	if err != nil {
		return nil, nil, err
	}
	defer release()
	wkb, err := source.Read(ctx, []string{source.Geom}, missingRows)
	if err != nil {
		return nil, nil, err
	}
	g, _ := errgroup.WithContext(ctx)
	workers := runtime.GOMAXPROCS(0)
	chunk := max(1, (len(missing)+workers-1)/workers)
	for from := 0; from < len(missing); from += chunk {
		to := min(len(missing), from+chunk)
		g.Go(func() error {
			for k := from; k < to; k++ {
				i := missing[k]
				var err error
				if p.Tolerance > 0 && idCol >= 0 {
					shapes[i], has[i], err = pr.Shapes.Make(source.ID(), ids[i], wkb.Bytes(0, k), p.Tolerance)
				} else {
					shapes[i], has[i], err = geom.FromWKB(wkb.Bytes(0, k), p.Tolerance)
				}
				if err != nil {
					return fmt.Errorf("feature %d: %w", ids[i], err)
				}
			}
			return nil
		})
	}
	return shapes, has, g.Wait()
}

// admit waits for room to read about n bytes of geometry.
func (pr *Provider) admit(ctx context.Context, n int64) (func(), error) {
	if pr.GeometryBudget == nil {
		return func() {}, nil
	}
	wctx, cancel := context.WithTimeout(ctx, pr.BudgetWait)
	defer cancel()
	busy := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrBusy
	}
	heavy := pr.HeavySlots != nil && n > pr.HeavyBytes
	if heavy {
		select {
		case pr.HeavySlots <- struct{}{}:
		case <-wctx.Done():
			return nil, busy()
		}
	}
	// A heavy request may take at most three quarters of the budget, so light ones always fit.
	n = min(max(n, 1), pr.BudgetBytes*3/4)
	if err := pr.GeometryBudget.Acquire(wctx, n); err != nil {
		if heavy {
			<-pr.HeavySlots
		}
		return nil, busy()
	}
	return func() {
		pr.GeometryBudget.Release(n)
		if heavy {
			<-pr.HeavySlots
		}
	}, nil
}

// firstPoint is the first point of a polygon or of a multipolygon's first member.
func firstPoint(g orb.Geometry) (orb.Point, bool) {
	if mp, ok := g.(orb.MultiPolygon); ok && len(mp) > 0 {
		g = mp[0]
	}
	if p, ok := g.(orb.Polygon); ok && len(p) > 0 && len(p[0]) > 0 {
		return p[0][0], true
	}
	return orb.Point{}, false
}

// spread is width plus height of a geometry's bounds, to rank features by how much they cover.
// Every ring counts: orb's Polygon.Bound looks at the outer ring only, and a simplified hole can
// poke out of its outer ring.
func spread(g orb.Geometry) float64 {
	b := geom.Bounds(g)
	return (b.Max[0] - b.Min[0]) + (b.Max[1] - b.Min[1])
}

func overview(item *catalog.Item, tolerance float64) string {
	best, href := -1.0, ""
	for _, a := range item.Assets {
		if strings.HasPrefix(a.Key, "overview") && a.Tolerance != nil && *a.Tolerance <= tolerance && *a.Tolerance > best {
			best, href = *a.Tolerance, a.Href
		}
	}
	return href
}

func projectExtent(e *[4]float64, wkid int) jsonx.Obj {
	if !geom.Mercator[wkid] {
		return query.Extent(jsonx.Obj{{K: "wkid", V: 4326}}, e[0], e[1], e[2], e[3])
	}
	xmin, ymin := geom.ExtentToMercator(e[0], e[1])
	xmax, ymax := geom.ExtentToMercator(e[2], e[3])
	return query.Extent(jsonx.Obj{{K: "wkid", V: 102100}, {K: "latestWkid", V: 3857}}, xmin, ymin, xmax, ymax)
}
