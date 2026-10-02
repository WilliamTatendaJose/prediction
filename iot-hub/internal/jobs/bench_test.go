package jobs

import (
	"fmt"
	"testing"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

func BenchmarkTumbling500Sensors(b *testing.B) {
	e := New(Outputs{Sensor: func(string, int64, map[string]any) error { return nil }})
	e.Set([]Spec{{ID: "avg", Enabled: true, Query: "SELECT avg(v) INTO [avg-{sensor}] FROM [s-*] GROUP BY sensor, TumblingWindow(minute, 1)"}})
	names := make([]string, 500)
	for i := range names {
		names[i] = fmt.Sprintf("s-%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Observe(store.Reading{Sensor: names[i%500], TS: t0.Add(time.Duration(i) * time.Millisecond).UnixMilli(), Values: map[string]any{"v": float64(i % 100)}})
	}
}

func BenchmarkSliding10k(b *testing.B) {
	e := New(Outputs{Sensor: func(string, int64, map[string]any) error { return nil }})
	e.Set([]Spec{{ID: "c", Enabled: true, Query: "SELECT count(v) INTO [cnt] FROM [s] GROUP BY SlidingWindow(hour, 1)"}})
	for i := 0; i < MaxSliding; i++ {
		e.Observe(store.Reading{Sensor: "s", TS: t0.Add(time.Duration(i) * 100 * time.Millisecond).UnixMilli(), Values: map[string]any{"v": 1.0}})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Observe(store.Reading{Sensor: "s", TS: t0.Add(time.Duration(MaxSliding+i) * 100 * time.Millisecond).UnixMilli(), Values: map[string]any{"v": 1.0}})
	}
}
