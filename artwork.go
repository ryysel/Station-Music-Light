package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Mini 2's directive snapshot wraps the current audio_play JSON at 1/2/6.
// Only the public cover identifier leaves the device. Never log the directive:
// the same envelope contains credentials for the audio stream.
func directiveBytes(data []byte, field uint64) []byte {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag == 0 {
			return nil
		}
		data = data[n:]
		switch tag & 7 {
		case 0:
			_, n = binary.Uvarint(data)
			if n <= 0 {
				return nil
			}
			data = data[n:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 || size > uint64(len(data)-n) {
				return nil
			}
			data = data[n:]
			if tag>>3 == field {
				return data[:size]
			}
			data = data[size:]
		case 1:
			if len(data) < 8 {
				return nil
			}
			data = data[8:]
		case 5:
			if len(data) < 4 {
				return nil
			}
			data = data[4:]
		default:
			return nil
		}
	}
	return nil
}

var musicCoverPath = regexp.MustCompile(`^avatars\.yandex\.net/get-music-content/[0-9]+/[A-Za-z0-9._-]+/%%$`)

func directiveCover(data []byte) string {
	var track struct {
		Screen   string `json:"screen_type"`
		Paused   bool   `json:"set_pause"`
		Metadata struct {
			Art string `json:"art_image_url"`
		} `json:"metadata"`
	}
	payload := directiveBytes(directiveBytes(directiveBytes(data, 1), 2), 6)
	if json.Unmarshal(payload, &track) != nil || track.Screen != "Music" || track.Paused || !musicCoverPath.MatchString(track.Metadata.Art) {
		return ""
	}
	return "https://" + strings.TrimSuffix(track.Metadata.Art, "%%") + "100x100"
}
func nativeCoverURL() string {
	paths, _ := filepath.Glob("/proc/asound/card*/pcm*p/sub*/status")
	for _, path := range paths {
		status, _ := os.ReadFile(path)
		if !bytes.HasPrefix(status, []byte("state: RUNNING\n")) {
			continue
		}
		const state = "/tmp/directiveSequencerState.proto"
		info, err := os.Lstat(state)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 256<<10 {
			return ""
		}
		f, err := os.Open(state)
		if err != nil {
			return ""
		}
		defer f.Close()
		data, err := readAllLimited(f, 256<<10)
		if err != nil {
			return ""
		}
		return directiveCover(data)
	}
	return ""
}

type nativeArtwork struct{ color atomic.Pointer[[3]float64] }

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("image exceeds size limit")
	}
	return b, err
}

// Pick the most populated colour neighbourhood rather than averaging
// complementary colours into grey. Sample at most a 64x64 grid.
func coverColor(r io.Reader) ([3]float64, bool) {
	var zero [3]float64
	b, err := readAllLimited(r, 1<<20)
	if err != nil {
		return zero, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 1024*1024/cfg.Height {
		return zero, false
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return zero, false
	}
	type cell struct {
		count int
		sum   [3]float64
	}
	var bins [512]cell
	best := 0
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y += max(1, (cfg.Height+63)/64) {
		for x := bounds.Min.X; x < bounds.Max.X; x += max(1, (cfg.Width+63)/64) {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			i := (r>>13)<<6 | (g>>13)<<3 | b>>13
			slot := &bins[i]
			slot.count++
			for j, v := range [3]uint32{r, g, b} {
				slot.sum[j] += float64(v) / 65535
			}
			if slot.count > bins[best].count {
				best = int(i)
			}
		}
	}
	if bins[best].count == 0 {
		return zero, false
	}
	rgb := bins[best].sum
	for j := range rgb {
		rgb[j] /= float64(bins[best].count)
	}
	return rgb, true
}

func (a *nativeArtwork) watch(ctx context.Context) {
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var cached, attempt string
	var color *[3]float64
	var retry time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			url := nativeCoverURL()
			if url != "" && url == cached {
				a.color.Store(color)
				continue
			}
			a.color.Store(nil)
			if url == "" || (url == attempt && now.Before(retry)) {
				continue
			}
			attempt = url
			retry = now.Add(15 * time.Second)
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				continue
			}
			rgb, ok := coverColor(resp.Body)
			resp.Body.Close()
			if !ok {
				continue
			}
			color = &rgb
			cached = url
			if nativeCoverURL() == url {
				a.color.Store(color)
			}
		}
	}
}
