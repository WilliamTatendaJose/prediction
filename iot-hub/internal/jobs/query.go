// Package jobs runs stream jobs: windowed aggregates over live readings,
// in the style of Azure Stream Analytics. A job is a small SQL-like query:
//
//	SELECT avg(level) AS level_avg
//	INTO [{sensor}-5m]
//	FROM [tank-*]
//	WHERE value >= 0
//	GROUP BY sensor, TumblingWindow(minute, 5)
//	HAVING level_avg > 80
//
// Windows: TumblingWindow(unit, n), HoppingWindow(unit, size, hop),
// SlidingWindow(unit, n). Aggregates: avg, min, max, sum, count, stddev,
// first, last, delta (last - first), increase (sum of rises: counters and
// meter totals, robust to resets). Outputs (INTO): a derived sensor
// ([name] or [{sensor}-suffix], optionally .field), alert (opens and clears
// an alarm per group while HAVING holds), or webhook:<target>.
//
// Windows close on event time: when readings show time has passed the
// window end plus the lateness allowance, or, when readings stop, once the
// wall clock has. Readings later than that are dropped and counted.
package jobs

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/williamtatendajose/prediction/iot-hub/internal/calc"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

// Spec is a stored job.
type Spec struct {
	ID          string `json:"id"`
	Query       string `json:"query"`
	Enabled     bool   `json:"enabled"`
	Lateness    string `json:"lateness,omitempty"` // default 5s
	Description string `json:"description,omitempty"`
}

type Window struct {
	Kind      string // tumbling | hopping | sliding
	Size, Hop time.Duration
}

type Output struct {
	Kind   string // sensor | alert | webhook
	Sensor string // template; may contain {sensor}
	Field  string
	Target string // webhook target id
}

// Plan is a compiled query.
type Plan struct {
	Agg, Field, Alias string
	Sensors           string // glob
	Where             *calc.Expr
	BySensor          bool
	Window            Window
	Having            *calc.Expr
	Out               Output
	Lateness          time.Duration
}

var (
	idRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	globRe   = regexp.MustCompile(`^[A-Za-z0-9_.*?\[\]-]{1,64}$`)
	sensorRe = regexp.MustCompile(`^[A-Za-z0-9_.{}-]{1,64}$`)
	aggs     = map[string]bool{"avg": true, "min": true, "max": true, "sum": true, "count": true, "stddev": true,
		"first": true, "last": true, "delta": true, "increase": true}
	slidingAggs = map[string]bool{"avg": true, "min": true, "max": true, "sum": true, "count": true, "stddev": true}
)

// MaxWindowsPerEvent bounds hopping windows (size / hop).
const MaxWindowsPerEvent = 60

func errf(f string, a ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(f, a...))
}

// clauses splits a query at its top-level keywords (outside brackets and
// parentheses), case-insensitively.
func clauses(q string) (map[string]string, error) {
	q = strings.Join(strings.Fields(q), " ") // "GROUP   BY", newlines
	keys := []string{"SELECT", "INTO", "FROM", "WHERE", "GROUP BY", "HAVING"}
	type hit struct {
		key string
		at  int
	}
	var hits []hit
	depth := 0
	up := strings.ToUpper(q)
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '(', '[':
			depth++
			continue
		case ')', ']':
			depth--
			continue
		}
		if depth != 0 || i > 0 && !unicode.IsSpace(rune(q[i-1])) {
			continue
		}
		for _, k := range keys {
			end := i + len(k)
			if strings.HasPrefix(up[i:], k) && (end == len(q) || unicode.IsSpace(rune(q[end]))) {
				hits = append(hits, hit{k, i})
				i = end - 1
				break
			}
		}
	}
	if depth != 0 {
		return nil, errf("unbalanced brackets or parentheses")
	}
	out := map[string]string{}
	for j, h := range hits {
		end := len(q)
		if j+1 < len(hits) {
			end = hits[j+1].at
		}
		if _, dup := out[h.key]; dup {
			return nil, errf("%s appears twice", h.key)
		}
		out[h.key] = strings.TrimSpace(q[h.at+len(h.key) : end])
	}
	if len(hits) == 0 || hits[0].key != "SELECT" || hits[0].at != 0 {
		return nil, errf("a query starts with SELECT")
	}
	return out, nil
}

func unbracket(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

var units = map[string]time.Duration{"second": time.Second, "seconds": time.Second, "ss": time.Second, "s": time.Second,
	"minute": time.Minute, "minutes": time.Minute, "mi": time.Minute, "n": time.Minute,
	"hour": time.Hour, "hours": time.Hour, "hh": time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour, "dd": 24 * time.Hour}

func parseWindow(s string) (Window, error) {
	name, args, ok := strings.Cut(s, "(")
	if !ok || !strings.HasSuffix(strings.TrimSpace(args), ")") {
		return Window{}, errf("window %q: want TumblingWindow(minute, 5), HoppingWindow(minute, 10, 5) or SlidingWindow(minute, 5)", s)
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(args), ")"), ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	unit, ok := units[strings.ToLower(parts[0])]
	if !ok {
		return Window{}, errf("window unit %q: second, minute, hour or day", parts[0])
	}
	nums := make([]time.Duration, 0, 2)
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return Window{}, errf("window length %q must be a positive whole number", p)
		}
		nums = append(nums, time.Duration(n)*unit)
	}
	var w Window
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tumblingwindow", "tumbling":
		if len(nums) != 1 {
			return w, errf("TumblingWindow(unit, size)")
		}
		w = Window{Kind: "tumbling", Size: nums[0], Hop: nums[0]}
	case "hoppingwindow", "hopping":
		if len(nums) != 2 {
			return w, errf("HoppingWindow(unit, size, hop)")
		}
		w = Window{Kind: "hopping", Size: nums[0], Hop: nums[1]}
		if w.Hop > w.Size || w.Size%w.Hop != 0 || w.Size/w.Hop > MaxWindowsPerEvent {
			return w, errf("hop must divide size, with at most %d windows per reading", MaxWindowsPerEvent)
		}
	case "slidingwindow", "sliding":
		if len(nums) != 1 {
			return w, errf("SlidingWindow(unit, size)")
		}
		w = Window{Kind: "sliding", Size: nums[0]}
		if w.Size > 24*time.Hour {
			return w, errf("a sliding window is at most a day")
		}
	default:
		return w, errf("unknown window %q", name)
	}
	if w.Size < time.Second || w.Size > 7*24*time.Hour {
		return w, errf("window size must be between 1 second and 7 days")
	}
	return w, nil
}

// Compile parses and checks a job.
func Compile(sp Spec) (*Plan, error) {
	if !idRe.MatchString(sp.ID) {
		return nil, errf("job id must be 1-40 of a-z, 0-9 and '-'")
	}
	if len(sp.Query) > 2000 {
		return nil, errf("query longer than 2000 characters")
	}
	c, err := clauses(sp.Query)
	if err != nil {
		return nil, err
	}
	p := &Plan{Lateness: 5 * time.Second}
	if sp.Lateness != "" {
		d, err := time.ParseDuration(sp.Lateness)
		if err != nil || d < 0 || d > time.Hour {
			return nil, errf("lateness must be a duration up to 1h")
		}
		p.Lateness = d
	}

	// SELECT agg(field) [AS alias]
	sel := c["SELECT"]
	expr, alias := sel, ""
	if i := strings.LastIndex(strings.ToUpper(sel), " AS "); i >= 0 {
		expr, alias = strings.TrimSpace(sel[:i]), strings.TrimSpace(sel[i+4:])
		if !identRe.MatchString(alias) {
			return nil, errf("alias %q: letters, digits and _", alias)
		}
	}
	fn, arg, ok := strings.Cut(expr, "(")
	if !ok || !strings.HasSuffix(arg, ")") {
		return nil, errf("SELECT one aggregate, e.g. SELECT avg(level) AS level_avg")
	}
	p.Agg, p.Field = strings.ToLower(strings.TrimSpace(fn)), strings.TrimSpace(strings.TrimSuffix(arg, ")"))
	if !aggs[p.Agg] {
		return nil, errf("aggregate %q: avg, min, max, sum, count, stddev, first, last, delta or increase", fn)
	}
	if !store.ValidID(p.Field) {
		return nil, errf("field %q", p.Field)
	}
	p.Alias = alias
	if p.Alias == "" {
		p.Alias = p.Agg + "_" + p.Field
	}

	// FROM sensors
	from, ok := c["FROM"]
	if !ok {
		return nil, errf("FROM [sensors] is required")
	}
	p.Sensors = unbracket(from)
	if !globRe.MatchString(p.Sensors) {
		return nil, errf("FROM %q: a sensor id or glob like tank-*", p.Sensors)
	}
	if _, err := path.Match(p.Sensors, ""); err != nil {
		return nil, errf("FROM %q: %v", p.Sensors, err)
	}

	// WHERE over the reading: value, or the field by name
	if w, ok := c["WHERE"]; ok {
		if p.Where, err = calc.Parse(w); err != nil {
			return nil, errf("WHERE: %v", err)
		}
		for _, v := range p.Where.Fields() {
			if v != "value" && v != p.Field {
				return nil, errf("WHERE can use value or %s, not %s", p.Field, v)
			}
		}
	}

	// GROUP BY [sensor,] window
	g, ok := c["GROUP BY"]
	if !ok {
		return nil, errf("GROUP BY with a window is required, e.g. GROUP BY TumblingWindow(minute, 5)")
	}
	if first, rest, ok := strings.Cut(g, ","); ok && !strings.Contains(first, "(") {
		if !strings.EqualFold(strings.TrimSpace(first), "sensor") {
			return nil, errf("GROUP BY can group by sensor only, not %q", strings.TrimSpace(first))
		}
		p.BySensor, g = true, rest
	}
	if p.Window, err = parseWindow(strings.TrimSpace(g)); err != nil {
		return nil, err
	}
	if p.Window.Kind == "sliding" && !slidingAggs[p.Agg] {
		return nil, errf("sliding windows support avg, min, max, sum, count and stddev")
	}

	// HAVING over the result: value, count, the alias, or the aggregate
	// written out (AVG(level) > 80, as in Stream Analytics).
	if h, ok := c["HAVING"]; ok {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(p.Agg) + `\s*\(\s*` + regexp.QuoteMeta(p.Field) + `\s*\)`)
		h = re.ReplaceAllString(h, "value")
		if p.Having, err = calc.Parse(h); err != nil {
			return nil, errf("HAVING: %v", err)
		}
		for _, v := range p.Having.Fields() {
			if v != "value" && v != "count" && v != p.Alias {
				return nil, errf("HAVING can use value, count or %s, not %s", p.Alias, v)
			}
		}
	}

	// INTO
	into, ok := c["INTO"]
	if !ok {
		return nil, errf("INTO is required: [sensor], alert or webhook:<target>")
	}
	into = unbracket(into)
	switch {
	case strings.EqualFold(into, "alert"):
		p.Out = Output{Kind: "alert"}
		if p.Having == nil {
			return nil, errf("INTO alert needs a HAVING condition (when to raise it)")
		}
	case strings.HasPrefix(strings.ToLower(into), "webhook:"):
		p.Out = Output{Kind: "webhook", Target: into[len("webhook:"):]}
		if p.Out.Target == "" {
			return nil, errf("INTO webhook:<target id>")
		}
	default:
		sensor, field, _ := strings.Cut(into, ".")
		if field == "" {
			field = p.Alias
		}
		if !sensorRe.MatchString(sensor) || !store.ValidID(field) {
			return nil, errf("INTO %q: a sensor id (may contain {sensor}) and optional .field", into)
		}
		if strings.Contains(sensor, "{sensor}") && !p.BySensor {
			return nil, errf("INTO uses {sensor}: add GROUP BY sensor")
		}
		if strings.ContainsAny(strings.ReplaceAll(sensor, "{sensor}", ""), "{}") {
			return nil, errf("INTO %q: only {sensor} can be substituted", into)
		}
		// A templated output can feed the job too: "tank-*" with
		// "{sensor}-1m" writes tank-1-1m, which tank-* matches. Try it with
		// a sample input name.
		if strings.Contains(sensor, "{sensor}") && !strings.Contains(p.Sensors, "[") {
			sample := strings.NewReplacer("*", "x", "?", "x").Replace(p.Sensors)
			if ok, _ := path.Match(p.Sensors, strings.ReplaceAll(sensor, "{sensor}", sample)); ok {
				return nil, errf("INTO %s would match FROM %s and feed the job itself; put the suffix first, e.g. INTO [avg-{sensor}]", sensor, p.Sensors)
			}
		}
		if !strings.Contains(sensor, "{sensor}") {
			if p.BySensor {
				return nil, errf("grouped by sensor: name the output with {sensor}, e.g. INTO [{sensor}-5m]")
			}
			if ok, _ := path.Match(p.Sensors, sensor); ok {
				return nil, errf("INTO %s is also an input of this job (it would feed itself)", sensor)
			}
		}
		p.Out = Output{Kind: "sensor", Sensor: sensor, Field: field}
	}
	return p, nil
}

// OutputSensor resolves the output sensor for a group.
func (p *Plan) OutputSensor(group string) string {
	return strings.ReplaceAll(p.Out.Sensor, "{sensor}", group)
}
