package pinger

import (
	"bytes"
	"image"
	"image/draw"
	"image/gif"
	"time"
)

// Animation is a decoded image file. Animated GIFs keep their compact
// paletted frames and compose full pictures on demand, so a long GIF costs
// little more memory than the file itself; anything else is one frame.
type Animation struct {
	Delays []time.Duration

	still  image.Image
	g      *gif.GIF
	canvas *image.NRGBA
	saved  *image.NRGBA
	shown  int
}

// DecodeAnimation decodes an image file, keeping every frame of an animated
// GIF together with its display time.
func DecodeAnimation(data []byte) (*Animation, error) {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if format == "gif" {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if len(g.Image) > 1 {
			return newGIFAnimation(g), nil
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return &Animation{Delays: []time.Duration{0}, still: img}, nil
}

func newGIFAnimation(g *gif.GIF) *Animation {
	bounds := image.Rect(0, 0, g.Config.Width, g.Config.Height)
	if bounds.Empty() {
		bounds = g.Image[0].Bounds()
	}
	delays := make([]time.Duration, len(g.Image))
	for i := range delays {
		delays[i] = 100 * time.Millisecond
		if i < len(g.Delay) && g.Delay[i] >= 2 {
			delays[i] = time.Duration(g.Delay[i]) * 10 * time.Millisecond
		}
	}
	return &Animation{Delays: delays, g: g, canvas: image.NewNRGBA(bounds), shown: -1}
}

// Len is the number of frames.
func (a *Animation) Len() int {
	return len(a.Delays)
}

// Bounds is the size of every frame.
func (a *Animation) Bounds() image.Rectangle {
	if a.g == nil {
		return a.still.Bounds()
	}
	return a.canvas.Bounds()
}

// Frame returns the complete picture of frame i. For a GIF the result is a
// shared buffer that the next Frame call overwrites. Frames are cheapest
// when requested in order; going back restarts from the first frame.
func (a *Animation) Frame(i int) image.Image {
	if a.g == nil {
		return a.still
	}
	if i < a.shown {
		clear(a.canvas.Pix)
		a.shown = -1
	}
	for a.shown < i {
		if a.shown >= 0 {
			a.dispose(a.shown)
		}
		n := a.shown + 1
		if a.disposal(n) == gif.DisposalPrevious {
			if a.saved == nil {
				a.saved = image.NewNRGBA(a.canvas.Bounds())
			}
			copy(a.saved.Pix, a.canvas.Pix)
		}
		f := a.g.Image[n]
		draw.Draw(a.canvas, f.Bounds(), f, f.Bounds().Min, draw.Over)
		a.shown = n
	}
	return a.canvas
}

func (a *Animation) disposal(i int) byte {
	if i < len(a.g.Disposal) {
		return a.g.Disposal[i]
	}
	return 0
}

func (a *Animation) dispose(i int) {
	switch a.disposal(i) {
	case gif.DisposalBackground:
		draw.Draw(a.canvas, a.g.Image[i].Bounds(), image.Transparent, image.Point{}, draw.Src)
	case gif.DisposalPrevious:
		copy(a.canvas.Pix, a.saved.Pix)
	}
}

// FrameAt picks the frame to show at now from the wall clock, so separate
// servers with synchronised clocks show the same frame at the same moment.
// It also returns when that frame ends; a single frame never ends (zero
// time).
func FrameAt(delays []time.Duration, now time.Time) (int, time.Time) {
	var cycle time.Duration
	for _, d := range delays {
		cycle += d
	}
	if len(delays) <= 1 || cycle <= 0 {
		return 0, time.Time{}
	}
	t := time.Duration(now.UnixNano() % int64(cycle))
	for i, d := range delays {
		if t < d {
			return i, now.Add(d - t)
		}
		t -= d
	}
	return len(delays) - 1, now.Add(delays[len(delays)-1])
}
