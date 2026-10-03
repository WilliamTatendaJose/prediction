package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

func pct(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *p*100)
}

func hm(sec float64) string {
	m := int(math.Round(sec / 60))
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", m/60, m%60)
}

func num(f float64) string {
	switch a := math.Abs(f); {
	case f == math.Trunc(f) && a < 1e15:
		return fmt.Sprintf("%.0f", f) // counts
	case a >= 100:
		return fmt.Sprintf("%.0f", f)
	case a >= 10:
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%.2f", f)
}

// band names the OEE level in words (never colour alone).
func band(p *float64) string {
	switch {
	case p == nil:
		return "no data"
	case *p >= 0.85:
		return "world class"
	case *p >= 0.6:
		return "typical"
	}
	return "below 60 %"
}

var verdictWords = map[string]string{"confirmed": "confirmed", "false_alarm": "false alarm", "expected": "expected", "acknowledged": "acknowledged"}

// Text is the plain-text report: the email's text part and the chat summary.
func (r Report) Text(loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", r.Title, r.Period)
	if len(r.Machines) > 0 {
		b.WriteString("\nMachines (OEE = availability × performance × quality)\n")
		for _, m := range r.Machines {
			x := m.Result
			fmt.Fprintf(&b, "• %s: OEE %s (%s) — A %s, P %s, Q %s; %s parts, %s rejected; ran %s of %s\n",
				m.Name, pct(x.OEE), band(x.OEE), pct(x.Availability), pct(x.Performance), pct(x.Quality),
				num(x.Total), num(x.Reject), hm(x.RunSec), hm(x.PlannedSec))
			for _, w := range x.Warnings {
				fmt.Fprintf(&b, "  ⚠ %s\n", w)
			}
		}
	}
	if len(r.Energy) > 0 {
		b.WriteString("\nEnergy\n")
		for _, e := range r.Energy {
			fmt.Fprintf(&b, "• %s · %s: %s %s\n", e.Name, e.Field, num(e.Consumed), e.Unit)
		}
	}
	a := r.Alarms
	fmt.Fprintf(&b, "\nAlarms: %d (%d unacknowledged, %d shelved)", a.Total, a.Unacked, a.Shelved)
	if a.Total > 0 {
		var parts []string
		for _, k := range []string{"range", "spike", "stale"} {
			if a.ByKind[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", a.ByKind[k], k))
			}
		}
		fmt.Fprintf(&b, " — %s", strings.Join(parts, ", "))
	}
	b.WriteString("\n")
	for i, x := range a.Longest {
		if i == 5 {
			break
		}
		state := verdictWords[x.Verdict]
		if state == "" {
			state = "not acknowledged"
		}
		where := x.Sensor
		if x.Field != "" {
			where += "." + x.Field
		}
		fmt.Fprintf(&b, "• %s %s, %s, %s: %s\n", time.UnixMilli(x.Start).In(loc).Format("15:04"), where, hm(x.DurationSec), state, x.Message)
	}
	if len(r.Silent) > 0 {
		fmt.Fprintf(&b, "\nSensors that went silent: %s\n", strings.Join(r.Silent, ", "))
	}
	if r.Link != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Link)
	}
	return b.String()
}

// html/template escapes every value: sensor names and alarm messages come
// from devices and must not inject markup into an email.
var page = template.Must(template.New("r").Funcs(template.FuncMap{
	"pct": pct, "hm": hm, "num": num, "band": band,
	"clock": func(ms int64, loc *time.Location) string { return time.UnixMilli(ms).In(loc).Format("15:04") },
	"verdict": func(v string) string {
		if w := verdictWords[v]; w != "" {
			return w
		}
		return "not acknowledged"
	},
	"color": func(p *float64) string {
		switch {
		case p == nil:
			return "#898781"
		case *p >= 0.85:
			return "#0ca30c"
		case *p >= 0.6:
			return "#b07800"
		}
		return "#d03b3b"
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.R.Title}} — {{.R.Period}}</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:#0b0b0b;background:#fff;margin:0;padding:24px;max-width:860px}
h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px;border-bottom:1px solid #e1e0d9;padding-bottom:4px}
.sub{color:#52514e;font-size:13px}table{border-collapse:collapse;width:100%;font-size:13px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #eeede8;vertical-align:top}th{color:#52514e;font-weight:600}
td.n{text-align:right;font-variant-numeric:tabular-nums}.big{font-size:18px;font-weight:600}.warn{color:#52514e;font-size:12px}
@media print{body{padding:0}a{color:inherit}}
</style></head><body>
<h1>{{.R.Title}}</h1>
<div class="sub">{{.R.Period}}</div>
{{if .R.Machines}}<h2>Machines</h2>
<table><tr><th>Machine</th><th>OEE</th><th>Availability</th><th>Performance</th><th>Quality</th><th>Parts</th><th>Rejected</th><th>Ran</th></tr>
{{range .R.Machines}}{{$x := .Result}}<tr><td>{{.Name}}</td>
<td><span class="big" style="color:{{color $x.OEE}}">{{pct $x.OEE}}</span><br><span class="sub">{{band $x.OEE}}</span></td>
<td class="n">{{pct $x.Availability}}</td><td class="n">{{pct $x.Performance}}</td><td class="n">{{pct $x.Quality}}</td>
<td class="n">{{num $x.Total}}</td><td class="n">{{num $x.Reject}}</td><td class="n">{{hm $x.RunSec}} / {{hm $x.PlannedSec}}</td></tr>
{{range $x.Warnings}}<tr><td></td><td colspan="7" class="warn">⚠ {{.}}</td></tr>{{end}}{{end}}
</table>
<p class="sub">OEE = availability × performance × quality. 85 % is world class; 60 % is typical.</p>{{end}}
{{if .R.Energy}}<h2>Energy</h2>
<table><tr><th>Meter</th><th>Field</th><th>Used</th></tr>
{{range .R.Energy}}<tr><td>{{.Name}}</td><td>{{.Field}}</td><td class="n">{{num .Consumed}} {{.Unit}}</td></tr>{{end}}</table>{{end}}
<h2>Alarms</h2>
<p>{{.R.Alarms.Total}} alarms · {{.R.Alarms.Unacked}} not acknowledged · {{.R.Alarms.Shelved}} shelved</p>
{{if .R.Alarms.Longest}}<table><tr><th>Start</th><th>Sensor</th><th>Kind</th><th>Duration</th><th>Status</th><th>Message</th></tr>
{{range .R.Alarms.Longest}}<tr><td>{{clock .Start $.Loc}}</td><td>{{.Sensor}}{{if .Field}}.{{.Field}}{{end}}</td><td>{{.Kind}}</td>
<td class="n">{{hm .DurationSec}}{{if .Open}} (still active){{end}}</td><td>{{verdict .Verdict}}{{if .AckedBy}} ({{.AckedBy}}){{end}}</td><td>{{.Message}}</td></tr>{{end}}</table>{{end}}
{{if .R.Silent}}<h2>Sensors that went silent</h2><p>{{range $i, $s := .R.Silent}}{{if $i}}, {{end}}{{$s}}{{end}}</p>{{end}}
{{if .R.Link}}<p><a href="{{.R.Link}}">Open the dashboard</a></p>{{end}}
</body></html>`))

func (r Report) HTML(loc *time.Location) string {
	var b bytes.Buffer
	_ = page.Execute(&b, struct {
		R   Report
		Loc *time.Location
	}{r, loc})
	return b.String()
}
