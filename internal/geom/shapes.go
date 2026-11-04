package geom

import (
	"math"
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
	// order, so a step holds its ids in chunks of neighbours, looked up by index rather than
	// hashed; ids past the chunks go in the maps.
	chunks []*chunk
	shapes map[int64]Shape
	none   map[int64]bool
}

const (
	chunkBits  = 7
	chunkLen   = 1 << chunkBits
	maxChunked = 1 << 26 // larger ids go in the maps
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
		e = &step{shapes: map[int64]Shape{}, none: map[int64]bool{}}
		s.steps.Put(key, e, 0)
	}
	return &Cached{s, key, e}
}

// Lookup returns the cached shape of feature id: found reports a cache hit, ok whether the
// feature has a geometry.
func (c *Cached) Lookup(id int64) (sh Shape, ok, found bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if id >= 0 && id < maxChunked {
		k := id >> chunkBits
		if k >= int64(len(c.chunks)) || c.chunks[k] == nil {
			return Shape{}, false, false
		}
		ch, i := c.chunks[k], id&(chunkLen-1)
		switch ch.state[i] {
		case 1:
			return ch.shapes[i], true, true
		case 2:
			return Shape{}, false, true
		}
		return Shape{}, false, false
	}
	if sh, hit := c.shapes[id]; hit {
		return sh, true, true
	}
	return Shape{}, false, c.none[id]
}

// Make simplifies wkb at the step and caches it. ok is false for no geometry.
func (c *Cached) Make(id int64, wkb []byte) (Shape, bool, error) {
	sh, ok, err := FromWKB(wkb, c.key.step)
	if err != nil {
		return sh, ok, err
	}
	c.mu.Lock()
	var n int64
	if id >= 0 && id < maxChunked {
		k := int(id >> chunkBits)
		if k >= len(c.chunks) {
			n += 8 * int64(k+1-len(c.chunks))
			c.chunks = append(c.chunks, make([]*chunk, k+1-len(c.chunks))...)
		}
		if c.chunks[k] == nil {
			c.chunks[k] = new(chunk)
			n += chunkBytes
		}
		ch, i := c.chunks[k], id&(chunkLen-1)
		switch {
		case ch.state[i] != 0:
		case ok:
			sh.Merc = &Projected{s: c.s, key: c.key}
			ch.shapes[i], ch.state[i] = sh, 1
			n += projectedBytes + size(sh.G)
		default:
			ch.state[i] = 2
		}
	} else {
		_, dup := c.shapes[id]
		switch {
		case dup || c.none[id]:
		case ok:
			sh.Merc = &Projected{s: c.s, key: c.key}
			c.shapes[id] = sh
			n = shapeBytes + projectedBytes + size(sh.G)
		default:
			c.none[id] = true
			n = noneBytes
		}
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
	s    *Shapes // the copy is charged to the entry that holds the shape
	key  stepKey
	memo Memo
}

// Memo holds one value an encoder derives from a shared geometry, for one key (a quantization
// scale), and replaces it when another key asks: the tiles of one zoom share a scale.
type Memo struct {
	v atomic.Pointer[memo]
	p *Projected
}

type memo struct {
	key [2]float64
	v   any
}

// Get is the value stored for key.
func (m *Memo) Get(key [2]float64) (any, bool) {
	if e := m.v.Load(); e != nil && e.key == key {
		return e.v, true
	}
	return nil, false
}

// Put stores v, of about n bytes, for key, charging it to the shape's cache entry.
func (m *Memo) Put(key [2]float64, v any, n int64) {
	m.v.Store(&memo{key, v})
	if m.p != nil && m.p.s != nil {
		m.p.s.steps.Grow(m.p.key, n)
	}
}

// Esri is g, the shape p belongs to, projected to Web Mercator and converted as ToEsri does.
func (p *Projected) Esri(g orb.Geometry) (*Esri, bool) {
	p.once.Do(func() {
		m := project.Geometry(orb.Clone(g), ToMercator)
		p.e, p.ok = ToEsri(m)
		p.memo.p = p
		p.e.Memo = &p.memo
		if p.s != nil {
			p.s.steps.Grow(p.key, size(m))
		}
	})
	return &p.e, p.ok
}

// Heap per map entry, measured: Go maps run between half and seven eighths full.
const (
	shapeBytes = int64(unsafe.Sizeof(int64(0))+unsafe.Sizeof(Shape{})) * 3 / 2
	noneBytes  = int64(16)
	// projectedBytes is a shape's empty Projected, rounded up to Go's size class.
	projectedBytes = (int64(unsafe.Sizeof(Projected{})) + 15) &^ 15
)

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
