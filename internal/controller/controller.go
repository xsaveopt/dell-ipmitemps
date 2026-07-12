package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
	"github.com/xsaveopt/dell-ipmitemps/internal/curve"
	"github.com/xsaveopt/dell-ipmitemps/internal/datasource"
	"github.com/xsaveopt/dell-ipmitemps/internal/ipmi"
	"github.com/xsaveopt/dell-ipmitemps/internal/predict"
	"github.com/xsaveopt/dell-ipmitemps/internal/procwatch"
)

const stateFile = "/run/dellipmifanctl.state"

type Controller struct {
	cfg     *config.Config
	ds      *datasource.Fetcher
	ipmi    *ipmi.Controller
	watcher *procwatch.Watcher
	log     *slog.Logger

	consecutiveFailures int
	inAutoMode          bool
	inFailSafe          bool
	lastSpeed           int
}

func New(cfg *config.Config, log *slog.Logger) *Controller {
	c := &Controller{
		cfg:       cfg,
		ds:        datasource.New(cfg.Datasource),
		ipmi:      ipmi.New(cfg.IPMI),
		log:       log,
		lastSpeed: -1,
	}
	if cfg.ProcessPrediction.Enabled {
		pp := cfg.ProcessPrediction
		c.watcher = procwatch.New(procwatch.Options{
			ProcPath:        pp.ProcPath,
			ModelPath:       pp.ModelPath,
			MinObservations: pp.MinObservations,
			ImpactThreshold: pp.ImpactThreshold,
			PreemptTTL:      pp.PreemptTTL.Std(),
			HoldMargin:      pp.HoldMargin,
			Decay:           pp.Decay,
		}, cfg.FanCurve, cfg.MinFanSpeed, log)
	}
	return c
}

func (c *Controller) Run(ctx context.Context) error {
	c.restoreState()

	if err := c.ipmi.Ping(ctx); err != nil {
		return fmt.Errorf("unable to reach BMC via ipmitool — check ipmi.* config: %w", err)
	}

	defer c.restoreAuto()

	if c.cfg.DisablePCIeCoolingResponse {
		c.log.Info("disabling BMC PCIe cooling response")
		if err := c.ipmi.DisablePCIeCooling(ctx); err != nil {
			c.log.Warn("failed to disable PCIe cooling response", "err", err)
		}
	}

	if err := c.ipmi.SetManual(ctx); err != nil {
		return fmt.Errorf("enabling manual fan control: %w", err)
	}
	c.log.Info("manual fan control enabled")

	ticker := time.NewTicker(time.Duration(c.cfg.PollInterval) * time.Second)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return nil
		}
		c.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Controller) poll(ctx context.Context) {
	readings := make([]curve.Reading, 0, len(c.cfg.Sensors))
	critical := false
	maxTemp := 0.0

	for _, s := range c.cfg.Sensors {
		temp, crit, err := c.readSensor(ctx, s)
		if err != nil {
			c.log.Warn("sensor fetch failed", "sensor", s.Name, "reason", err.Error(), "query", s.Query)
			c.handleFetchFailure(ctx)
			return
		}
		if crit {
			critical = true
		}
		if temp > maxTemp {
			maxTemp = temp
		}
		readings = append(readings, curve.Reading{Name: s.Name, Temp: temp, Weight: s.Weight})
	}

	c.consecutiveFailures = 0

	if critical {
		c.fallbackToBMC(ctx, "critical temp detected")
		return
	}

	if c.inAutoMode {
		c.log.Info("temperatures normal — resuming manual control")
		if err := c.ipmi.SetManual(ctx); err != nil {
			c.log.Warn("failed to resume manual control", "err", err)
			return
		}
		if c.cfg.DisablePCIeCoolingResponse {
			if err := c.ipmi.DisablePCIeCooling(ctx); err != nil {
				c.log.Warn("failed to re-disable PCIe cooling response", "err", err)
			}
		}
		c.inAutoMode = false
		c.lastSpeed = -1
	}

	if c.inFailSafe {
		c.log.Info("sensor data restored — resuming curve control")
		c.inFailSafe = false
		c.lastSpeed = -1
	}

	target := curve.ComputeFanSpeed(c.cfg.FanCurve, c.cfg.MinFanSpeed, readings)

	if c.watcher != nil {
		c.watcher.Poll(time.Now(), maxTemp)
		if held, ok := c.watcher.Hold(maxTemp); ok && held < target {
			c.log.Info("holding fan speed for known transient", "percent", held, "curve_target", target)
			target = held
		}
		if floor := c.watcher.Floor(); floor > target {
			c.log.Info("raising fan floor for predicted load", "percent", floor, "curve_target", target)
			target = floor
		}
	}

	if target == c.lastSpeed {
		c.log.Debug("fan speed unchanged", "percent", target)
		return
	}

	c.log.Info("setting fan speed", "percent", target)
	if err := c.ipmi.SetSpeed(ctx, target); err != nil {
		c.log.Warn("failed to set fan speed", "err", err)
		return
	}
	c.lastSpeed = target
	c.saveState(target)
}

func (c *Controller) readSensor(ctx context.Context, s config.Sensor) (float64, bool, error) {
	if !c.cfg.Prediction.Enabled {
		temp, err := c.ds.FetchTemp(ctx, s.Query)
		if err != nil {
			return 0, false, err
		}
		c.log.Info("sensor reading", "sensor", s.Name, "temp_c", temp, "weight", s.Weight)
		crit := temp >= c.cfg.TempCritical
		if crit {
			c.log.Warn("critical temperature", "sensor", s.Name, "temp_c", temp, "limit_c", c.cfg.TempCritical)
		}
		return temp, crit, nil
	}

	p := c.cfg.Prediction
	samples, err := c.ds.FetchRange(ctx, s.Query, p.Lookback.Std(), p.Step.Std())
	if err != nil {
		return 0, false, err
	}
	eff := predict.Effective(samples, p.Quantile, p.TrendHorizon.Std().Seconds())
	crit := predict.SustainedAbove(samples, c.cfg.TempCritical, p.CriticalDwell.Std().Seconds())
	c.log.Info("sensor reading", "sensor", s.Name,
		"effective_c", eff, "last_c", predict.Last(samples), "samples", len(samples), "weight", s.Weight)
	if crit {
		c.log.Warn("critical temperature sustained", "sensor", s.Name, "limit_c", c.cfg.TempCritical, "dwell_s", p.CriticalDwell.Std().Seconds())
	}
	return eff, crit, nil
}

func (c *Controller) handleFetchFailure(ctx context.Context) {
	c.consecutiveFailures++
	c.log.Warn("fetch failed", "failures", c.consecutiveFailures, "max", c.cfg.MaxFetchFailures)
	if c.inFailSafe || c.consecutiveFailures < c.cfg.MaxFetchFailures {
		return
	}

	speed := c.cfg.FetchFailureFanSpeed
	c.log.Warn("too many consecutive fetch failures — pinning fans to fail-safe speed", "percent", speed)
	if err := c.ipmi.SetManual(ctx); err != nil {
		c.log.Warn("failed to ensure manual fan control for fail-safe", "err", err)
	}
	if err := c.ipmi.SetSpeed(ctx, speed); err != nil {
		c.log.Warn("failed to set fail-safe fan speed", "err", err)
		return
	}
	c.inAutoMode = false
	c.inFailSafe = true
	c.lastSpeed = speed
	c.saveState(speed)
}

func (c *Controller) handBackToBMC(ctx context.Context) {
	c.withRetry("set BMC auto mode", func() error { return c.ipmi.SetAuto(ctx) })
	c.withRetry("restore BMC PCIe cooling response", func() error { return c.ipmi.EnablePCIeCooling(ctx) })
}

func (c *Controller) withRetry(op string, fn func() error) {
	const attempts = 3
	var err error
	for i := 1; i <= attempts; i++ {
		if err = fn(); err == nil {
			return
		}
		c.log.Warn("ipmi command failed, retrying", "op", op, "attempt", i, "err", err)
		if i < attempts {
			time.Sleep(500 * time.Millisecond)
		}
	}
	c.log.Error("ipmi command failed after retries", "op", op, "err", err)
}

func (c *Controller) fallbackToBMC(ctx context.Context, reason string) {
	if c.inAutoMode {
		return
	}
	c.log.Warn("handing control back to BMC", "reason", reason)
	c.handBackToBMC(ctx)
	c.inAutoMode = true
	c.inFailSafe = false
}

func (c *Controller) restoreAuto() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.log.Info("restoring BMC automatic fan control")
	c.handBackToBMC(ctx)
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		c.log.Warn("failed to remove state file", "file", stateFile, "err", err)
	}
}

func (c *Controller) restoreState() {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	raw := strings.TrimSpace(string(data))
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 || v > 100 {
		c.log.Warn("state file has invalid value; ignoring", "file", stateFile, "value", raw)
		_ = os.Remove(stateFile)
		return
	}
	c.lastSpeed = v
	c.log.Info("restored last speed from state", "percent", v)
}

func (c *Controller) saveState(v int) {
	if err := os.WriteFile(stateFile, []byte(strconv.Itoa(v)), 0o644); err != nil {
		c.log.Warn("failed to write state file", "file", stateFile, "err", err)
	}
}
