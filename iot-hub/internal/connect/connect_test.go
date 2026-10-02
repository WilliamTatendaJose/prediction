package connect

import (
	"math"
	"os"
	"testing"
	"time"
)

func TestDecodeOrders(t *testing.T) {
	// 123.456 as float32 = 0x42F6E979
	cases := []struct {
		order string
		w     []uint16
	}{
		{"ABCD", []uint16{0x42F6, 0xE979}}, {"", []uint16{0x42F6, 0xE979}},
		{"CDAB", []uint16{0xE979, 0x42F6}}, {"BADC", []uint16{0xF642, 0x79E9}}, {"DCBA", []uint16{0x79E9, 0xF642}},
	}
	for _, c := range cases {
		got := decode(Register{Type: "float32", Order: c.order}, c.w)
		if math.Abs(got-123.456) > 1e-4 {
			t.Errorf("float32 %s: %v", c.order, got)
		}
	}
	if v := decode(Register{Type: "int32"}, []uint16{0xFFFF, 0xFFFE}); v != -2 {
		t.Errorf("int32: %v", v)
	}
	if v := decode(Register{Type: "int16"}, []uint16{0xFF38}); v != -200 {
		t.Errorf("int16: %v", v)
	}
	if v := decode(Register{Type: "uint16", Order: "BADC"}, []uint16{0x3412}); v != 0x1234 {
		t.Errorf("uint16 BADC: %v", v)
	}
	// 1.5 as float64 = 0x3FF8000000000000, CDAB reverses the 4 words
	if v := decode(Register{Type: "float64", Order: "CDAB"}, []uint16{0, 0, 0, 0x3FF8}); v != 1.5 {
		t.Errorf("float64 CDAB: %v", v)
	}
	if v := decode(Register{Type: "uint32", Order: "CDAB"}, []uint16{0x0001, 0x0002}); v != 0x00020001 {
		t.Errorf("uint32 CDAB: %v", v)
	}
	sc := 0.1
	if v := (Register{Type: "int16", Scale: &sc, Offset: -40}).value([]uint16{650}); math.Abs(v.(float64)-25) > 1e-9 {
		t.Errorf("scale/offset: %v", v)
	}
	bit := 3
	if v := (Register{Bit: &bit}).value([]uint16{0b1000}); v != true {
		t.Errorf("bit: %v", v)
	}
	if v := (Register{Type: "float32"}).value([]uint16{0x7FC0, 0}); v != nil {
		t.Errorf("NaN must be dropped: %v", v)
	}
}

func TestPlanBlocks(t *testing.T) {
	regs := []Register{
		{Field: "a", Table: "holding", Address: 0, Type: "int16"},
		{Field: "b", Table: "holding", Address: 1, Type: "float32"}, // 1-2
		{Field: "c", Table: "holding", Address: 8, Type: "int16"},   // gap 5: merged
		{Field: "d", Table: "holding", Address: 40, Type: "int16"},  // gap too big
		{Field: "e", Table: "input", Address: 1, Type: "int16"},
		{Field: "f", Table: "coil", Address: 0},
		{Field: "g", Table: "coil", Address: 5},
		{Field: "h", Table: "holding", Address: 160, Type: "int16"},
		{Field: "i", Table: "holding", Address: 41, Type: "float64"},
	}
	bs := planBlocks(regs, 8)
	type span struct {
		table      string
		start, end uint16
		n          int
	}
	var got []span
	for _, b := range bs {
		got = append(got, span{b.table, b.start, b.end, len(b.regs)})
	}
	want := []span{{"coil", 0, 6, 2}, {"holding", 0, 9, 3}, {"holding", 40, 45, 2}, {"holding", 160, 161, 1}, {"input", 1, 2, 1}}
	if len(got) != len(want) {
		t.Fatalf("blocks %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	// The 125-register limit splits long runs.
	var long []Register
	for i := 0; i < 130; i++ {
		long = append(long, Register{Field: "x", Table: "holding", Address: uint16(i), Type: "uint16"})
	}
	if bs := planBlocks(long, 0); len(bs) != 2 || bs[0].end-bs[0].start != 125 {
		t.Fatalf("limit: %d blocks, first %d", len(bs), bs[0].end-bs[0].start)
	}
}

func TestFilter(t *testing.T) {
	f := newFilter(time.Minute)
	t0 := time.Unix(1000, 0)
	rule := func(field string) exception {
		switch field {
		case "slow":
			return exception{on: true, deadband: 0.5}
		case "state":
			return exception{on: true}
		}
		return exception{}
	}
	out, _ := f.apply(t0, map[string]any{"fast": 1.0, "slow": 10.0, "state": "Run"}, rule)
	if len(out) != 3 {
		t.Fatalf("first sample sends all: %v", out)
	}
	out, sk := f.apply(t0.Add(time.Second), map[string]any{"fast": 1.0, "slow": 10.3, "state": "Run"}, rule)
	if len(out) != 1 || out["fast"] != 1.0 || sk != 2 {
		t.Fatalf("deadband/onChange suppress; plain field always sent: %v skipped %d", out, sk)
	}
	out, _ = f.apply(t0.Add(2*time.Second), map[string]any{"slow": 10.6, "state": "Stop"}, rule)
	if len(out) != 2 {
		t.Fatalf("changes beyond deadband are sent: %v", out)
	}
	out, _ = f.apply(t0.Add(2*time.Minute), map[string]any{"slow": 10.6, "state": "Stop"}, rule)
	if len(out) != 2 {
		t.Fatalf("heartbeat re-sends unchanged values: %v", out)
	}
}

func TestLoadValidation(t *testing.T) {
	bad := []string{
		`{"modbus":[{"name":"x","url":"tcp://h:502","units":[{"unitId":1,"sensor":"s","registers":[{"field":"a","table":"holding","address":0,"type":"int17"}]}]}]}`,
		`{"modbus":[{"name":"x","url":"tcp://h:502","units":[{"unitId":1,"sensor":"s","registers":[{"field":"a","table":"holding","address":0,"type":"int32","bit":3}]}]}]}`,
		`{"modbus":[{"name":"x","url":"tcp://h:502","units":[{"unitId":1,"sensor":"s","registers":[{"field":"a","tabel":"holding"}]}]}]}`,
		`{"opcua":[{"name":"x","endpoint":"opc.tcp://h:4840","sensor":"s","nodes":[{"field":"a","nodeId":"ns=x;i=1"}]}]}`,
		`{"opcua":[{"name":"x","endpoint":"opc.tcp://h:4840","sensor":"s","nodes":[{"field":"a"}]}]}`,
		`{"opcua":[{"name":"x","endpoint":"opc.tcp://h:4840","sensor":"s","securityPolicy":"Basic128","nodes":[{"field":"a","nodeId":"ns=2;i=1"}]}]}`,
	}
	for i, b := range bad {
		p := t.TempDir() + "/c.json"
		writeFile(t, p, b)
		if _, err := Load(p); err == nil {
			t.Errorf("config %d should be rejected", i)
		}
	}
	p := t.TempDir() + "/ok.json"
	writeFile(t, p, `{"modbus":[{"name":"x","url":"tcp://h:502","interval":"500ms","units":[{"unitId":1,"sensor":"s","registers":[{"field":"a","table":"input","address":3,"type":"float32","order":"CDAB"}]}]}]}`)
	c, err := Load(p)
	if err != nil || time.Duration(c.Modbus[0].Interval) != 500*time.Millisecond {
		t.Fatalf("valid config: %v %+v", err, c)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
