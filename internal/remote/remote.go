// Package remote reads byte ranges of a file over https, keeping what it fetched in a shared,
// byte-bounded cache. A File implements io.ReaderAt for the parquet reader.
//
// Every cache key carries the file's version (its ETag, or Last-Modified and size), because a
// re-ingest overwrites the same URL: a new version gets new keys and old entries age out.
package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/lru"
)

const (
	tailBytes = 256 << 10 // first read of a file: enough for most footers
	minRead   = 64 << 10  // a miss reads at least this much
	gap       = 64 << 10  // join ranges this close; fewer requests beat a few skipped bytes
	maxSpan   = 16 << 20  // but keep each request a manageable size
	parallel  = 16        // requests in flight per prefetch
	attempts  = 4         // tries per range, with jittered backoff between them
	partSize  = 4 << 20   // a larger range is fetched in parallel parts
)

type spanKey struct {
	file       string
	start, end int64
}

// Store is the page cache shared by every file, and the HTTP client that fills it.
type Store struct {
	cache  *lru.Cache[spanKey, []byte]
	client *http.Client
}

// NewStore makes a page cache of budget bytes. Requests use a pool of HTTP/1.1 connections:
// one multiplexed HTTP/2 connection lets a single stalled stream hold up the rest.
func NewStore(budget int64) *Store {
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       32,
	}
	return &Store{cache: lru.New[spanKey, []byte](budget), client: &http.Client{Transport: t}}
}

// File is one remote (or local) file at one version.
type File struct {
	URL     string
	Version string // ETag, or Last-Modified and size
	Size    int64
	store   *Store
	local   *os.File

	mu    sync.Mutex
	spans []span // what this file has in the cache, by start; entries may since be evicted
	group singleflight.Group
}

type span struct{ start, end int64 }

// ID names the file at its version, for cache keys.
func (f *File) ID() string { return f.URL + "@" + f.Version }

func version(h http.Header, size int64) string {
	if e := h.Get("ETag"); e != "" {
		return e
	}
	return h.Get("Last-Modified") + "/" + strconv.FormatInt(size, 10)
}

func isHTTP(url string) bool {
	return strings.HasPrefix(url, "http:") || strings.HasPrefix(url, "https:")
}

// Version asks for the current version of url with one HEAD (or a stat for a local file).
func (s *Store) Version(ctx context.Context, url string) (string, error) {
	if !isHTTP(url) {
		st, err := os.Stat(url)
		if err != nil {
			return "", err
		}
		return st.ModTime().String() + "/" + strconv.FormatInt(st.Size(), 10), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("head %s: %w", url, err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("head %s: status %d", url, res.StatusCode)
	}
	return version(res.Header, res.ContentLength), nil
}

// Open fetches the tail of url (where parquet keeps its footer), learning its size and version.
func (s *Store) Open(ctx context.Context, url string) (*File, error) {
	if !isHTTP(url) {
		f, err := os.Open(url)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		return &File{URL: url, Version: st.ModTime().String() + "/" + strconv.FormatInt(st.Size(), 10),
			Size: st.Size(), store: s, local: f}, nil
	}
	f := &File{URL: url, store: s}
	var body []byte
	err := retry(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=-%d", tailBytes))
		res, err := s.client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		// One byte past the tail: a 200 longer than that is a server ignoring Range on a big file.
		if body, err = io.ReadAll(io.LimitReader(res.Body, tailBytes+1)); err != nil {
			return err
		}
		switch res.StatusCode {
		case http.StatusOK:
			if len(body) > tailBytes {
				return permanent(errNoRanges)
			}
			f.Size = int64(len(body))
		case http.StatusPartialContent:
			cr := res.Header.Get("Content-Range")
			total, err := strconv.ParseInt(cr[strings.LastIndexByte(cr, '/')+1:], 10, 64)
			if err != nil || int64(len(body)) > total {
				return permanent(fmt.Errorf("bad Content-Range %q", cr))
			}
			f.Size = total
		default:
			return statusError(res.StatusCode)
		}
		f.Version = version(res.Header, f.Size)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", url, err)
	}
	f.put(f.Size-int64(len(body)), f.Size, body)
	return f, nil
}

// ReadAt serves p from the cache, fetching what is missing.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if f.local != nil {
		return f.local.ReadAt(p, off)
	}
	if off >= f.Size {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), f.Size)
	b, s, ok := f.lookup(off, end)
	if !ok {
		var err error
		if b, err = f.fetch(context.Background(), off, min(f.Size, max(end, off+minRead))); err != nil {
			return 0, err
		}
		s = off
	}
	n := copy(p, b[off-s:end-s])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Range returns the bytes [start, end), from the cache or fetched (in parallel parts). The caller
// owns the result even if the cache cannot keep it.
func (f *File) Range(ctx context.Context, start, end int64) ([]byte, error) {
	if f.local != nil {
		b := make([]byte, end-start)
		if _, err := f.local.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return b, nil
	}
	if b, s, ok := f.lookup(start, end); ok {
		return b[start-s : end-s], nil
	}
	return f.fetch(ctx, start, end)
}

// Prefetch fetches the given [start, end) ranges, joined and in parallel, so the reads that
// follow hit the cache.
func (f *File) Prefetch(ctx context.Context, ranges [][2]int64) error {
	if f.local != nil || len(ranges) == 0 {
		return nil
	}
	var todo [][2]int64
	for _, r := range ranges {
		if _, _, ok := f.lookup(r[0], r[1]); !ok {
			todo = append(todo, r)
		}
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i][0] < todo[j][0] })
	var joined [][2]int64
	for _, r := range todo {
		if n := len(joined); n > 0 && r[0]-joined[n-1][1] <= gap && r[1]-joined[n-1][0] <= maxSpan {
			joined[n-1][1] = max(joined[n-1][1], r[1])
		} else {
			joined = append(joined, r)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for _, r := range joined {
		g.Go(func() error {
			_, err := f.fetch(gctx, r[0], r[1])
			return err
		})
	}
	return g.Wait()
}

func (f *File) lookup(start, end int64) ([]byte, int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := sort.Search(len(f.spans), func(i int) bool { return f.spans[i].start > start }) - 1
	for checked := 0; i >= 0 && checked < 64; i, checked = i-1, checked+1 {
		s := f.spans[i]
		if s.end < end {
			continue
		}
		b, ok := f.store.cache.Get(spanKey{f.ID(), s.start, s.end})
		if !ok {
			f.spans = append(f.spans[:i], f.spans[i+1:]...)
			continue
		}
		return b, s.start, true
	}
	return nil, 0, false
}

func (f *File) put(start, end int64, b []byte) {
	f.store.cache.Put(spanKey{f.ID(), start, end}, b, int64(len(b)))
	f.mu.Lock()
	defer f.mu.Unlock()
	i := sort.Search(len(f.spans), func(i int) bool { return f.spans[i].start >= start })
	if i < len(f.spans) && f.spans[i] == (span{start, end}) {
		return
	}
	f.spans = append(f.spans, span{})
	copy(f.spans[i+1:], f.spans[i:])
	f.spans[i] = span{start, end}
}

// fetch reads [start, end) once however many callers ask at the same time. The shared request
// outlives any one caller's context; a caller that gives up just stops waiting.
func (f *File) fetch(ctx context.Context, start, end int64) ([]byte, error) {
	ch := f.group.DoChan(fmt.Sprint(start, "-", end), func() (any, error) {
		b, err := f.getParts(start, end)
		if err != nil {
			return nil, fmt.Errorf("read %s [%d, %d): %w", f.URL, start, end, err)
		}
		f.put(start, end, b)
		return b, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]byte), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// getParts reads [start, end), splitting a large range into parallel requests: one connection
// to the CDN tops out well below what several give.
func (f *File) getParts(start, end int64) ([]byte, error) {
	b := make([]byte, end-start)
	g := errgroup.Group{}
	g.SetLimit(parallel)
	for at := start; at < end; at += partSize {
		to := min(end, at+partSize)
		g.Go(func() error {
			return retry(context.Background(), func(ctx context.Context) error {
				return f.get(ctx, at, to, b[at-start:to-start])
			})
		})
	}
	return b, g.Wait()
}

func (f *File) get(ctx context.Context, start, end int64, b []byte) error {
	t0 := time.Now()
	defer func() {
		slog.Debug("range", "url", f.URL, "start", start, "bytes", end-start, "ms", time.Since(t0).Milliseconds())
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return permanent(err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	if strings.HasPrefix(f.Version, `"`) {
		req.Header.Set("If-Match", f.Version) // a re-ingest mid-read fails rather than mixes versions
	}
	res, err := f.store.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return permanent(errNoRanges) // reading the whole file for every range would not end well
	}
	if res.StatusCode != http.StatusPartialContent {
		return statusError(res.StatusCode)
	}
	if _, err := io.ReadFull(res.Body, b); err != nil {
		return fmt.Errorf("short read: %w", err)
	}
	return nil
}

var errNoRanges = errors.New("server ignored the Range header")

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func permanent(err error) error { return permanentError{err} }

// ErrChanged is a range read refused because the file was replaced (a re-ingest) after it was opened.
var ErrChanged = errors.New("data file changed since it was opened")

// statusError retries server errors and gives up on the rest (a 412 means the file changed).
func statusError(code int) error {
	if code == http.StatusPreconditionFailed {
		return permanent(ErrChanged)
	}
	err := fmt.Errorf("status %d", code)
	if code >= 500 || code == http.StatusTooManyRequests {
		return err
	}
	return permanent(err)
}

// retry runs try up to `attempts` times with a per-try timeout and jittered exponential backoff.
func retry(ctx context.Context, try func(context.Context) error) error {
	var err error
	for a := 0; a < attempts; a++ {
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = try(tctx)
		cancel()
		var p permanentError
		if err == nil || errors.As(err, &p) || ctx.Err() != nil {
			return err
		}
		backoff := time.Duration(50<<a) * time.Millisecond
		select {
		case <-time.After(backoff/2 + rand.N(backoff)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
