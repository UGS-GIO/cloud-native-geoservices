package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// benchBodies are a ~430 KB PBF (the qfaults, geolunits and wells responses concatenated, which
// protobuf reads as one message holding every feature) and a ~440 KB JSON query response.
func benchBodies(t testing.TB) (pbfBody, jsonBody []byte) {
	ts := fixture(t)
	fetch := func(q string) []byte {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ugs/rest/services/"+q, nil)
		req.Header.Set("Accept-Encoding", "identity")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return b
	}
	for _, q := range []string{"qfaults/default", "geolunits/default", "wells/by-purpose"} {
		pbfBody = append(pbfBody, fetch(q+"/FeatureServer/0/query?f=pbf&where=1=1&outFields=*&resultType=tile")...)
	}
	return pbfBody, fetch("qfaults/default/FeatureServer/0/query?f=json&where=1=1&outFields=*")
}

func TestNegotiate(t *testing.T) {
	all, _ := parseCodings("zstd,br,gzip")
	brFirst, _ := parseCodings("br,zstd,gzip")
	brOnly, _ := parseCodings("br")
	tests := []struct {
		header string
		offer  []*coding
		want   string // "" is identity
	}{
		{"", all, ""},
		{"gzip", []*coding{gzipCoding}, "gzip"},
		{"gzip, deflate, br, zstd", all, "zstd"}, // Chrome
		{"gzip, deflate, br, zstd", brFirst, "br"},
		{"gzip, deflate, br", all, "br"}, // Safari
		{"gzip;q=1, br;q=0.5", brFirst, "gzip"},
		{"GZIP;Q=0.8", all, "gzip"},
		{"gzip ; q=0.001", all, "gzip"},
		{"gzip;q=0", all, ""},
		{"gzip;q=abc", all, ""},
		{"gzip;level=1;q=0", all, ""},
		{"br;q=0", brOnly, ""},
		{"br;q=0, *", all, "zstd"},
		{"zstd;q=0, br;q=0, *", all, "gzip"},
		{"*", all, "zstd"},
		{"*;q=0", all, ""},
		{"*;q=0, identity", all, ""},
		{"deflate", all, ""},
		{"zstd", brOnly, ""},
		{"gzip, zstd", brOnly, "gzip"},
		{"gzip;q=0.5, identity", all, ""},
		{"gzip;q=0.5, identity;q=0.1", all, "gzip"},
		{"identity;q=0, br", all, "br"},
	}
	for _, tt := range tests {
		got := ""
		if c := negotiate(tt.header, tt.offer); c != nil {
			got = c.name
		}
		if got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestParseCodings(t *testing.T) {
	if cs, err := parseCodings(" zstd, BR,gzip"); err != nil || len(cs) != 3 || cs[0].name != "zstd" || cs[1].name != "br" {
		t.Errorf("parse: %v %v", cs, err)
	}
	for _, bad := range []string{"", "deflate", "gzip,gzip", "gzip,"} {
		if _, err := parseCodings(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// browserDecode decodes as browsers do: the standard library's gzip, the Brotli reference decoder's
// port, and zstd with the 8 MiB window browsers allow.
func browserDecode(t testing.TB, name string, b []byte) []byte {
	var r io.Reader
	var err error
	switch name {
	case "gzip":
		r, err = gzip.NewReader(bytes.NewReader(b))
	case "br":
		r = brotli.NewReader(bytes.NewReader(b))
	case "zstd":
		var d *zstd.Decoder
		d, err = zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderMaxWindow(8<<20))
		if err == nil {
			defer d.Close()
			r = d
		}
	}
	if err != nil {
		t.Fatal(name, err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(name, err)
	}
	return out
}

func TestCodingRoundTrip(t *testing.T) {
	pbfBody, jsonBody := benchBodies(t)
	var big bytes.Buffer // past the 8 MiB zstd window
	for i := 0; big.Len() < 12<<20; i++ {
		fmt.Fprintf(&big, `{"id":%d,"v":%d},`, i, i*7919%104729)
	}
	for name, c := range codings {
		for _, body := range [][]byte{pbfBody, jsonBody, big.Bytes()} {
			out := c.encode(body)
			if !bytes.Equal(browserDecode(t, name, out), body) {
				t.Errorf("%s: round trip differs", name)
			}
			if again := c.encode(body); !bytes.Equal(again, out) {
				t.Errorf("%s: output not deterministic", name)
			}
			if back, err := c.decode(out); err != nil || !bytes.Equal(back, body) {
				t.Errorf("%s: decode: %v", name, err)
			}
		}
	}
}

func TestCompressionHeaders(t *testing.T) {
	ts := fixtureAt(t, mustAbs("../../testdata"), "zstd,br")
	u := ts.URL + "/ugs/rest/services/qfaults/default/FeatureServer/0/query?f=json&where=1=1&outFields=*"
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	fetch := func(accept string, header ...string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if accept != "-" {
			req.Header.Set("Accept-Encoding", accept)
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, b
	}
	_, plain := fetch("-")
	var base string
	for _, tt := range []struct{ accept, coding string }{
		{"-", ""}, {"zstd", "zstd"}, {"gzip, deflate, br, zstd", "zstd"}, {"br", "br"}, {"gzip", "gzip"}, {"deflate", ""},
	} {
		for range 2 { // computed or transcoded, then cached
			res, body := fetch(tt.accept)
			tag, ce := res.Header.Get("ETag"), res.Header.Get("Content-Encoding")
			if ce != tt.coding || res.Header.Get("Vary") != "Accept-Encoding" {
				t.Fatalf("%q: Content-Encoding %q Vary %q", tt.accept, ce, res.Header.Get("Vary"))
			}
			if ce == "" {
				base = tag
			} else {
				if !strings.HasSuffix(tag, "-"+ce+`"`) || baseTag(tag) != base {
					t.Errorf("%q: ETag %s, plain %s", tt.accept, tag, base)
				}
				body = browserDecode(t, ce, body)
			}
			if !bytes.Equal(body, plain) {
				t.Errorf("%q: body differs", tt.accept)
			}
			if res, _ := fetch(tt.accept, "If-None-Match", tag); res.StatusCode != http.StatusNotModified ||
				res.Header.Get("Vary") != "Accept-Encoding" || res.Header.Get("ETag") != tag {
				t.Errorf("%q: If-None-Match %s gave %d", tt.accept, tag, res.StatusCode)
			}
		}
	}
}

func mustAbs(p string) string {
	abs, _ := filepath.Abs(p)
	return abs
}

// BenchmarkCodings encodes each body once per op; out-bytes is the encoded size.
func BenchmarkCodings(b *testing.B) {
	pbfBody, jsonBody := benchBodies(b)
	for _, body := range []struct {
		name string
		b    []byte
	}{{"pbf", pbfBody}, {"json", jsonBody}} {
		for _, name := range []string{"gzip", "br", "zstd"} {
			c := codings[name]
			b.Run(body.name+"/"+name, func(b *testing.B) {
				b.SetBytes(int64(len(body.b)))
				for b.Loop() {
					c.encode(body.b)
				}
				b.ReportMetric(float64(len(c.encode(body.b))), "out-bytes")
			})
		}
	}
}

// BenchmarkCacheHit serves a cached PBF in the primary coding, in another coding cached under its
// own ETag, and in another coding transcoded on every request (the cache was too small to keep it).
func BenchmarkCacheHit(b *testing.B) {
	pbfBody, _ := benchBodies(b)
	s := New(nil, 0, "", Limits{}, 64<<20, nil)
	s.Compress("zstd,br,gzip")
	out := s.pack(&encoded{body: bytes.Clone(pbfBody)})
	b.Run("primary", func(b *testing.B) {
		for b.Loop() {
			s.represent(out, codings["zstd"], `"t"`)
		}
	})
	for _, name := range []string{"br", "gzip"} {
		b.Run(name+"/cached", func(b *testing.B) {
			for b.Loop() {
				s.represent(out, codings[name], `"t"`)
			}
		})
		b.Run(name+"/transcoded", func(b *testing.B) {
			for b.Loop() {
				s.responses.Put(codedTag(`"t"`, codings[name]), nil, 1<<40) // evicts itself
				s.represent(out, codings[name], `"t"`)
			}
		})
	}
	b.Run("identity", func(b *testing.B) {
		for b.Loop() {
			s.represent(out, nil, `"t"`)
		}
	})
}
