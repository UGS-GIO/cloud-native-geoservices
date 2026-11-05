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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/orb"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/catalog"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/esri"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/js"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/plan"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/pq"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/query"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/remote"
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
	// PrecisionMM rounds JSON coordinates to about this many millimeters when the request does not
	// set geometryPrecision; 0 keeps full precision.
	PrecisionMM float64
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

// Version names everything a render's responses depend on: its catalog entry, data file,
// overviews and Esri renderer.
func (pr *Provider) Version(ctx context.Context, topicID, render string) (string, error) {
	item, err := pr.Catalog.Topic(ctx, topicID)
	if err != nil {
		return "", err
	}
	var layer *pq.Layer
	var d *catalog.Drawing
	var hrefs []string
	for _, a := range item.Assets {
		if strings.HasPrefix(a.Key, "overview") && a.Tolerance != nil || lodOf(a) >= 0 {
			hrefs = append(hrefs, a.Href)
		}
	}
	overviews := make([]string, len(hrefs))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { layer, err = pr.Open(gctx, item.Data()); return })
	g.Go(func() (err error) { d, err = pr.Catalog.Drawing(gctx, item, render); return })
	for i, href := range hrefs {
		// An overview that fails to open fails only the queries that read it.
		g.Go(func() error {
			if l, err := pr.Open(gctx, href); err == nil {
				overviews[i] = l.ID()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}
	return strings.Join(append([]string{item.Version, layer.ID(), d.Version}, overviews...), "|"), nil
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
		MinScale: style["minScale"], MaxScale: style["maxScale"], Levels: levels(item)}
	c := &query.Collection{Meta: meta, PrecisionMM: pr.PrecisionMM}
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
	// A Map Viewer tile reads the topic's tile-ready level for its zoom where it has one. Other
	// small-scale requests on a heavy layer read its coarsest generalized copy that is still finer
	// than the request (an `overview` asset).
	source := layer
	if lod, lp := pr.tileLevel(ctx, item, p, params, maxRecords); lod != nil {
		source, p = lod, lp
	} else if ov := overview(item, max(p.Tolerance, p.Pixel)); ov != "" && p.Kind == plan.Features && p.Geometry && len(p.IDs) == 0 {
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
	// Map Viewer gives a tile's drawing scale only as its quantization tolerance. A plain query
	// at that tolerance is not thinned: it pages through every feature.
	var draw float64
	switch {
	case p.Page == nil:
	case params["resultType"] == "tile":
		draw = max(p.Tolerance, p.Pixel)
	default:
		draw = p.Tolerance
	}
	thin := draw > 0
	// An overview's rows are small, so a drawing tile reads all of them rather than cutting the
	// Hilbert-ordered rows off partway across the tile.
	readLimit := unlimited
	if source == layer {
		readLimit = pr.MaxGeometryRows
	}
	sel := pq.Selection{IDs: p.IDs, Box: p.Box, Page: p.Page}
	if geom.Mercator[source.EPSG] && p.Box != nil {
		sel.Box = mercatorBox(p.Box)
	}
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
	feats, values, err := pr.features(ctx, source, p, readRows, draw, nil, spatial)
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
		c.Features[i] = props(values, p.Columns, &feats[k])
	}
	releaseShaped(feats)
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
		feats, values, err := pr.features(ctx, source, p, rows[:n], 0, known[:n], spatial)
		rows, known = rows[n:], known[n:]
		out := make([]query.Feature, len(feats))
		for i := range feats {
			out[i] = props(values, p.Columns, &feats[i])
		}
		releaseShaped(feats)
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
	row   int     // index in the rows read, for its attributes
}

// scratch is what building one request's features needs only while it runs, pooled: a statewide
// tile reads tens of thousands of rows, and these otherwise grow from nothing every time.
type scratch struct {
	shapes   []geom.Shape
	has      []bool
	ids      []int64
	missing  []int // indexes of the rows whose shapes fill reads
	geomRows []int64
	classes  map[string]uint64
	packed   map[uint64]struct{} // specks seen, by packed cell and class number
	cells    map[cell]bool       // specks seen whose cell or class does not pack
}

var scratches = sync.Pool{New: func() any { return new(scratch) }}

// maxPooledRows is the most rows a pooled buffer keeps; a rare huge read's go to the collector.
const maxPooledRows = 1 << 17

// release pools s. A pooled buffer is all zeros: a request writes only within the length it
// asked for, so clearing that much restores it (and drops the geometries, which belong to the
// shape cache).
func (s *scratch) release() {
	if max(cap(s.shapes), cap(s.has), cap(s.ids), cap(s.missing), cap(s.geomRows)) > maxPooledRows {
		return
	}
	clear(s.shapes)
	clear(s.has)
	clear(s.ids)
	s.missing = s.missing[:0]
	// Clearing a big map costs its every bucket on each small request after.
	if len(s.classes)+len(s.packed)+len(s.cells) > 4096 {
		s.classes, s.packed, s.cells = nil, nil, nil
	} else {
		clear(s.classes)
		clear(s.packed)
		clear(s.cells)
	}
	scratches.Put(s)
}

var shapedPool = sync.Pool{New: func() any { return []shaped(nil) }}

// releaseShaped pools feats once its features are copied out.
func releaseShaped(feats []shaped) {
	if cap(feats) > maxPooledRows {
		return
	}
	clear(feats)
	shapedPool.Put(feats[:0]) //nolint:staticcheck // a slice header is small next to what it saves
}

// features reads the plan's columns (and geometry) at rows and builds the features, thinning
// specks to one per thin-sized cell per symbol class when drawing (thin > 0). A speck simplified
// away, or fits in a cell. A known row is already known to meet the geometry filter, and its
// geometry is not read; a row whose shape's bounds meet the filter is marked known, so Run does
// not work them out again.
func (pr *Provider) features(ctx context.Context, source *pq.Layer, p *plan.Plan, rows []int64, thin float64, known []bool, spatial *query.Spatial) ([]shaped, *pq.Rows, error) {
	sc := scratches.Get().(*scratch)
	defer sc.release()
	// Pooled buffers are all zeros past their length, so these start cleared.
	n := len(rows)
	sc.shapes, sc.has = slices.Grow(sc.shapes, n)[:n], slices.Grow(sc.has, n)[:n]
	idCol := slices.Index(p.Columns, "feature_id")
	// A tile-ready level is already drawn for its zoom, in Web Mercator: its shapes are neither
	// simplified nor cached, and it is thinned in metres.
	merc := geom.Mercator[source.EPSG]
	if merc {
		thin *= metresPerDegree
	}
	var cached *geom.Cached
	if p.Geometry && p.Tolerance > 0 && idCol >= 0 && !merc {
		cached = pr.Shapes.At(source.ID(), p.Tolerance)
	}
	// Where feature_id is the row number + 1, the cached shapes are found before anything is
	// read, so only the others' geometry is fetched; elsewhere the ids come with the attributes.
	byRow := cached != nil && source.IDIsRow
	if byRow {
		sc.find(cached, rows, known, nil, idCol)
	}
	if err := pr.warm(ctx, source, p, rows, known, sc, byRow); err != nil {
		return nil, nil, err
	}
	values, err := source.Read(ctx, p.Columns, rows)
	if err != nil {
		return nil, nil, err
	}
	if p.Geometry && !byRow {
		sc.find(cached, rows, known, values, idCol)
	}
	if err := pr.fill(ctx, source, p, cached, rows, sc, merc); err != nil {
		return nil, nil, err
	}
	shapes, has := sc.shapes, sc.has
	if p.Present {
		bbox, err := source.Read(ctx, pq.BBox[:1], rows)
		if err != nil {
			return nil, nil, err
		}
		for i := range rows {
			has[i] = bbox.Get(0, i) != nil
		}
	}
	out := slices.Grow(shapedPool.Get().([]shaped), len(rows))
	var key []byte
	for i := range rows {
		if (p.Geometry || p.Present) && !has[i] {
			continue
		}
		var g orb.Geometry
		if p.Geometry {
			g = shapes[i].G
		}
		b := shapes[i].Bound
		speck := shapes[i].Speck || thin > 0 && g != nil && b.Max[0]-b.Min[0] <= thin && b.Max[1]-b.Min[1] <= thin
		if thin > 0 && speck {
			if pt, ok := firstPoint(g); ok {
				key = speckKey(values, p.Columns, i, key)
				if sc.seen(math.Floor(pt[0]/thin), math.Floor(pt[1]/thin), key) {
					continue
				}
			}
		}
		// A tile-ready level's rows are those whose bbox meets the tile, kept as meeting it: its
		// shapes are not in lon/lat for the exact test, and drawing the few that miss costs nothing.
		k := known != nil && known[i] || merc
		if !k && spatial != nil && g != nil {
			pass, sure := spatial.Decide(shapes[i].Bound)
			k = pass && sure
		}
		f := query.Feature{Geom: g, Known: k}
		f.Merc = shapes[i].Merc
		out = append(out, shaped{f: f, speck: speck, bound: b, row: i})
	}
	return out, values, nil
}

// props gives f its attributes, from values at its row: only for the features a response keeps,
// as a dense tile keeps its largest few thousand of many more.
func props(values *pq.Rows, cols []string, f *shaped) query.Feature {
	f.f.Props = make([]any, len(cols))
	for c := range cols {
		f.f.Props[c] = values.Get(c, f.row)
	}
	return f.f
}

// warm fetches the attributes at rows and, together with them, the geometry fill will read: the
// rows find left missing, or every unknown row when the ids come with the attributes.
func (pr *Provider) warm(ctx context.Context, source *pq.Layer, p *plan.Plan, rows []int64, known []bool, sc *scratch, byRow bool) error {
	geomRows := sc.geomRows[:0]
	if p.Geometry {
		if byRow {
			for _, i := range sc.missing {
				geomRows = append(geomRows, rows[i])
			}
		} else {
			for i, r := range rows {
				if known == nil || !known[i] {
					geomRows = append(geomRows, r)
				}
			}
		}
	}
	sc.geomRows = geomRows
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

// speckKey is row i's symbol class: its attributes but feature_id, which the JavaScript provider
// joined with "|". A key starting with 0 holds them by AppendKey; one starting with 1, when some
// text holds a "|", holds them joined.
func speckKey(values *pq.Rows, cols []string, i int, buf []byte) []byte {
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
			return append(append(buf[:0], 1), strings.Join(parts, "|")...)
		}
	}
	return buf
}

// seen reports a speck already kept in cell x, y for symbol class key, and marks this one.
// Classes are numbered per request, and a cell in range packs with its class number into one
// word, which hashes far faster than a key holding floats and a string.
func (s *scratch) seen(x, y float64, key []byte) bool {
	if s.classes == nil {
		s.classes, s.packed = map[string]uint64{}, map[uint64]struct{}{}
	}
	id, ok := s.classes[string(key)]
	if !ok {
		id = uint64(len(s.classes))
		s.classes[string(key)] = id
	}
	const half = 1 << 23
	if id < 1<<16 && x >= -half && x < half && y >= -half && y < half {
		k := uint64(int64(x)+half)<<40 | uint64(int64(y)+half)<<16 | id
		if _, dup := s.packed[k]; dup {
			return true
		}
		s.packed[k] = struct{}{}
		return false
	}
	if s.cells == nil {
		s.cells = map[cell]bool{}
	}
	k := cell{x, y, string(key)}
	if s.cells[k] {
		return true
	}
	s.cells[k] = true
	return false
}

// find takes the rows' shapes the cache holds, and lists the rest in s.missing with their ids:
// by row number when values is nil, else from the feature_id column. A known row has a geometry
// that is not read.
func (s *scratch) find(cached *geom.Cached, rows []int64, known []bool, values *pq.Rows, idCol int) {
	s.ids = slices.Grow(s.ids, len(rows))[:len(rows)]
	missing := s.missing[:0]
	for i, r := range rows {
		if known != nil && known[i] {
			s.has[i] = true
			continue
		}
		switch {
		case values == nil:
			s.ids[i] = r + 1
		case idCol >= 0:
			s.ids[i], _ = values.Get(idCol, i).(int64)
		}
		if cached != nil {
			if sh, ok, found := cached.Lookup(s.ids[i]); found {
				s.shapes[i], s.has[i] = sh, ok
				continue
			}
		}
		missing = append(missing, i)
	}
	s.missing = missing
}

// fill reads and simplifies the geometry of the rows find left missing (in parallel), caching
// the shapes when there is a cache.
func (pr *Provider) fill(ctx context.Context, source *pq.Layer, p *plan.Plan, cached *geom.Cached, rows []int64, sc *scratch, merc bool) error {
	missing := sc.missing
	if len(missing) == 0 {
		return nil
	}
	missingRows := make([]int64, len(missing))
	for k, i := range missing {
		missingRows[k] = rows[i]
	}
	release, err := pr.admit(ctx, source.GeomBytes(missingRows))
	if err != nil {
		return err
	}
	defer release()
	wkb, err := source.Read(ctx, []string{source.Geom}, missingRows)
	if err != nil {
		return err
	}
	shapes, has, ids := sc.shapes, sc.has, sc.ids
	g, _ := errgroup.WithContext(ctx)
	workers := runtime.GOMAXPROCS(0)
	chunk := max(1, (len(missing)+workers-1)/workers)
	for from := 0; from < len(missing); from += chunk {
		to := min(len(missing), from+chunk)
		g.Go(func() error {
			for k := from; k < to; k++ {
				i := missing[k]
				var err error
				switch {
				case cached != nil:
					shapes[i], has[i], err = cached.Make(ids[i], wkb.Bytes(0, k))
				case merc:
					if b := wkb.Bytes(0, k); len(b) > 0 {
						var g orb.Geometry
						if g, err = geom.ReadWKB(b); err == nil {
							shapes[i], has[i] = geom.Shape{G: g, Bound: geom.Bounds(g), Merc: geom.InMercator(g)}, true
						}
					}
				default:
					shapes[i], has[i], err = geom.FromWKB(wkb.Bytes(0, k), p.Tolerance)
				}
				if err != nil {
					return fmt.Errorf("feature %d: %w", ids[i], err)
				}
			}
			return nil
		})
	}
	return g.Wait()
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

// levels are the zoom levels of the item's overviews, as AGOL's optimized drawing lists them: an
// overview's tolerance is about one pixel (1.40625 / 2^z degrees) at its level.
func levels(item *catalog.Item) []int {
	var zs []int
	for _, a := range item.Assets {
		if strings.HasPrefix(a.Key, "overview") && a.Tolerance != nil && *a.Tolerance > 0 {
			zs = append(zs, min(max(int(math.Round(math.Log2(1.40625 / *a.Tolerance))), 1), 17))
		}
	}
	slices.Sort(zs)
	return slices.Compact(zs)
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

// metresPerDegree converts the plan's degrees back to the Web Mercator metres it read them from.
const metresPerDegree = 111320

// lod0Resolution is the metres a pixel covers at web map zoom 0.
const lod0Resolution = 156543.03392804097

// lodOf is the zoom of a `tiles_lodNN` asset, -1 for any other.
func lodOf(a catalog.Asset) int {
	s, ok := strings.CutPrefix(a.Key, "tiles_lod")
	if !ok {
		return -1
	}
	if z, err := strconv.Atoi(s); err == nil && z >= 0 {
		return z
	}
	return -1
}

// tileLevel opens the tile-ready level a Map Viewer tile reads, and plans the request against it:
// the coarsest level at least as fine as the tile's pixel. It gives nil, so the tile reads as any
// other request, where the topic has no such level, the level lacks a field the request needs,
// or the response is not plain Web Mercator.
func (pr *Provider) tileLevel(ctx context.Context, item *catalog.Item, p *plan.Plan, params query.Raw, maxRecords int64) (*pq.Layer, *plan.Plan) {
	if params["resultType"] != "tile" || p.Kind != plan.Features || !p.Geometry || len(p.IDs) > 0 || p.Pixel <= 0 ||
		!geom.Mercator[p.OutSR] || params["geometryPrecision"] != "" || params["f"] != "pbf" && params["f"] != "json" {
		return nil, nil
	}
	want := math.Log2(lod0Resolution/(max(p.Tolerance, p.Pixel)*metresPerDegree)) - 1e-6
	href, best := "", math.MaxInt
	for _, a := range item.Assets {
		if z := lodOf(a); z >= 0 && float64(z) >= want && z < best {
			href, best = a.Href, z
		}
	}
	if href == "" {
		return nil, nil
	}
	lod, err := pr.Open(ctx, href)
	if err != nil || !geom.Mercator[lod.EPSG] || slices.ContainsFunc(p.Columns, func(c string) bool { return !lod.Has(c) }) {
		return nil, nil
	}
	lp, err := plan.New(params, plan.Layer{Columns: lod.Columns, Point: lod.Point, BBox: lod.HasBBox}, maxRecords)
	if err != nil {
		return nil, nil
	}
	return lod, lp
}

// mercatorBox is a lon/lat box in Web Mercator.
func mercatorBox(b *[4]float64) *[4]float64 {
	var m [4]float64
	m[0], m[1] = geom.ExtentToMercator(b[0], b[1])
	m[2], m[3] = geom.ExtentToMercator(b[2], b[3])
	return &m
}
