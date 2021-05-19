package benchmetrics

import "testing"

type key string

var counts = map[key]uint64{}

func IncCounter(name key) {
	counts[name]++
}

func ResetCounters() {
	counts = map[key]uint64{}
}

func Report(b *testing.B) {
	for name, checks := range counts {
		b.ReportMetric(float64(int(checks)/b.N), string(name)+"/op")
	}
}

const CheckExpression key = "seen"
const SliceNotValid key = "array-new"
const SliceNotSet key = "array-reset"
