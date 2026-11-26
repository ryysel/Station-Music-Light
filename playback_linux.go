package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Use the firmware speaker mixer; never attach to a segment just because it
// has a plausible buffer size. ALSA places the mixing sums at ipc_key+1.
type playbackMix struct {
	key int
	pcm string
}

var keyField = regexp.MustCompile(`(?m)^\s*ipc_key\s+(\d+)\b`)
var hwField = regexp.MustCompile(`(?:slave\.)?pcm\s+"hw:(\d+),(\d+)(?:,(\d+))?"`)
var mixerBlock = regexp.MustCompile(`(?m)^\s*pcm\.(?:dmix_spk|dmixer)\s*\{`)

func parsePlaybackMix(text string) (playbackMix, error) {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i], _, _ = strings.Cut(l, "#")
	}
	text = strings.Join(lines, "\n")
	for _, loc := range mixerBlock.FindAllStringIndex(text, -1) {
		block := text[loc[1]:]
		depth := 1
		end := 0
		for ; end < len(block); end++ {
			if block[end] == '{' {
				depth++
			}
			if block[end] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if depth != 0 {
			continue
		}
		block = block[:end]
		k := keyField.FindStringSubmatch(block)
		h := hwField.FindStringSubmatch(block)
		if len(k) == 0 || len(h) == 0 {
			continue
		}
		key, err := strconv.Atoi(k[1])
		if err != nil || key < 1 || key >= 1<<30 {
			continue
		}
		sub := h[3]
		if sub == "" {
			sub = "0"
		}
		return playbackMix{key + 1, fmt.Sprintf("/proc/asound/card%s/pcm%sp/sub%s/", h[1], h[2], sub)}, nil
	}
	return playbackMix{}, fmt.Errorf("unsupported ALSA playback mixer")
}
func (m playbackMix) stockPlayerAlone() bool {
	data, err := os.ReadFile("/proc/sysvipc/shm")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || f[0] != strconv.Itoa(m.key) || f[6] != "1" {
			continue
		}
		pid, err := strconv.Atoi(f[4])
		if err != nil || pid <= 0 {
			return false
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		return err == nil && strings.HasSuffix(exe, "/quasar/maind")
	}
	return false
}
func (m playbackMix) sample(s *spectrum, envelope *float64) bool {
	pcm := m.pcm
	params, err := os.ReadFile(pcm + "hw_params")
	if err != nil {
		return false
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(params), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if ok {
			fields[key] = value
		}
	}
	frames, err := strconv.Atoi(fields["buffer_size"])
	if err != nil || frames < analysisWindowSize || frames > 262144 || fields["channels"] != "2" {
		return false
	}
	switch fields["format"] {
	case "S16_LE", "S32_LE":
	default:
		return false
	}
	rateText, _, _ := strings.Cut(fields["rate"], " ")
	rate, err := strconv.Atoi(rateText)
	if err != nil || rate < 22050 || rate > 192000 {
		return false
	}
	s.rate = rate
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
	size := frames * 2 * 4
	id, err := unix.SysvShmGet(m.key, size, 0)
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

	start := (int(cursor%uint64(frames)) + frames - analysisWindowSize) % frames
	peak := 0.0
	for i := range s.pcmWindow {
		offset := ((start + i) % frames) * 8
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
