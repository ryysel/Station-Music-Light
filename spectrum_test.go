package main

import (
	"math"
	"testing"
)

func TestToneLightsItsZone(t *testing.T) {
	for zone, freq := range []float64{80, 900, 8000} {
		var s spectrum
		for i := range s.pcmWindow {
			s.pcmWindow[i] = .6 * math.Sin(2*math.Pi*freq*float64(i)/48000)
			s.pcmRightWindow[i] = -s.pcmWindow[i]
		}
		for i := 0; i < 10; i++ {
			s.analyzeFrequencyRanges()
		}
		led := frame([3]float64{1, 0, 0}, &s, true)
		for i := 0; i < 3; i++ {
			if led[2*i] != led[2*i+1] || i != zone && led[2*i] >= led[2*zone] {
				t.Fatalf("tone %.0f Hz: %v", freq, led)
			}
		}
	}
}
