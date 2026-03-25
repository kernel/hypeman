package otel

var commonDurationHistogramBuckets = []float64{
	0.001, 0.0025, 0.005, 0.010, 0.025, 0.050, 0.075,
	0.100, 0.150, 0.200, 0.300, 0.500, 0.750,
	1, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10,
	20, 30, 45, 60, 90, 120,
}

var buildDurationHistogramBuckets = []float64{
	0.100, 0.250, 0.500, 1, 2.5, 5, 10,
	20, 30, 45, 60, 90, 120, 180, 300, 600, 900, 1800,
}

// CommonDurationHistogramBuckets returns the standard duration bucket set for
// Hypeman duration histograms.
func CommonDurationHistogramBuckets() []float64 {
	return append([]float64(nil), commonDurationHistogramBuckets...)
}

// BuildDurationHistogramBuckets returns the slower-moving duration bucket set
// used for build-style operations.
func BuildDurationHistogramBuckets() []float64 {
	return append([]float64(nil), buildDurationHistogramBuckets...)
}
