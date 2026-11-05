package pbf

import (
	"bytes"
	"math"
	"math/rand"
	"testing"

	"github.com/paulmach/orb"

	"github.com/UGS-GIO/cloud-native-geoservices/internal/geom"
)

// A geometry encoded from its cached steps matches encoding it directly, on every tile grid of a
// zoom (whole steps from the origin), and a grid off whole steps takes the direct path.
func TestStepsMatchDirect(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	const r = 20037508.342789244
	for z := 5; z <= 16; z++ {
		res := 2 * r / math.Exp2(float64(z)) / 512
		for k := 0; k < 30; k++ {
			x0 := -12.4e6 + rnd.Float64()*2e5
			y0 := 4.4e6 + rnd.Float64()*2e5
			var ring []orb.Point
			for i := 0; i < 200; i++ {
				a := 2 * math.Pi * float64(i) / 200
				d := (1 + rnd.Float64()) * res * 40
				ring = append(ring, orb.Point{x0 + d*math.Cos(a), y0 + d*math.Sin(a)})
			}
			ring = append(ring, ring[0])
			path := []orb.Point{{x0, y0}, {x0 + res/3, y0}, {x0 + 7*res, y0 - 3*res}}
			for _, e := range []geom.Esri{{Kind: geom.EsriPolygon, Parts: [][]orb.Point{ring}}, {Kind: geom.EsriPolyline, Parts: [][]orb.Point{path, ring}}} {
				shared := e
				shared.Memo = new(geom.Memo)
				col, row := math.Floor((x0+r)/(res*512)), math.Floor((r-y0)/(res*512))
				for _, d := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {-1, 2}} {
					tr := &geom.Transform{XScale: res, YScale: res, XTranslate: -r + (col+d[0])*512*res, YTranslate: r - (row+d[1])*512*res}
					var a, b scratch
					want, okw := a.geometry(nil, &e, tr)
					got, okg := b.geometry(nil, &shared, tr)
					if okw != okg || !bytes.Equal(want, got) {
						t.Fatalf("z%d tile %v: cached steps differ from direct encoding", z, d)
					}
				}
				off := &geom.Transform{XScale: res, YScale: res, XTranslate: -r + col*512*res + res/3, YTranslate: r - row*512*res}
				if _, _, ok := aligned(off); ok {
					t.Fatalf("z%d: a translate a third of a step off the grid counts as aligned", z)
				}
			}
		}
	}
}
