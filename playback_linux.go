package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Mini 2's stock dmix_spk has ipc_key 1024, stereo S16_LE and a
// 7680-frame ring. ALSA keeps the signed 32-bit mixing sums at key+1.
// Read the playback ring only; the capture devices belong to Quasar.
const mini2MixFrames = 7680

func sampleMini2Mix(s *spectrum, envelope *float64) bool {
	const pcm = "/proc/asound/card0/pcm0p/sub0/"
	params, err := os.ReadFile(pcm + "hw_params")
	if err != nil {
		return false
	}
	for _, field := range []string{"format: S16_LE\n", "channels: 2\n", "rate: 48000 (48000/1)\n", "buffer_size: 7680\n"} {
		if !bytes.Contains(params, []byte(field)) {
			return false
		}
	}
	status, err := os.ReadFile(pcm + "status")
	if err != nil || !bytes.HasPrefix(status, []byte("state: RUNNING\n")) {
		return false
	}
	_, tail, ok := strings.Cut(string(status), "hw_ptr")
	if !ok {
		return false
	}
	line, _, _ := strings.Cut(tail, "\n")
	_, value, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	cursor, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return false
	}
	const size = mini2MixFrames * 2 * 4
	id, err := unix.SysvShmGet(1025, size, 0)
	if err != nil {
		return false
	}
	ring, err := unix.SysvShmAttach(id, 0, unix.SHM_RDONLY)
	if err != nil {
		return false
	}
	defer unix.SysvShmDetach(ring)
	if len(ring) != size {
		return false
	}

	start := (int(cursor%mini2MixFrames) + mini2MixFrames - analysisWindowSize) % mini2MixFrames
	peak := 0.0
	for i := range s.pcmWindow {
		offset := ((start + i) % mini2MixFrames) * 8
		l := float64(int32(binary.LittleEndian.Uint32(ring[offset:])))
		r := float64(int32(binary.LittleEndian.Uint32(ring[offset+4:])))
		peak = math.Max(peak, math.Max(math.Abs(l), math.Abs(r)))
		s.pcmWindow[i], s.pcmRightWindow[i] = l, r
	}
	// The sum ring is after the native volume control. Use one slow-release
	// reference for all bands, retaining both transients and silence.
	*envelope = math.Max(64, math.Max(peak, *envelope*.985))
	scale := .8 / *envelope
	for i := range s.pcmWindow {
		s.pcmWindow[i] *= scale
		s.pcmRightWindow[i] *= scale
	}
	s.analyzeFrequencyRanges()
	return true
}
