package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"

	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type luna struct {
	url, token string
	client     *http.Client
}

func (l luna) post(ctx context.Context, path string, body any, out any) error {
	return l.request(ctx, "POST", path, body, out)
}
func (l luna) request(ctx context.Context, method, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.url+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Luna-Token", l.token)
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Luna HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func observeSpeech(ctx context.Context, l luna, idleUntil *atomic.Int64) {
	for ctx.Err() == nil {
		var e struct {
			Phase string `json:"phase"`
		}
		err := l.post(ctx, "/speech/next", struct{}{}, &e)
		if err == nil && e.Phase == "idle" {
			idleUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
		} else {
			idleUntil.Store(0)
		}
		delay := 100 * time.Millisecond
		if err != nil {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}
func frame(rgb [3]float64, s *spectrum, animated bool, count int) []string {
	if rgb == [3]float64{} {
		rgb = [3]float64{.22, .22, .27}
	}
	levels := [3]float64{s.history[0], math.Max(s.history[1], s.history[2]), s.history[3]}
	out := make([]string, count)
	for i := range out {
		brightness := .75
		if animated {
			brightness = .09 + .9*math.Pow(levels[i*3/count], 1.3)
		}
		out[i] = fmt.Sprintf("#%02x%02x%02x", int(rgb[0]*255*brightness), int(rgb[1]*255*brightness), int(rgb[2]*255*brightness))
	}
	return out
}
func run(ctx context.Context, l luna, lights output, mix playbackMix) {
	var art nativeArtwork
	go art.watch(ctx)
	var idleUntil atomic.Int64
	go observeSpeech(ctx, l, &idleUntil)
	var spectrum spectrum
	var envelope float64
	owned := false
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		if owned && (lights.fb != nil || time.Now().UnixNano() < idleUntil.Load()) {
			_ = lights.off(context.Background())
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			rgb := art.color.Load()
			if idleUntil.Load() <= time.Now().UnixNano() {
				if lights.fb != nil && owned && lights.off(ctx) != nil {
					continue
				}
				owned = false
				continue
			}
			if rgb == nil || !mix.stockPlayerAlone() {
				if owned {
					if err := lights.off(ctx); err == nil {
						owned = false
					}
				}
				continue
			}
			animated := mix.sample(&spectrum, &envelope)
			if time.Now().UnixNano() >= idleUntil.Load() {
				if lights.fb != nil && owned && lights.off(ctx) != nil {
					continue
				}
				owned = false
				continue
			}
			if err := lights.draw(ctx, frame(*rgb, &spectrum, animated, lights.count)); err == nil {
				owned = true
			}
		}
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	l := luna{strings.TrimRight(os.Getenv("LUNA_API_URL"), "/"), os.Getenv("LUNA_API_TOKEN"), &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if l.url == "" || l.token == "" {
		fmt.Fprintln(os.Stderr, "Start this plugin through Luna")
		os.Exit(1)
	}
	lights, err := deviceOutput(ctx, l)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if lights.fb != nil {
		defer lights.fb.Close()
	}
	conf, err := os.ReadFile("/etc/asound.conf")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read ALSA configuration")
		return
	}
	mix, err := parsePlaybackMix(string(conf))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	run(ctx, l, lights, mix)
}
