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
		led := frame([3]float64{1, 0, 0}, &s, true, 6)
		for i := 0; i < 3; i++ {
			if led[2*i] != led[2*i+1] || i != zone && led[2*i] >= led[2*zone] {
				t.Fatalf("tone %.0f Hz: %v", freq, led)
			}
		}
	}
}

func TestOutputGeometry(t *testing.T) {
	for _, count := range []int{3, 4, 6, 24, 89} {
		s := spectrum{history: [4]float64{1, .4, .5, .1}}
		colors := frame([3]float64{1, 0, 0}, &s, true, count)
		if len(colors) != count || colors[0] <= colors[count-1] {
			t.Fatalf("%d zones: %v", count, colors)
		}
	}
}
func TestSpeakerMixer(t *testing.T) {
	for _, tt := range []struct {
		config string
		key    int
		path   string
	}{
		{"pcm.dmix_spk {\n type dmix\n ipc_key 1024\n slave.pcm \"hw:0,0\"\n}", 1025, "/proc/asound/card0/pcm0p/sub0/"},
		{"pcm.dmixer { # speaker mixer\n ipc_key 2048\n slave { pcm \"hw:1,2,0\" }\n}", 2049, "/proc/asound/card1/pcm2p/sub0/"},
	} {
		m, err := parsePlaybackMix(tt.config)
		if err != nil || m.key != tt.key || m.pcm != tt.path {
			t.Fatalf("%v: %v", m, err)
		}
	}
	if _, err := parsePlaybackMix("pcm.mic { ipc_key 1024 }"); err == nil {
		t.Fatal("accepted capture profile")
	}
}
