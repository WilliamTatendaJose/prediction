package api

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/ingest"
)

const (
	maxBatchLines = 5000
	maxBatchBytes = 4 << 20 // decompressed: a small gzip can't expand without bound
)

var batchIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// batches remembers how far each recent batch got, so a retry after a
// lost response skips what was already ingested.
type batches struct {
	mu    sync.Mutex
	done  map[string]int
	order []string
}

func (b *batches) get(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done[id]
}

func (b *batches) set(id string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done == nil {
		b.done = map[string]int{}
	}
	if _, ok := b.done[id]; !ok {
		b.order = append(b.order, id)
		if len(b.order) > 10000 {
			delete(b.done, b.order[0])
			b.order = b.order[1:]
		}
	}
	b.done[id] = n
}

type batchLine struct {
	S string         `json:"s"`
	T int64          `json:"t"`
	V map[string]any `json:"v"`
}

// ingestBatch: POST /api/ingest/batch, NDJSON lines {"s":sensor,"t":ms,"v":{...}}
// (gzip allowed). X-Batch-Id makes retries idempotent. Used by edge hubs
// forwarding their backlog. Lines the caller may not write, or that are
// malformed, are rejected and skipped; a quota stop answers 429 with how
// many lines were taken, and the sender continues from there later.
func (s *Server) ingestBatch(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(identityKey{}).(*auth.Identity)
	bid := r.Header.Get("X-Batch-Id")
	if bid != "" && !batchIDRe.MatchString(bid) {
		writeErr(w, http.StatusBadRequest, errors.New("bad X-Batch-Id"))
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxBatchBytes)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		defer zr.Close()
		body = io.LimitReader(zr, maxBatchBytes+1)
	}
	if bid != "" { // scoped per identity: one sender can't skip another's lines
		bid = id.Tenant + "/" + id.ID + "/" + bid
	}
	skip := 0
	if bid != "" {
		skip = s.batches.get(bid)
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 64<<10) // edges send lines under 60 KiB
	line, accepted, rejected, duplicate := 0, 0, 0, 0
	var errs []string
	reject := func(e string) {
		rejected++
		if len(errs) < 5 {
			errs = append(errs, fmt.Sprintf("line %d: %s", line, e))
		}
	}
	status := http.StatusOK
	for sc.Scan() {
		line++
		if line > maxBatchLines {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("at most %d lines per batch", maxBatchLines))
			return
		}
		if line <= skip {
			duplicate++
			continue
		}
		var l batchLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.S == "" || len(l.V) == 0 {
			reject("want {\"s\":sensor,\"t\":ms,\"v\":{field:value}}")
			continue
		}
		if !id.Can(auth.Ingest, l.S) {
			reject("not allowed to write " + l.S)
			continue
		}
		rd, err := s.Pipeline.HandleValues(l.S, l.T, l.V)
		if errors.Is(err, ingest.ErrQuota) {
			line-- // not taken: the sender resends it
			status = http.StatusTooManyRequests
			break
		}
		if err != nil {
			reject(err.Error())
			continue
		}
		accepted++
		if s.OnIngest != nil {
			s.OnIngest(rd)
		}
	}
	if err := sc.Err(); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("reading batch: %w", err))
		return
	}
	if bid != "" {
		s.batches.set(bid, line)
	}
	writeJSON(w, status, map[string]any{"processed": line, "accepted": accepted, "rejected": rejected,
		"duplicate": duplicate, "errors": errs})
}

func (s *Server) forwardStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Forward == nil {
		writeErr(w, http.StatusNotFound, errors.New("forwarding is off (set -forward)"))
		return
	}
	writeJSON(w, http.StatusOK, s.Forward())
}
