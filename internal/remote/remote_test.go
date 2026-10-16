package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// server serves one swappable file with range requests and an ETag per version.
type server struct {
	mu      sync.Mutex
	data    []byte
	etag    string
	failing atomic.Int32 // fail this many requests with a 503 first
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.failing.Add(-1) >= 0 {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	data, etag := s.data, s.etag
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
}

func (s *server) swap(data []byte, etag string) {
	s.mu.Lock()
	s.data, s.etag = data, etag
	s.mu.Unlock()
}

func fill(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func TestVersionedReads(t *testing.T) {
	srv := &server{}
	srv.swap(fill(1<<20, 'a'), `"v1"`)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	store := NewStore(64 << 20)

	f1, err := store.Open(ctx, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := f1.ReadAt(buf, 100); err != nil || buf[0] != 'a' {
		t.Fatalf("read %q %v", buf, err)
	}

	// A re-ingest overwrites the URL: the version changes and the new file gets its own keys.
	srv.swap(fill(1<<20, 'b'), `"v2"`)
	if v, err := store.Version(ctx, ts.URL); err != nil || v != `"v2"` {
		t.Fatalf("version %q %v", v, err)
	}
	f2, err := store.Open(ctx, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f1.ID() == f2.ID() {
		t.Fatal("versions share cache keys")
	}
	if _, err := f2.ReadAt(buf, 100); err != nil || buf[0] != 'b' {
		t.Fatalf("new version read %q %v", buf, err)
	}
	// The old file's cached bytes are its own; an uncached read of it fails rather than mixing.
	if _, err := f1.ReadAt(buf, 100); err != nil || buf[0] != 'a' {
		t.Fatalf("old cached read %q %v", buf, err)
	}
	if _, err := f1.ReadAt(buf, 500<<10); !errors.Is(err, ErrChanged) {
		t.Fatalf("reading an overwritten version should fail with ErrChanged (If-Match), got %v", err)
	}
}

func TestRetriesServerErrors(t *testing.T) {
	srv := &server{}
	srv.swap(fill(300<<10, 'c'), `"x"`)
	srv.failing.Store(2)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	f, err := NewStore(1<<20).Open(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != 300<<10 {
		t.Fatalf("size %d", f.Size)
	}
}

func TestPrefetchParts(t *testing.T) {
	data := make([]byte, 10<<20)
	for i := range data {
		data[i] = byte(i)
	}
	srv := &server{}
	srv.swap(data, `"p"`)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	f, err := NewStore(64<<20).Open(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Prefetch(context.Background(), [][2]int64{{0, 9 << 20}}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1000)
	if _, err := f.ReadAt(got, 5<<20); err != nil || !bytes.Equal(got, data[5<<20:5<<20+1000]) {
		t.Fatal("parts were joined wrongly")
	}
	if _, err := f.ReadAt(got, int64(len(data))-10); err != io.EOF {
		t.Fatal(fmt.Sprint("short read at the end should say EOF, got ", err))
	}
}

func TestRefusesAServerIgnoringRanges(t *testing.T) {
	big := make([]byte, tailBytes+1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(big) // 200 with the whole file, whatever the Range
	}))
	defer ts.Close()
	if _, err := NewStore(1<<20).Open(context.Background(), ts.URL); !errors.Is(err, errNoRanges) {
		t.Fatalf("Open = %v, want errNoRanges", err)
	}
}
