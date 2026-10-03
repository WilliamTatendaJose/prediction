package calc

import "testing"

func TestConditions(t *testing.T) {
	vars := func(m map[string]float64) func(string) (float64, bool) {
		return func(k string) (float64, bool) { v, ok := m[k]; return v, ok }
	}
	v := vars(map[string]float64{"level": 95, "flow": 0, "temp": 21.5})
	for src, want := range map[string]float64{
		"level > 90":                              1,
		"level >= 95 and temp < 20":               0,
		"level > 90 AND temp < 25":                1,
		"level < 10 or temp = 21.5":               1,
		"not level > 90":                          0,
		"!(level > 90)":                           0,
		"level != 95":                             0,
		"level <> 90":                             1,
		"if(level > 90, 1, 0) + 1":                2,
		"flow > 0 and level / flow > 2":           0, // short-circuit: no division by zero
		"flow == 0 || level / flow > 2":           1,
		"if(flow > 0 && level/flow > 2, 5, 6)":    6,
		"level - 5 > 89 + 1":                      0,
		"(level > 90) + (temp > 20) + (flow > 0)": 2,
	} {
		e, err := Parse(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got, err := e.Eval(v); err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", src, got, err, want)
		}
	}
	for _, bad := range []string{"level >", "and level", "level > > 2", "level = = 1", "orange > 1 or"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	// "android" and "order" are field names, not keywords.
	e, err := Parse("android + order")
	if err != nil || len(e.Fields()) != 2 {
		t.Fatalf("keyword prefix: %v %v", err, e)
	}
}
