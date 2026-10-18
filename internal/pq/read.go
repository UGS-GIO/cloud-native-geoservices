package pq

import (
	"context"
	"fmt"
	"runtime"
	"sort"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/lru"
	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/remote"
)

// Reader opens layers and holds the caches they share.
type Reader struct {
	store   *remote.Store
	decoded *lru.Cache[runKey, *Values]
	layouts *lru.Cache[chunkKey, *layout] // where each page of a chunk is
	flight  singleflight.Group
	decode  chan struct{} // bounds concurrent decoding to the CPUs
	// inflight bounds the column chunk bytes being fetched and decoded at once, across requests,
	// so many large chunks (100+ MB of geometry each) cannot all be held at the same time.
	inflight     *semaphore.Weighted
	inflightSize int64
}

// NewReader makes a reader with a page cache and a decoded-value cache of the given budgets, and
// at most inflightBytes of column chunks being read at once.
func NewReader(pageBytes, decodedBytes, inflightBytes int64) *Reader {
	return &Reader{store: remote.NewStore(pageBytes), decoded: lru.New[runKey, *Values](decodedBytes),
		layouts: lru.New[chunkKey, *layout](16 << 20), decode: make(chan struct{}, runtime.GOMAXPROCS(0)),
		inflight: semaphore.NewWeighted(inflightBytes), inflightSize: inflightBytes}
}

// Version is the current version of url, from one HEAD request.
func (r *Reader) Version(ctx context.Context, url string) (string, error) {
	return r.store.Version(ctx, url)
}

// chunkKey names one column chunk of one file version.
type chunkKey struct {
	file     string
	leaf, rg int
}

// runKey names one decoded run of a chunk: the whole chunk (page -1) or one page.
type runKey struct {
	chunkKey
	page int
}

// run is a decoded run and the row (within its row group) it starts at.
type run struct {
	first int64
	v     *Values
}

// rgSpan is the rows[from:to] that fall in row group g, and those rows numbered within it.
type rgSpan struct {
	g, from, to int
	rel         []int64
}

// Cell locates one value.
type Cell struct {
	V *Values
	I int
}

// Rows holds the values of some columns at some rows: Cells[column][row].
type Rows struct {
	Cols  []string
	Cells [][]Cell
}

// Get is column c's value at row i.
func (r *Rows) Get(c, i int) any {
	cell := r.Cells[c][i]
	return cell.V.Get(cell.I)
}

// AppendKey is Values.AppendKey for column c at row i.
func (r *Rows) AppendKey(buf []byte, c, i int, sep byte) ([]byte, bool) {
	cell := r.Cells[c][i]
	return cell.V.AppendKey(buf, cell.I, sep)
}

// Bytes is column c's bytes at row i.
func (r *Rows) Bytes(c, i int) []byte {
	cell := r.Cells[c][i]
	return cell.V.Bytes(cell.I)
}

// groupOf is the row group holding row.
func (l *Layer) groupOf(row int64) int {
	return sort.Search(len(l.Groups), func(i int) bool { return l.Groups[i].Start > row }) - 1
}

// spans splits the sorted rows by row group.
func (l *Layer) spans(rows []int64) ([]rgSpan, error) {
	var spans []rgSpan
	for i := 0; i < len(rows); {
		if rows[i] < 0 || rows[i] >= l.Rows {
			return nil, fmt.Errorf("row %d out of range", rows[i])
		}
		sp := rgSpan{g: l.groupOf(rows[i]), from: i}
		for start := l.Groups[sp.g].Start; i < len(rows) && rows[i] < start+l.Groups[sp.g].Rows; i++ {
			sp.rel = append(sp.rel, rows[i]-start)
		}
		sp.to = i
		spans = append(spans, sp)
	}
	return spans, nil
}

// Read returns the values of cols at the sorted row numbers rows, decoding each run once.
func (l *Layer) Read(ctx context.Context, cols []string, rows []int64) (*Rows, error) {
	out := &Rows{Cols: cols, Cells: make([][]Cell, len(cols))}
	spans, err := l.spans(rows)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	leaves := make([]leaf, len(cols))
	for c, name := range cols {
		lf, ok := l.leaves[name]
		if !ok {
			return nil, fmt.Errorf("no column %q", name)
		}
		leaves[c] = lf
	}

	groups := make([]int, len(spans))
	for s, sp := range spans {
		groups[s] = sp.g
	}
	if err := l.prefetch(ctx, leaves, groups); err != nil {
		return nil, err
	}
	// Every (column, row group) in parallel: cached runs, or fetch and decode.
	runs := make([][][]run, len(cols))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(32)
	for c, lf := range leaves {
		runs[c] = make([][]run, len(spans))
		for s, sp := range spans {
			g.Go(func() (err error) {
				runs[c][s], err = l.runs(gctx, lf, sp.g, sp.rel)
				return err
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	for c := range cols {
		cells := make([]Cell, len(rows))
		for s, sp := range spans {
			rs := runs[c][s]
			start := l.Groups[sp.g].Start
			k := 0
			for i := sp.from; i < sp.to; i++ {
				rel := rows[i] - start
				for k+1 < len(rs) && rs[k+1].first <= rel {
					k++
				}
				cells[i] = Cell{rs[k].v, int(rel - rs[k].first)}
			}
		}
		out.Cells[c] = cells
	}
	return out, nil
}

// AllRows is every row number of the file.
func (l *Layer) AllRows() []int64 {
	out := make([]int64, l.Rows)
	for i := range out {
		out[i] = int64(i)
	}
	return out
}
