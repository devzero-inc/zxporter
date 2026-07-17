package nodemon

import (
	"math"
	"testing"
)

func TestClampFloatToUint64(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want uint64
	}{
		{name: "NaN", in: math.NaN(), want: 0},
		{name: "+Inf", in: math.Inf(1), want: 0},
		{name: "-Inf", in: math.Inf(-1), want: 0},
		{name: "negative", in: -42.5, want: 0},
		{name: "zero", in: 0, want: 0},
		{name: "positive truncates", in: 1234.9, want: 1234},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampFloatToUint64(tt.in); got != tt.want {
				t.Errorf("clampFloatToUint64(%v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
