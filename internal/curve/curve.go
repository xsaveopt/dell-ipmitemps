package curve

import "github.com/xsaveopt/dell-ipmitemps/internal/config"

type Reading struct {
	Name   string
	Temp   float64
	Weight float64
}

func Speed(points []config.CurvePoint, temp float64) int {
	n := len(points)
	if n == 0 {
		return 0
	}
	if temp <= points[0].Temp {
		return points[0].Percent
	}
	if temp >= points[n-1].Temp {
		return 100
	}
	for i := 1; i < n; i++ {
		if temp <= points[i].Temp {
			t0, t1 := points[i-1].Temp, points[i].Temp
			s0, s1 := float64(points[i-1].Percent), float64(points[i].Percent)
			return int(s0 + (temp-t0)/(t1-t0)*(s1-s0))
		}
	}
	return points[n-1].Percent
}

func ComputeFanSpeed(points []config.CurvePoint, minSpeed int, readings []Reading) int {
	maxSpeed := minSpeed
	for _, r := range readings {
		raw := Speed(points, r.Temp)
		weighted := int(float64(minSpeed) + (float64(raw)-float64(minSpeed))*r.Weight)
		if weighted > maxSpeed {
			maxSpeed = weighted
		}
	}
	if maxSpeed > 100 {
		maxSpeed = 100
	}
	return maxSpeed
}
