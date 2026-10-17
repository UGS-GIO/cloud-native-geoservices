package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Responses of at least this many bytes are sent gzipped to clients that accept it.
const gzipMin = 1024

var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

// compress gzips body. The same bytes always compress to the same output, so a gzipped
// response keeps one ETag; the cache keeps the compressed form, so a hit is never recompressed.
func compress(body []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(body) / 3)
	w := gzipWriters.Get().(*gzip.Writer)
	w.Reset(&buf)
	w.Write(body)
	w.Close()
	w.Reset(io.Discard) // the pooled writer must not keep this body alive
	gzipWriters.Put(w)
	return buf.Bytes()
}

func decompress(gz []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil
	}
	b, _ := io.ReadAll(r)
	return b
}

// pack readies a computed response for the cache: large bodies are kept compressed only.
func pack(out *encoded) *encoded {
	if len(out.body) >= gzipMin {
		out.gz, out.body = compress(out.body), nil
	}
	return out
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}

// gzipTag marks the ETag of the gzipped representation, which must differ from the plain one.
func gzipTag(tag string) string {
	return strings.TrimSuffix(tag, `"`) + `-gzip"`
}
