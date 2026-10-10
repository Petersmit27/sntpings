package pinger

import (
	"bytes"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/png"
	"testing"
	"time"
)

func TestFrameAt(t *testing.T) {
	delays := []time.Duration{100 * time.Millisecond, 300 * time.Millisecond}
	at := func(ms int64) time.Time { return time.UnixMilli(1_000_000_000_000 + ms) }
	for _, tt := range []struct {
		ms        int64
		frame     int
		remaining time.Duration
	}{
		{0, 0, 100 * time.Millisecond},
		{99, 0, time.Millisecond},
		{100, 1, 300 * time.Millisecond},
		{399, 1, time.Millisecond},
		{400, 0, 100 * time.Millisecond},
	} {
		now := at(tt.ms)
		frame, until := FrameAt(delays, now)
		if frame != tt.frame || until.Sub(now) != tt.remaining {
			t.Errorf("at %dms: frame %d ends in %v, want frame %d ending in %v", tt.ms, frame, until.Sub(now), tt.frame, tt.remaining)
		}
	}
	if frame, until := FrameAt([]time.Duration{0}, at(5)); frame != 0 || !until.IsZero() {
		t.Errorf("single frame = %d, %v", frame, until)
	}
}

func paletted(w, h int, fill color.Color, r image.Rectangle) *image.Paletted {
	p := image.NewPaletted(r, append(palette.Plan9[:255:255], color.Transparent))
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			p.Set(x, y, fill)
		}
	}
	return p
}

func TestDecodeAnimationComposesGIF(t *testing.T) {
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	g := &gif.GIF{
		Image: []*image.Paletted{
			paletted(4, 4, red, image.Rect(0, 0, 4, 4)),
			paletted(4, 4, blue, image.Rect(2, 2, 4, 4)),
			paletted(4, 4, blue, image.Rect(0, 0, 1, 1)),
		},
		Delay:    []int{50, 0, 10},
		Disposal: []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalNone},
		Config:   image.Config{Width: 4, Height: 4},
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	a, err := DecodeAnimation(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if a.Len() != 3 || a.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Fatalf("len %d bounds %v", a.Len(), a.Bounds())
	}
	wantDelays := []time.Duration{500 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond}
	for i, d := range wantDelays {
		if a.Delays[i] != d {
			t.Errorf("delay %d = %v, want %v", i, a.Delays[i], d)
		}
	}
	isRGB := func(c color.Color, want color.RGBA) bool {
		r, g, b, al := c.RGBA()
		return uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B && uint8(al>>8) == want.A
	}
	check := func(i int) {
		t.Helper()
		f := a.Frame(i)
		switch i {
		case 0:
			if !isRGB(f.At(3, 3), red) || !isRGB(f.At(0, 0), red) {
				t.Errorf("frame 1 should be all red")
			}
		case 1:
			if !isRGB(f.At(3, 3), blue) || !isRGB(f.At(0, 0), red) {
				t.Errorf("frame 2 should be red with a blue corner")
			}
		case 2:
			if _, _, _, al := f.At(3, 3).RGBA(); al != 0 {
				t.Errorf("frame 3: area disposed to background should be transparent")
			}
			if !isRGB(f.At(0, 0), blue) || !isRGB(f.At(1, 1), red) {
				t.Errorf("frame 3 should keep red with a new blue pixel")
			}
		}
	}
	for _, i := range []int{0, 1, 2, 0, 2, 1, 1} {
		check(i)
	}
}

func TestDecodeAnimationStillImage(t *testing.T) {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 3, 2)))
	a, err := DecodeAnimation(buf.Bytes())
	if err != nil || a.Len() != 1 || a.Frame(0).Bounds().Dx() != 3 {
		t.Fatalf("len=%d err=%v", a.Len(), err)
	}
}
