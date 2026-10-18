package pq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

// pageLoc is one data page: its bytes (header included) and the rows it holds.
type pageLoc struct {
	off, size   int64
	first, rows int64
	raw         int64 // uncompressed size, to size the decode buffer
}

// layout is where a column chunk's dictionary and data pages are.
type layout struct {
	dict  [2]int64 // [start, end) of the dictionary page; zero when none
	pages []pageLoc
}

func (lay *layout) firsts() []int64 {
	out := make([]int64, len(lay.pages))
	for i, p := range lay.pages {
		out[i] = p.first
	}
	return out
}

// pagesFor is the pages holding the sorted rows rel, given each page's first row.
func pagesFor(firsts []int64, rel []int64) []int {
	var pages []int
	p := 0
	for _, r := range rel {
		for p+1 < len(firsts) && firsts[p+1] <= r {
			p++
		}
		if len(pages) == 0 || pages[len(pages)-1] != p {
			pages = append(pages, p)
		}
	}
	return pages
}

// want is the pages of a chunk holding the sorted rows rel (nil: every row), or nil when the
// chunk is read whole. Geometry is always read by page, so memory holds only the pages requests
// touch. Other columns are read by page where the offset index places their pages and only some
// are wanted, else whole, decoding a dictionary once.
func (l *Layer) want(ctx context.Context, lf leaf, g int, rel []int64) ([]int, *layout, error) {
	if lf.kind != KindBytes && (rel == nil || !lf.flat || l.chunkMeta(lf, g).OffsetIndexOffset == 0) {
		return nil, nil, nil
	}
	lay, err := l.layout(ctx, lf, g)
	if err != nil {
		return nil, nil, err
	}
	pages := pagesFor(lay.firsts(), rel)
	if lf.kind != KindBytes && len(pages) == len(lay.pages) {
		return nil, lay, nil
	}
	if pages == nil {
		pages = []int{} // no row wanted is no page, not the whole chunk
	}
	return pages, lay, nil
}

// runs returns decoded runs covering the sorted rows rel (nil: every row) of one column chunk,
// in row order, decoding the chunk whole or the pages want picks. A later request fetches just
// the pages it misses.
func (l *Layer) runs(ctx context.Context, lf leaf, g int, rel []int64) ([]run, error) {
	ck := chunkKey{l.ID(), lf.index, g}
	if v, ok := l.reader.decoded.Get(runKey{ck, -1}); ok {
		return []run{{0, v}}, nil
	}
	pages, lay, err := l.want(ctx, lf, g, rel)
	if err != nil {
		return nil, err
	}
	if pages == nil && lf.kind != KindBytes {
		got, err := l.decodeOnce(ctx, ck, []int{-1}, func() (map[int]*Values, error) {
			v, err := l.decodeChunk(ctx, lf, g)
			return map[int]*Values{-1: v}, err
		})
		if err != nil {
			return nil, err
		}
		return []run{{0, got[-1]}}, nil
	}
	out := make([]run, len(pages))
	var missing []int
	for i, p := range pages {
		out[i].first = lay.pages[p].first
		if v, ok := l.reader.decoded.Get(runKey{ck, p}); ok {
			out[i].v = v
		} else {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	got, err := l.decodeOnce(ctx, ck, missing, func() (map[int]*Values, error) {
		return l.decodePages(ctx, lf, g, lay, missing)
	})
	if err != nil {
		return nil, err
	}
	for i, p := range pages {
		if out[i].v == nil {
			out[i].v = got[p]
		}
	}
	return out, nil
}

// decodeOnce runs decode once however many requests want the same pages at the same time, and
// keeps the pages it returns in the decoded cache. The shared work outlives any one caller.
func (l *Layer) decodeOnce(ctx context.Context, ck chunkKey, pages []int, decode func() (map[int]*Values, error)) (map[int]*Values, error) {
	if len(pages) == 1 {
		if v, ok := l.reader.decoded.Get(runKey{ck, pages[0]}); ok {
			return map[int]*Values{pages[0]: v}, nil
		}
	}
	ch := l.reader.flight.DoChan(fmt.Sprint(ck, pages), func() (any, error) {
		got, err := decode()
		if err != nil {
			return nil, err
		}
		for p, v := range got {
			l.reader.decoded.Put(runKey{ck, p}, v, v.Size())
		}
		return got, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(map[int]*Values), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *Layer) chunk(lf leaf, g int) *parquet.FileColumnChunk {
	return l.pf.RowGroups()[g].ColumnChunks()[lf.index].(*parquet.FileColumnChunk)
}

func (l *Layer) chunkMeta(lf leaf, g int) *format.ColumnChunk {
	return &l.pf.Metadata().RowGroups[g].Columns[lf.index]
}

// chunkRange is the bytes of a column chunk, dictionary included.
func (l *Layer) chunkRange(lf leaf, g int) (start, end int64) {
	cm := l.chunkMeta(lf, g).MetaData
	start = cm.DataPageOffset
	if cm.DictionaryPageOffset > 0 {
		start = cm.DictionaryPageOffset
	}
	return start, start + cm.TotalCompressedSize
}

// layout finds where a chunk's pages are: from its page index, or by walking the page headers of
// the fetched chunk once (no decompression), remembered per file version.
func (l *Layer) layout(ctx context.Context, lf leaf, g int) (*layout, error) {
	ck := chunkKey{l.ID(), lf.index, g}
	if lay, ok := l.reader.layouts.Get(ck); ok {
		return lay, nil
	}
	v, err, _ := l.reader.flight.Do(fmt.Sprint("layout", ck), func() (any, error) {
		lay, err := l.readLayout(ctx, lf, g)
		if err == nil {
			l.reader.layouts.Put(ck, lay, int64(48*len(lay.pages)+64))
		}
		return lay, err
	})
	if err != nil {
		return nil, err
	}
	return v.(*layout), nil
}

func (l *Layer) readLayout(ctx context.Context, lf leaf, g int) (*layout, error) {
	cm := l.chunkMeta(lf, g)
	lay := &layout{}
	if cm.OffsetIndexOffset != 0 {
		oi, err := l.chunk(lf, g).OffsetIndex()
		if err == nil && oi.NumPages() > 0 {
			rows := l.Groups[g].Rows
			for i := range oi.NumPages() {
				next := rows
				if i+1 < oi.NumPages() {
					next = oi.FirstRowIndex(i + 1)
				}
				lay.pages = append(lay.pages, pageLoc{off: oi.Offset(i), size: oi.CompressedPageSize(i),
					first: oi.FirstRowIndex(i), rows: next - oi.FirstRowIndex(i)})
			}
			if d := cm.MetaData.DictionaryPageOffset; d > 0 {
				lay.dict = [2]int64{d, lay.pages[0].off}
			}
			return lay, nil
		}
	}
	start, end := l.chunkRange(lf, g)
	release, err := l.hold(ctx, end-start)
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := l.file.Range(ctx, start, end)
	if err != nil {
		return nil, err
	}
	var protocol thrift.CompactProtocol
	var row int64
	for off := int64(0); off < int64(len(data)); {
		r := protocol.NewReaderFromBytes(data[off:])
		var h format.PageHeader
		if err := thrift.NewDecoder(r).Decode(&h); err != nil {
			return nil, fmt.Errorf("column %d row group %d: page header at %d: %w", lf.index, g, start+off, err)
		}
		size := int64(r.BytesRead()) + int64(h.CompressedPageSize)
		n := int64(-1)
		switch {
		case h.Type == format.DictionaryPage:
			lay.dict = [2]int64{start + off, start + off + size}
		case h.DataPageHeaderV2.Valid:
			n = int64(h.DataPageHeaderV2.V.NumRows)
		case h.DataPageHeader.Valid:
			n = int64(h.DataPageHeader.V.NumValues) // a flat column: one value per row
		}
		if n >= 0 {
			lay.pages = append(lay.pages, pageLoc{start + off, size, row, n, int64(h.UncompressedPageSize)})
			row += n
		}
		off += size
	}
	if row != l.Groups[g].Rows {
		return nil, fmt.Errorf("column %d row group %d: pages hold %d rows, not %d", lf.index, g, row, l.Groups[g].Rows)
	}
	return lay, nil
}

// hold waits until n bytes of column chunk may be held at once (capped at the whole allowance).
func (l *Layer) hold(ctx context.Context, n int64) (func(), error) {
	n = min(n, l.reader.inflightSize)
	if err := l.reader.inflight.Acquire(ctx, n); err != nil {
		return nil, err
	}
	return func() { l.reader.inflight.Release(n) }, nil
}

// acquire waits for a decoding slot.
func (l *Layer) acquire(ctx context.Context) (func(), error) {
	select {
	case l.reader.decode <- struct{}{}:
		return func() { <-l.reader.decode }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maxPrefetch caps what one prefetch holds: past it, chunks are fetched as they are decoded.
const maxPrefetch = 32 << 20

// prefetch fetches what reading leaves at the spans' rows needs and is not decoded yet (whole
// chunks, or pages), joined into a few requests, so decoding reads from the cache. Geometry
// whose pages only its chunk's page headers locate is left to the read.
func (l *Layer) prefetch(ctx context.Context, leaves []leaf, spans []rgSpan) error {
	var ranges [][2]int64
	var total int64
	add := func(r [2]int64) { ranges, total = append(ranges, r), total+r[1]-r[0] }
	for _, lf := range leaves {
		for _, sp := range spans {
			ck := chunkKey{l.ID(), lf.index, sp.g}
			if _, ok := l.reader.decoded.Get(runKey{ck, -1}); ok {
				continue
			}
			if lf.kind == KindBytes && l.chunkMeta(lf, sp.g).OffsetIndexOffset == 0 {
				continue
			}
			pages, lay, err := l.want(ctx, lf, sp.g, sp.rel)
			if err != nil {
				return err
			}
			if pages == nil && lf.kind != KindBytes {
				start, end := l.chunkRange(lf, sp.g)
				add([2]int64{start, end})
				continue
			}
			missing := false
			for _, p := range pages {
				if _, ok := l.reader.decoded.Get(runKey{ck, p}); !ok {
					add([2]int64{lay.pages[p].off, lay.pages[p].off + lay.pages[p].size})
					missing = true
				}
			}
			if missing && lay.dict[1] > 0 {
				add(lay.dict)
			}
		}
	}
	if len(ranges) < 2 || total > maxPrefetch {
		return nil
	}
	release, err := l.hold(ctx, total)
	if err != nil {
		return err
	}
	defer release()
	return l.file.Prefetch(ctx, ranges)
}

// decodeChunk decodes a whole column chunk.
func (l *Layer) decodeChunk(ctx context.Context, lf leaf, g int) (*Values, error) {
	start, end := l.chunkRange(lf, g)
	release, err := l.hold(ctx, end-start)
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := l.file.Range(ctx, start, end)
	if err != nil {
		return nil, err
	}
	done, err := l.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	b := newBuilder(lf.kind, int(l.Groups[g].Rows), 0)
	if err := readPages(l.chunk(lf, g).PagesFrom(chunkBytes{data, start}), lf, b, -1); err != nil {
		return nil, fmt.Errorf("column %d row group %d: %w", lf.index, g, err)
	}
	return b.done(), nil
}

// decodePages decodes some pages of a chunk. Each page is handed to the page reader as a chunk
// of its own (the dictionary, then that page), so no other page is fetched or decompressed.
func (l *Layer) decodePages(ctx context.Context, lf leaf, g int, lay *layout, want []int) (map[int]*Values, error) {
	ranges := make([][2]int64, 0, len(want)+1)
	if lay.dict[1] > 0 {
		ranges = append(ranges, lay.dict)
	}
	var total int64
	for _, p := range want {
		pg := lay.pages[p]
		ranges = append(ranges, [2]int64{pg.off, pg.off + pg.size})
		total += pg.size
	}
	release, err := l.hold(ctx, total)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := l.file.Prefetch(ctx, ranges); err != nil {
		return nil, err
	}
	var dict []byte
	if lay.dict[1] > 0 {
		if dict, err = l.file.Range(ctx, lay.dict[0], lay.dict[1]); err != nil {
			return nil, err
		}
	}
	base, _ := l.chunkRange(lf, g)
	out := make(map[int]*Values, len(want))
	for _, p := range want {
		pg := lay.pages[p]
		page, err := l.file.Range(ctx, pg.off, pg.off+pg.size)
		if err != nil {
			return nil, err
		}
		done, err := l.acquire(ctx)
		if err != nil {
			return nil, err
		}
		b := newBuilder(lf.kind, int(pg.rows), int(pg.raw))
		virtual := chunkBytes{append(append(make([]byte, 0, len(dict)+len(page)), dict...), page...), base}
		err = readPages(l.chunk(lf, g).PagesFrom(virtual), lf, b, 1)
		done()
		if err != nil {
			return nil, fmt.Errorf("column %d row group %d page %d: %w", lf.index, g, p, err)
		}
		out[p] = b.done()
	}
	return out, nil
}

// readPages converts up to max data pages (all when max < 0) into b.
func readPages(pages *parquet.FilePages, lf leaf, b *builder, max int) error {
	defer pages.Close()
	for n := 0; max < 0 || n < max; n++ {
		page, err := pages.ReadPage()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		err = readValues(page, lf, b)
		parquet.Release(page)
		if err != nil {
			return err
		}
	}
	return nil
}

// chunkBytes serves fetched chunk bytes to the page reader at the chunk's file offsets.
type chunkBytes struct {
	data  []byte
	start int64
}

func (c chunkBytes) ReadAt(p []byte, off int64) (int, error) {
	i := off - c.start
	if i < 0 || i >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[i:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// readValues converts one page's values into b.
func readValues(page parquet.Page, lf leaf, b *builder) error {
	buf := make([]parquet.Value, 1024)
	vr := page.Values()
	for {
		n, err := vr.ReadValues(buf)
		for _, v := range buf[:n] {
			if v.IsNull() {
				b.null()
			} else {
				lf.conv(b, v)
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && n == 0) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// columnIndex is a chunk's column index, nil when it has none or it does not decode.
func (l *Layer) columnIndex(ctx context.Context, lf leaf, g int) (*format.ColumnIndex, error) {
	k := [2]int{g, lf.index}
	l.mu.Lock()
	ci, ok := l.indexes[k]
	l.mu.Unlock()
	if ok {
		return ci, nil
	}
	cc := l.chunkMeta(lf, g)
	if cc.ColumnIndexOffset == 0 {
		return nil, nil
	}
	b, err := l.file.Range(ctx, cc.ColumnIndexOffset, cc.ColumnIndexOffset+int64(cc.ColumnIndexLength))
	if err != nil {
		return nil, err
	}
	// The library reads a column index whole-file or without its null counts; nulls matter here.
	ci = new(format.ColumnIndex)
	if thrift.Unmarshal(new(thrift.CompactProtocol), bytes.Clone(b), ci) != nil {
		ci = nil
	}
	l.mu.Lock()
	if l.indexes == nil {
		l.indexes = map[[2]int]*format.ColumnIndex{}
	}
	l.indexes[k] = ci
	l.mu.Unlock()
	return ci, nil
}
