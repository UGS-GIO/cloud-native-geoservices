package server

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// Responses of at least this many bytes are compressed for clients that accept it.
const compressMin = 1024

// A coding is an HTTP content coding. Each encodes the same bytes to the same output, so a
// compressed response keeps one ETag.
type coding struct {
	name   string
	encode func([]byte) []byte
	decode func([]byte) ([]byte, error)
}

type resetWriter interface {
	io.WriteCloser
	Reset(io.Writer)
}

// pooled encodes with writers from a pool.
func pooled(newWriter func() resetWriter) func([]byte) []byte {
	pool := &sync.Pool{New: func() any { return newWriter() }}
	return func(body []byte) []byte {
		var buf bytes.Buffer
		buf.Grow(len(body) / 3)
		w := pool.Get().(resetWriter)
		w.Reset(&buf)
		w.Write(body)
		w.Close()
		w.Reset(io.Discard) // the pooled writer must not keep this body alive
		pool.Put(w)
		return buf.Bytes()
	}
}

// zstd frames stay within the 8 MiB window that browsers decode (RFC 9659).
var (
	zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(8<<20))
	zstdDecoder, _ = zstd.NewReader(nil)
)

func readAll(r io.Reader, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

var gzipCoding = &coding{"gzip",
	pooled(func() resetWriter { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }),
	func(b []byte) ([]byte, error) { return readAll(gzip.NewReader(bytes.NewReader(b))) }}

var codings = map[string]*coding{
	"gzip": gzipCoding,
	"br": {"br", pooled(func() resetWriter { return brotli.NewWriterLevel(nil, 4) }),
		func(b []byte) ([]byte, error) { return io.ReadAll(brotli.NewReader(bytes.NewReader(b))) }},
	"zstd": {"zstd", func(body []byte) []byte { return zstdEncoder.EncodeAll(body, make([]byte, 0, len(body)/3)) },
		func(b []byte) ([]byte, error) { return zstdDecoder.DecodeAll(b, nil) }},
}

// parseCodings reads a comma list of coding names, most preferred first.
func parseCodings(list string) ([]*coding, error) {
	var out []*coding
	for _, name := range strings.Split(list, ",") {
		c, ok := codings[strings.ToLower(strings.TrimSpace(name))]
		if !ok || slices.Contains(out, c) {
			return nil, fmt.Errorf("compression %q: want a list of gzip, br, zstd", list)
		}
		out = append(out, c)
	}
	return out, nil
}

// negotiate picks a coding from Accept-Encoding (RFC 9110 §12.5.3): of those offered, the client's
// highest q, ties in the server's order; else gzip; nil is identity. No header means identity.
func negotiate(header string, offer []*coding) *coding {
	q := map[string]float64{}
	for _, part := range strings.Split(header, ",") {
		params := strings.Split(part, ";")
		name, v := strings.ToLower(strings.TrimSpace(params[0])), 1.0
		for _, p := range params[1:] {
			if k, val, _ := strings.Cut(p, "="); strings.EqualFold(strings.TrimSpace(k), "q") {
				f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil || f < 0 || f > 1 {
					f = -1 // malformed: ignore the element
				}
				v = f
			}
		}
		if name != "" && v >= 0 {
			q[name] = v
		}
	}
	accept := func(name string) float64 {
		if v, ok := q[name]; ok {
			return v
		}
		return q["*"] // 0 when absent
	}
	var best *coding
	bestQ := 0.0
	for _, c := range offer {
		if v := accept(c.name); v > bestQ {
			best, bestQ = c, v
		}
	}
	if best == nil {
		if v := accept("gzip"); v > 0 {
			best, bestQ = gzipCoding, v
		}
	}
	if v, ok := q["identity"]; ok && v > bestQ {
		return nil
	}
	return best
}

// pack readies a computed response for the cache: large bodies are kept in the primary coding only.
func (s *Server) pack(out *encoded) *encoded {
	if len(out.body) >= compressMin {
		out.packed, out.coding, out.body = s.codings[0].encode(out.body), s.codings[0], nil
	}
	return out
}

// represent returns out's body in coding c (nil: identity), and the coding it is in. A coding other
// than the cached one is transcoded once and cached under its own ETag.
func (s *Server) represent(out *encoded, c *coding, tag string) ([]byte, *coding) {
	switch {
	case out.packed == nil && (c == nil || len(out.body) < compressMin):
		return out.body, nil
	case out.packed == nil:
		return c.encode(out.body), c
	case c == out.coding:
		return out.packed, c
	case c == nil:
		b, _ := out.coding.decode(out.packed)
		return b, nil
	}
	key := codedTag(tag, c)
	if hit, ok := s.responses.Get(key); ok {
		return hit.packed, c
	}
	plain, _ := out.coding.decode(out.packed)
	b := c.encode(plain)
	s.responses.Put(key, &encoded{packed: b, coding: c}, int64(len(b)+256))
	return b, c
}

// codedTag marks the ETag of a compressed representation, which must differ from the plain one.
func codedTag(tag string, c *coding) string {
	return strings.TrimSuffix(tag, `"`) + "-" + c.name + `"`
}

// baseTag strips the suffix codedTag adds.
func baseTag(tag string) string {
	for name := range codings {
		if t, ok := strings.CutSuffix(tag, "-"+name+`"`); ok {
			return t + `"`
		}
	}
	return tag
}
