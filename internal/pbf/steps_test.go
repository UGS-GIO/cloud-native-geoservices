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

// A Ready geometry writes as its parts would, on the tile grids of its scale and, through its
// parts, on any other grid.
func TestReadyMatchesDirect(t *testing.T) {
	rnd := rand.New(rand.NewSource(2))
	const r = 20037508.342789244
	for z := 5; z <= 15; z++ {
		res := 2 * r / math.Exp2(float64(z)) / 512
		for k := 0; k < 30; k++ {
			// Points on the level's grid, as the writer snaps them: whole steps times the step.
			i0, j0 := math.Round((-12.4e6+rnd.Float64()*2e5)/res), math.Round((4.4e6+rnd.Float64()*2e5)/res)
			x0, y0 := i0*res, j0*res
			var ring []orb.Point
			for i := 0; i < 50; i++ {
				a := 2 * math.Pi * float64(i) / 50
				d := (1 + rnd.Float64()) * 20
				ring = append(ring, orb.Point{(i0 + math.Round(d*math.Cos(a))) * res, (j0 + math.Round(d*math.Sin(a))) * res})
			}
			ring = append(ring, ring[0])
			path := []orb.Point{{x0, y0}, {(i0 + 7) * res, (j0 - 3) * res}}
			for _, e := range []geom.Esri{{Kind: geom.EsriPolygon, Parts: [][]orb.Point{ring}}, {Kind: geom.EsriPolyline, Parts: [][]orb.Point{path, ring}}} {
				ready, _, _, ok := geom.AppendReady(nil, &e, res, nil, nil)
				if !ok {
					t.Fatal("no part left")
				}
				col, row := math.Floor((x0+r)/(res*512)), math.Floor((r-y0)/(res*512))
				for _, tr := range []*geom.Transform{
					{XScale: res, YScale: res, XTranslate: -r + col*512*res, YTranslate: r - row*512*res},
					{XScale: res * (1 + 3e-12), YScale: res * (1 + 3e-12), XTranslate: -r + (col-1)*512*res, YTranslate: r - (row+2)*512*res},
					{XScale: 2 * res, YScale: 2 * res, XTranslate: -r + col*512*res, YTranslate: r - row*512*res},
				} {
					var a, b scratch
					want, okw := a.geometry(nil, &e, tr)
					got, okg := b.geometry(nil, &geom.Esri{Kind: e.Kind, Ready: ready}, tr)
					if okw != okg || !bytes.Equal(want, got) {
						t.Fatalf("z%d scale %v: Ready writes differently\n%x\n%x", z, tr.XScale, want, got)
					}
				}
			}
		}
	}
}
