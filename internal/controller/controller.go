package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sratabix/dell-ipmitemps/internal/config"
	"github.com/sratabix/dell-ipmitemps/internal/curve"
	"github.com/sratabix/dell-ipmitemps/internal/datasource"
	"github.com/sratabix/dell-ipmitemps/internal/ipmi"
)

const stateFile = "/run/dellipmifanctl.state"

type Controller struct {
	cfg  *config.Config
	ds   *datasource.Fetcher
	ipmi *ipmi.Controller
	log  *slog.Logger

	consecutiveFailures int
	inAutoMode          bool
	lastSpeed           int
}

func New(cfg *config.Config, log *slog.Logger) *Controller {
	return &Controller{
		cfg:       cfg,
		ds:        datasource.New(cfg.Datasource),
		ipmi:      ipmi.New(cfg.IPMI),
		log:       log,
		lastSpeed: -1,
	}
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

	for _, s := range c.cfg.Sensors {
		temp, err := c.ds.FetchTemp(ctx, s.Query)
		if err != nil {
			c.log.Warn("sensor fetch failed", "sensor", s.Name, "reason", err.Error(), "query", s.Query)
			c.handleFetchFailure(ctx)
			return
		}
		c.log.Info("sensor reading", "sensor", s.Name, "temp_c", temp, "weight", s.Weight)

		if temp >= c.cfg.TempCritical {
			c.log.Warn("critical temperature", "sensor", s.Name, "temp_c", temp, "limit_c", c.cfg.TempCritical)
			critical = true
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

	target := curve.ComputeFanSpeed(c.cfg.FanCurve, c.cfg.MinFanSpeed, readings)
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

func (c *Controller) handleFetchFailure(ctx context.Context) {
	c.consecutiveFailures++
	if c.inAutoMode {
		return
	}
	c.log.Warn("fetch failed", "failures", c.consecutiveFailures, "max", c.cfg.MaxFetchFailures)
	if c.consecutiveFailures >= c.cfg.MaxFetchFailures {
		c.fallbackToBMC(ctx, "too many consecutive fetch failures")
	}
}

func (c *Controller) fallbackToBMC(ctx context.Context, reason string) {
	if c.inAutoMode {
		return
	}
	c.log.Warn("handing control back to BMC", "reason", reason)
	if err := c.ipmi.SetAuto(ctx); err != nil {
		c.log.Warn("failed to set auto mode", "err", err)
		return
	}
	if c.cfg.DisablePCIeCoolingResponse {
		if err := c.ipmi.EnablePCIeCooling(ctx); err != nil {
			c.log.Warn("failed to restore PCIe cooling response", "err", err)
		}
	}
	c.inAutoMode = true
}

func (c *Controller) restoreAuto() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.log.Info("restoring BMC automatic fan control")
	if err := c.ipmi.SetAuto(ctx); err != nil {
		c.log.Warn("failed to restore auto mode", "err", err)
	}
	if c.cfg.DisablePCIeCoolingResponse {
		c.log.Info("restoring BMC PCIe cooling response")
		if err := c.ipmi.EnablePCIeCooling(ctx); err != nil {
			c.log.Warn("failed to restore PCIe cooling response", "err", err)
		}
	}
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
