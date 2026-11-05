package geom

import (
	"encoding/binary"
	"math"

	"github.com/paulmach/orb"
	"google.golang.org/protobuf/encoding/protowire"
)

// Ready is a path or polygon already on a quantization grid, as bytes with no pointers in them, so
// a cache can hold many without the garbage collector visiting each: the Esri parts ToEsri gives,
// quantized at one scale from the origin and encoded as the pbf format writes them. A Map Viewer
// tile's grid is that grid shifted by whole steps, so writing it changes only each part's first
// point.
//
// Layout: the Esri kind; the scale (8 bytes); the pbf lengths field, after its size; the part
// count; then per part its first point's zigzag x and y, and the size and bytes of its later
// points' zigzag steps.
type Ready []byte

// AppendReady appends e quantized at scale as a Ready; ok is false when no part is left. lengths
// and coords are scratch, handed back for reuse.
func AppendReady(buf []byte, e *Esri, scale float64, lengths []uint32, coords []int64) (_ []byte, _ []uint32, _ []int64, ok bool) {
	t := Transform{XScale: scale, YScale: scale}
	lengths, coords, ok = t.Quantized(e, lengths[:0], coords[:0])
	if !ok || e.Kind != EsriPolyline && e.Kind != EsriPolygon {
		return buf, lengths, coords, false
	}
	buf = binary.LittleEndian.AppendUint64(append(buf, byte(e.Kind)), math.Float64bits(scale))
	size := 0
	for _, n := range lengths {
		size += protowire.SizeVarint(uint64(n))
	}
	field := protowire.SizeTag(2) + protowire.SizeVarint(uint64(size)) + size
	buf = protowire.AppendVarint(buf, uint64(field))
	buf = protowire.AppendVarint(protowire.AppendTag(buf, 2, protowire.BytesType), uint64(size))
	for _, n := range lengths {
		buf = protowire.AppendVarint(buf, uint64(n))
	}
	buf = protowire.AppendVarint(buf, uint64(len(lengths)))
	at := 0
	for _, n := range lengths {
		buf = protowire.AppendVarint(buf, protowire.EncodeZigZag(coords[at]))
		buf = protowire.AppendVarint(buf, protowire.EncodeZigZag(coords[at+1]))
		size := 0
		for _, c := range coords[at+2 : at+2*int(n)] {
			size += protowire.SizeVarint(protowire.EncodeZigZag(c))
		}
		buf = protowire.AppendVarint(buf, uint64(size))
		for _, c := range coords[at+2 : at+2*int(n)] {
			buf = protowire.AppendVarint(buf, protowire.EncodeZigZag(c))
		}
		at += 2 * int(n)
	}
	return buf, lengths, coords, true
}

// Kind is r's Esri kind.
func (r Ready) Kind() EsriKind { return EsriKind(r[0]) }

// Scale is the quantization scale r is on.
func (r Ready) Scale() float64 { return math.Float64frombits(binary.LittleEndian.Uint64(r[1:])) }

// uvarint reads a varint at r[*at], moving at past it.
func (r Ready) uvarint(at *int) uint64 {
	v, n := protowire.ConsumeVarint(r[*at:])
	*at += n
	return v
}

// AppendPBF writes r's pbf lengths and coords fields on the grid shifted by kx, ky steps: x counts
// from the translate, y down from it.
func (r Ready) AppendPBF(b []byte, kx, ky int64) []byte {
	at := 9
	field := int(r.uvarint(&at))
	b = append(b, r[at:at+field]...)
	at += field
	parts := int(r.uvarint(&at))
	// The coords field's size: the parts' shifted first points and their steps as they are.
	size, from := 0, at
	for range parts {
		x, y := protowire.DecodeZigZag(r.uvarint(&at))-kx, protowire.DecodeZigZag(r.uvarint(&at))+ky
		size += protowire.SizeVarint(protowire.EncodeZigZag(x)) + protowire.SizeVarint(protowire.EncodeZigZag(y))
		n := int(r.uvarint(&at))
		size += n
		at += n
	}
	b = protowire.AppendVarint(protowire.AppendTag(b, 3, protowire.BytesType), uint64(size))
	at = from
	for range parts {
		x, y := protowire.DecodeZigZag(r.uvarint(&at))-kx, protowire.DecodeZigZag(r.uvarint(&at))+ky
		b = protowire.AppendVarint(protowire.AppendVarint(b, protowire.EncodeZigZag(x)), protowire.EncodeZigZag(y))
		n := int(r.uvarint(&at))
		b = append(b, r[at:at+n]...)
		at += n
	}
	return b
}

// Esri is r's parts back as Web Mercator points on its grid, for any output but a tile on it.
func (r Ready) Esri() Esri {
	scale := r.Scale()
	at := 9
	field := int(r.uvarint(&at))
	at += field
	parts := int(r.uvarint(&at))
	e := Esri{Kind: r.Kind(), Parts: make([][]orb.Point, 0, parts)}
	for range parts {
		x, y := protowire.DecodeZigZag(r.uvarint(&at)), protowire.DecodeZigZag(r.uvarint(&at))
		n := int(r.uvarint(&at))
		end := at + n
		part := []orb.Point{{float64(x) * scale, -float64(y) * scale}}
		for at < end {
			x += protowire.DecodeZigZag(r.uvarint(&at))
			y += protowire.DecodeZigZag(r.uvarint(&at))
			part = append(part, orb.Point{float64(x) * scale, -float64(y) * scale})
		}
		e.Parts = append(e.Parts, part)
	}
	return e
}
