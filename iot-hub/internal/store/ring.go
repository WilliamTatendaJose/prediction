package store

// Ring is a fixed-capacity time series buffer. It allocates once at creation
// and never grows: 12 bytes per point (int64 ms timestamp + float32 value).
// It is not safe for concurrent use; Store guards it.
type Ring struct {
	ts   []int64
	vals []float32
	head int // next write position
	n    int // number of valid points
}

func NewRing(capacity int) *Ring {
	return &Ring{ts: make([]int64, capacity), vals: make([]float32, capacity)}
}

func (r *Ring) Push(ts int64, v float32) {
	r.ts[r.head] = ts
	r.vals[r.head] = v
	r.head = (r.head + 1) % len(r.ts)
	if r.n < len(r.ts) {
		r.n++
	}
}

func (r *Ring) Len() int { return r.n }

// Last returns up to limit of the newest points with ts > since, oldest first.
func (r *Ring) Last(limit int, since int64) (ts []int64, vals []float32) {
	if limit <= 0 || limit > r.n {
		limit = r.n
	}
	ts = make([]int64, 0, limit)
	vals = make([]float32, 0, limit)
	c := len(r.ts)
	start := (r.head - limit + c) % c
	for i := 0; i < limit; i++ {
		j := (start + i) % c
		if r.ts[j] > since {
			ts = append(ts, r.ts[j])
			vals = append(vals, r.vals[j])
		}
	}
	return ts, vals
}
