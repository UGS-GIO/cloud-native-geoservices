package geom

import (
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/paulmach/orb"
)

// polygons are n WKB polygons of 8 to 40 points, each in its own unit cell.
func polygons(t testing.TB, n int) [][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	out := make([][]byte, n)
	for i := range out {
		k := 8 + rng.IntN(33)
		ring := make(orb.Ring, 0, k+1)
		for j := range k {
			a, r := 2*math.Pi*float64(j)/float64(k), 0.3+0.2*rng.Float64()
			ring = append(ring, orb.Point{float64(i) + r*math.Cos(a), r * math.Sin(a)})
		}
		out[i] = mustWKB(t, orb.Polygon{append(ring, ring[0])})
	}
	return out
}

// The size estimate tracks the heap the cache really holds, and counts a feature once.
func TestShapesAccounting(t *testing.T) {
	wkbs := polygons(t, 40000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s := NewShapes(1 << 30)
	c := s.At("f", 0.01)
	for i, b := range wkbs {
		if i%5 == 0 {
			c.Make(int64(i), nil)
		} else {
			c.Make(int64(i), b)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	got, want := float64(after.HeapAlloc-before.HeapAlloc), float64(s.steps.Bytes())
	if got < 0.8*want || got > 1.25*want {
		t.Errorf("heap grew %.0f bytes, estimate %.0f", got, want)
	}
	c.Make(1, wkbs[1])
	c.Make(0, nil)
	if s.steps.Bytes() != int64(want) {
		t.Errorf("making a cached feature again grew the estimate by %.0f", float64(s.steps.Bytes())-want)
	}
	if sh, ok, found := c.Lookup(1); !found || !ok || sh.G == nil {
		t.Error("feature 1 should be cached")
	}
	runtime.KeepAlive(wkbs)
}
