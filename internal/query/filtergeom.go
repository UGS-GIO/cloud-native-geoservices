package query

import (
	"strconv"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/geom"
)

// filterGeometry reads the request's `geometry` (Esri JSON, or "xmin,ymin,xmax,ymax" / "x,y")
// into a lon/lat geometry, as winnow did before its intersects test.
func filterGeometry(v any, inSR any) (orb.Geometry, error) {
	var g orb.Geometry
	srid := 0
	switch x := v.(type) {
	case float64:
		return nil, nil
	case string:
		parts := strings.Split(x, ",")
		nums := make([]float64, 0, 4)
		for i, s := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				if i == 4 {
					break
				}
				return nil, bad("Invalid geometry: " + x)
			}
			if i == 4 {
				srid = int(f)
				break
			}
			nums = append(nums, f)
		}
		switch len(nums) {
		case 2:
			g = orb.Point{nums[0], nums[1]}
		case 4:
			g = envelope(nums[0], nums[1], nums[2], nums[3])
		default:
			return nil, bad("Invalid geometry: " + x)
		}
	case map[string]any:
		if sr, ok := x["spatialReference"]; ok {
			srid, _ = wkid(sr)
		}
		var err error
		if g, err = esriToGeometry(x); err != nil {
			return nil, err
		}
	default:
		return nil, bad("Invalid geometry")
	}
	if srid == 0 && inSR != nil {
		srid, _ = wkid(inSR)
	}
	if srid != 0 && !supported(srid) {
		return nil, bad("Unsupported spatial reference: " + strconv.Itoa(srid))
	}
	if geom.Mercator[srid] || srid == 3785 {
		g = geom.Map(g, geom.ToLonLat)
	}
	return g, nil
}

func envelope(xmin, ymin, xmax, ymax float64) orb.Polygon {
	return orb.Polygon{{{xmin, ymin}, {xmax, ymin}, {xmax, ymax}, {xmin, ymax}, {xmin, ymin}}}
}

func num(m map[string]any, k string) (float64, bool) {
	f, ok := m[k].(float64)
	return f, ok
}

func points(v any) ([]orb.Point, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]orb.Point, 0, len(arr))
	for _, p := range arr {
		xy, ok := p.([]any)
		if !ok || len(xy) < 2 {
			return nil, false
		}
		x, ok1 := xy[0].(float64)
		y, ok2 := xy[1].(float64)
		if !ok1 || !ok2 {
			return nil, false
		}
		out = append(out, orb.Point{x, y})
	}
	return out, true
}

func esriToGeometry(m map[string]any) (orb.Geometry, error) {
	if xmin, ok := num(m, "xmin"); ok {
		ymin, _ := num(m, "ymin")
		xmax, _ := num(m, "xmax")
		ymax, _ := num(m, "ymax")
		return envelope(xmin, ymin, xmax, ymax), nil
	}
	if x, ok := num(m, "x"); ok {
		y, _ := num(m, "y")
		return orb.Point{x, y}, nil
	}
	if ps, ok := m["points"]; ok {
		pts, ok := points(ps)
		if !ok {
			return nil, bad("Invalid geometry")
		}
		return orb.MultiPoint(pts), nil
	}
	if paths, ok := m["paths"].([]any); ok {
		var mls orb.MultiLineString
		for _, p := range paths {
			pts, ok := points(p)
			if !ok {
				return nil, bad("Invalid geometry")
			}
			mls = append(mls, pts)
		}
		if len(mls) == 1 {
			return mls[0], nil
		}
		return mls, nil
	}
	if rings, ok := m["rings"].([]any); ok {
		var outers []orb.Polygon
		var holes []orb.Ring
		for _, r := range rings {
			pts, ok := points(r)
			if !ok {
				return nil, bad("Invalid geometry")
			}
			ring := orb.Ring(pts)
			if len(ring) > 0 && ring[0] != ring[len(ring)-1] {
				ring = append(ring, ring[0])
			}
			if esriClockwise(ring) {
				outers = append(outers, orb.Polygon{ring})
			} else {
				holes = append(holes, ring)
			}
		}
	hole:
		for _, h := range holes {
			for i := range outers {
				if len(h) > 0 && planar.RingContains(outers[i][0], h[0]) {
					outers[i] = append(outers[i], h)
					continue hole
				}
			}
			outers = append(outers, orb.Polygon{h})
		}
		if len(outers) == 1 {
			return outers[0], nil
		}
		return orb.MultiPolygon(outers), nil
	}
	return nil, bad("Invalid geometry")
}

func esriClockwise(r orb.Ring) bool {
	total := 0.0
	for i := 0; i+1 < len(r); i++ {
		total += (r[i+1][0] - r[i][0]) * (r[i+1][1] + r[i][1])
	}
	return total >= 0
}
