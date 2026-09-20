package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
	"github.com/xsaveopt/dell-ipmitemps/internal/datasource"
)

func smoother(s config.Smoothing, last int) *Controller {
	return &Controller{cfg: &config.Config{Smoothing: s, TempCritical: 90}, lastSpeed: last}
}

var std = config.Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 100, MaxStepDown: 5, UrgentTemp: 80}

func TestSmoothDisabledPassesThrough(t *testing.T) {
	c := smoother(config.Smoothing{}, 30)
	if got := c.smooth(90, 40); got != 90 {
		t.Errorf("got %d, want 90", got)
	}
}

func TestSmoothNoLastSpeedPassesThrough(t *testing.T) {
	c := smoother(std, -1)
	if got := c.smooth(72, 40); got != 72 {
		t.Errorf("got %d, want 72", got)
	}
}

func TestSmoothDeadbandHoldsSmallMoves(t *testing.T) {
	c := smoother(std, 30)
	if got := c.smooth(33, 40); got != 30 {
		t.Errorf("rising within deadband: got %d, want 30", got)
	}
	if got := c.smooth(27, 40); got != 30 {
		t.Errorf("falling within deadband: got %d, want 30", got)
	}
	if got := c.smooth(34, 40); got != 34 {
		t.Errorf("past deadband: got %d, want 34", got)
	}
}

func TestSmoothRisesImmediatelyWhenNeeded(t *testing.T) {
	c := smoother(std, 30)
	if got := c.smooth(85, 60); got != 85 {
		t.Errorf("got %d, a genuine rise must not be rate-limited", got)
	}
}

func TestSmoothRespectsMaxStepUpWhenSet(t *testing.T) {
	s := std
	s.MaxStepUp = 10
	c := smoother(s, 30)
	if got := c.smooth(85, 60); got != 40 {
		t.Errorf("got %d, want 40", got)
	}
}

func TestSmoothLimitsDescent(t *testing.T) {
	c := smoother(std, 60)
	if got := c.smooth(20, 30); got != 55 {
		t.Errorf("got %d, want 55", got)
	}
}

func TestSmoothUrgentTempBypasses(t *testing.T) {
	s := std
	s.MaxStepUp = 5
	c := smoother(s, 30)
	if got := c.smooth(100, 80); got != 100 {
		t.Errorf("got %d, urgent temperatures must bypass smoothing", got)
	}
}

type fakeIPMI struct {
	log    []string
	speeds []int

	failOnce map[string]int
	failAll  map[string]bool
}

func newFakeIPMI() *fakeIPMI {
	return &fakeIPMI{failOnce: map[string]int{}, failAll: map[string]bool{}}
}

func (f *fakeIPMI) record(op string) error {
	f.log = append(f.log, op)
	if f.failAll[op] {
		return errors.New(op + " refused")
	}
	if f.failOnce[op] > 0 {
		f.failOnce[op]--
		return errors.New(op + " refused")
	}
	return nil
}

func (f *fakeIPMI) Ping(context.Context) error               { return f.record("Ping") }
func (f *fakeIPMI) SetManual(context.Context) error          { return f.record("SetManual") }
func (f *fakeIPMI) SetAuto(context.Context) error            { return f.record("SetAuto") }
func (f *fakeIPMI) DisablePCIeCooling(context.Context) error { return f.record("DisablePCIeCooling") }
func (f *fakeIPMI) EnablePCIeCooling(context.Context) error  { return f.record("EnablePCIeCooling") }

func (f *fakeIPMI) SetSpeed(_ context.Context, pct int) error {
	f.speeds = append(f.speeds, pct)
	return f.record("SetSpeed")
}

func (f *fakeIPMI) count(op string) int {
	n := 0
	for _, e := range f.log {
		if e == op {
			n++
		}
	}
	return n
}

func (f *fakeIPMI) lastSpeed() int {
	if len(f.speeds) == 0 {
		return -1
	}
	return f.speeds[len(f.speeds)-1]
}

type fakeDS struct {
	temps    map[string]float64
	series   map[string][]datasource.Sample
	err      error
	queries  []string
	lookback time.Duration
	step     time.Duration
}

func (d *fakeDS) FetchTemp(_ context.Context, query string) (float64, error) {
	d.queries = append(d.queries, query)
	if d.err != nil {
		return 0, d.err
	}
	v, ok := d.temps[query]
	if !ok {
		return 0, errors.New("no such series: " + query)
	}
	return v, nil
}

func (d *fakeDS) FetchRange(_ context.Context, query string, lookback, step time.Duration) ([]datasource.Sample, error) {
	d.queries = append(d.queries, query)
	d.lookback, d.step = lookback, step
	if d.err != nil {
		return nil, d.err
	}
	s, ok := d.series[query]
	if !ok {
		return nil, errors.New("no such series: " + query)
	}
	return s, nil
}

func flat(v float64, n int) []datasource.Sample {
	s := make([]datasource.Sample, n)
	for i := range s {
		s[i] = datasource.Sample{T: float64(i) * 10, V: v}
	}
	return s
}

func baseConfig() *config.Config {
	return &config.Config{
		Datasource: config.Datasource{Type: config.Prometheus},
		IPMI:       config.IPMI{Local: true, User: "root"},
		Sensors:    []config.Sensor{{Name: "cpu", Query: "cpu_temp", Weight: 1}},
		FanCurve: []config.CurvePoint{
			{Temp: 30, Percent: 10},
			{Temp: 50, Percent: 30},
			{Temp: 65, Percent: 55},
			{Temp: 75, Percent: 80},
			{Temp: 82, Percent: 100},
		},
		TempCritical:         90,
		MinFanSpeed:          10,
		PollInterval:         30,
		MaxFetchFailures:     3,
		FetchFailureFanSpeed: 100,
	}
}

func newHarness(t *testing.T, cfg *config.Config) (*Controller, *fakeDS, *fakeIPMI) {
	t.Helper()
	ds := &fakeDS{temps: map[string]float64{}, series: map[string][]datasource.Sample{}}
	fi := newFakeIPMI()
	c := &Controller{
		cfg:        cfg,
		ds:         ds,
		ipmi:       fi,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		statePath:  filepath.Join(t.TempDir(), "dellipmifanctl.state"),
		retryPause: 0,
		lastSpeed:  -1,
	}
	return c, ds, fi
}

func readState(t *testing.T, c *Controller) string {
	t.Helper()
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func TestPollAppliesCurveSpeed(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65

	c.poll(context.Background())

	if fi.lastSpeed() != 55 {
		t.Errorf("speed = %d, want 55 from the curve", fi.lastSpeed())
	}
	if c.lastSpeed != 55 {
		t.Errorf("lastSpeed = %d, want 55", c.lastSpeed)
	}
	if got := readState(t, c); got != "55" {
		t.Errorf("state file = %q, want 55", got)
	}
	if fi.count("SetAuto") != 0 {
		t.Error("a normal poll must never hand control to the BMC")
	}
}

func TestPollFloorsAtMinFanSpeed(t *testing.T) {
	cfg := baseConfig()
	cfg.MinFanSpeed = 25
	c, ds, fi := newHarness(t, cfg)
	ds.temps["cpu_temp"] = 10

	c.poll(context.Background())

	if fi.lastSpeed() != 25 {
		t.Errorf("speed = %d, want the min_fan_speed floor of 25", fi.lastSpeed())
	}
}

func TestPollTakesTheHottestSensor(t *testing.T) {
	cfg := baseConfig()
	cfg.Sensors = []config.Sensor{
		{Name: "cpu", Query: "cpu_temp", Weight: 1},
		{Name: "inlet", Query: "inlet_temp", Weight: 1},
	}
	c, ds, fi := newHarness(t, cfg)
	ds.temps["cpu_temp"] = 40
	ds.temps["inlet_temp"] = 75

	c.poll(context.Background())

	if fi.lastSpeed() != 80 {
		t.Errorf("speed = %d, want 80 driven by the hotter sensor", fi.lastSpeed())
	}
	if len(ds.queries) != 2 {
		t.Errorf("queried %v, want both sensors read", ds.queries)
	}
}

func TestPollSkipsRedundantSetSpeed(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65

	c.poll(context.Background())
	c.poll(context.Background())

	if fi.count("SetSpeed") != 1 {
		t.Errorf("SetSpeed called %d times, want 1 for an unchanged target", fi.count("SetSpeed"))
	}
}

func TestPollLeavesStateAloneWhenSetSpeedFails(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65
	fi.failAll["SetSpeed"] = true

	c.poll(context.Background())

	if c.lastSpeed != -1 {
		t.Errorf("lastSpeed = %d, want it untouched after a failed SetSpeed", c.lastSpeed)
	}
	if got := readState(t, c); got != "" {
		t.Errorf("state file = %q, want nothing written after a failed SetSpeed", got)
	}
}

func TestPollResetsFailureCountOnSuccess(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65
	c.consecutiveFailures = 2

	c.poll(context.Background())

	if c.consecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d, want it reset", c.consecutiveFailures)
	}
	if fi.count("SetSpeed") != 1 {
		t.Error("want the curve applied after recovery")
	}
}

func TestPollCriticalHandsBackToBMC(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 95

	c.poll(context.Background())

	if !c.inAutoMode {
		t.Error("want the controller in BMC auto mode")
	}
	if fi.count("SetAuto") != 1 {
		t.Errorf("SetAuto called %d times, want 1", fi.count("SetAuto"))
	}
	if fi.count("EnablePCIeCooling") != 1 {
		t.Errorf("EnablePCIeCooling called %d times, want 1", fi.count("EnablePCIeCooling"))
	}
	if fi.count("SetSpeed") != 0 {
		t.Error("the critical path must not keep driving fans manually")
	}
}

func TestPollCriticalAtExactlyTheThreshold(t *testing.T) {
	c, ds, _ := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 90

	c.poll(context.Background())

	if !c.inAutoMode {
		t.Error("temp_critical is inclusive, want the BMC fallback")
	}
}

func TestPollCriticalClearsFailSafe(t *testing.T) {
	c, ds, _ := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 95
	c.inFailSafe = true

	c.poll(context.Background())

	if c.inFailSafe {
		t.Error("handing to the BMC must clear the fail-safe flag")
	}
}

func TestPollResumesManualWhenTemperaturesNormalise(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 95
	c.poll(context.Background())

	ds.temps["cpu_temp"] = 65
	c.poll(context.Background())

	if c.inAutoMode {
		t.Error("want manual control resumed")
	}
	if fi.count("SetManual") != 1 {
		t.Errorf("SetManual called %d times, want 1", fi.count("SetManual"))
	}
	if fi.lastSpeed() != 55 {
		t.Errorf("speed = %d, want the curve reapplied", fi.lastSpeed())
	}
}

func TestPollResumeReDisablesPCIeCooling(t *testing.T) {
	cfg := baseConfig()
	cfg.DisablePCIeCoolingResponse = true
	c, ds, fi := newHarness(t, cfg)
	c.inAutoMode = true
	ds.temps["cpu_temp"] = 65

	c.poll(context.Background())

	if fi.count("DisablePCIeCooling") != 1 {
		t.Errorf("DisablePCIeCooling called %d times, want 1 on resume", fi.count("DisablePCIeCooling"))
	}
}

func TestPollResumeSkipsPCIeCoolingWhenNotConfigured(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	c.inAutoMode = true
	ds.temps["cpu_temp"] = 65

	c.poll(context.Background())

	if fi.count("DisablePCIeCooling") != 0 {
		t.Error("PCIe cooling must stay untouched when the knob is off")
	}
}

func TestPollResumeAbortsWhenManualCannotBeRestored(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	c.inAutoMode = true
	ds.temps["cpu_temp"] = 65
	fi.failAll["SetManual"] = true

	c.poll(context.Background())

	if !c.inAutoMode {
		t.Error("a failed SetManual must leave the controller believing the BMC is in charge")
	}
	if fi.count("SetSpeed") != 0 {
		t.Error("must not set a speed while manual control is unconfirmed")
	}
}

func TestPollLeavesFailSafeWhenDataReturns(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.err = errors.New("prometheus down")
	for range 3 {
		c.poll(context.Background())
	}
	if !c.inFailSafe {
		t.Fatalf("want the fail-safe engaged, log = %v", fi.log)
	}

	ds.err = nil
	ds.temps["cpu_temp"] = 65
	c.poll(context.Background())

	if c.inFailSafe {
		t.Error("want the fail-safe cleared once data returns")
	}
	if fi.lastSpeed() != 55 {
		t.Errorf("speed = %d, want the curve resumed", fi.lastSpeed())
	}
}

func TestPollFetchFailureDoesNotHandToBMC(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.err = errors.New("prometheus down")

	for range 5 {
		c.poll(context.Background())
	}

	if fi.count("SetAuto") != 0 {
		t.Error("lost sensor data must fail safe in manual, never hand to the BMC")
	}
	if fi.lastSpeed() != 100 {
		t.Errorf("speed = %d, want the fail-safe speed", fi.lastSpeed())
	}
}

func TestPollStopsAtTheFirstFailingSensor(t *testing.T) {
	cfg := baseConfig()
	cfg.Sensors = []config.Sensor{
		{Name: "cpu", Query: "missing", Weight: 1},
		{Name: "inlet", Query: "inlet_temp", Weight: 1},
	}
	c, ds, fi := newHarness(t, cfg)
	ds.temps["inlet_temp"] = 70

	c.poll(context.Background())

	if fi.count("SetSpeed") != 0 {
		t.Error("a partial sensor read must not drive the curve")
	}
	if c.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", c.consecutiveFailures)
	}
}

func TestPollSmoothingRespectsTheFloor(t *testing.T) {
	cfg := baseConfig()
	cfg.MinFanSpeed = 40
	cfg.Smoothing = config.Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 100, MaxStepDown: 5, UrgentTemp: 80}
	c, ds, fi := newHarness(t, cfg)
	c.lastSpeed = 44
	ds.temps["cpu_temp"] = 20

	c.poll(context.Background())

	if fi.lastSpeed() != 40 {
		t.Errorf("speed = %d, want the min_fan_speed floor to win over the slew limit", fi.lastSpeed())
	}
}

func TestReadSensorInstant(t *testing.T) {
	cases := []struct {
		name     string
		temp     float64
		critical float64
		wantCrit bool
	}{
		{name: "cool", temp: 40, critical: 90},
		{name: "warm", temp: 89.9, critical: 90},
		{name: "at threshold", temp: 90, critical: 90, wantCrit: true},
		{name: "above threshold", temp: 120, critical: 90, wantCrit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TempCritical = tc.critical
			c, ds, _ := newHarness(t, cfg)
			ds.temps["cpu_temp"] = tc.temp

			temp, crit, err := c.readSensor(context.Background(), cfg.Sensors[0])
			if err != nil {
				t.Fatalf("readSensor: %v", err)
			}
			if temp != tc.temp {
				t.Errorf("temp = %v, want %v", temp, tc.temp)
			}
			if crit != tc.wantCrit {
				t.Errorf("critical = %v, want %v", crit, tc.wantCrit)
			}
		})
	}
}

func TestReadSensorInstantPropagatesError(t *testing.T) {
	c, ds, _ := newHarness(t, baseConfig())
	boom := errors.New("query failed")
	ds.err = boom

	_, _, err := c.readSensor(context.Background(), c.cfg.Sensors[0])
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

func TestReadSensorPredictionUsesTheWindow(t *testing.T) {
	cfg := baseConfig()
	cfg.Prediction = config.Prediction{
		Enabled:       true,
		Lookback:      config.Duration(120 * time.Second),
		Step:          config.Duration(10 * time.Second),
		Quantile:      0.6,
		TrendHorizon:  config.Duration(30 * time.Second),
		CriticalDwell: config.Duration(15 * time.Second),
	}
	c, ds, _ := newHarness(t, cfg)
	ds.series["cpu_temp"] = flat(50, 8)

	temp, crit, err := c.readSensor(context.Background(), cfg.Sensors[0])
	if err != nil {
		t.Fatalf("readSensor: %v", err)
	}
	if temp != 50 {
		t.Errorf("effective = %v, want 50 for a flat series", temp)
	}
	if crit {
		t.Error("a flat 50C series is not critical")
	}
	if ds.lookback != 120*time.Second || ds.step != 10*time.Second {
		t.Errorf("FetchRange got lookback=%v step=%v, want 2m/10s", ds.lookback, ds.step)
	}
}

func TestReadSensorPredictionIgnoresALoneSpike(t *testing.T) {
	cfg := baseConfig()
	cfg.Prediction = config.Prediction{
		Enabled:       true,
		Lookback:      config.Duration(120 * time.Second),
		Step:          config.Duration(10 * time.Second),
		Quantile:      0.6,
		CriticalDwell: config.Duration(15 * time.Second),
	}
	c, ds, _ := newHarness(t, cfg)
	samples := flat(40, 10)
	samples[len(samples)-1].V = 120
	ds.series["cpu_temp"] = samples

	temp, crit, err := c.readSensor(context.Background(), cfg.Sensors[0])
	if err != nil {
		t.Fatalf("readSensor: %v", err)
	}
	if crit {
		t.Error("a single spike must not trip the sustained critical check")
	}
	if temp > 60 {
		t.Errorf("effective = %v, want the spike damped well below it", temp)
	}
}

func TestReadSensorPredictionSustainedCritical(t *testing.T) {
	cfg := baseConfig()
	cfg.Prediction = config.Prediction{
		Enabled:       true,
		Lookback:      config.Duration(120 * time.Second),
		Step:          config.Duration(10 * time.Second),
		Quantile:      0.6,
		CriticalDwell: config.Duration(30 * time.Second),
	}
	c, ds, _ := newHarness(t, cfg)
	ds.series["cpu_temp"] = flat(95, 8)

	_, crit, err := c.readSensor(context.Background(), cfg.Sensors[0])
	if err != nil {
		t.Fatalf("readSensor: %v", err)
	}
	if !crit {
		t.Error("a sustained 95C series must be critical")
	}
}

func TestReadSensorPredictionPropagatesError(t *testing.T) {
	cfg := baseConfig()
	cfg.Prediction = config.Prediction{Enabled: true, Lookback: config.Duration(time.Minute), Step: config.Duration(time.Second), Quantile: 0.6}
	c, ds, _ := newHarness(t, cfg)
	boom := errors.New("range query failed")
	ds.err = boom

	_, _, err := c.readSensor(context.Background(), cfg.Sensors[0])
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

func TestHandleFetchFailureCountsBeforeActing(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())

	c.handleFetchFailure(context.Background())
	c.handleFetchFailure(context.Background())

	if c.consecutiveFailures != 2 {
		t.Errorf("consecutiveFailures = %d, want 2", c.consecutiveFailures)
	}
	if c.inFailSafe {
		t.Error("must not fail safe before max_fetch_failures is reached")
	}
	if len(fi.log) != 0 {
		t.Errorf("ipmi calls %v, want none below the threshold", fi.log)
	}
}

func TestHandleFetchFailureEngagesFailSafeAtTheThreshold(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())

	for range 3 {
		c.handleFetchFailure(context.Background())
	}

	if !c.inFailSafe {
		t.Fatal("want the fail-safe engaged at max_fetch_failures")
	}
	if fi.count("SetManual") != 1 {
		t.Errorf("SetManual called %d times, want 1", fi.count("SetManual"))
	}
	if fi.lastSpeed() != 100 {
		t.Errorf("speed = %d, want the fail-safe speed of 100", fi.lastSpeed())
	}
	if c.lastSpeed != 100 {
		t.Errorf("lastSpeed = %d, want 100", c.lastSpeed)
	}
	if got := readState(t, c); got != "100" {
		t.Errorf("state file = %q, want 100", got)
	}
	if fi.count("SetAuto") != 0 {
		t.Error("lost sensor data must never hand control to the BMC")
	}
}

func TestHandleFetchFailureUsesConfiguredSpeed(t *testing.T) {
	cfg := baseConfig()
	cfg.FetchFailureFanSpeed = 70
	cfg.MaxFetchFailures = 1
	c, _, fi := newHarness(t, cfg)

	c.handleFetchFailure(context.Background())

	if fi.lastSpeed() != 70 {
		t.Errorf("speed = %d, want the configured fail-safe speed of 70", fi.lastSpeed())
	}
}

func TestHandleFetchFailureIsIdempotent(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFetchFailures = 1
	c, _, fi := newHarness(t, cfg)

	c.handleFetchFailure(context.Background())
	before := len(fi.log)
	c.handleFetchFailure(context.Background())
	c.handleFetchFailure(context.Background())

	if len(fi.log) != before {
		t.Errorf("ipmi log grew to %v, want no repeat commands while already in fail-safe", fi.log)
	}
	if c.consecutiveFailures != 3 {
		t.Errorf("consecutiveFailures = %d, want it to keep counting", c.consecutiveFailures)
	}
}

func TestHandleFetchFailureStillPinsSpeedWhenSetManualFails(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFetchFailures = 1
	c, _, fi := newHarness(t, cfg)
	fi.failAll["SetManual"] = true

	c.handleFetchFailure(context.Background())

	if !c.inFailSafe {
		t.Error("a failed SetManual must not stop the fail-safe speed being applied")
	}
	if fi.lastSpeed() != 100 {
		t.Errorf("speed = %d, want 100", fi.lastSpeed())
	}
}

func TestHandleFetchFailureDoesNotLatchWhenSpeedCannotBeSet(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFetchFailures = 1
	c, _, fi := newHarness(t, cfg)
	fi.failAll["SetSpeed"] = true

	c.handleFetchFailure(context.Background())

	if c.inFailSafe {
		t.Error("the fail-safe must not latch when the speed was never applied")
	}
	if c.lastSpeed != -1 {
		t.Errorf("lastSpeed = %d, want it untouched", c.lastSpeed)
	}
	if got := readState(t, c); got != "" {
		t.Errorf("state file = %q, want nothing persisted", got)
	}
}

func TestHandleFetchFailureRetriesOnTheNextPoll(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFetchFailures = 1
	c, _, fi := newHarness(t, cfg)
	fi.failOnce["SetSpeed"] = 1

	c.handleFetchFailure(context.Background())
	if c.inFailSafe {
		t.Fatal("first attempt must not latch")
	}
	c.handleFetchFailure(context.Background())

	if !c.inFailSafe {
		t.Error("want the fail-safe engaged once the speed lands")
	}
	if fi.count("SetSpeed") != 2 {
		t.Errorf("SetSpeed called %d times, want a retry", fi.count("SetSpeed"))
	}
}

func TestHandleFetchFailureClearsAutoMode(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFetchFailures = 1
	c, _, _ := newHarness(t, cfg)
	c.inAutoMode = true

	c.handleFetchFailure(context.Background())

	if c.inAutoMode {
		t.Error("taking manual control for the fail-safe must clear the auto-mode flag")
	}
}

func TestFallbackToBMC(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	c.inFailSafe = true

	c.fallbackToBMC(context.Background(), "critical temp detected")

	if !c.inAutoMode {
		t.Error("want inAutoMode set")
	}
	if c.inFailSafe {
		t.Error("want inFailSafe cleared")
	}
	want := []string{"SetAuto", "EnablePCIeCooling"}
	if len(fi.log) != len(want) {
		t.Fatalf("ipmi log = %v, want %v", fi.log, want)
	}
	for i := range want {
		if fi.log[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, fi.log[i], want[i])
		}
	}
}

func TestFallbackToBMCIsANoOpWhenAlreadyInAuto(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	c.inAutoMode = true

	c.fallbackToBMC(context.Background(), "another critical reading")

	if len(fi.log) != 0 {
		t.Errorf("ipmi log = %v, want no repeat hand-back", fi.log)
	}
}

func TestFallbackToBMCStillMarksAutoWhenIPMIFails(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	fi.failAll["SetAuto"] = true
	fi.failAll["EnablePCIeCooling"] = true

	c.fallbackToBMC(context.Background(), "critical temp detected")

	if !c.inAutoMode {
		t.Error("want the controller to record the hand-back even after retries failed")
	}
	if fi.count("SetAuto") != retryAttempts {
		t.Errorf("SetAuto attempted %d times, want %d", fi.count("SetAuto"), retryAttempts)
	}
}

func TestHandBackToBMCAlwaysRestoresPCIeCooling(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	fi.failAll["SetAuto"] = true

	c.handBackToBMC(context.Background())

	if fi.count("EnablePCIeCooling") != 1 {
		t.Errorf("EnablePCIeCooling called %d times, want it attempted even after SetAuto gave up", fi.count("EnablePCIeCooling"))
	}
}

func TestHandBackToBMCRetriesBothCommands(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	fi.failOnce["SetAuto"] = 2
	fi.failOnce["EnablePCIeCooling"] = 1

	c.handBackToBMC(context.Background())

	if fi.count("SetAuto") != 3 {
		t.Errorf("SetAuto called %d times, want 3", fi.count("SetAuto"))
	}
	if fi.count("EnablePCIeCooling") != 2 {
		t.Errorf("EnablePCIeCooling called %d times, want 2", fi.count("EnablePCIeCooling"))
	}
}

func TestWithRetry(t *testing.T) {
	cases := []struct {
		name      string
		failFirst int
		wantCalls int
	}{
		{name: "first attempt", failFirst: 0, wantCalls: 1},
		{name: "second attempt", failFirst: 1, wantCalls: 2},
		{name: "last attempt", failFirst: 2, wantCalls: 3},
		{name: "gives up", failFirst: 99, wantCalls: retryAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newHarness(t, baseConfig())
			calls := 0
			c.withRetry("set BMC auto mode", func() error {
				calls++
				if calls <= tc.failFirst {
					return errors.New("busy")
				}
				return nil
			})
			if calls != tc.wantCalls {
				t.Errorf("fn called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestWithRetryWaitsBetweenAttempts(t *testing.T) {
	c, _, _ := newHarness(t, baseConfig())
	c.retryPause = 20 * time.Millisecond

	start := time.Now()
	c.withRetry("set BMC auto mode", func() error { return errors.New("busy") })
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Errorf("elapsed %v, want a pause between each of the %d attempts", elapsed, retryAttempts)
	}
}

func TestRestoreState(t *testing.T) {
	cases := []struct {
		name      string
		contents  string
		write     bool
		want      int
		wantFile  bool
		wantClear bool
	}{
		{name: "absent", want: -1},
		{name: "valid", contents: "55", write: true, want: 55, wantFile: true},
		{name: "zero", contents: "0", write: true, want: 0, wantFile: true},
		{name: "hundred", contents: "100", write: true, want: 100, wantFile: true},
		{name: "trailing newline", contents: "42\n", write: true, want: 42, wantFile: true},
		{name: "surrounding whitespace", contents: "  42  \n", write: true, want: 42, wantFile: true},
		{name: "above range", contents: "101", write: true, want: -1, wantClear: true},
		{name: "negative", contents: "-1", write: true, want: -1, wantClear: true},
		{name: "not a number", contents: "fast", write: true, want: -1, wantClear: true},
		{name: "empty", contents: "", write: true, want: -1, wantClear: true},
		{name: "float", contents: "55.5", write: true, want: -1, wantClear: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newHarness(t, baseConfig())
			if tc.write {
				if err := os.WriteFile(c.statePath, []byte(tc.contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			c.restoreState()

			if c.lastSpeed != tc.want {
				t.Errorf("lastSpeed = %d, want %d", c.lastSpeed, tc.want)
			}
			_, err := os.Stat(c.statePath)
			switch {
			case tc.wantFile && err != nil:
				t.Errorf("state file should have been kept: %v", err)
			case tc.wantClear && err == nil:
				t.Error("an invalid state file should have been removed")
			}
		})
	}
}

func TestRestoreStateIgnoresAnUnreadablePath(t *testing.T) {
	c, _, _ := newHarness(t, baseConfig())
	c.statePath = filepath.Join(t.TempDir(), "no-such-dir", "state")

	c.restoreState()

	if c.lastSpeed != -1 {
		t.Errorf("lastSpeed = %d, want -1", c.lastSpeed)
	}
}

func TestSaveState(t *testing.T) {
	c, _, _ := newHarness(t, baseConfig())
	for _, v := range []int{0, 37, 100} {
		c.saveState(v)
		if got := readState(t, c); got != strconv.Itoa(v) {
			t.Errorf("state file = %q, want %d", got, v)
		}
	}
}

func TestSaveStateSurvivesAnUnwritablePath(t *testing.T) {
	c, _, _ := newHarness(t, baseConfig())
	c.statePath = filepath.Join(t.TempDir(), "no-such-dir", "state")

	c.saveState(55)
}

func TestSaveStateRoundTrips(t *testing.T) {
	c, _, _ := newHarness(t, baseConfig())
	c.saveState(73)

	fresh, _, _ := newHarness(t, baseConfig())
	fresh.statePath = c.statePath
	fresh.restoreState()

	if fresh.lastSpeed != 73 {
		t.Errorf("lastSpeed = %d, want 73 restored across a restart", fresh.lastSpeed)
	}
}

func TestRestoreAutoHandsBackAndClearsState(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	c.saveState(60)

	c.restoreAuto()

	if fi.count("SetAuto") != 1 {
		t.Errorf("SetAuto called %d times, want 1", fi.count("SetAuto"))
	}
	if fi.count("EnablePCIeCooling") != 1 {
		t.Errorf("EnablePCIeCooling called %d times, want 1", fi.count("EnablePCIeCooling"))
	}
	if _, err := os.Stat(c.statePath); err == nil {
		t.Error("want the state file removed on shutdown")
	}
}

func TestRestoreAutoWithNoStateFile(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())

	c.restoreAuto()

	if fi.count("SetAuto") != 1 {
		t.Errorf("SetAuto called %d times, want 1", fi.count("SetAuto"))
	}
}
