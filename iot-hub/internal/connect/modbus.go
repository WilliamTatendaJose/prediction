package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
)

// ModbusConfig is one Modbus connection: a TCP device or gateway, or one
// RS-485 bus. Each unit on it (slave id) becomes one hub sensor.
type ModbusConfig struct {
	Name string `json:"name"`
	// URL: tcp://host:502, rtu:///dev/ttyUSB0, rtuovertcp://host:4001
	URL      string   `json:"url"`
	Interval Duration `json:"interval"` // poll period, default 1s
	Timeout  Duration `json:"timeout"`  // per request, default 2s
	// Heartbeat for report-by-exception fields, default 60s.
	Heartbeat Duration `json:"heartbeat"`
	// RTU serial settings (ignored for TCP).
	Baud     uint   `json:"baud"`
	DataBits uint   `json:"dataBits"`
	Parity   string `json:"parity"` // none | even | odd
	StopBits uint   `json:"stopBits"`
	// MaxGap: unmapped registers a block read may span to save a request.
	MaxGap int          `json:"maxGap"`
	Units  []ModbusUnit `json:"units"`
}

type ModbusUnit struct {
	UnitID    uint8      `json:"unitId"`
	Sensor    string     `json:"sensor"`
	Registers []Register `json:"registers"`
}

// Register maps one PLC value to a hub field. Addresses are 0-based protocol
// addresses (holding register 40001 is address 0).
type Register struct {
	Field   string `json:"field"`
	Table   string `json:"table"` // holding | input | coil | discrete
	Address uint16 `json:"address"`
	// Type: int16 uint16 int32 uint32 float32 int64 uint64 float64 (registers);
	// coils and discrete inputs are always bool.
	Type string `json:"type"`
	// Order of bytes across the value's words, A = most significant byte:
	// ABCD (big-endian, default), CDAB (word swap, common on Modicon and
	// many meters), BADC (byte swap), DCBA (little-endian).
	Order string `json:"order"`
	// Bit extracts one bit (0 = LSB) of a 16-bit register as a bool.
	Bit      *int     `json:"bit"`
	Scale    *float64 `json:"scale"`
	Offset   float64  `json:"offset"`
	Unit     string   `json:"unit"`
	Label    string   `json:"label"`
	OnChange bool     `json:"onChange"`
	Deadband float64  `json:"deadband"`
}

var fieldRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func words(typ string) int {
	switch typ {
	case "", "int16", "uint16":
		return 1
	case "int32", "uint32", "float32":
		return 2
	case "int64", "uint64", "float64":
		return 4
	}
	return 0
}

func (c *ModbusConfig) validate() error {
	if c.Name == "" || c.URL == "" {
		return fmt.Errorf("%w: name and url are required", errConfig)
	}
	if len(c.Units) == 0 {
		return fmt.Errorf("%w: no units", errConfig)
	}
	switch strings.ToLower(c.Parity) {
	case "", "none", "even", "odd":
	default:
		return fmt.Errorf("%w: parity must be none, even or odd", errConfig)
	}
	for _, u := range c.Units {
		if !fieldRe.MatchString(u.Sensor) {
			return fmt.Errorf("%w: unit %d: bad sensor id %q", errConfig, u.UnitID, u.Sensor)
		}
		seen := map[string]bool{}
		for _, r := range u.Registers {
			if !fieldRe.MatchString(r.Field) || seen[r.Field] {
				return fmt.Errorf("%w: %s: bad or duplicate field %q", errConfig, u.Sensor, r.Field)
			}
			seen[r.Field] = true
			switch r.Table {
			case "holding", "input":
				if words(r.Type) == 0 {
					return fmt.Errorf("%w: %s.%s: unknown type %q", errConfig, u.Sensor, r.Field, r.Type)
				}
				if r.Bit != nil && (*r.Bit < 0 || *r.Bit > 15 || words(r.Type) != 1) {
					return fmt.Errorf("%w: %s.%s: bit must be 0-15 on a 16-bit register", errConfig, u.Sensor, r.Field)
				}
			case "coil", "discrete":
			default:
				return fmt.Errorf("%w: %s.%s: table must be holding, input, coil or discrete", errConfig, u.Sensor, r.Field)
			}
			switch r.Order {
			case "", "ABCD", "CDAB", "BADC", "DCBA":
			default:
				return fmt.Errorf("%w: %s.%s: order must be ABCD, CDAB, BADC or DCBA", errConfig, u.Sensor, r.Field)
			}
		}
	}
	return nil
}

// block is one read request covering several registers.
type block struct {
	table      string
	start, end uint16 // [start, end) in registers or bits
	regs       []int  // indexes into unit.Registers
	split      bool   // the device rejected the span: read registers singly
}

const (
	maxRegsPerRead = 125 // protocol limit for function 3/4
	maxBitsPerRead = 2000
)

// planBlocks groups registers of the same table into the fewest reads.
func planBlocks(regs []Register, maxGap int) []*block {
	idx := make([]int, len(regs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		ra, rb := regs[idx[a]], regs[idx[b]]
		if ra.Table != rb.Table {
			return ra.Table < rb.Table
		}
		return ra.Address < rb.Address
	})
	var out []*block
	var cur *block
	for _, i := range idx {
		r := regs[i]
		n := uint16(1)
		limit := uint16(maxBitsPerRead)
		if r.Table == "holding" || r.Table == "input" {
			n, limit = uint16(words(r.Type)), maxRegsPerRead
		}
		end := r.Address + n
		if cur != nil && cur.table == r.Table && int(r.Address) <= int(cur.end)+maxGap && end-cur.start <= limit {
			cur.end = max(cur.end, end)
			cur.regs = append(cur.regs, i)
			continue
		}
		cur = &block{table: r.Table, start: r.Address, end: end, regs: []int{i}}
		out = append(out, cur)
	}
	return out
}

// decode turns a register's words into a number according to type/order.
func decode(r Register, w []uint16) float64 {
	b := make([]byte, 2*len(w))
	for i, x := range w {
		binary.BigEndian.PutUint16(b[2*i:], x)
	}
	switch r.Order {
	case "CDAB": // reverse word order
		for i, j := 0, len(w)-1; i < j; i, j = i+1, j-1 {
			b[2*i], b[2*i+1], b[2*j], b[2*j+1] = b[2*j], b[2*j+1], b[2*i], b[2*i+1]
		}
	case "BADC": // swap bytes within each word
		for i := 0; i < len(b); i += 2 {
			b[i], b[i+1] = b[i+1], b[i]
		}
	case "DCBA": // fully little-endian
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
	}
	switch r.Type {
	case "", "uint16":
		return float64(binary.BigEndian.Uint16(b))
	case "int16":
		return float64(int16(binary.BigEndian.Uint16(b)))
	case "uint32":
		return float64(binary.BigEndian.Uint32(b))
	case "int32":
		return float64(int32(binary.BigEndian.Uint32(b)))
	case "float32":
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case "uint64":
		return float64(binary.BigEndian.Uint64(b))
	case "int64":
		return float64(int64(binary.BigEndian.Uint64(b)))
	case "float64":
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return math.NaN()
}

func (r Register) value(w []uint16) any {
	if r.Bit != nil {
		return w[0]&(1<<uint(*r.Bit)) != 0
	}
	v := decode(r, w)
	if r.Scale != nil {
		v *= *r.Scale
	}
	v += r.Offset
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil // a NaN from the PLC is "no reading", not a value
	}
	return v
}

type modbusConn struct {
	cfg    ModbusConfig
	t      Target
	st     status
	plans  [][]*block // per unit
	filter []*filter  // per unit
	mu     sync.Mutex // serialises use of client (one bus, one request at a time)
	client *modbus.ModbusClient
	down   bool // in an outage (for log de-duplication)
}

func newModbus(cfg ModbusConfig, t Target) *modbusConn {
	m := &modbusConn{cfg: cfg, t: t}
	m.st.s = Status{Name: cfg.Name, Kind: "modbus", Endpoint: cfg.URL}
	if m.cfg.MaxGap == 0 {
		m.cfg.MaxGap = 8
	}
	for _, u := range cfg.Units {
		m.plans = append(m.plans, planBlocks(u.Registers, m.cfg.MaxGap))
		m.filter = append(m.filter, newFilter(cfg.Heartbeat.or(time.Minute)))
	}
	return m
}

func (m *modbusConn) Status() Status { return m.st.get() }

func (m *modbusConn) open() error {
	parity := modbus.PARITY_NONE
	switch strings.ToLower(m.cfg.Parity) {
	case "even":
		parity = modbus.PARITY_EVEN
	case "odd":
		parity = modbus.PARITY_ODD
	}
	c, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL: m.cfg.URL, Speed: m.cfg.Baud, DataBits: m.cfg.DataBits, Parity: parity,
		StopBits: m.cfg.StopBits, Timeout: m.cfg.Timeout.or(2 * time.Second),
	})
	if err != nil {
		return err
	}
	if err := c.Open(); err != nil {
		return err
	}
	m.client = c
	return nil
}

func (m *modbusConn) close() {
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
}

func (m *modbusConn) Run(ctx context.Context) {
	if m.t.Define != nil {
		for _, u := range m.cfg.Units {
			f := map[string]FieldInfo{}
			for _, r := range u.Registers {
				f[r.Field] = FieldInfo{Unit: r.Unit, Label: r.Label}
			}
			m.t.Define(u.Sensor, f)
		}
	}
	interval := m.cfg.Interval.or(time.Second)
	var bo backoff
	tick := time.NewTicker(interval)
	defer tick.Stop()
	defer m.close()
	for {
		if m.client == nil {
			if err := m.open(); err != nil {
				m.st.fail(err)
				m.logOnce(err)
				if !sleep(ctx, bo.next()) {
					return
				}
				continue
			}
		}
		if err := m.pollAll(); err != nil {
			m.st.fail(err)
			m.logOnce(err)
			m.close() // reconnect: a half-dead TCP session is the common failure
			if !sleep(ctx, bo.next()) {
				return
			}
			continue
		}
		bo.reset()
		if m.down && m.t.Logf != nil {
			m.t.Logf("modbus %s: reading again", m.cfg.Name)
		}
		m.down = false
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// logOnce logs the first failure of an outage only; recovery is logged in Run.
func (m *modbusConn) logOnce(err error) {
	if !m.down && m.t.Logf != nil {
		m.t.Logf("modbus %s: %v (retrying with backoff)", m.cfg.Name, err)
	}
	m.down = true
}

func (m *modbusConn) pollAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ui, u := range m.cfg.Units {
		if err := m.client.SetUnitId(u.UnitID); err != nil {
			return err
		}
		now := time.Now()
		values := map[string]any{}
		reads := 0
		for _, b := range m.plans[ui] {
			n, err := m.readBlock(u, b, values)
			reads += n
			if err != nil {
				return fmt.Errorf("unit %d (%s): %w", u.UnitID, u.Sensor, err)
			}
		}
		m.st.ok(reads)
		rules := map[string]exception{}
		for _, r := range u.Registers {
			rules[r.Field] = exception{on: r.OnChange || r.Deadband > 0, deadband: r.Deadband}
		}
		out, skipped := m.filter[ui].apply(now, values, func(f string) exception { return rules[f] })
		m.st.counted(len(out), skipped)
		if len(out) > 0 {
			m.t.Ingest(u.Sensor, now.UnixMilli(), out)
		}
	}
	return nil
}

// readBlock reads one block (or its registers singly once the device has
// refused the span) and decodes values into out. Returns requests made.
func (m *modbusConn) readBlock(u ModbusUnit, b *block, out map[string]any) (int, error) {
	if !b.split {
		err := m.readSpan(u, b.table, b.start, b.end, b.regs, out)
		if err == nil {
			return 1, nil
		}
		// Gaps in a span may be unmapped on the device; fall back to one
		// request per register for this block from now on.
		if !errors.Is(err, modbus.ErrIllegalDataAddress) || len(b.regs) == 1 {
			return 1, err
		}
		b.split = true
		if m.t.Logf != nil {
			m.t.Logf("modbus %s: unit %d rejected %s %d-%d as one read; reading registers singly", m.cfg.Name, u.UnitID, b.table, b.start, b.end-1)
		}
	}
	n := 0
	for _, i := range b.regs {
		r := u.Registers[i]
		end := r.Address + 1
		if b.table == "holding" || b.table == "input" {
			end = r.Address + uint16(words(r.Type))
		}
		n++
		if err := m.readSpan(u, b.table, r.Address, end, []int{i}, out); err != nil {
			return n, fmt.Errorf("%s %s@%d: %w", r.Field, b.table, r.Address, err)
		}
	}
	return n, nil
}

func (m *modbusConn) readSpan(u ModbusUnit, table string, start, end uint16, regs []int, out map[string]any) error {
	qty := end - start
	switch table {
	case "coil", "discrete":
		var bits []bool
		var err error
		if table == "coil" {
			bits, err = m.client.ReadCoils(start, qty)
		} else {
			bits, err = m.client.ReadDiscreteInputs(start, qty)
		}
		if err != nil {
			return err
		}
		for _, i := range regs {
			r := u.Registers[i]
			out[r.Field] = bits[r.Address-start]
		}
	default:
		rt := modbus.HOLDING_REGISTER
		if table == "input" {
			rt = modbus.INPUT_REGISTER
		}
		w, err := m.client.ReadRegisters(start, qty, rt)
		if err != nil {
			return err
		}
		for _, i := range regs {
			r := u.Registers[i]
			off := r.Address - start
			if v := r.value(w[off : off+uint16(words(r.Type))]); v != nil {
				out[r.Field] = v
			}
		}
	}
	return nil
}
