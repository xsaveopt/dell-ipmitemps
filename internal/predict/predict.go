package predict

import (
	"math"
	"sort"

	"github.com/xsaveopt/dell-ipmitemps/internal/datasource"
)

func Quantile(samples []datasource.Sample, q float64) float64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	vs := make([]float64, n)
	for i, s := range samples {
		vs[i] = s.V
	}
	sort.Float64s(vs)
	if n == 1 {
		return vs[0]
	}
	if q <= 0 {
		return vs[0]
	}
	if q >= 1 {
		return vs[n-1]
	}
	rank := q * float64(n-1)
	lo := int(math.Floor(rank))
	frac := rank - float64(lo)
	return vs[lo] + frac*(vs[lo+1]-vs[lo])
}

func Slope(samples []datasource.Sample) float64 {
	n := len(samples)
	if n < 2 {
		return 0
	}
	t0 := samples[0].T
	var sumT, sumV, sumTV, sumTT float64
	for _, s := range samples {
		t := s.T - t0
		sumT += t
		sumV += s.V
		sumTV += t * s.V
		sumTT += t * t
	}
	fn := float64(n)
	denom := fn*sumTT - sumT*sumT
	if denom == 0 {
		return 0
	}
	return (fn*sumTV - sumT*sumV) / denom
}

func Last(samples []datasource.Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[len(samples)-1].V
}

func Effective(samples []datasource.Sample, q, horizonSec float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	robust := Quantile(samples, q)
	slope := Slope(samples)
	if slope < 0 {
		slope = 0
	}
	projection := Last(samples) + slope*horizonSec
	return math.Max(robust, projection)
}

func SustainedAbove(samples []datasource.Sample, threshold, dwellSec float64) bool {
	n := len(samples)
	if n == 0 {
		return false
	}
	cutoff := samples[n-1].T - dwellSec
	above := false
	for _, s := range samples {
		if s.T < cutoff {
			continue
		}
		above = true
		if s.V < threshold {
			return false
		}
	}
	return above
}
