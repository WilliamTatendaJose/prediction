package calc

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Spec defines a calculated field in a sensor definition:
//
//	{"calc": {"formula": "voltage * current / 1000"}}
//	{"calc": {"integrate": "power_kw", "per": "1h"}}      // kWh from kW
//
// A formula may use the sensor's measured fields (not other calculated
// fields, so there are no chains or cycles to resolve). An integral may
// integrate any non-integral field, including a formula.
type Spec struct {
	Formula   string `json:"formula,omitempty"`
	Integrate string `json:"integrate,omitempty"`
	Per       string `json:"per,omitempty"`    // integration unit, default 1h
	MaxGap    string `json:"maxGap,omitempty"` // longer gaps are not integrated, default 5m
}

// Validate checks one sensor's calculated fields together.
func Validate(specs map[string]*Spec) error {
	for name, s := range specs {
		switch {
		case s.Formula != "" && s.Integrate != "":
			return fmt.Errorf("%s: use either formula or integrate, not both", name)
		case s.Formula != "":
			e, err := Parse(s.Formula)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, r := range e.Fields() {
				if r == name {
					return fmt.Errorf("%s: a formula cannot refer to itself", name)
				}
				if specs[r] != nil {
					return fmt.Errorf("%s: refers to calculated field %s (formulas use measured fields only)", name, r)
				}
			}
		case s.Integrate != "":
			if s.Integrate == name {
				return fmt.Errorf("%s: cannot integrate itself", name)
			}
			if src := specs[s.Integrate]; src != nil && src.Integrate != "" {
				return fmt.Errorf("%s: cannot integrate another integral", name)
			}
			for _, d := range []string{s.Per, s.MaxGap} {
				if d != "" {
					if v, err := time.ParseDuration(d); err != nil || v <= 0 {
						return fmt.Errorf("%s: bad duration %q", name, d)
					}
				}
			}
		default:
			return fmt.Errorf("%s: calc needs a formula or integrate", name)
		}
	}
	return nil
}

type integ struct {
	lastTS int64
	lastV  float64
	total  float64
	init   bool // total seeded
	have   bool // lastTS/lastV valid
}

// Engine computes calculated fields as readings arrive. It is safe for
// concurrent use.
type Engine struct {
	mu     sync.Mutex
	exprs  map[string]*Expr  // formula text -> compiled
	integs map[string]*integ // sensor \x00 field
	Errors func(sensor, field string, err error)
}

func NewEngine() *Engine {
	return &Engine{exprs: map[string]*Expr{}, integs: map[string]*integ{}}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func dur(s string, def time.Duration) float64 {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return float64(d.Milliseconds())
	}
	return float64(def.Milliseconds())
}

// Apply returns calculated values for one incoming reading. values are the
// fields that just arrived; last holds the sensor's latest known values
// (used for formula inputs not in this reading, and to seed integrals after
// a restart).
func (e *Engine) Apply(sensor string, ts int64, values, last map[string]any, specs map[string]*Spec) map[string]any {
	out := map[string]any{}
	vars := func(n string) (float64, bool) {
		if v, ok := values[n]; ok {
			return toFloat(v)
		}
		if v, ok := out[n]; ok {
			return toFloat(v)
		}
		return toFloat(last[n])
	}
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic

	e.mu.Lock()
	defer e.mu.Unlock()
	// Formulas first: they only depend on measured fields. Recompute only
	// when one of their inputs arrived, so a formula does not repeat a
	// stale value on every unrelated reading.
	for _, n := range names {
		s := specs[n]
		if s.Formula == "" {
			continue
		}
		x := e.exprs[s.Formula]
		if x == nil {
			var err error
			if x, err = Parse(s.Formula); err != nil {
				continue // validated on definition; unreachable in practice
			}
			e.exprs[s.Formula] = x
		}
		fresh := false
		for _, r := range x.Fields() {
			if _, ok := values[r]; ok {
				fresh = true
			}
		}
		if !fresh {
			continue
		}
		v, err := x.Eval(vars)
		if err != nil {
			if e.Errors != nil && !errors.Is(err, ErrMissing) {
				e.Errors(sensor, n, err)
			}
			continue
		}
		out[n] = v
	}
	// Integrals: trapezoid rule over the source's samples.
	for _, n := range names {
		s := specs[n]
		if s.Integrate == "" {
			continue
		}
		src, ok := values[s.Integrate]
		if !ok {
			src, ok = out[s.Integrate]
		}
		if !ok {
			continue
		}
		v, ok := toFloat(src)
		if !ok {
			continue
		}
		k := sensor + "\x00" + n
		st := e.integs[k]
		if st == nil {
			st = &integ{}
			e.integs[k] = st
		}
		if !st.init { // continue from the last stored total (survives restarts)
			st.total, _ = toFloat(last[n])
			st.init = true
		}
		if st.have {
			dt := float64(ts - st.lastTS)
			if dt <= 0 {
				continue // out of order or duplicate: ignore
			}
			if dt <= dur(s.MaxGap, 5*time.Minute) {
				st.total += (st.lastV + v) / 2 * dt / dur(s.Per, time.Hour)
			}
		}
		st.lastTS, st.lastV, st.have = ts, v, true
		out[n] = st.total
	}
	return out
}

// Forget drops integral state of a sensor (after its definition changes).
func (e *Engine) Forget(sensor string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.integs {
		if len(k) > len(sensor) && k[:len(sensor)] == sensor && k[len(sensor)] == 0 {
			delete(e.integs, k)
		}
	}
}
