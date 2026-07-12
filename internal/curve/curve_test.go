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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeFanSpeed(testCurve, minSpeed, tc.readings); got != tc.want {
				t.Errorf("ComputeFanSpeed = %d, want %d", got, tc.want)
			}
		})
	}
}
