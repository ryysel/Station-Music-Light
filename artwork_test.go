package main

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func TestStockMusicDirective(t *testing.T) {
	wrap := func(field byte, p []byte) []byte {
		return append(binary.AppendUvarint([]byte{field<<3 | 2}, uint64(len(p))), p...)
	}
	const cover = "avatars.yandex.net/get-music-content/123/album-1/%%"
	for _, tc := range []struct {
		screen, art  string
		paused, want bool
	}{
		{"Music", cover, false, true}, {"Music", cover, true, false}, {"Radio", cover, false, false}, {"Music", "example.com/a", false, false}, {"Music", "", false, false},
	} {
		p := []byte(fmt.Sprintf(`{"screen_type":%q,"set_pause":%t,"metadata":{"art_image_url":%q}}`, tc.screen, tc.paused, tc.art))
		b := wrap(1, wrap(2, wrap(6, p)))
		want := ""
		if tc.want {
			want = "https://avatars.yandex.net/get-music-content/123/album-1/100x100"
		}
		if got := directiveCover(b); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if directiveCover(b[:len(b)-1]) != "" {
			t.Fatal("accepted partial snapshot")
		}
	}
}
