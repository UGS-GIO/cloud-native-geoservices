package geom

import (
	"math"
	"sync"
	"unsafe"

	"github.com/paulmach/orb"

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
	mu     sync.RWMutex
	shapes map[int64]Shape
	none   map[int64]bool
}

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
	_, dup := c.shapes[id]
	n := noneBytes
	if dup || c.none[id] {
		n = 0
	} else if ok {
		c.shapes[id] = sh
		n = shapeBytes + size(sh.G)
	} else {
		c.none[id] = true
	}
	c.mu.Unlock()
	c.s.steps.Grow(c.key, n)
	return sh, ok, nil
}

// Heap per map entry, measured: Go maps run between half and seven eighths full.
const (
	shapeBytes = int64(unsafe.Sizeof(int64(0))+unsafe.Sizeof(Shape{})) * 3 / 2
	noneBytes  = int64(16)
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
