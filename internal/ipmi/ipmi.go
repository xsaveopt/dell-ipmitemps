package ipmi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sratabix/dell-ipmitemps/internal/config"
)

type Controller struct {
	cfg config.IPMI
}

func New(cfg config.IPMI) *Controller {
	return &Controller{cfg: cfg}
}

func (c *Controller) Ping(ctx context.Context) error {
	return c.run(ctx, "mc", "info")
}

func (c *Controller) SetManual(ctx context.Context) error {
	return c.run(ctx, "raw", "0x30", "0x30", "0x01", "0x00")
}

func (c *Controller) SetAuto(ctx context.Context) error {
	return c.run(ctx, "raw", "0x30", "0x30", "0x01", "0x01")
}

func (c *Controller) SetSpeed(ctx context.Context, pct int) error {
	return c.run(ctx, "raw", "0x30", "0x30", "0x02", "0xff", fmt.Sprintf("0x%02x", pct))
}

func (c *Controller) DisablePCIeCooling(ctx context.Context) error {
	return c.run(ctx, "raw", "0x30", "0xce", "0x00", "0x16", "0x05",
		"0x00", "0x00", "0x00", "0x05", "0x00", "0x01", "0x00", "0x00")
}

func (c *Controller) EnablePCIeCooling(ctx context.Context) error {
	return c.run(ctx, "raw", "0x30", "0xce", "0x00", "0x16", "0x05",
		"0x00", "0x00", "0x00", "0x05", "0x00", "0x00", "0x00", "0x00")
}

func (c *Controller) run(ctx context.Context, args ...string) error {
	full := args
	if !c.cfg.Local {
		full = append([]string{"-I", "lanplus", "-H", c.cfg.Host, "-U", c.cfg.User, "-P", c.cfg.Pass}, args...)
	}

	out, err := exec.CommandContext(ctx, "ipmitool", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ipmitool %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
