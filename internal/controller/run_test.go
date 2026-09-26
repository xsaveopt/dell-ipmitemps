package controller

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type cancelAfterFetch struct {
	*fakeDS
	cancel context.CancelFunc
}

func (d cancelAfterFetch) FetchTemp(ctx context.Context, query string) (float64, error) {
	v, err := d.fakeDS.FetchTemp(ctx, query)
	d.cancel()
	return v, err
}

func runOnePoll(t *testing.T, c *Controller, ds *fakeDS) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ds = cancelAfterFetch{fakeDS: ds, cancel: cancel}
	return c.Run(ctx)
}

func sameCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ipmi log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q (log %v)", i, got[i], want[i], got)
		}
	}
}

func TestRunFailsWhenTheBMCIsUnreachable(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	fi.failAll["Ping"] = true
	c.saveState(40)

	err := c.Run(context.Background())

	if err == nil {
		t.Fatal("want an error when Ping fails")
	}
	sameCalls(t, fi.log, []string{"Ping"})
	if got := readState(t, c); got != "40" {
		t.Errorf("state file = %q, want it left alone when control was never taken", got)
	}
}

func TestRunFailsAndHandsBackWhenManualCannotBeEnabled(t *testing.T) {
	c, _, fi := newHarness(t, baseConfig())
	fi.failAll["SetManual"] = true

	err := c.Run(context.Background())

	if err == nil || !strings.Contains(err.Error(), "SetManual refused") {
		t.Fatalf("err = %v, log = %v, want a SetManual failure", err, fi.log)
	}
	if fi.count("SetSpeed") != 0 {
		t.Error("must not drive fans when manual control was refused")
	}
	if fi.count("SetAuto") != 1 || fi.count("EnablePCIeCooling") != 1 {
		t.Errorf("ipmi log = %v, want the deferred hand-back to the BMC", fi.log)
	}
}

func TestRunDisablesPCIeCoolingBeforeManual(t *testing.T) {
	cfg := baseConfig()
	cfg.DisablePCIeCoolingResponse = true
	c, ds, fi := newHarness(t, cfg)
	ds.temps["cpu_temp"] = 65

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sameCalls(t, fi.log, []string{"Ping", "DisablePCIeCooling", "SetManual", "SetSpeed", "SetAuto", "EnablePCIeCooling"})
}

func TestRunContinuesWhenPCIeCoolingCannotBeDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.DisablePCIeCoolingResponse = true
	c, ds, fi := newHarness(t, cfg)
	fi.failAll["DisablePCIeCooling"] = true
	ds.temps["cpu_temp"] = 65

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v, want a failed DisablePCIeCooling to be only a warning", err)
	}
	if fi.lastSpeed() != 55 {
		t.Errorf("speed = %d, want the curve applied despite the warning", fi.lastSpeed())
	}
}

func TestRunSkipsPCIeCoolingWhenNotConfigured(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi.count("DisablePCIeCooling") != 0 {
		t.Error("PCIe cooling must stay untouched when the knob is off")
	}
}

func TestRunRestoresAutoOnCancel(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v, want a clean exit on cancel", err)
	}

	sameCalls(t, fi.log, []string{"Ping", "SetManual", "SetSpeed", "SetAuto", "EnablePCIeCooling"})
	if _, err := os.Stat(c.statePath); err == nil {
		t.Error("want the state file removed on shutdown")
	}
}

func TestRunWithAnAlreadyCancelledContext(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.temps["cpu_temp"] = 65
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi.count("SetSpeed") != 0 {
		t.Error("must not poll once the context is already done")
	}
	if fi.count("SetAuto") != 1 {
		t.Errorf("SetAuto called %d times, want the BMC restored", fi.count("SetAuto"))
	}
}

func TestRunRestoresLastSpeedFromState(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	c.saveState(55)
	ds.temps["cpu_temp"] = 65

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi.count("SetSpeed") != 0 {
		t.Errorf("SetSpeed called %d times, want none when the restored speed matches the curve", fi.count("SetSpeed"))
	}
}

func TestRunRestoresAutoWhenPollingFails(t *testing.T) {
	c, ds, fi := newHarness(t, baseConfig())
	ds.err = errors.New("prometheus down")

	if err := runOnePoll(t, c, ds); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi.count("SetAuto") != 1 {
		t.Errorf("SetAuto called %d times, want the BMC restored after a failing poll", fi.count("SetAuto"))
	}
}
