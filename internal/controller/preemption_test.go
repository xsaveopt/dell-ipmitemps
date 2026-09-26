package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
	"github.com/xsaveopt/dell-ipmitemps/internal/procwatch"
)

func transientStat() *procwatch.Stat {
	return &procwatch.Stat{PeakRise: 30, DurationSec: 30, TransientScore: 1, Observations: 3, LastSeen: time.Now().Unix()}
}

func sustainedStat() *procwatch.Stat {
	return &procwatch.Stat{PeakRise: 30, DurationSec: 30, TransientScore: 0, Observations: 3, LastSeen: time.Now().Unix()}
}

func predictionConfig(t *testing.T, model procwatch.Model) config.ProcessPrediction {
	t.Helper()
	dir := t.TempDir()
	pp := config.ProcessPrediction{
		Enabled:         true,
		ProcPath:        filepath.Join(dir, "proc"),
		ModelPath:       filepath.Join(dir, "model.json"),
		MinObservations: 2,
		ImpactThreshold: 3,
		PreemptTTL:      config.Duration(60 * time.Second),
		HoldMargin:      1.25,
		Decay:           0.3,
	}
	if err := os.MkdirAll(pp.ProcPath, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp.ModelPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return pp
}

func spawn(t *testing.T, procPath, pid, name string) {
	t.Helper()
	d := filepath.Join(procPath, pid)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "comm"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withWatcher(t *testing.T, cfg *config.Config, model procwatch.Model) (*Controller, *fakeDS, *fakeIPMI) {
	t.Helper()
	cfg.ProcessPrediction = predictionConfig(t, model)
	c, ds, fi := newHarness(t, cfg)
	pp := cfg.ProcessPrediction
	c.watcher = procwatch.New(procwatch.Options{
		ProcPath:        pp.ProcPath,
		ModelPath:       pp.ModelPath,
		MinObservations: pp.MinObservations,
		ImpactThreshold: pp.ImpactThreshold,
		PreemptTTL:      pp.PreemptTTL.Std(),
		HoldMargin:      pp.HoldMargin,
		Decay:           pp.Decay,
	}, cfg.FanCurve, cfg.MinFanSpeed, c.log)
	return c, ds, fi
}

func TestPollHoldsSpeedForAKnownTransient(t *testing.T) {
	cfg := baseConfig()
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{"burst": transientStat()})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "100", "burst")
	c.poll(ctx)

	ds.temps["cpu_temp"] = 60
	c.poll(ctx)
	if fi.lastSpeed() != 20 {
		t.Errorf("speed = %d, want the hold at 20 inside the learned envelope", fi.lastSpeed())
	}

	ds.temps["cpu_temp"] = 80
	c.poll(ctx)
	if fi.lastSpeed() != 94 {
		t.Errorf("speed = %d, want the curve once the envelope is exceeded", fi.lastSpeed())
	}
}

func TestPollRaisesTheFloorForAKnownSustainedLoad(t *testing.T) {
	cfg := baseConfig()
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{"render": sustainedStat()})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.poll(ctx)

	if fi.lastSpeed() != 67 {
		t.Errorf("speed = %d, want the pre-warm floor of 67", fi.lastSpeed())
	}
}

func TestPollFloorWinsOverSmoothing(t *testing.T) {
	cfg := baseConfig()
	cfg.Smoothing = config.Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 10, MaxStepDown: 5, UrgentTemp: 85}
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{"render": sustainedStat()})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.poll(ctx)

	if fi.lastSpeed() != 67 {
		t.Errorf("speed = %d, want the floor to override the slew limit", fi.lastSpeed())
	}
}

func TestPollFloorWinsOverHold(t *testing.T) {
	cfg := baseConfig()
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{"burst": transientStat(), "render": sustainedStat()})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "100", "burst")
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.poll(ctx)
	ds.temps["cpu_temp"] = 60
	c.poll(ctx)

	if fi.lastSpeed() != 67 {
		t.Errorf("speed = %d, want the floor to beat the hold", fi.lastSpeed())
	}
	if fi.count("SetSpeed") != 2 {
		t.Errorf("speeds = %v, want only the curve then the floor", fi.speeds)
	}
}

func TestPollHoldNeverDropsBelowMinFanSpeed(t *testing.T) {
	cfg := baseConfig()
	cfg.MinFanSpeed = 35
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{"burst": transientStat()})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "100", "burst")
	c.poll(ctx)
	ds.temps["cpu_temp"] = 60
	c.poll(ctx)

	for _, s := range fi.speeds {
		if s < 35 {
			t.Errorf("speeds = %v, want none below min_fan_speed", fi.speeds)
			break
		}
	}
	if fi.lastSpeed() != 35 {
		t.Errorf("speed = %d, want the hold clamped to min_fan_speed", fi.lastSpeed())
	}
}

func TestPollIgnoresUnknownProcesses(t *testing.T) {
	cfg := baseConfig()
	c, ds, fi := withWatcher(t, cfg, procwatch.Model{})
	ctx := context.Background()

	ds.temps["cpu_temp"] = 40
	c.poll(ctx)
	spawn(t, cfg.ProcessPrediction.ProcPath, "300", "unknown")
	ds.temps["cpu_temp"] = 65
	c.poll(ctx)

	if fi.lastSpeed() != 55 {
		t.Errorf("speed = %d, want the plain curve for an unlearned process", fi.lastSpeed())
	}
}

func TestNewWiresTheProcessWatcher(t *testing.T) {
	cfg := baseConfig()
	cfg.ProcessPrediction = predictionConfig(t, procwatch.Model{"render": sustainedStat()})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	c := New(cfg, log)

	if c.watcher == nil {
		t.Fatal("want a watcher when process_prediction is enabled")
	}
	if c.ds == nil || c.ipmi == nil {
		t.Error("want the datasource and ipmi clients built")
	}
	if c.statePath != stateFile || c.retryPause != retryPauseDelay || c.lastSpeed != -1 {
		t.Errorf("statePath=%q retryPause=%v lastSpeed=%d, want the defaults", c.statePath, c.retryPause, c.lastSpeed)
	}

	c.watcher.Poll(time.Now(), 40)
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.watcher.Poll(time.Now(), 40)
	if f := c.watcher.Floor(); f != 67 {
		t.Errorf("Floor() = %d, want 67 from the configured model, proc path and curve", f)
	}
}

func TestNewPassesMinObservationsToTheWatcher(t *testing.T) {
	cfg := baseConfig()
	cfg.ProcessPrediction = predictionConfig(t, procwatch.Model{"render": sustainedStat()})
	cfg.ProcessPrediction.MinObservations = 5

	c := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	c.watcher.Poll(time.Now(), 40)
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.watcher.Poll(time.Now(), 40)
	if f := c.watcher.Floor(); f != 0 {
		t.Errorf("Floor() = %d, want 0 while below min_observations", f)
	}
}

func TestNewPassesMinFanSpeedToTheWatcher(t *testing.T) {
	cfg := baseConfig()
	cfg.MinFanSpeed = 80
	stat := sustainedStat()
	stat.PeakRise = 5
	cfg.ProcessPrediction = predictionConfig(t, procwatch.Model{"render": stat})

	c := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	c.watcher.Poll(time.Now(), 40)
	spawn(t, cfg.ProcessPrediction.ProcPath, "200", "render")
	c.watcher.Poll(time.Now(), 40)
	if f := c.watcher.Floor(); f != 80 {
		t.Errorf("Floor() = %d, want it clamped up to min_fan_speed 80", f)
	}
}

func TestNewWithoutProcessPrediction(t *testing.T) {
	c := New(baseConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if c.watcher != nil {
		t.Error("want no watcher when process_prediction is disabled")
	}
}
