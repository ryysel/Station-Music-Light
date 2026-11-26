package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type output struct {
	l     luna
	path  string
	count int
	fb    *framebufferLED
}

func deviceOutput(ctx context.Context, l luna) (output, error) {
	var d struct {
		Model    string
		Features []string
	}
	if err := l.request(ctx, "GET", "/device", nil, &d); err != nil {
		return output{}, err
	}
	if d.Model == "yandexstation_2" {
		fb, err := openStation2LED()
		return output{l: l, count: station2LEDs, fb: fb}, err
	}
	for _, zone := range []string{"ring", "logo"} {
		prefix := "led." + zone + "."
		rgb, count := false, 0
		for _, f := range d.Features {
			if f == prefix+"rgb" {
				rgb = true
			}
			if suffix, ok := strings.CutPrefix(f, prefix); ok {
				if n, err := strconv.Atoi(suffix); err == nil && n > 0 && n <= 256 {
					count = n
				}
			}
		}
		if rgb && count > 0 {
			return output{l: l, path: "/led/" + zone, count: count}, nil
		}
	}
	return output{}, fmt.Errorf("no supported RGB ring or logo")
}
func (o output) draw(ctx context.Context, colors []string) error {
	if o.fb != nil {
		return o.fb.Colors(ctx, colors)
	}
	return o.l.post(ctx, o.path, map[string]any{"colors": colors}, nil)
}
func (o output) off(ctx context.Context) error {
	if o.fb != nil {
		return o.fb.Release(ctx)
	}
	return o.l.post(ctx, "/led/off/"+strings.TrimPrefix(o.path, "/led/"), struct{}{}, nil)
}
