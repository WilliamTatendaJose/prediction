package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonvetter/modbus"
)

// fakePLC maps only some holding registers, like many real PLCs: a read
// that spans an unmapped address fails with IllegalDataAddress.
type fakePLC struct {
	mu       sync.Mutex
	hr       map[uint16]uint16
	coils    map[uint16]bool
	requests atomic.Int64
}

func (p *fakePLC) HandleHoldingRegisters(r *modbus.HoldingRegistersRequest) ([]uint16, error) {
	p.requests.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]uint16, r.Quantity)
	for i := range out {
		v, ok := p.hr[r.Addr+uint16(i)]
		if !ok {
			return nil, modbus.ErrIllegalDataAddress
		}
		out[i] = v
	}
	return out, nil
}

func (p *fakePLC) HandleCoils(r *modbus.CoilsRequest) ([]bool, error) {
	p.requests.Add(1)
	out := make([]bool, r.Quantity)
	for i := range out {
		out[i] = p.coils[r.Addr+uint16(i)]
	}
	return out, nil
}

func (p *fakePLC) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (p *fakePLC) HandleInputRegisters(*modbus.InputRegistersRequest) ([]uint16, error) {
	return nil, modbus.ErrIllegalFunction
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestModbusConnectorEndToEnd(t *testing.T) {
	addr := freePort(t)
	plc := &fakePLC{hr: map[uint16]uint16{0: 0xFF38, 1: 0x4088, 2: 0x0000, 9: 0b1000}, coils: map[uint16]bool{0: true}}
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{URL: "tcp://" + addr, MaxClients: 2}, plc)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	scale, bit := 0.1, 3
	cfg := ModbusConfig{Name: "t", URL: "tcp://" + addr, Interval: Duration(50 * time.Millisecond), Units: []ModbusUnit{{
		UnitID: 1, Sensor: "line", Registers: []Register{
			{Field: "temp", Table: "holding", Address: 0, Type: "int16", Scale: &scale},
			{Field: "press", Table: "holding", Address: 1, Type: "float32"},
			{Field: "fault", Table: "holding", Address: 9, Bit: &bit},
			{Field: "run", Table: "coil", Address: 0, OnChange: true},
		}}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []map[string]any
	defined := map[string]FieldInfo{}
	conn := newModbus(cfg, Target{
		Ingest: func(sensor string, ts int64, v map[string]any) {
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		},
		Define: func(sensor string, f map[string]FieldInfo) { defined = f },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { conn.Run(ctx); close(done) }()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 || len(defined) != 4 {
		t.Fatalf("polls %d, defined %v", len(got), defined)
	}
	first := got[0]
	if first["temp"] != -20.0 || first["press"] != 4.25 || first["fault"] != true || first["run"] != true {
		t.Fatalf("decoded %v", first)
	}
	// run is onChange and constant: sent once, then suppressed until heartbeat.
	for _, g := range got[1:] {
		if _, ok := g["run"]; ok {
			t.Fatalf("unchanged onChange field re-sent: %v", g)
		}
	}
	st := conn.Status()
	if !st.Connected || st.Skipped == 0 {
		t.Fatalf("status %+v", st)
	}
	// The gapped block 0-9 is rejected once, then read as single registers;
	// it must not be retried as a block every poll.
	polls := int64(len(got))
	if r := plc.requests.Load(); r > polls*4+3 {
		t.Fatalf("%d requests for %d polls: fallback not remembered", r, polls)
	}
}

func TestModbusConnectorRecovers(t *testing.T) {
	addr := freePort(t)
	cfg := ModbusConfig{Name: "t", URL: "tcp://" + addr, Interval: Duration(50 * time.Millisecond), Timeout: Duration(200 * time.Millisecond),
		Units: []ModbusUnit{{UnitID: 1, Sensor: "s", Registers: []Register{{Field: "a", Table: "holding", Address: 0}}}}}
	var n atomic.Int64
	conn := newModbus(cfg, Target{Ingest: func(string, int64, map[string]any) { n.Add(1) }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go conn.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	if conn.Status().Connected || conn.Status().Errors == 0 {
		t.Fatalf("PLC is down: %+v", conn.Status())
	}
	srv, _ := modbus.NewServer(&modbus.ServerConfiguration{URL: "tcp://" + addr, MaxClients: 2}, &fakePLC{hr: map[uint16]uint16{0: 7}})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	deadline := time.Now().Add(5 * time.Second) // first retry after 1-2 s of backoff
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n.Load() == 0 || !conn.Status().Connected {
		t.Fatalf("did not recover: %+v", conn.Status())
	}
}
