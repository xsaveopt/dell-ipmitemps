package curve

import (
	"testing"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
)

var testCurve = []config.CurvePoint{
	{Temp: 30, Percent: 10},
	{Temp: 50, Percent: 30},
	{Temp: 65, Percent: 55},
	{Temp: 75, Percent: 80},
	{Temp: 82, Percent: 100},
}

func TestSpeed(t *testing.T) {
	cases := []struct {
		temp float64
		want int
	}{
		{20, 10},
		{30, 10},
		{40, 20},
		{50, 30},
		{70, 67},
		{82, 100},
		{95, 100},
	}
	for _, tc := range cases {
		if got := Speed(testCurve, tc.temp); got != tc.want {
			t.Errorf("Speed(%g) = %d, want %d", tc.temp, got, tc.want)
		}
	}
}

func TestSpeedEmpty(t *testing.T) {
	if got := Speed(nil, 50); got != 0 {
		t.Errorf("Speed(nil) = %d, want 0", got)
	}
}

func TestComputeFanSpeed(t *testing.T) {
	const minSpeed = 10

	cases := []struct {
		name     string
		readings []Reading
		want     int
	}{
		{
			name:     "no readings floors at minimum",
			readings: nil,
			want:     minSpeed,
		},
		{
			name:     "weight 1.0 uses curve as-is",
			readings: []Reading{{Name: "cpu", Temp: 50, Weight: 1.0}},
			want:     30,
		},
		{
			name:     "weight softens toward minimum",
			readings: []Reading{{Name: "nvme", Temp: 50, Weight: 0.8}},
			want:     26,
		},
		{
			name: "max across sensors wins",
			readings: []Reading{
				{Name: "cpu", Temp: 50, Weight: 1.0},
				{Name: "gpu", Temp: 75, Weight: 1.0},
			},
			want: 80,
		},
		{
			name:     "clamped to 100",
			readings: []Reading{{Name: "cpu", Temp: 82, Weight: 1.5}},
			want:     100,
		},
		{
			name:     "half weight halves the rise above minimum",
			readings: []Reading{{Name: "nvme", Temp: 75, Weight: 0.5}},
			want:     45,
		},
		{
			name: "low weight sensor does not beat a full weight one",
			readings: []Reading{
				{Name: "nvme", Temp: 82, Weight: 0.2},
				{Name: "cpu", Temp: 65, Weight: 1.0},
			},
			want: 55,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeFanSpeed(testCurve, minSpeed, tc.readings); got != tc.want {
				t.Errorf("ComputeFanSpeed = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestComputeFanSpeedWeightBelowOneNeverDropsUnderMinimum(t *testing.T) {
	readings := []Reading{{Name: "nvme", Temp: 30, Weight: 0.5}}
	if got := ComputeFanSpeed(testCurve, 40, readings); got != 40 {
		t.Errorf("ComputeFanSpeed = %d, want the minimum of 40", got)
	}
}

func TestComputeFanSpeedMinimumAboveFirstPoint(t *testing.T) {
	const minSpeed = 40
	cases := []struct {
		temp float64
		want int
	}{
		{20, 40},
		{30, 40},
		{50, 40},
		{65, 55},
		{82, 100},
	}
	for _, tc := range cases {
		readings := []Reading{{Name: "cpu", Temp: tc.temp, Weight: 1.0}}
		if got := ComputeFanSpeed(testCurve, minSpeed, readings); got != tc.want {
			t.Errorf("ComputeFanSpeed at %g = %d, want %d", tc.temp, got, tc.want)
		}
	}
}

func TestSpeedSinglePoint(t *testing.T) {
	points := []config.CurvePoint{{Temp: 50, Percent: 40}}
	cases := []struct {
		temp float64
		want int
	}{
		{20, 40},
		{49.9, 40},
		{50.1, 100},
		{90, 100},
	}
	for _, tc := range cases {
		if got := Speed(points, tc.temp); got != tc.want {
			t.Errorf("Speed(%g) = %d, want %d", tc.temp, got, tc.want)
		}
	}
}

func TestSpeedNonMonotonicPercentInterpolatesEachSegment(t *testing.T) {
	points := []config.CurvePoint{
		{Temp: 30, Percent: 50},
		{Temp: 50, Percent: 20},
		{Temp: 70, Percent: 60},
	}
	cases := []struct {
		temp float64
		want int
	}{
		{20, 50},
		{40, 35},
		{50, 20},
		{60, 40},
		{70, 100},
	}
	for _, tc := range cases {
		if got := Speed(points, tc.temp); got != tc.want {
			t.Errorf("Speed(%g) = %d, want %d", tc.temp, got, tc.want)
		}
	}
}
