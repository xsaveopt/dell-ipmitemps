package predict

import (
	"math"
	"testing"

	"github.com/xsaveopt/dell-ipmitemps/internal/datasource"
)

const step = 10.0

func mk(vals ...float64) []datasource.Sample {
	s := make([]datasource.Sample, len(vals))
	for i, v := range vals {
		s[i] = datasource.Sample{T: float64(i) * step, V: v}
	}
	return s
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestQuantile(t *testing.T) {
	s := mk(10, 20, 30, 40, 50)
	if got := Quantile(s, 0.5); !approx(got, 30) {
		t.Errorf("median = %v, want 30", got)
	}
	if got := Quantile(s, 0); !approx(got, 10) {
		t.Errorf("q0 = %v, want 10", got)
	}
	if got := Quantile(s, 1); !approx(got, 50) {
		t.Errorf("q1 = %v, want 50", got)
	}
	if got := Quantile(nil, 0.9); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestQuantileRejectsSingleSpike(t *testing.T) {
	// One 80°C sample among many ~40°C readings: a high quantile stays near baseline.
	s := mk(40, 41, 39, 40, 80, 40, 41, 40, 39, 40)
	if got := Quantile(s, 0.9); got > 50 {
		t.Errorf("q0.9 = %v, expected the lone spike to be rejected (<50)", got)
	}
}

func TestSlope(t *testing.T) {
	if got := Slope(mk(0, 10, 20, 30)); !approx(got, 1) {
		t.Errorf("rising slope = %v, want 1 (°/s)", got)
	}
	if got := Slope(mk(30, 20, 10, 0)); !approx(got, -1) {
		t.Errorf("falling slope = %v, want -1", got)
	}
	if got := Slope(mk(50, 50, 50)); !approx(got, 0) {
		t.Errorf("flat slope = %v, want 0", got)
	}
	if got := Slope(mk(42)); got != 0 {
		t.Errorf("single-point slope = %v, want 0", got)
	}
}

func TestEffectiveTransientIgnored(t *testing.T) {
	s := mk(40, 40, 40, 75, 40, 40, 40, 40, 40, 40)
	if got := Effective(s, 0.9, 30); got > 50 {
		t.Errorf("effective = %v, transient spike should not drive it up", got)
	}
}

func TestEffectiveSustainedRiseRampsEarly(t *testing.T) {
	s := mk(40, 44, 48, 52, 56, 60)
	// Slope is 0.4°/s; over a 30s horizon that projects ~12° above the last sample.
	if got := Effective(s, 0.9, 30); got < 70 {
		t.Errorf("effective = %v, sustained rise should project ahead (>=70)", got)
	}
}

func TestEffectiveSpikeOnNewestSampleIgnored(t *testing.T) {
	s := mk(40, 40, 41, 40, 39, 40, 41, 40, 40, 75)
	if got := Effective(s, 0.9, 30); got > 50 {
		t.Errorf("effective = %v, a spike on the last sample should not drive it up", got)
	}
}

func TestEffectiveStepChangeIsFollowed(t *testing.T) {
	// Not a blip: the temperature steps up and stays there for most of the window.
	s := mk(40, 40, 41, 62, 63, 62, 63, 64, 63, 64)
	if got := Effective(s, 0.6, 30); got < 60 {
		t.Errorf("effective = %v, a sustained step should be followed (>=60)", got)
	}
}

func TestTrendEndpointResistsOutlier(t *testing.T) {
	flat := mk(40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	spiked := mk(40, 40, 40, 40, 40, 40, 40, 40, 40, 80)
	_, clean := Trend(flat)
	sl, dirty := Trend(spiked)
	if math.Abs(dirty-clean) > 1 {
		t.Errorf("fitted end moved from %v to %v on one outlier", clean, dirty)
	}
	if sl > 0.01 {
		t.Errorf("slope = %v, one outlier should not create a trend", sl)
	}
}

func TestTrendTracksSustainedClimb(t *testing.T) {
	sl, end := Trend(mk(40, 44, 48, 52, 56, 60))
	if !approx(sl, 0.4) {
		t.Errorf("slope = %v, want 0.4", sl)
	}
	if !approx(end, 60) {
		t.Errorf("fitted end = %v, want 60", end)
	}
}

func TestSustainedAbove(t *testing.T) {
	// Brief spike: only one recent sample above 90 → not sustained.
	brief := mk(40, 40, 40, 40, 95)
	if SustainedAbove(brief, 90, 15) {
		t.Error("brief spike should not count as sustained")
	}
	// Genuine excursion across the whole dwell window.
	held := mk(40, 40, 40, 95, 96)
	if !SustainedAbove(held, 90, 15) {
		t.Error("sustained excursion should be detected")
	}
}
