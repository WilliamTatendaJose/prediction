package forecast

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestLinearDrainETA(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var y []float64
	for i := 0; i < 200; i++ { // tank: 100 % falling 0.2 %/step, sensor noise 0.3
		y = append(y, 100-0.2*float64(i)+r.NormFloat64()*0.3)
	}
	res, err := Forecast(y, 400, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := Cross(res, 15, "below")
	// True crossing: 100 - 0.2 t = 15 at t = 425, i.e. 226 steps after t = 199.
	if c.Step == 0 || math.Abs(float64(c.Step)-226) > 226*0.05 {
		t.Fatalf("ETA step %d, want ~226 (%s %+v)", c.Step, res.Method, res.Params)
	}
	if !(c.Early <= c.Step && c.Step <= c.Late || c.Late == 0) {
		t.Fatalf("band order: early %d step %d late %d", c.Early, c.Step, c.Late)
	}
	if res.Skill < 0.5 {
		t.Fatalf("a clean trend should beat naive clearly: skill %.2f", res.Skill)
	}
}

func TestSeasonalBeatsPlain(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var y []float64
	for i := 0; i < 24*10; i++ { // 10 days hourly: daily cycle + slow trend
		y = append(y, 20+0.01*float64(i)+5*math.Sin(2*math.Pi*float64(i)/24)+r.NormFloat64()*0.4)
	}
	res, err := Forecast(y, 24, 24)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "holt-winters" || res.Skill < 0.5 {
		t.Fatalf("method %s skill %.2f", res.Method, res.Skill)
	}
	// Next-day forecast should follow the cycle: check peak position.
	peak := 0
	for i, v := range res.Yhat {
		if v > res.Yhat[peak] {
			peak = i
		}
	}
	// sin peaks at hour 6 of each 24 h cycle; the forecast starts at i=240 (hour 0).
	if peak < 4 || peak > 8 {
		t.Fatalf("forecast peak at +%d h, want ~+6 h: %v", peak+1, res.Yhat)
	}
	plain, _ := Forecast(y, 24, 0)
	if res.MAE >= plain.MAE {
		t.Fatalf("seasonal MAE %.2f should beat plain %.2f", res.MAE, plain.MAE)
	}
}

func TestFlatNoFalseCrossing(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	var y []float64
	for i := 0; i < 300; i++ {
		y = append(y, 50+r.NormFloat64())
	}
	res, err := Forecast(y, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.Yhat[len(res.Yhat)-1]-50) > 3 {
		t.Fatalf("flat series drifted to %.2f (%+v)", res.Yhat[len(res.Yhat)-1], res.Params)
	}
	if c := Cross(res, 15, "below"); c.Step != 0 {
		t.Fatalf("no crossing expected, got step %d", c.Step)
	}
}

func TestEdges(t *testing.T) {
	if _, err := Forecast([]float64{1, 2, 3}, 5, 0); err != ErrTooShort {
		t.Fatalf("short: %v", err)
	}
	y := make([]float64, 20)
	y[3] = math.NaN()
	if _, err := Forecast(y, 5, 0); err == nil {
		t.Fatal("NaN must be rejected")
	}
	// Constant series: no division by zero, zero skill reported sanely.
	for i := range y {
		y[i] = 7
	}
	res, err := Forecast(y, 5, 0)
	if err != nil || res.Yhat[4] != 7 || math.IsNaN(res.Skill) {
		t.Fatalf("constant: %+v %v", res, err)
	}
}
