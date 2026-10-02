package calc

import (
	"math"
	"strings"
	"testing"
)

func vars(m map[string]float64) func(string) (float64, bool) {
	return func(n string) (float64, bool) { v, ok := m[n]; return v, ok }
}

func TestParseEval(t *testing.T) {
	v := vars(map[string]float64{"voltage": 230, "current": 4, "a.b": 3, "running": 1})
	cases := map[string]float64{
		"voltage * current / 1000": 0.92,
		"1 + 2 * 3":                7,
		"(1 + 2) * 3":              9,
		"2 ^ 3 ^ 2":                512, // right-associative
		"-2 ^ 2":                   -4,  // unary minus binds looser than ^
		"10 % 4":                   2,
		"max(1, voltage, 3)":       230,
		"min(current, 2, 9)":       2,
		"round(2/3, 2)":            0.67,
		"clamp(voltage, 0, 100)":   100,
		"if(running, current, 0)":  4,
		"if(0, missing_field, 5)":  5, // lazy: untaken branch needs no input
		"abs(-3) + sqrt(16)":       7,
		"a.b * 2":                  6,
		"1.5e3 + 1E-3":             1500.001,
		"  voltage*current  ":      920,
	}
	for src, want := range cases {
		e, err := Parse(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		got, err := e.Eval(v)
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("%q = %v (%v), want %v", src, got, err, want)
		}
	}
	for _, bad := range []string{"", "1 +", "(1", "1)", "foo(1)", "max(1)", "2 ** 3", "1 $ 2", "os.exit(1)",
		strings.Repeat("(", 60) + "1" + strings.Repeat(")", 60), strings.Repeat("1+", 300) + "1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	for src, msg := range map[string]string{"1/0": "division", "sqrt(-1)": "negative", "missing * 2": "missing", "10^400": "finite"} {
		e, _ := Parse(src)
		if _, err := e.Eval(v); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: want error containing %q, got %v", src, msg, err)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := map[string]*Spec{"p": {Formula: "v*i"}, "e": {Integrate: "p", Per: "1h"}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]map[string]*Spec{
		"self":      {"p": {Formula: "p*2"}},
		"chain":     {"p": {Formula: "v*i"}, "q": {Formula: "p*2"}},
		"both":      {"p": {Formula: "1", Integrate: "v"}},
		"none":      {"p": {}},
		"int-int":   {"e": {Integrate: "v"}, "e2": {Integrate: "e"}},
		"bad dur":   {"e": {Integrate: "v", Per: "soon"}},
		"bad parse": {"p": {Formula: "v*"}},
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestIntegrals(t *testing.T) {
	specs := map[string]*Spec{"power_kw": {Formula: "voltage * current / 1000"}, "energy_kwh": {Integrate: "power_kw"}}
	e := NewEngine()
	var out map[string]any
	// 1 h at 230 V x 8.6957 A ~= 2 kW, one sample per minute.
	for i := 0; i <= 60; i++ {
		out = e.Apply("m", int64(i)*60_000, map[string]any{"voltage": 230.0, "current": 2000 / 230.0}, nil, specs)
	}
	if got := out["energy_kwh"].(float64); math.Abs(got-2) > 1e-9 {
		t.Fatalf("constant 2 kW for 1 h: %v kWh", got)
	}
	// Linear ramp 0 -> 60 kW over 1 h: exact for the trapezoid rule = 30 kWh.
	e2 := NewEngine()
	ramp := map[string]*Spec{"e": {Integrate: "kw"}}
	for i := 0; i <= 60; i++ {
		out = e2.Apply("r", int64(i)*60_000, map[string]any{"kw": float64(i)}, nil, ramp)
	}
	if got := out["e"].(float64); math.Abs(got-30) > 1e-9 {
		t.Fatalf("ramp: %v kWh, want 30", got)
	}
	// A gap longer than maxGap (default 5 m) adds nothing; out-of-order is ignored.
	e2.Apply("r", 60*60_000+10*60_000, map[string]any{"kw": 60.0}, nil, ramp)
	out = e2.Apply("r", 60*60_000+5_000, map[string]any{"kw": 1e6}, nil, ramp)
	if _, ok := out["e"]; ok {
		t.Fatal("out-of-order sample must be ignored")
	}
	out = e2.Apply("r", 60*60_000+11*60_000, map[string]any{"kw": 60.0}, nil, ramp)
	if got := out["e"].(float64); math.Abs(got-31) > 1e-9 {
		t.Fatalf("after gap: %v, want 30 + 1 minute at 60 kW = 31", got)
	}
	// After a restart the total continues from the stored value.
	e3 := NewEngine()
	slow := map[string]*Spec{"e": {Integrate: "kw", MaxGap: "15m"}} // a meter read every 10 minutes
	e3.Apply("r", 0, map[string]any{"kw": 6.0}, map[string]any{"e": 100.0}, slow)
	out = e3.Apply("r", 600_000, map[string]any{"kw": 6.0}, map[string]any{"e": 100.0}, slow)
	if got := out["e"].(float64); math.Abs(got-101) > 1e-9 {
		t.Fatalf("restart continuation: %v, want 101", got)
	}
}

func TestFormulaOnlyOnFreshInput(t *testing.T) {
	e := NewEngine()
	specs := map[string]*Spec{"p": {Formula: "v * i"}}
	out := e.Apply("m", 1, map[string]any{"temp": 20.0}, map[string]any{"v": 2.0, "i": 3.0}, specs)
	if _, ok := out["p"]; ok {
		t.Fatal("no input changed: formula must not repeat a stale value")
	}
	out = e.Apply("m", 2, map[string]any{"i": 4.0, "running": true}, map[string]any{"v": 2.0}, specs)
	if out["p"] != 8.0 {
		t.Fatalf("uses last known v with fresh i: %v", out)
	}
}
