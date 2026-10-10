package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"slices"
	"testing"
	"time"

	"sntping/pinger"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func stillAnim(t *testing.T, img image.Image) *pinger.Animation {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	a, err := pinger.DecodeAnimation(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func gifBytes(t *testing.T) []byte {
	t.Helper()
	g := &gif.GIF{Config: image.Config{Width: 4, Height: 3}}
	for i, c := range []color.Color{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}} {
		p := image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Transparent, c})
		for y := 0; y < 3; y++ {
			for x := 0; x < 4; x++ {
				if (x+y+i)%3 != 0 {
					p.SetColorIndex(x, y, 1)
				}
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPlanMatchesFullRender(t *testing.T) {
	const w, h = 40, 30
	data := gifBytes(t)
	anim, _ := pinger.DecodeAnimation(data)
	ref, _ := pinger.DecodeAnimation(data)
	under := solid(20, 20, color.NRGBA{200, 200, 200, 255})
	over := solid(5, 5, color.NRGBA{10, 20, 30, 255})

	layers := []layer{
		{name: "under", key: "u", anim: stillAnim(t, under), x: 0, y: 0, scale: 1, nearest: true},
		{name: "gif", key: "g", anim: anim, delays: anim.Delays, x: 50, y: 50, scale: 3, nearest: true},
		{name: "over", key: "o", anim: stillAnim(t, over), x: 55, y: 50, scale: 1, nearest: true},
	}
	p, _, errs := newPlan(w, h, 16, 0, 1, layers, nil)
	if len(errs) > 0 || !p.animated() {
		t.Fatalf("errs=%v animated=%v", errs, p.animated())
	}

	for _, f := range []int{0, 2, 1, 1, 0, 2, 0} {
		got := p.targets([]int{0, f, 0})
		full, _, err := pinger.Compose(w, h, []pinger.Layer{
			{Img: layers[0].anim.Frame(0), XPct: 0, YPct: 0, Scale: 1, Nearest: true},
			{Img: ref.Frame(f), XPct: 50, YPct: 50, Scale: 3, Nearest: true},
			{Img: layers[2].anim.Frame(0), XPct: 55, YPct: 50, Scale: 1, Nearest: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := pinger.PackImage(full, 0, 0, 16)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("frame %d: plan gives %d pixels, full render %d, contents differ", f, len(got), len(want))
		}
	}
}

func TestScaledAnimCachesFramesSteppedOver(t *testing.T) {
	anim, _ := pinger.DecodeAnimation(gifBytes(t))
	sa := newScaledAnim(anim, image.Pt(8, 6), true)
	sa.frame(2)
	for i, f := range sa.frames {
		if f == nil {
			t.Fatalf("frame %d not cached after stepping to frame 2", i)
		}
	}
	cached := sa.frames[1]
	if sa.frame(1) != cached {
		t.Fatal("frame 1 rebuilt instead of served from cache")
	}
}

func TestScaledAnimWithoutCacheStillCorrect(t *testing.T) {
	data := gifBytes(t)
	anim, _ := pinger.DecodeAnimation(data)
	ref, _ := pinger.DecodeAnimation(data)
	sa := newScaledAnim(anim, image.Pt(4, 3), true)
	sa.frames, sa.scratch = nil, image.NewNRGBA(image.Rect(0, 0, 4, 3))
	for _, f := range []int{1, 0, 2, 2, 1} {
		got := sa.frame(f)
		want := ref.Frame(f)
		for y := 0; y < 3; y++ {
			for x := 0; x < 4; x++ {
				_, _, _, ga := got.At(x, y).RGBA()
				_, _, _, wa := want.At(x, y).RGBA()
				if ga != wa {
					t.Fatalf("frame %d pixel (%d,%d): alpha %d, want %d", f, x, y, ga, wa)
				}
			}
		}
	}
}

func TestPlayDelays(t *testing.T) {
	ms := time.Millisecond
	base := []time.Duration{50 * ms, 100 * ms}
	for _, tt := range []struct {
		si   pinger.SceneImage
		want []time.Duration
	}{
		{pinger.SceneImage{}, base},
		{pinger.SceneImage{Speed: 1}, base},
		{pinger.SceneImage{Speed: 0.5}, []time.Duration{100 * ms, 200 * ms}},
		{pinger.SceneImage{Speed: 2}, []time.Duration{25 * ms, 50 * ms}},
		{pinger.SceneImage{Speed: 0.5, FrameMs: 300}, []time.Duration{300 * ms, 300 * ms}},
	} {
		if got := playDelays(base, tt.si); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: %v, want %v", tt.si, got, tt.want)
		}
	}
	if got := playDelays([]time.Duration{0}, pinger.SceneImage{Speed: 0.5}); got[0] != 0 {
		t.Errorf("still image got delay %v", got[0])
	}
}

func TestShardsCoverSceneExactlyOnce(t *testing.T) {
	const w, h, total = 40, 30, 5
	data := gifBytes(t)
	newLayers := func() []layer {
		anim, _ := pinger.DecodeAnimation(data)
		return []layer{
			{name: "under", key: "u", anim: stillAnim(t, solid(20, 20, color.NRGBA{200, 200, 200, 255})), x: 0, y: 0, scale: 1, nearest: true},
			{name: "gif", key: "g", anim: anim, delays: anim.Delays, x: 50, y: 50, scale: 3, nearest: true},
		}
	}
	whole, _, _ := newPlan(w, h, 16, 0, 1, newLayers(), nil)
	shards := make([]*plan, total)
	for i := range shards {
		shards[i], _, _ = newPlan(w, h, 16, i, total, newLayers(), nil)
	}
	for _, f := range []int{0, 2, 1} {
		want := whole.targets([]int{0, f})
		var got []uint64
		for i, p := range shards {
			part := p.targets([]int{0, f})
			if len(part) == 0 || len(part) > len(want)/2 {
				t.Errorf("frame %d: shard %d has %d of %d pixels", f, i, len(part), len(want))
			}
			got = append(got, part...)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("frame %d: shards give %d pixels, whole scene %d, or contents differ", f, len(got), len(want))
		}
	}
}
