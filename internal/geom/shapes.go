package geom

import (
	"math"
	"sync"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/lru"
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

func (s *Shapes) step(file string, tolerance float64) (*step, stepKey) {
	key := stepKey{file, Step(tolerance)}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.steps.Get(key)
	if !ok {
		e = &step{shapes: map[int64]Shape{}, none: map[int64]bool{}}
		s.steps.Put(key, e, 0)
	}
	return e, key
}

// Lookup returns the cached shape of feature id at tolerance's step: found reports a cache hit,
// ok whether the feature has a geometry.
func (s *Shapes) Lookup(file string, id int64, tolerance float64) (sh Shape, ok, found bool) {
	e, _ := s.step(file, tolerance)
	e.mu.RLock()
	defer e.mu.RUnlock()
	if sh, hit := e.shapes[id]; hit {
		return sh, true, true
	}
	return Shape{}, false, e.none[id]
}

// Make simplifies wkb at tolerance's step and caches it. ok is false for no geometry.
func (s *Shapes) Make(file string, id int64, wkb []byte, tolerance float64) (Shape, bool, error) {
	e, key := s.step(file, tolerance)
	sh, ok, err := FromWKB(wkb, key.step)
	if err != nil {
		return sh, ok, err
	}
	e.mu.Lock()
	if ok {
		e.shapes[id] = sh
	} else {
		e.none[id] = true
	}
	e.mu.Unlock()
	n := int64(8)
	if ok {
		n = size(sh.G)
	}
	s.steps.Grow(key, n)
	return sh, ok, nil
}

// size is a rough heap size of a geometry.
func size(g orb.Geometry) int64 {
	switch g := g.(type) {
	case orb.Point:
		return 48
	case orb.MultiPoint:
		return 24 + 16*int64(len(g))
	case orb.LineString:
		return 24 + 16*int64(len(g))
	case orb.Ring:
		return 24 + 16*int64(len(g))
	case orb.MultiLineString:
		return sizes(g)
	case orb.Polygon:
		return sizes(g)
	case orb.MultiPolygon:
		return sizes(g)
	}
	return 16
}

func sizes[S ~[]T, T orb.Geometry](s S) int64 {
	n := int64(24)
	for _, m := range s {
		n += size(m)
	}
	return n
}
