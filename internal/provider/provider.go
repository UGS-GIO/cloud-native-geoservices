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

	"github.com/UGS-GIO/geoparquet-featureserver/internal/catalog"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/esri"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/geom"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/js"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/plan"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/pq"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/query"
	"github.com/UGS-GIO/geoparquet-featureserver/internal/remote"
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
	// A small-scale query reads an overview next: open it now rather than after the data file.
	for _, a := range item.Assets {
		if strings.HasPrefix(a.Key, "overview") && a.Tolerance != nil {
			go pr.Open(context.WithoutCancel(ctx), a.Href)
		}
	}
	var layer *pq.Layer
	var d *catalog.Drawing
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { layer, err = pr.Open(gctx, item.Data()); return })
	g.Go(func() (err error) { d, err = pr.Catalog.Drawing(gctx, item, render); return })
	if err := g.Wait(); err != nil {
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
		MaxRecordCount: int(pr.MaxRecords), TileMaxRecordCount: int(pr.TileMaxRecords), Renderer: style["renderer"], LabelingInfo: style["labelingInfo"],
		MinScale: style["minScale"], MaxScale: style["maxScale"]}
	c := &query.Collection{Meta: meta}
	if !isQuery {
		return c, nil
	}

	maxRecords := pr.MaxRecords
	if params["resultType"] == "tile" {
		maxRecords = pr.TileMaxRecords
	}
	p, err := plan.New(params, plan.Layer{Columns: layer.Columns, Point: layer.Point, BBox: layer.HasBBox}, maxRecords)
	if err != nil {
		return nil, query.Bad(err.Error())
	}
	// Small-scale requests on a heavy layer read its coarsest generalized copy that is still finer
	// than the request (an `overview` asset).
	source := layer
	if ov := overview(item, p.Tolerance); ov != "" && p.Kind == plan.Features && p.Geometry && len(p.IDs) == 0 {
		if source, err = pr.Open(ctx, ov); err != nil {
			return nil, err
		}
		if p, err = plan.New(params, plan.Layer{Columns: source.Columns, Point: source.Point, BBox: source.HasBBox}, maxRecords); err != nil {
			return nil, query.Bad(err.Error())
		}
	}
	if p.Kind != plan.Features {
		n := layer.Rows
		c.Count = &n
		if p.Kind == plan.Extent && layer.Extent != nil {
			c.Extent = query.OutputSR(p.OutSR).Envelope(*layer.Extent)
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
	if p.Stream && source == layer {
		return c, pr.stream(ctx, c, source, p, rows, params)
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
	spatial, _ := params.Spatial() // Run reports a bad one
	feats, err := pr.features(ctx, source, p, readRows, thin, nil, spatial)
	if err != nil {
		return nil, err
	}
	// A drawing tile over the limit keeps its largest features, as the vector tiles drop the
	// smallest: flagging it exceeded makes Map Viewer split it into four and ask again, repeatedly.
	tile := params["resultType"] == "tile" && thin
	order := make([]int, len(feats))
	for i := range order {
		order[i] = i
	}
	if tile && int64(len(feats)) > limit {
		for i := range feats {
			if !feats[i].speck {
				feats[i].size = spread(feats[i].bound)
			}
		}
		// A stable sort by size, as indexes: the features are too large to move around.
		slices.SortFunc(order, func(a, b int) int {
			return cmp.Or(cmp.Compare(feats[b].size, feats[a].size), cmp.Compare(a, b))
		})
	}
	c.Exceeded = !tile && (int64(len(feats)) > limit || thin && int64(len(rows)) > readLimit)
	if int64(len(order)) > limit {
		order = order[:limit]
	}
	c.Features = make([]query.Feature, len(order))
	for i, k := range order {
		c.Features[i] = feats[k].f
	}
	return c, nil
}

// stream gives a count or ids every row the filter selects: the first chunk now, the rest through
// c.Next, each reading at most MaxGeometryRows geometries. A row its bbox settles is dropped or
// kept without reading its geometry.
func (pr *Provider) stream(ctx context.Context, c *query.Collection, source *pq.Layer, p *plan.Plan, rows []int64, params query.Raw) error {
	var known []bool
	spatial, err := params.Spatial()
	if err != nil || spatial == nil || !source.HasBBox {
		known = make([]bool, len(rows))
	} else if rows, known, err = settle(ctx, source, spatial, rows); err != nil {
		return err
	}
	next := func() ([]query.Feature, error) {
		if len(rows) == 0 {
			return nil, nil
		}
		n, read := 0, int64(0)
		for ; n < len(rows) && read < max(pr.MaxGeometryRows, 1); n++ {
			if !known[n] {
				read++
			}
		}
		feats, err := pr.features(ctx, source, p, rows[:n], false, known[:n], spatial)
		rows, known = rows[n:], known[n:]
		out := make([]query.Feature, len(feats))
		for i := range feats {
			out[i] = feats[i].f
		}
		return out, err
	}
	feats, err := next()
	c.Features, c.Next = feats, next
	return err
}

// settle drops the rows whose bbox misses the filter and marks those whose bbox meets it. A row
// in pages whose bounds the filter covers is marked without reading its bbox.
func settle(ctx context.Context, source *pq.Layer, spatial *query.Spatial, rows []int64) ([]int64, []bool, error) {
	covered, err := source.Covered(ctx, rows, func(b [4]float64) bool {
		return spatial.Covers(orb.Bound{Min: orb.Point{b[0], b[1]}, Max: orb.Point{b[2], b[3]}})
	})
	if err != nil {
		return nil, nil, err
	}
	var rest []int64
	for i, r := range rows {
		if !covered[i] {
			rest = append(rest, r)
		}
	}
	bbox, err := source.Read(ctx, pq.BBox, rest)
	if err != nil {
		return nil, nil, err
	}
	keep, known := rows[:0:0], []bool{}
	j := -1
	for i, r := range rows {
		if covered[i] {
			keep, known = append(keep, r), append(known, true)
			continue
		}
		j++
		var b [4]float64
		ok := true
		for k := range b {
			if b[k], ok = bbox.Get(k, j).(float64); !ok {
				break
			}
		}
		if !ok {
			continue // no geometry
		}
		if pass, sure := spatial.Decide(orb.Bound{Min: orb.Point{b[0], b[1]}, Max: orb.Point{b[2], b[3]}}); !sure || pass {
			keep, known = append(keep, r), append(known, sure)
		}
	}
	return keep, known, nil
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
	bound orb.Bound
	size  float64 // spread, when ranking a tile's features
}

// features reads the plan's columns (and geometry) at rows and builds the features, thinning
// specks to one per tolerance-sized cell per symbol class when drawing. A known row is already
// known to meet the geometry filter, and its geometry is not read; a row whose shape's bounds
// meet the filter is marked known, so Run does not work them out again.
func (pr *Provider) features(ctx context.Context, source *pq.Layer, p *plan.Plan, rows []int64, thin bool, known []bool, spatial *query.Spatial) ([]shaped, error) {
	if err := pr.warm(ctx, source, p, rows, known); err != nil {
		return nil, err
	}
	values, err := source.Read(ctx, p.Columns, rows)
	if err != nil {
		return nil, err
	}
	shapes, has, err := pr.shapes(ctx, source, p, values, rows, known)
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
	var key []byte
	for i := range rows {
		if (p.Geometry || p.Present) && !has[i] {
			continue
		}
		var g orb.Geometry
		if p.Geometry {
			g = shapes[i].G
		}
		if thin && shapes[i].Speck {
			if pt, ok := firstPoint(g); ok {
				var class string
				class, key = speckClass(values, p.Columns, i, key)
				k := cell{math.Floor(pt[0] / p.Tolerance), math.Floor(pt[1] / p.Tolerance), class}
				if cells[k] {
					continue
				}
				cells[k] = true
			}
		}
		props := make([]any, len(p.Columns))
		for c := range p.Columns {
			props[c] = values.Get(c, i)
		}
		k := known != nil && known[i]
		if !k && spatial != nil && g != nil {
			pass, sure := spatial.Decide(shapes[i].Bound)
			k = pass && sure
		}
		out = append(out, shaped{f: query.Feature{Props: props, Geom: g, Known: k}, speck: shapes[i].Speck, bound: shapes[i].Bound})
	}
	return out, nil
}

// warm fetches the attributes at rows and the geometry shapes will read, together: shapes needs
// the attributes (feature_id) before it knows which geometry to read. A file whose feature_id is
// the row number + 1 looks its shapes up by row here; elsewhere every unknown row's is fetched.
func (pr *Provider) warm(ctx context.Context, source *pq.Layer, p *plan.Plan, rows []int64, known []bool) error {
	var geomRows []int64
	if p.Geometry {
		var cached *geom.Cached
		if p.Tolerance > 0 && source.IDIsRow && slices.Contains(p.Columns, "feature_id") {
			cached = pr.Shapes.At(source.ID(), p.Tolerance)
		}
		for i, r := range rows {
			if known != nil && known[i] {
				continue
			}
			if cached != nil {
				if _, _, found := cached.Lookup(r + 1); found {
					continue
				}
			}
			geomRows = append(geomRows, r)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return source.Warm(gctx, p.Columns, rows) })
	if len(geomRows) > 0 {
		g.Go(func() error {
			release, err := pr.admit(gctx, source.GeomBytes(geomRows))
			if err != nil {
				return err
			}
			defer release()
			return source.Warm(gctx, []string{source.Geom}, geomRows)
		})
	}
	return g.Wait()
}

// speckClass is row i's symbol class: its attributes but feature_id, which the JavaScript provider
// joined with "|". They compare by AppendKey unless some text holds a "|", then as joined.
func speckClass(values *pq.Rows, cols []string, i int, buf []byte) (string, []byte) {
	buf = append(buf[:0], 0)
	for c, name := range cols {
		if name == "feature_id" {
			continue
		}
		var ok bool
		if buf, ok = values.AppendKey(buf, c, i, '|'); !ok {
			var parts []string
			for c, name := range cols {
				if name != "feature_id" {
					parts = append(parts, js.Join(values.Get(c, i)))
				}
			}
			return "\x01" + strings.Join(parts, "|"), buf
		}
	}
	return string(buf), buf
}

// shapes are the rows' geometries at the plan's tolerance: from the shape cache, reading and
// simplifying WKB (in parallel) only for the rows it does not hold.
func (pr *Provider) shapes(ctx context.Context, source *pq.Layer, p *plan.Plan, values *pq.Rows, rows []int64, known []bool) ([]geom.Shape, []bool, error) {
	shapes := make([]geom.Shape, len(rows))
	has := make([]bool, len(rows))
	if !p.Geometry {
		return shapes, has, nil
	}
	idCol := slices.Index(p.Columns, "feature_id")
	var cached *geom.Cached
	if p.Tolerance > 0 && idCol >= 0 {
		cached = pr.Shapes.At(source.ID(), p.Tolerance)
	}
	ids := make([]int64, len(rows))
	var missing []int
	for i := range rows {
		if known != nil && known[i] {
			has[i] = true
			continue
		}
		if idCol >= 0 {
			ids[i], _ = values.Get(idCol, i).(int64)
		}
		if cached != nil {
			if sh, ok, found := cached.Lookup(ids[i]); found {
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
				if cached != nil {
					shapes[i], has[i], err = cached.Make(ids[i], wkb.Bytes(0, k))
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

// spread is width plus height of a geometry's bounds (geom.Bounds, which counts every ring), to
// rank features by how much they cover.
func spread(b orb.Bound) float64 {
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
