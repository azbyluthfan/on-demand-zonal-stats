package query

import (
	"math"
	"testing"

	"on-demand-zonal-stats/internal/raster"
)

// reduce applies the layer scale to sums and means, never to fractions.
func TestReduceScaleAndStats(t *testing.T) {
	r := raster.Result{Sum: 30, Weight: 4, Classes: map[int64]float64{10: 1, 50: 3}}
	cases := []struct {
		m    RasterMetric
		want float64
	}{
		{RasterMetric{Stat: StatSum}, 3},
		{RasterMetric{}, 3}, // default is sum
		{RasterMetric{Stat: StatMean}, 0.75},
		{RasterMetric{Stat: StatClassFraction, Classes: []int64{50}}, 0.75},
		{RasterMetric{Stat: StatClassFraction, Classes: []int64{10, 50, 80}}, 1},
	}
	for _, c := range cases {
		if got := c.m.reduce(r, 0.1); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%+v = %v, want %v", c.m, got, c.want)
		}
	}
	var tot raster.Result
	addResult(&tot, r)
	addResult(&tot, raster.Result{Sum: 10, Weight: 4, Classes: map[int64]float64{10: 4}})
	if got := (RasterMetric{Stat: StatClassFraction, Classes: []int64{10}}).reduce(tot, 1); got != 5.0/8 {
		t.Errorf("area fraction %v, want 5/8 (coverage-weighted)", got)
	}
	if got := (RasterMetric{Stat: StatMean}).reduce(tot, 1); got != 5 {
		t.Errorf("area mean %v, want 5", got)
	}
}
