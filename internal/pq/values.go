package pq

// Kind is how a column's decoded values are stored.
type Kind uint8

const (
	KindInt    Kind = iota // integers, and dates/timestamps as epoch milliseconds
	KindFloat              // floats, doubles, decimals
	KindBool               // booleans
	KindString             // text
	KindBytes              // binary (the WKB geometry)
)

// Values is one decoded run of a column: typed slices rather than boxed values, so a cached
// column chunk costs about what its data does and gives the garbage collector few pointers.
type Values struct {
	Kind   Kind
	N      int
	Valid  []bool // nil when no value is null
	Ints   []int64
	Floats []float64
	Bools  []bool
	Data   []byte   // KindString/KindBytes: values back to back
	Offs   []uint32 // KindString/KindBytes: value i is Data[Offs[i]:Offs[i+1]]
}

// Null reports whether value i is null.
func (v *Values) Null(i int) bool { return v.Valid != nil && !v.Valid[i] }

// Get returns value i as nil, int64, float64, bool or string (bytes come back as []byte).
func (v *Values) Get(i int) any {
	if v.Null(i) {
		return nil
	}
	switch v.Kind {
	case KindInt:
		return v.Ints[i]
	case KindFloat:
		return v.Floats[i]
	case KindBool:
		return v.Bools[i]
	case KindString:
		return string(v.Data[v.Offs[i]:v.Offs[i+1]])
	default:
		return v.Data[v.Offs[i]:v.Offs[i+1]:v.Offs[i+1]]
	}
}

// Bytes returns value i's bytes (nil when null) without copying.
func (v *Values) Bytes(i int) []byte {
	if v.Null(i) || v.Offs == nil {
		return nil
	}
	return v.Data[v.Offs[i]:v.Offs[i+1]:v.Offs[i+1]]
}

// Size is roughly what the values hold in memory.
func (v *Values) Size() int64 {
	return int64(len(v.Valid) + 8*len(v.Ints) + 8*len(v.Floats) + len(v.Bools) + len(v.Data) + 4*len(v.Offs) + 96)
}

type builder struct {
	v       Values
	anyNull bool
}

// newBuilder starts n values; dataHint sizes the byte buffer of a text or binary column.
func newBuilder(kind Kind, n, dataHint int) *builder {
	b := &builder{v: Values{Kind: kind}}
	b.v.Valid = make([]bool, 0, n)
	switch kind {
	case KindInt:
		b.v.Ints = make([]int64, 0, n)
	case KindFloat:
		b.v.Floats = make([]float64, 0, n)
	case KindBool:
		b.v.Bools = make([]bool, 0, n)
	default:
		b.v.Offs = make([]uint32, 1, n+1)
		b.v.Data = make([]byte, 0, dataHint)
	}
	return b
}

func (b *builder) null() {
	b.anyNull = true
	b.v.Valid = append(b.v.Valid, false)
	switch b.v.Kind {
	case KindInt:
		b.v.Ints = append(b.v.Ints, 0)
	case KindFloat:
		b.v.Floats = append(b.v.Floats, 0)
	case KindBool:
		b.v.Bools = append(b.v.Bools, false)
	default:
		b.v.Offs = append(b.v.Offs, uint32(len(b.v.Data)))
	}
}

func (b *builder) int(x int64) {
	b.v.Valid = append(b.v.Valid, true)
	b.v.Ints = append(b.v.Ints, x)
}

func (b *builder) float(x float64) {
	b.v.Valid = append(b.v.Valid, true)
	b.v.Floats = append(b.v.Floats, x)
}

func (b *builder) bool(x bool) {
	b.v.Valid = append(b.v.Valid, true)
	b.v.Bools = append(b.v.Bools, x)
}

func (b *builder) bytes(x []byte) {
	b.v.Valid = append(b.v.Valid, true)
	b.v.Data = append(b.v.Data, x...)
	b.v.Offs = append(b.v.Offs, uint32(len(b.v.Data)))
}

func (b *builder) done() *Values {
	b.v.N = len(b.v.Valid)
	if !b.anyNull {
		b.v.Valid = nil
	}
	return &b.v
}
