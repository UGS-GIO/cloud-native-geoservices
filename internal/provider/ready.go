package provider

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"

	"github.com/paulmach/orb"
	"golang.org/x/sync/errgroup"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/lru"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/plan"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/pq"
	"github.com/UGS-GIO/cloud-native-geoservices/internal/query"
)

// A tile-ready level's rows are kept as geom.Ready bytes in chunks of neighbouring rows, read and
// quantized at the level's own pixel once, and written straight into each tile that meets them.
// A chunk holds no pointers but its byte array, so the collector has next to nothing to visit.

const readyRows = 64

type readyKey struct {
	file  string
	chunk int64
}

type readyChunk struct {
	data   []byte
	offs   [readyRows + 1]uint32 // row j is data[offs[j]:offs[j+1]], empty for no geometry
	bounds [readyRows]orb.Bound
}

// readyBytes is a chunk's heap: its arrays and its data.
func (c *readyChunk) bytes() int64 {
	return int64(cap(c.data)) + 4*(readyRows+1) + 32*readyRows + 64
}

// ReadyCache holds tile-ready chunks across requests.
type ReadyCache = lru.Cache[readyKey, *readyChunk]

// NewReadyCache makes a cache of about budget bytes.
func NewReadyCache(budget int64) *ReadyCache { return lru.New[readyKey, *readyChunk](budget) }

// ready returns the chunks holding rows (sorted), reading and building those not cached.
func (pr *Provider) ready(ctx context.Context, source *pq.Layer, scale float64, rows []int64) (map[int64]*readyChunk, error) {
	out := map[int64]*readyChunk{}
	var missing []int64
	for _, r := range rows {
		k := r / readyRows
		if _, seen := out[k]; seen || len(missing) > 0 && missing[len(missing)-1] == k {
			continue
		}
		if c, ok := pr.Ready.Get(readyKey{source.ID(), k}); ok {
			out[k] = c
		} else {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	var read []int64
	for _, k := range missing {
		for r := k * readyRows; r < min((k+1)*readyRows, source.Rows); r++ {
			read = append(read, r)
		}
	}
	release, err := pr.admit(ctx, source.GeomBytes(read))
	if err != nil {
		return nil, err
	}
	defer release()
	wkb, err := source.Read(ctx, []string{source.Geom}, read)
	if err != nil {
		return nil, err
	}
	built := make([]*readyChunk, len(missing))
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	at := 0
	for i, k := range missing {
		n := int(min((k+1)*readyRows, source.Rows) - k*readyRows)
		from := at
		at += n
		g.Go(func() (err error) {
			built[i], err = buildReady(wkb, from, n, scale)
			if err != nil {
				err = fmt.Errorf("row %d: %w", k*readyRows, err)
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for i, k := range missing {
		pr.Ready.Put(readyKey{source.ID(), k}, built[i], built[i].bytes())
		out[k] = built[i]
	}
	return out, nil
}

// buildReady quantizes the n geometries at wkb's cells from on.
func buildReady(wkb *pq.Rows, from, n int, scale float64) (*readyChunk, error) {
	c := &readyChunk{}
	var lengths []uint32
	var coords []int64
	var data []byte
	for j := range n {
		c.offs[j] = uint32(len(data))
		b := wkb.Bytes(0, from+j)
		if len(b) == 0 {
			continue
		}
		g, err := geom.ReadWKB(b)
		if err != nil {
			return nil, err
		}
		e, ok := geom.ToEsri(g)
		if !ok {
			continue
		}
		mark := len(data)
		if data, lengths, coords, ok = geom.AppendReady(data, &e, scale, lengths, coords); ok {
			c.bounds[j] = geom.Bounds(g)
		} else {
			data = data[:mark]
		}
	}
	for j := n; j <= readyRows; j++ {
		c.offs[j] = uint32(len(data))
	}
	c.data = slices.Clip(data)
	return c, nil
}

// readyFeatures builds the features of a tile-ready level at rows, thinning specks to one per
// thin-sized cell (degrees) per symbol class.
func (pr *Provider) readyFeatures(ctx context.Context, source *pq.Layer, scale float64, p *plan.Plan, rows []int64, thin float64) ([]shaped, *pq.Rows, error) {
	g, gctx := errgroup.WithContext(ctx)
	var values *pq.Rows
	var chunks map[int64]*readyChunk
	g.Go(func() (err error) { values, err = source.Read(gctx, p.Columns, rows); return })
	g.Go(func() (err error) { chunks, err = pr.ready(gctx, source, scale, rows); return })
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	sc := scratches.Get().(*scratch)
	defer sc.release()
	thin *= metresPerDegree
	out := slices.Grow(shapedPool.Get().([]shaped), len(rows))
	var key []byte
	var c *readyChunk
	k := int64(-1)
	for i, r := range rows {
		if r/readyRows != k {
			k = r / readyRows
			c = chunks[k]
		}
		j := r % readyRows
		if c.offs[j] == c.offs[j+1] {
			continue
		}
		b := c.bounds[j]
		speck := thin > 0 && b.Max[0]-b.Min[0] <= thin && b.Max[1]-b.Min[1] <= thin
		if speck {
			key = speckKey(values, p.Columns, i, key)
			if sc.seen(math.Floor(b.Min[0]/thin), math.Floor(b.Min[1]/thin), key) {
				continue
			}
		}
		// Rows are those whose covering meets the tile: drawing the few that only nearly do
		// costs nothing, and their shapes are not in lon/lat for the exact test.
		f := query.Feature{Geom: b, Known: true, Ready: geom.Ready(c.data[c.offs[j]:c.offs[j+1]:c.offs[j+1]])}
		out = append(out, shaped{f: f, speck: speck, bound: b, row: i})
	}
	return out, values, nil
}

// largest keeps, in order, the indexes of the limit largest features, the earliest of those tied
// at the smallest size kept.
func largest(feats []shaped, order []int, limit int) []int {
	sizes := make([]float64, len(feats))
	for i := range feats {
		sizes[i] = feats[i].size
	}
	slices.Sort(sizes)
	least := sizes[len(sizes)-limit]
	ties := limit
	for i := range feats {
		if feats[i].size > least {
			ties--
		}
	}
	kept := order[:0]
	for _, i := range order {
		if s := feats[i].size; s > least || s == least && ties > 0 {
			if s == least {
				ties--
			}
			kept = append(kept, i)
		}
	}
	return kept
}
