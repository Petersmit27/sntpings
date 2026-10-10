package main

import (
	"fmt"
	"image"
	"image/color"

	xdraw "golang.org/x/image/draw"

	"sntping/pinger"
)

// scaledCacheBytes bounds the pre-scaled frames kept per animation. A GIF
// that does not fit is rendered on the fly, always in frame order.
const scaledCacheBytes = 64 << 20

// scaledAnim hands out an animation's frames already scaled to their size on
// the canvas. Frames are cached when they all fit, and every frame stepped
// over on the way to a missing one is cached too, so one replay fills the
// cache instead of repeating each time the clock skips ahead.
type scaledAnim struct {
	anim    *pinger.Animation
	size    image.Point
	nearest bool
	frames  []*image.NRGBA
	scratch *image.NRGBA
	pos     int
}

func newScaledAnim(anim *pinger.Animation, size image.Point, nearest bool) *scaledAnim {
	s := &scaledAnim{anim: anim, size: size, nearest: nearest, pos: -1}
	if 4*size.X*size.Y*anim.Len() <= scaledCacheBytes {
		s.frames = make([]*image.NRGBA, anim.Len())
	} else {
		s.scratch = image.NewNRGBA(image.Rectangle{Max: size})
	}
	return s
}

func (s *scaledAnim) frame(i int) *image.NRGBA {
	if s.frames != nil && s.frames[i] != nil {
		return s.frames[i]
	}
	start := s.pos + 1
	if i < start {
		start = 0
	}
	var out *image.NRGBA
	for j := start; j <= i; j++ {
		src := s.anim.Frame(j)
		if j < i && (s.frames == nil || s.frames[j] != nil) {
			continue
		}
		dst := s.scratch
		if s.frames != nil {
			dst = image.NewNRGBA(image.Rectangle{Max: s.size})
			s.frames[j] = dst
		}
		scaler(s.nearest).Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
		out = dst
	}
	s.pos = i
	return out
}

func scaler(nearest bool) xdraw.Scaler {
	if nearest {
		return xdraw.NearestNeighbor
	}
	return xdraw.CatmullRom
}

// boundsOnly stands in for an image when only its size matters.
type boundsOnly image.Rectangle

func (b boundsOnly) ColorModel() color.Model { return color.NRGBAModel }
func (b boundsOnly) Bounds() image.Rectangle { return image.Rectangle(b) }
func (b boundsOnly) At(int, int) color.Color { return color.NRGBA{} }

// plan splits a scene into pixels that never change, packed once, and the
// small area under the animated layers, re-rendered per frame. It keeps only
// the pixels of this server's shard.
type plan struct {
	w, h         int
	minAlpha     int
	shard, total int
	layers       []layer
	scaled       []*scaledAnim
	rects        []image.Rectangle
	static       []uint64
	area         image.Rectangle
	buf          *image.NRGBA
}

// newPlan lays out the scene. scaledCache carries pre-scaled animations over
// from the previous scene so moving images does not rescale every frame.
func newPlan(w, h, minAlpha, shard, total int, layers []layer, scaledCache map[string]*scaledAnim) (*plan, map[string]*scaledAnim, []string) {
	p := &plan{w: w, h: h, minAlpha: minAlpha, shard: shard, total: total, layers: layers}
	canvas := image.Rect(0, 0, w, h)
	nextCache := map[string]*scaledAnim{}
	var errs []string
	var all image.Rectangle
	for _, l := range layers {
		r, err := pinger.LayerRect(w, h, pinger.Layer{Img: boundsOnly(l.anim.Bounds()), XPct: l.x, YPct: l.y, Scale: l.scale})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", l.name, err))
		}
		p.rects = append(p.rects, r)
		all = all.Union(r)

		var sa *scaledAnim
		if l.anim.Len() > 1 && !r.Empty() {
			key := fmt.Sprintf("%s|%dx%d|%v", l.key, r.Dx(), r.Dy(), l.nearest)
			if sa = scaledCache[key]; sa == nil {
				sa = newScaledAnim(l.anim, r.Size(), l.nearest)
			}
			nextCache[key] = sa
			p.area = p.area.Union(r)
		}
		p.scaled = append(p.scaled, sa)
	}
	all = all.Intersect(canvas)
	p.area = p.area.Intersect(canvas)

	if !all.Empty() {
		buf := image.NewNRGBA(all)
		p.compose(buf, make([]int, len(layers)))
		for _, t := range pinger.PackImage(buf, 0, 0, minAlpha) {
			if p.mine(t) && !image.Pt(int(t>>48&0xFFFF), int(t>>32&0xFFFF)).In(p.area) {
				p.static = append(p.static, t)
			}
		}
	}
	if !p.area.Empty() {
		p.buf = image.NewNRGBA(p.area)
	}
	return p, nextCache, errs
}

func (p *plan) animated() bool {
	return p.buf != nil
}

// targets returns every visible pixel for the given frame of each layer.
func (p *plan) targets(frames []int) []uint64 {
	if p.buf == nil {
		return append([]uint64(nil), p.static...)
	}
	p.compose(p.buf, frames)
	region := pinger.PackImage(p.buf, 0, 0, p.minAlpha)
	out := make([]uint64, 0, len(p.static)+len(region)/max(1, p.total)+1)
	out = append(out, p.static...)
	for _, t := range region {
		if p.mine(t) {
			out = append(out, t)
		}
	}
	return out
}

func (p *plan) mine(t uint64) bool {
	return pinger.ShardOf(int(t>>48&0xFFFF), int(t>>32&0xFFFF), p.total) == p.shard
}

func (p *plan) compose(dst *image.NRGBA, frames []int) {
	pl := make([]pinger.Layer, len(p.layers))
	for i, l := range p.layers {
		if sa := p.scaled[i]; sa != nil {
			pl[i] = pinger.Layer{Img: sa.frame(frames[i]), XPct: l.x, YPct: l.y, Scale: 1, Nearest: true}
		} else {
			pl[i] = pinger.Layer{Img: l.anim.Frame(0), XPct: l.x, YPct: l.y, Scale: l.scale, Nearest: l.nearest}
		}
	}
	pinger.ComposeInto(dst, p.w, p.h, pl)
}
