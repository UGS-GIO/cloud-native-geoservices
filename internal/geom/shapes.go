package geom

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/project"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/lru"
)

// Shapes caches simplified geometries per file and zoom step, by feature id, least recently used
// step out first. Simplifying is most of a warm small-scale request; tiles at one zoom share a
// tolerance, so a feature is simplified once per zoom. Callers never change what they get back.
type Shapes struct {
	mu    sync.Mutex // creating a step
	steps *lru.Cache[stepKey, *step]
}

type stepKey struct {
	file string
	step float64
}

type step struct {
	mu sync.RWMutex
	// Feature ids are row numbers, and a tile's rows are near one another in the file's spatial
	// order, so a step holds its ids in chunks of neighbours, found by index rather than hashed.
	chunks []*chunk
	far    map[int64]*chunk // chunks of ids at or past maxChunked, or negative
	// pending is what shapes' Web Mercator copies and encodings came to since the step was last
	// charged; At charges it, once a request, rather than every shape locking the cache.
	pending atomic.Int64
}

const (
	chunkBits  = 7
	chunkLen   = 1 << chunkBits
	maxChunked = 1 << 26
)

type chunk struct {
	shapes [chunkLen]Shape
	state  [chunkLen]uint8 // 0 not cached, 1 a shape, 2 no geometry
}

const chunkBytes = int64(unsafe.Sizeof(chunk{}))

// NewShapes makes a cache of about budget bytes.
func NewShapes(budget int64) *Shapes { return &Shapes{steps: lru.New[stepKey, *step](budget)} }

// Step is the power of two at or below tolerance, so a zoom's tiles share one entry.
func Step(tolerance float64) float64 { return math.Exp2(math.Floor(math.Log2(tolerance))) }

// Cached is one file's shapes at one zoom step.
type Cached struct {
	s   *Shapes
	key stepKey
	*step
}

// At returns the shapes of file at tolerance's step; a request resolves it once for all its rows.
func (s *Shapes) At(file string, tolerance float64) *Cached {
	key := stepKey{file, Step(tolerance)}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.steps.Get(key)
	if !ok {
		e = &step{}
		s.steps.Put(key, e, 0)
	} else if n := e.pending.Swap(0); n != 0 {
		s.steps.Grow(key, n)
	}
	return &Cached{s, key, e}
}

// slot is id's chunk and index in it, making the chunk when create is set; n is the bytes that
// took. Callers hold the step's lock: a read lock without create.
func (st *step) slot(id int64, create bool) (ch *chunk, i int, n int64) {
	k, i := id>>chunkBits, int(id&(chunkLen-1))
	if id >= 0 && id < maxChunked {
		if k < int64(len(st.chunks)) && st.chunks[k] != nil {
			return st.chunks[k], i, 0
		}
		if !create {
			return nil, i, 0
		}
		if k >= int64(len(st.chunks)) {
			was := cap(st.chunks)
			st.chunks = slices.Grow(st.chunks, int(k)+1-len(st.chunks))[:k+1]
			n += 8 * int64(cap(st.chunks)-was)
		}
		st.chunks[k] = new(chunk)
		return st.chunks[k], i, n + chunkBytes
	}
	if ch := st.far[k]; ch != nil || !create {
		return ch, i, 0
	}
	if st.far == nil {
		st.far = map[int64]*chunk{}
	}
	st.far[k] = new(chunk)
	return st.far[k], i, chunkBytes + farEntryBytes
}

// projectedBytes is an empty Projected, rounded up to Go's size class.
const projectedBytes = (int64(unsafe.Sizeof(Projected{})) + 15) &^ 15

// farEntryBytes is a far map entry's heap, measured: Go maps run between half and seven eighths
// full.
const farEntryBytes = int64(unsafe.Sizeof(int64(0))+unsafe.Sizeof((*chunk)(nil))) * 3 / 2

// Lookup returns the cached shape of feature id: found reports a cache hit, ok whether the
// feature has a geometry.
func (c *Cached) Lookup(id int64) (sh Shape, ok, found bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, i, _ := c.slot(id, false)
	if ch == nil {
		return Shape{}, false, false
	}
	switch ch.state[i] {
	case 1:
		return ch.shapes[i], true, true
	case 2:
		return Shape{}, false, true
	}
	return Shape{}, false, false
}

// Make simplifies wkb at the step and caches it. ok is false for no geometry.
func (c *Cached) Make(id int64, wkb []byte) (Shape, bool, error) {
	sh, ok, err := FromWKB(wkb, c.key.step)
	if err != nil {
		return sh, ok, err
	}
	c.mu.Lock()
	ch, i, n := c.slot(id, true)
	switch {
	case ch.state[i] != 0:
	case ok:
		// Only filled slots get one: the collector scans what it holds, and most chunks are sparse.
		sh.Merc = &Projected{memo: Memo{st: c.step}}
		ch.shapes[i], ch.state[i] = sh, 1
		n += projectedBytes + size(sh.G)
	default:
		ch.state[i] = 2
	}
	c.mu.Unlock()
	c.s.steps.Grow(c.key, n)
	return sh, ok, nil
}

// Projected is a cached shape in Web Mercator as Esri parts, worked out on first use and shared
// by every request at its step, so a warm tile neither copies nor projects the shape again.
// Callers never change what they get back.
type Projected struct {
	once sync.Once
	e    Esri
	ok   bool
	memo Memo
}

// Memo holds one value an encoder derives from a shared geometry, for one key (a quantization
// scale), and replaces it when another key asks: the tiles of one zoom share a scale.
type Memo struct {
	v  atomic.Pointer[memo]
	st *step // charged for what it holds
}

type memo struct {
	key [2]float64
	v   any
	n   int64
}

// Get is the value stored for key.
func (m *Memo) Get(key [2]float64) (any, bool) {
	if e := m.v.Load(); e != nil && e.key == key {
		return e.v, true
	}
	return nil, false
}

// Put stores v, of about n bytes, for key, in place of what it held.
func (m *Memo) Put(key [2]float64, v any, n int64) {
	if old := m.v.Swap(&memo{key, v, n}); old != nil {
		n -= old.n
	}
	if m.st != nil { // a Memo outside the cache charges nothing
		m.st.pending.Add(n)
	}
}

// Esri is g, the shape p belongs to, projected to Web Mercator and converted as ToEsri does.
func (p *Projected) Esri(g orb.Geometry) (*Esri, bool) {
	p.once.Do(func() {
		m := project.Geometry(orb.Clone(g), ToMercator)
		p.e, p.ok = ToEsri(m)
		p.e.Memo = &p.memo
		p.memo.st.pending.Add(size(m))
	})
	return &p.e, p.ok
}

// size is a rough heap size of a geometry: its boxed value and backing arrays.
func size(g orb.Geometry) int64 {
	switch g := g.(type) {
	case orb.Point:
		return 16
	case orb.MultiPoint:
		return 24 + 16*int64(cap(g))
	case orb.LineString:
		return 24 + 16*int64(cap(g))
	case orb.Ring:
		return 24 + 16*int64(cap(g))
	case orb.MultiLineString:
		return sizes(g)
	case orb.Polygon:
		return sizes(g)
	case orb.MultiPolygon:
		return sizes(g)
	}
	return 0
}

func sizes[S ~[]T, T orb.Geometry](s S) int64 {
	n := 24 + 24*int64(cap(s))
	for _, m := range s {
		n += size(m) - 24 // members are slice headers in the array, not boxed
	}
	return n
}
