package pq

import (
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/parquet-go/parquet-go"
)

// A native GeoArrow geometry column (GeoParquet's point, linestring, polygon, multipoint,
// multilinestring and multipolygon encodings, with separated x and y) is nested lists of a
// struct of x and y. Each of its two coordinate leaves decodes to one value a row, holding the
// row's entries back to back: a byte of repetition level, high bit set when the entry has no
// coordinate, and then the coordinate's 8 bytes. Read joins a row's x and y into WKB, so the
// rest of the server reads either encoding alike.

// wkbCodes are the WKB type codes of the native encodings; a code's nesting is its list depth.
var wkbCodes = map[string]uint32{"point": 1, "linestring": 2, "polygon": 3, "multipoint": 4,
	"multilinestring": 5, "multipolygon": 6}

var depths = map[uint32]int{1: 0, 2: 1, 4: 1, 3: 2, 5: 2, 6: 3}

const noCoord = 0x80

// coordLeaves finds the x and y leaves of a native geometry column called name.
func coordLeaves(pf *parquet.File, name string) (x, y *parquet.Column) {
	c := pf.Root().Column(name)
	for c != nil && !c.Leaf() {
		if x, y = c.Column("x"), c.Column("y"); x != nil && y != nil {
			return x, y
		}
		kids := c.Columns()
		if len(kids) != 1 {
			return nil, nil
		}
		c = kids[0]
	}
	return nil, nil
}

// coordLeaf is how a coordinate leaf decodes: into its row's entries.
func coordLeaf(c *parquet.Column) leaf {
	maxDef := c.MaxDefinitionLevel()
	return leaf{index: c.Index(), kind: KindBytes, flat: c.MaxRepetitionLevel() == 0, coord: true, maxDef: maxDef}
}

// entry appends one coordinate leaf value to the row it belongs to.
func (b *builder) entry(v parquet.Value, maxDef int) {
	if v.RepetitionLevel() == 0 {
		b.endRow()
		b.v.Valid = append(b.v.Valid, true)
		b.open = true
	}
	rep := byte(v.RepetitionLevel())
	if v.DefinitionLevel() < maxDef {
		b.v.Data = append(b.v.Data, rep|noCoord)
		return
	}
	b.v.Data = binary.LittleEndian.AppendUint64(append(b.v.Data, rep), math.Float64bits(v.Double()))
}

// endRow closes the row entry is filling.
func (b *builder) endRow() {
	if b.open {
		b.v.Offs = append(b.v.Offs, uint32(len(b.v.Data)))
		b.open = false
	}
}

// crsCode is the EPSG code of a GeoParquet column's PROJJSON crs, 0 when it gives none.
func crsCode(crs json.RawMessage) int {
	var c struct {
		ID struct {
			Authority string `json:"authority"`
			Code      any    `json:"code"`
		} `json:"id"`
	}
	if json.Unmarshal(crs, &c) != nil || c.ID.Authority != "EPSG" {
		return 0
	}
	switch code := c.ID.Code.(type) {
	case float64:
		return int(code)
	case string:
		n := 0
		for _, r := range code {
			if r < '0' || r > '9' {
				return 0
			}
			n = n*10 + int(r-'0')
		}
		return n
	}
	return 0
}

// nest is one row's coordinates and how they nest: counts[k] holds, for each element at list
// level k (the row is level 0), how many elements of level k+1 it has.
type nest struct {
	counts [3][]uint32
	xs, ys []float64
}

// assemble reads a row's x and y entries into n, for a column depth lists deep. Entries without
// a coordinate (an empty or null list) are left out.
func (n *nest) assemble(xb, yb []byte, depth int) {
	for k := range n.counts {
		n.counts[k] = n.counts[k][:0]
	}
	n.xs, n.ys = n.xs[:0], n.ys[:0]
	if depth > 0 {
		n.counts[0] = append(n.counts[0], 0)
	}
	for i, j := 0, 0; i < len(xb) && j < len(yb); {
		rep, has := int(xb[i]&^noCoord), xb[i]&noCoord == 0
		yhas := yb[j]&noCoord == 0
		i, j = i+1, j+1
		var x, y float64
		if has {
			x = math.Float64frombits(binary.LittleEndian.Uint64(xb[i:]))
			i += 8
		}
		if yhas {
			y = math.Float64frombits(binary.LittleEndian.Uint64(yb[j:]))
			j += 8
		}
		if !has || !yhas {
			continue
		}
		// A new element starts at the repetition level (the row's first at level 1) and every
		// level below it, and at any level that has none open yet (after an empty one was left out).
		from := max(rep, 1)
		for k := 1; k < min(from, depth); k++ {
			if len(n.counts[k]) == 0 {
				from = k
				break
			}
		}
		for k := from; k <= depth; k++ {
			n.counts[k-1][len(n.counts[k-1])-1]++
			if k < depth {
				n.counts[k] = append(n.counts[k], 0)
			}
		}
		n.xs, n.ys = append(n.xs, x), append(n.ys, y)
	}
}

// appendWKB writes n as little-endian WKB of type code, or nothing when it has no coordinate.
func (n *nest) appendWKB(buf []byte, code uint32) []byte {
	if len(n.xs) == 0 {
		return buf
	}
	w := wkbWriter{n: n, buf: buf}
	w.geometry(code, 0)
	return w.buf
}

type wkbWriter struct {
	n   *nest
	buf []byte
	pt  int    // next coordinate
	at  [3]int // next element at each level
}

func (w *wkbWriter) header(code uint32) {
	w.buf = binary.LittleEndian.AppendUint32(append(w.buf, 1), code)
}

func (w *wkbWriter) u32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }

func (w *wkbWriter) point() {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(w.n.xs[w.pt]))
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(w.n.ys[w.pt]))
	w.pt++
}

// children is how many elements of level+1 the next element of level has.
func (w *wkbWriter) children(level int) uint32 {
	c := w.n.counts[level][w.at[level]]
	w.at[level]++
	return c
}

// geometry writes the next element of level as a geometry of type code.
func (w *wkbWriter) geometry(code uint32, level int) {
	w.header(code)
	switch code {
	case 1:
		w.point()
	case 2: // points
		c := w.children(level)
		w.u32(c)
		for range c {
			w.point()
		}
	case 3: // rings of points
		c := w.children(level)
		w.u32(c)
		for range c {
			p := w.children(level + 1)
			w.u32(p)
			for range p {
				w.point()
			}
		}
	case 4:
		c := w.children(level)
		w.u32(c)
		for range c {
			w.header(1)
			w.point()
		}
	case 5, 6:
		c := w.children(level)
		w.u32(c)
		for range c {
			w.geometry(code-3, level+1)
		}
	}
}
