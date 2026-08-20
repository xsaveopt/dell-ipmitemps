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
	slope, _ := Trend(samples)
	return slope
}

func Trend(samples []datasource.Sample) (slope, fittedEnd float64) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	if n == 1 {
		return 0, samples[0].V
	}

	t0 := samples[0].T
	pairs := make([]float64, 0, n*(n-1)/2)
	for i := range samples {
		for j := i + 1; j < n; j++ {
			dt := samples[j].T - samples[i].T
			if dt == 0 {
				continue
			}
			pairs = append(pairs, (samples[j].V-samples[i].V)/dt)
		}
	}

	level := make([]float64, n)
	if len(pairs) > 0 {
		slope = median(pairs)
	}
	for i, s := range samples {
		level[i] = s.V - slope*(s.T-t0)
	}
	return slope, median(level) + slope*(samples[n-1].T-t0)
}

func median(vs []float64) float64 {
	n := len(vs)
	if n == 0 {
		return 0
	}
	s := make([]float64, n)
	copy(s, vs)
	sort.Float64s(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
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
	slope, fittedEnd := Trend(samples)
	if slope < 0 {
		slope = 0
	}
	projection := fittedEnd + slope*horizonSec
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
