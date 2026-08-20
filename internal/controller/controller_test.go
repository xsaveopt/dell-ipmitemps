package controller

import (
	"testing"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
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
