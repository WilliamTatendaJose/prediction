// Package analytics answers time-range questions (bucketed series, summary
// statistics, anomaly history). It reads the database when one is
// configured and falls back to the in-memory rings otherwise, so the API is
// the same with or without persistence; Source in each result says which.
package analytics

import (
	"context"
	"math"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
	"github.com/williamtatendajose/prediction/iot-hub/internal/tsdb"
)

type Service struct {
	Store    *store.Store
	DB       tsdb.DB // nil = memory only
	Detector *anomaly.Detector
}

// Series is columnar: index i of every slice describes bucket i.
type Series struct {
	Field  string    `json:"field"`
	From   int64     `json:"from"`
	To     int64     `json:"to"`
	Bucket int64     `json:"bucket"`
	Source string    `json:"source"` // "db" or "memory"
	T      []int64   `json:"t"`
	N      []int64   `json:"n"`
	Min    []float64 `json:"min"`
	Max    []float64 `json:"max"`
	Avg    []float64 `json:"avg"`
}

type Stats struct {
	tsdb.Stats
	Field  string `json:"field"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Source string `json:"source"`
}

func (s *Service) source() string {
	if s.DB != nil {
		return "db"
	}
	return "memory"
}

func (s *Service) Series(ctx context.Context, sensor, field string, from, to, bucket int64) (Series, error) {
	if bucket <= 0 {
		bucket = tsdb.AutoBucket(from, to, 600)
	}
	out := Series{Field: field, From: from, To: to, Bucket: bucket, Source: s.source(),
		T: []int64{}, N: []int64{}, Min: []float64{}, Max: []float64{}, Avg: []float64{}}
	var bs []tsdb.Bucket
	var err error
	if s.DB != nil {
		bs, err = s.DB.Series(ctx, sensor, field, from, to, bucket)
	} else {
		bs, err = s.memSeries(sensor, field, from, to, bucket)
	}
	if err != nil {
		return out, err
	}
	for _, b := range bs {
		out.T = append(out.T, b.T)
		out.N = append(out.N, b.N)
		out.Min = append(out.Min, b.Min)
		out.Max = append(out.Max, b.Max)
		out.Avg = append(out.Avg, b.Avg)
	}
	return out, nil
}

func (s *Service) Stats(ctx context.Context, sensor, field string, from, to int64) (Stats, error) {
	out := Stats{Field: field, From: from, To: to, Source: s.source()}
	var err error
	if s.DB != nil {
		out.Stats, err = s.DB.Stats(ctx, sensor, field, from, to)
		return out, err
	}
	ts, vs, err := s.Store.History(sensor, field, 0, from-1)
	if err != nil {
		return out, err
	}
	var n int64
	var sum, sumsq float64
	mn, mx := math.Inf(1), math.Inf(-1)
	for i, t := range ts {
		if t >= to {
			break
		}
		v := float64(vs[i])
		n++
		sum += v
		sumsq += v * v
		mn, mx = min(mn, v), max(mx, v)
	}
	out.Stats = tsdb.StatsFromSums(n, sum, sumsq, mn, mx)
	return out, nil
}

func (s *Service) memSeries(sensor, field string, from, to, bucket int64) ([]tsdb.Bucket, error) {
	ts, vs, err := s.Store.History(sensor, field, 0, from-1)
	if err != nil {
		return nil, err
	}
	out := []tsdb.Bucket{}
	var sum float64
	for i, t := range ts {
		if t >= to {
			break
		}
		v := float64(vs[i])
		bt := t - t%bucket
		if len(out) == 0 || out[len(out)-1].T != bt {
			if len(out) > 0 {
				last := &out[len(out)-1]
				last.Avg = sum / float64(last.N)
			}
			out = append(out, tsdb.Bucket{T: bt, Min: v, Max: v})
			sum = 0
		}
		b := &out[len(out)-1]
		b.N++
		sum += v
		b.Min, b.Max = min(b.Min, v), max(b.Max, v)
	}
	if len(out) > 0 {
		last := &out[len(out)-1]
		last.Avg = sum / float64(last.N)
	}
	return out, nil
}

// Events returns anomaly episodes, newest first. Active-only queries always
// come from the detector, which is authoritative for what is open now.
func (s *Service) Events(ctx context.Context, q tsdb.EventQuery) ([]anomaly.Event, error) {
	if s.DB != nil && !q.ActiveOnly {
		return s.DB.Events(ctx, q)
	}
	out := []anomaly.Event{}
	if s.Detector == nil {
		return out, nil
	}
	src := s.Detector.Recent()
	if q.ActiveOnly {
		src = s.Detector.Active()
	}
	for _, e := range src {
		if (q.Sensor != "" && e.Sensor != q.Sensor) || e.Start < q.From || e.Start >= q.To {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}
