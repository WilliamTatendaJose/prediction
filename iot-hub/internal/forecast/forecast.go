// Package forecast projects a sensor series forward with exponential
// smoothing: Holt (level + damped trend) or additive Holt-Winters (+ daily
// season). Parameters are chosen by out-of-sample error on the last part of
// the history, and the result reports that error next to a naive "no
// change" forecast, so a caller can see whether the forecast has any skill.
//
// It answers operational questions like "when will the tank reach 15 %?"
// without a separate ML service. It is not a model of the process: it
// extrapolates recent behaviour and will miss changes it has not seen.
package forecast

import (
	"errors"
	"math"
)

type Params struct {
	Alpha, Beta, Gamma, Phi float64
	Period                  int  // 0 = no season
	Trend                   bool // false = level only (flat projection)
}

type Result struct {
	Method string    `json:"method"` // holt | holt-winters
	Params Params    `json:"params"`
	Yhat   []float64 `json:"yhat"`
	Lo     []float64 `json:"lo"` // approximate 80 % band
	Hi     []float64 `json:"hi"`
	// Holdout error (MAE) of this method vs repeating the last value, on the
	// most recent 20 % of the history. Skill = 1 - MAE/NaiveMAE (>0 = better
	// than naive).
	MAE      float64 `json:"mae"`
	NaiveMAE float64 `json:"naiveMae"`
	Skill    float64 `json:"skill"`
	Sigma    float64 `json:"sigma"` // one-step residual std
}

var ErrTooShort = errors.New("not enough history to forecast (need at least 12 points)")

// state of a fitted model after consuming y.
type state struct {
	level, trend float64
	season       []float64
	sse          float64
	n            int
}

func initState(y []float64, p Params) state {
	s := state{level: y[0]}
	if p.Period > 0 && len(y) >= 2*p.Period {
		// Season from the first two cycles; level/trend from their means.
		m1, m2 := mean(y[:p.Period]), mean(y[p.Period:2*p.Period])
		s.level = m1
		s.trend = (m2 - m1) / float64(p.Period)
		s.season = make([]float64, p.Period)
		for i := 0; i < p.Period; i++ {
			s.season[i] = (y[i] - m1 + y[i+p.Period] - m2) / 2
		}
	} else if len(y) > 1 {
		s.trend = slope(y[:min(len(y), 20)])
	}
	if !p.Trend {
		s.trend = 0
	}
	return s
}

// slope is the least-squares slope per step: robust to a noisy endpoint.
func slope(y []float64) float64 {
	n := float64(len(y))
	mx, my := (n-1)/2, mean(y)
	var num, den float64
	for i, v := range y {
		dx := float64(i) - mx
		num += dx * (v - my)
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	return num / den
}

func mean(x []float64) float64 {
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

// fit runs the recursions over y, accumulating one-step squared errors.
func fit(y []float64, p Params) state {
	s := initState(y, p)
	for t := 1; t < len(y); t++ {
		var seas float64
		if s.season != nil {
			seas = s.season[t%p.Period]
		}
		pred := s.level + p.Phi*s.trend + seas
		e := y[t] - pred
		s.sse += e * e
		s.n++
		prevLevel := s.level
		s.level = p.Alpha*(y[t]-seas) + (1-p.Alpha)*(s.level+p.Phi*s.trend)
		if p.Trend {
			s.trend = p.Beta*(s.level-prevLevel) + (1-p.Beta)*p.Phi*s.trend
		}
		if s.season != nil {
			s.season[t%p.Period] = p.Gamma*(y[t]-s.level) + (1-p.Gamma)*seas
		}
	}
	return s
}

// project forecasts h steps after the last observation (index n-1).
func project(s state, p Params, n, h int) []float64 {
	out := make([]float64, h)
	damp := 0.0
	pow := 1.0
	for k := 1; k <= h; k++ {
		pow *= p.Phi
		damp += pow
		v := s.level + damp*s.trend
		if s.season != nil {
			v += s.season[(n-1+k)%p.Period]
		}
		out[k-1] = v
	}
	return out
}

func mae(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += math.Abs(a[i] - b[i])
	}
	return s / float64(len(a))
}

// Forecast projects h steps ahead. season is the season length in points
// (e.g. one day of buckets); 0 disables seasonality. The seasonal model is
// only used if there are at least three seasons and it beats the plain one
// on the holdout.
func Forecast(y []float64, h, season int) (Result, error) {
	if len(y) < 12 {
		return Result{}, ErrTooShort
	}
	for _, v := range y {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Result{}, errors.New("history contains NaN or Inf")
		}
	}
	split := len(y) - max(len(y)/5, 2)
	train, test := y[:split], y[split:]
	naive := make([]float64, len(test))
	for i := range naive {
		naive[i] = train[len(train)-1]
	}
	naiveMAE := mae(naive, test)

	type cand struct {
		p   Params
		err float64
	}
	// search fits one model family: parameters by one-step error over the
	// whole training history (stable: uses every point), then scores the
	// family by multi-step error on the holdout (relevant: what a forecast
	// over a horizon actually gets wrong). Families are compared by score.
	search := func(trend bool, per int) cand {
		best := cand{err: math.Inf(1)}
		bestSSE := math.Inf(1)
		betas, phis := []float64{0}, []float64{1}
		if trend {
			betas, phis = []float64{0.01, 0.05, 0.1, 0.3}, []float64{0.9, 0.98, 1}
		}
		gammas := []float64{0}
		if per > 0 {
			gammas = []float64{0.05, 0.1, 0.3}
		}
		for _, a := range []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.7, 0.9} {
			for _, b := range betas {
				for _, ph := range phis {
					for _, g := range gammas {
						p := Params{Alpha: a, Beta: b, Gamma: g, Phi: ph, Period: per, Trend: trend}
						if s := fit(train, p); s.sse < bestSSE {
							bestSSE = s.sse
							best = cand{p, mae(project(s, p, len(train), len(test)), test)}
						}
					}
				}
			}
		}
		return best
	}
	// Parsimony: a richer model must beat the simpler one by 10 % out of
	// sample, otherwise noise in the holdout invents trends and seasons
	// (which would mean false "limit reached in N hours" warnings).
	const margin = 0.9
	best := search(false, 0)
	if c := search(true, 0); c.err < margin*best.err {
		best = c
	}
	if season > 1 && len(train) >= 3*season {
		for _, trend := range []bool{false, true} {
			if c := search(trend, season); c.err < margin*best.err {
				best = c
			}
		}
	}
	p := best.p
	s := fit(y, p)
	sigma := 0.0
	if s.n > 0 {
		sigma = math.Sqrt(s.sse / float64(s.n))
	}
	r := Result{Method: "holt", Params: p, Yhat: project(s, p, len(y), h), MAE: best.err, NaiveMAE: naiveMAE, Sigma: sigma}
	if p.Period > 0 {
		r.Method = "holt-winters"
	}
	if naiveMAE > 0 {
		r.Skill = 1 - best.err/naiveMAE
	}
	// Approximate 80 % band: one-step sigma growing with the square root of
	// the horizon (exact for a random walk; a rough guide otherwise).
	r.Lo, r.Hi = make([]float64, h), make([]float64, h)
	for k := range r.Yhat {
		w := 1.2816 * sigma * math.Sqrt(float64(k+1))
		r.Lo[k], r.Hi[k] = r.Yhat[k]-w, r.Yhat[k]+w
	}
	return r, nil
}

// Crossing is when a forecast first reaches a threshold.
type Crossing struct {
	Threshold float64 `json:"threshold"`
	Side      string  `json:"side"` // below | above
	// Step indexes (1-based) of the first crossing of the central forecast
	// and of the early/late edges of the band; 0 = not within the horizon.
	Step, Early, Late int
}

// Cross finds when yhat (and the band) go below/above threshold.
func Cross(r Result, threshold float64, side string) Crossing {
	c := Crossing{Threshold: threshold, Side: side}
	hit := func(v float64) bool {
		if side == "below" {
			return v <= threshold
		}
		return v >= threshold
	}
	first := func(xs []float64) int {
		for i, v := range xs {
			if hit(v) {
				return i + 1
			}
		}
		return 0
	}
	c.Step = first(r.Yhat)
	if side == "below" {
		c.Early, c.Late = first(r.Lo), first(r.Hi)
	} else {
		c.Early, c.Late = first(r.Hi), first(r.Lo)
	}
	return c
}
