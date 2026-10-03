package ingest

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		field, in string
		want      map[string]any
		ts        int64
	}{
		{"", `{"t":1.5,"ts":1700000000}`, map[string]any{"t": 1.5}, 1700000000000},
		{"", `{"t":1,"timestamp":"2024-01-01T00:00:00Z"}`, map[string]any{"t": 1.0}, 1704067200000},
		{"", `21.5`, map[string]any{"value": 21.5}, 0},
		{"level", `42`, map[string]any{"level": 42.0}, 0},
		{"door", `open`, map[string]any{"door": "open"}, 0},
		{"on", `true`, map[string]any{"on": true}, 0},
		{"level", `{"value":3}`, map[string]any{"level": 3.0}, 0},
	}
	for _, c := range cases {
		got, ts, err := Parse(c.field, []byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if ts != c.ts || len(got) != len(c.want) {
			t.Fatalf("%s: got %v ts=%d", c.in, got, ts)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Fatalf("%s: %s=%v want %v", c.in, k, got[k], v)
			}
		}
	}
	if _, _, err := Parse("", []byte("{bad")); err == nil {
		t.Fatal("expected error")
	}
	if _, ts, _ := Parse("", []byte(`{"a":1,"ts":99999999999999}`)); ts != 0 {
		t.Fatal("future timestamp should be rejected")
	}
}
