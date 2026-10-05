package logattr

import (
	"testing"
	"time"
)

func TestSeconds(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want float64
	}{
		{0, 0},
		{1234567 * time.Microsecond, 1.234},
		{30 * time.Second, 30},
		{999 * time.Microsecond, 0},
	} {
		if got := Seconds("k", tc.d).Value.Float64(); got != tc.want {
			t.Errorf("Seconds(%v) = %v, want %v", tc.d, got, tc.want)
		}
	}
}

func TestSecondsMicro(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want float64
	}{
		{0, 0},
		{12345678 * time.Nanosecond, 0.012345},
		{2*time.Second + 500*time.Microsecond, 2.0005},
	} {
		if got := SecondsMicro("k", tc.d).Value.Float64(); got != tc.want {
			t.Errorf("SecondsMicro(%v) = %v, want %v", tc.d, got, tc.want)
		}
	}
}
