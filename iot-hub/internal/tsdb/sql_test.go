package tsdb

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/williamtatendajose/prediction/iot-hub/internal/anomaly"
)

// Postgres runs when IOTHUB_TEST_PG is set, e.g.
// IOTHUB_TEST_PG=postgres://postgres@localhost:55432/iothub_test
func backends(t *testing.T) map[string]DB {
	ctx := context.Background()
	out := map[string]DB{}
	db, err := Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	out["sqlite"] = db
	if u := os.Getenv("IOTHUB_TEST_PG"); u != "" {
		pg, err := Open(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range []string{"readings", "rollup_1m", "series", "anomalies"} {
			pg.(*sqlDB).db.Exec("TRUNCATE " + tb)
		}
		out["postgres"] = pg
	}
	t.Cleanup(func() {
		for _, d := range out {
			d.Close()
		}
	})
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

func TestBackends(t *testing.T) {
	for name, db := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// 10 minutes at 1 Hz: value = second index, so stats are known.
			const base = int64(1_700_000_000_000) - int64(1_700_000_000_000)%600000 // 10-minute aligned
			var pts []Point
			for i := int64(0); i < 600; i++ {
				pts = append(pts, Point{Sensor: "env", Field: "t", TS: base + i*1000, Value: float64(i)})
			}
			pts = append(pts, Point{Sensor: "env", Field: "door", TS: base, Text: "Open", IsText: true})
			// Split across two batches to exercise rollup merging.
			if err := db.WritePoints(ctx, pts[:250]); err != nil {
				t.Fatal(err)
			}
			if err := db.WritePoints(ctx, pts[250:]); err != nil {
				t.Fatal(err)
			}

			raw, err := db.Series(ctx, "env", "t", base, base+600_000, 30_000)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) != 20 || raw[0].N != 30 || raw[0].Min != 0 || raw[0].Max != 29 || !near(raw[0].Avg, 14.5) {
				t.Fatalf("raw buckets: %d %+v", len(raw), raw[0])
			}
			roll, err := db.Series(ctx, "env", "t", base, base+600_000, 120_000)
			if err != nil {
				t.Fatal(err)
			}
			if len(roll) != 5 || roll[1].N != 120 || roll[1].Min != 120 || roll[1].Max != 239 || !near(roll[1].Avg, 179.5) {
				t.Fatalf("rollup buckets: %d %+v", len(roll), roll)
			}

			// mean of 0..599 = 299.5; population std = sqrt((n^2-1)/12)
			want := math.Sqrt((600*600 - 1) / 12.0)
			st, err := db.Stats(ctx, "env", "t", base, base+600_000)
			if err != nil {
				t.Fatal(err)
			}
			if st.N != 600 || !near(st.Mean, 299.5) || !near(st.Std, want) || st.Max != 599 {
				t.Fatalf("raw stats %+v want std %g", st, want)
			}
			// > 6h window forces the rollup path; must agree exactly.
			st2, err := db.Stats(ctx, "env", "t", base-7*3600_000, base+600_000)
			if err != nil {
				t.Fatal(err)
			}
			if st2.N != st.N || !near(st2.Mean, st.Mean) || !near(st2.Std, st.Std) {
				t.Fatalf("rollup stats %+v != raw %+v", st2, st)
			}
			if b, _ := db.Series(ctx, "nope", "t", 0, base, 1000); len(b) != 0 {
				t.Fatal("unknown series must be empty")
			}

			e := anomaly.Event{ID: "env.t.spike.1", Sensor: "env", Field: "t", Kind: "spike", Start: base, Value: 9, Score: 5.5, Message: "m"}
			if err := db.SaveEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
			act, _ := db.Events(ctx, EventQuery{From: 0, To: base + 1, ActiveOnly: true})
			if len(act) != 1 {
				t.Fatalf("active %+v", act)
			}
			e.End = base + 5000
			if err := db.SaveEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
			evs, _ := db.Events(ctx, EventQuery{Sensor: "env", From: 0, To: base + 1})
			if len(evs) != 1 || evs[0].End != base+5000 || evs[0].Score != 5.5 {
				t.Fatalf("events %+v", evs)
			}
			_ = db.SaveEvent(ctx, anomaly.Event{ID: "x", Sensor: "env", Kind: "stale", Start: base + 1})
			if err := db.CloseOpenEvents(ctx, base+9); err != nil {
				t.Fatal(err)
			}
			if act, _ := db.Events(ctx, EventQuery{From: 0, To: base + 10, ActiveOnly: true}); len(act) != 0 {
				t.Fatalf("CloseOpenEvents left %+v", act)
			}

			// Prune raw before minute 5; rollups stay.
			if err := db.Prune(ctx, base+300_000, 0, 0); err != nil {
				t.Fatal(err)
			}
			st, _ = db.Stats(ctx, "env", "t", base, base+600_000)
			if st.N != 300 {
				t.Fatalf("after prune raw n=%d", st.N)
			}
			st2, _ = db.Stats(ctx, "env", "t", base-7*3600_000, base+600_000)
			if st2.N != 600 {
				t.Fatalf("rollups must survive raw prune, n=%d", st2.N)
			}
		})
	}
}

func TestAutoBucket(t *testing.T) {
	for _, c := range []struct{ span, want int64 }{
		{3600e3, 10e3}, {6 * 3600e3, 60e3}, {24 * 3600e3, 300e3}, {7 * 24 * 3600e3, 1800e3},
	} {
		if got := AutoBucket(0, c.span, 600); got != c.want {
			t.Errorf("span %d: got %d want %d", c.span, got, c.want)
		}
	}
}
