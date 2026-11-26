package main

import "math"

const analysisWindowSize = 2048

type spectrum struct {
	pcmWindow, pcmRightWindow [analysisWindowSize]float64
	bands, peaks              [8]float64
	history                   [4]float64
}

// Native-rate analysis avoids the old unfiltered 12 kHz decimator, which
// aliased treble into bass/mids and could never measure the advertised 8 kHz.
var analysisHann = func() (w [analysisWindowSize]float64) {
	for i := range w {
		w[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/float64(len(w)))
	}
	return
}()

func analysisHop(rate int) int {
	hop := rate / 50
	if hop < 1 {
		return 1
	}
	if hop > analysisWindowSize {
		return analysisWindowSize
	}
	return hop
}

// fftSpectrum is an in-place radix-2 FFT. All buffers are bounded; coefficients
// are shared by the channels, and no package-controlled code is executed.
func fftSpectrum(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		angle := -2 * math.Pi / float64(size)
		step := complex(math.Cos(angle), math.Sin(angle))
		for base := 0; base < n; base += size {
			w := complex(1, 0)
			for j := 0; j < size/2; j++ {
				a, b := x[base+j], w*x[base+j+size/2]
				x[base+j], x[base+j+size/2] = a+b, a-b
				w *= step
			}
		}
	}
}

func spectrumRangeLevel(power float64) float64 {
	if power <= 1e-12 {
		return 0
	}
	// Fixed dBFS reference, not per-band AGC: a quiet unrelated band must
	// not become "full" merely because it is the loudest noise in that band.
	db := 10 * math.Log10(power)
	return math.Max(0, math.Min(1, (db+60)/54))
}

func smoothSpectrum(previous, target, dt float64) float64 {
	tau := .16
	if target > previous {
		tau = .025
	}
	value := previous + (target-previous)*(1-math.Exp(-dt/tau))
	if target == 0 && value < .001 {
		return 0
	}
	return value
}

func (s *spectrum) analyzeFrequencyRanges() {
	const rate = 48000
	var left, right [analysisWindowSize]complex128
	windowPower := 0.0
	for i, w := range analysisHann {
		left[i] = complex(s.pcmWindow[i]*w, 0)
		right[i] = complex(s.pcmRightWindow[i]*w, 0)
		windowPower += w * w
	}
	fftSpectrum(left[:])
	fftSpectrum(right[:])
	edges := [9]float64{20, 85, 175, 350, 700, 1400, 2800, 5600, 20000}
	var energy [8]float64
	normalization := 2 / (float64(analysisWindowSize) * windowPower)
	for bin := 1; bin < analysisWindowSize/2; bin++ {
		hz := float64(bin) * float64(rate) / analysisWindowSize
		if hz < edges[0] || hz >= edges[8] {
			continue
		}
		band := 0
		for band < 7 && hz >= edges[band+1] {
			band++
		}
		l, r := left[bin], right[bin]
		energy[band] += (real(l)*real(l) + imag(l)*imag(l) + real(r)*real(r) + imag(r)*imag(r)) * .5 * normalization
	}
	dt := float64(analysisHop(rate)) / float64(rate)
	for i, power := range energy {
		s.bands[i] = smoothSpectrum(s.bands[i], spectrumRangeLevel(power), dt)
		s.peaks[i] = math.Max(s.bands[i], s.peaks[i]*math.Exp(-dt/.35))
	}
	// The three-zone EQ and four-bar telemetry use the same measured ranges,
	// not the old isolated 95/330/1050/3100 Hz probes.
	for i := range s.history {
		target := spectrumRangeLevel(energy[i*2] + energy[i*2+1])
		s.history[i] = smoothSpectrum(s.history[i], target, dt)
	}
}
