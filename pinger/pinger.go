// Package pinger draws an image on the SNTpings canvas by sending ICMPv6
// echo requests to addresses that encode the pixel coordinate and colour.
//
// Address format:
//
//	<PREFIX /64>:<X>:<Y>:<B><G>:<R><A>
//
// All values are hexadecimal. X/Y are the 16-bit canvas coordinates,
// B/G/R/A are the 8-bit colour channels.
//
// Example: pixel (25,25) colour #FFD100 at full opacity with prefix
// 2001:db8:b317:a000::/64 becomes 2001:db8:b317:a000:0019:0019:00d1:ffff
package pinger

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"math/rand"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
)

// Options controls a single pinger run.
type Options struct {
	CanvasW, CanvasH int
	Fit              string
	Scale            float64
	RW, RH           int
	X, Y             Pos
	XSet, YSet       bool
	Nearest          bool
	Adjust           <-chan Adjust
	Sent, Failed     *atomic.Int64 // optional counters that outlive the run
	MinAlpha         int
	Workers          int
	PPS              int
	Loop             bool
	Interval         time.Duration
	Shuffle          bool
	Seed             int64
	KitchenSink      bool
	DVD              bool
	DVDSpeed         int
	Hue              float64
	MaxPkts          int
	Bind             string
	Network          string
	Dry              bool
	Silent           bool
	Stdout, Stderr   io.Writer
}

// Run renders img onto the canvas and sends it. It blocks until the run
// finishes (single pass, or until ctx is cancelled when looping).
func Run(ctx context.Context, img image.Image, prefix [8]byte, o Options) error {
	stdout, stderr := o.Stdout, o.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if o.CanvasW <= 0 {
		o.CanvasW = 3840
	}
	if o.CanvasH <= 0 {
		o.CanvasH = 2160
	}
	if o.Network == "" {
		o.Network = "udp6"
	}
	if o.Bind == "" {
		o.Bind = "::"
	}

	doShuffle := o.Shuffle || o.KitchenSink
	doLoop := o.Loop || o.KitchenSink || o.DVD || o.Hue != 0 || o.Adjust != nil

	var rng *rand.Rand
	if doShuffle {
		s := o.Seed
		if s == 0 {
			s = time.Now().UnixNano()
		}
		rng = rand.New(rand.NewSource(s))
		if !o.Silent {
			fmt.Fprintf(stderr, "order       : shuffled (seed %d)\n", s)
		}
	}

	build := func(d drawOpts) ([]uint64, image.Rectangle, error) {
		canvas, r, err := renderCanvas(img, o.CanvasW, o.CanvasH, d)
		if err != nil {
			return nil, r, err
		}
		t := buildTargets(canvas, o.MinAlpha)
		if rng != nil {
			shuffleTargets(rng, t)
		}
		if o.MaxPkts > 0 && o.MaxPkts < len(t) {
			t = t[:o.MaxPkts]
		}
		return t, r, nil
	}

	targets, placement, err := build(drawOpts{
		Fit:     o.Fit,
		Scale:   o.Scale,
		RW:      o.RW,
		RH:      o.RH,
		X:       o.X,
		Y:       o.Y,
		XSet:    o.XSet,
		YSet:    o.YSet,
		Nearest: o.Nearest,
	})
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("image produced no visible pixels (try lowering -min-alpha)")
	}

	if o.KitchenSink && !o.Silent {
		fmt.Fprintln(stderr, "mode        : kitchen sink (shuffle + loop + drift)")
	}
	dvdSpeed := 0
	if o.DVD {
		dvdSpeed = o.DVDSpeed
		if dvdSpeed <= 0 {
			dvdSpeed = 4
		}
		if !o.Silent {
			fmt.Fprintf(stderr, "mode        : dvd bounce (speed %d px/pass)\n", dvdSpeed)
		}
	}
	if o.Hue != 0 && !o.Silent {
		fmt.Fprintf(stderr, "mode        : hue sweep (%.1f deg/pass, continuous)\n", o.Hue)
	}

	nw := o.Workers
	if nw <= 0 {
		nw = max(1, 2*runtime.NumCPU())
	}

	if !o.Silent {
		fmt.Fprintf(stderr, "canvas      : %dx%d, image at %s\n", o.CanvasW, o.CanvasH, describe(placement))
		fmt.Fprintf(stderr, "pixels      : %d packets per pass\n", len(targets))
		fmt.Fprintf(stderr, "workers     : %d\n", nw)
		if o.PPS > 0 {
			fmt.Fprintf(stderr, "rate        : %d pps\n", o.PPS)
		} else {
			fmt.Fprintln(stderr, "rate        : unlimited")
		}
	}

	if o.Dry {
		for _, t := range targets {
			fmt.Fprintln(stdout, formatTarget(prefix, t))
		}
		return nil
	}

	var (
		liveMu      sync.Mutex
		want        liveState
		wantVersion int
		cancelPass  context.CancelFunc
	)
	shown, shownVersion := want, 0
	if o.Adjust != nil {
		want = initialLive(img, placement, o)
		shown = want
		fmt.Fprintln(stderr, "live        : arrows move 1%, shift+arrows 10%, +/- scale, q quits")
		fmt.Fprintf(stderr, "position    : %s  (%s)\n", want.flags(), describe(placement))
		go func() {
			for a := range o.Adjust {
				liveMu.Lock()
				want = want.apply(a)
				wantVersion++
				if cancelPass != nil {
					cancelPass()
				}
				liveMu.Unlock()
			}
		}()
	}

	var lim *limiter
	if o.PPS > 0 {
		lim = newLimiter(int64(o.PPS))
		defer lim.close()
	}

	sent, failed := o.Sent, o.Failed
	if sent == nil {
		sent = new(atomic.Int64)
	}
	if failed == nil {
		failed = new(atomic.Int64)
	}
	if !o.Silent {
		printStats(ctx, stderr, sent, failed)
	}

	vx, vy := dvdSpeed, dvdSpeed
	hueStart := 0.0

	for pass := 1; ; pass++ {
		if pass > 1 {
			switch {
			case o.DVD:
				vx, vy = stepBounce(targets, vx, vy, o.CanvasW, o.CanvasH)
			case o.KitchenSink:
				shiftTargets(targets, rng.Intn(41)-20, rng.Intn(41)-20, o.CanvasW, o.CanvasH)
				if doShuffle {
					shuffleTargets(rng, targets)
				}
			}
			if o.Hue != 0 {
				hueStart += o.Hue
				hueStart = math.Mod(hueStart, 360)
				if hueStart < 0 {
					hueStart += 360
				}
			}
		}

		liveMu.Lock()
		passCtx, cancel := context.WithCancel(ctx)
		cancelPass = cancel
		next, nextVersion := want, wantVersion
		liveMu.Unlock()
		if nextVersion != shownVersion {
			t, r, err := build(next.drawOpts(o.Nearest))
			switch {
			case err != nil:
				fmt.Fprintf(stderr, "\r\x1b[2Kposition    : %s: %v\n", next.flags(), err)
			case len(t) == 0:
				fmt.Fprintf(stderr, "\r\x1b[2Kposition    : %s: no visible pixels, keeping the old image\n", next.flags())
			default:
				targets = t
				shown = next
				fmt.Fprintf(stderr, "\r\x1b[2Kposition    : %s  (%s, %d packets per pass)\n", next.flags(), describe(r), len(t))
			}
			shownVersion = nextVersion
		}

		packet := buildPacket()
		var wg sync.WaitGroup
		n := len(targets)
		chunk := (n + nw - 1) / nw
		for w := 0; w < nw; w++ {
			start := w * chunk
			end := min(start+chunk, n)
			if start >= end {
				break
			}
			wg.Add(1)
			go func(id, start, end int) {
				defer wg.Done()
				runWorker(passCtx, id, prefix, targets[start:end], start, n, packet, hueStart, o.Hue, o.Network, o.Bind, lim, stderr, sent, failed)
			}(w, start, end)
		}
		wg.Wait()
		interrupted := passCtx.Err() != nil
		cancel()

		if !doLoop || ctx.Err() != nil {
			break
		}
		if interrupted {
			continue
		}
		if !o.Silent {
			if o.Interval > 0 {
				fmt.Fprintf(stderr, "\npass %d done, sleeping %s\n", pass, o.Interval)
			} else {
				fmt.Fprintf(stderr, "\npass %d done\n", pass)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(o.Interval):
		}
		if ctx.Err() != nil {
			break
		}
	}

	if !o.Silent {
		fmt.Fprintf(stderr, "\ndone: sent=%d failed=%d\n", sent.Load(), failed.Load())
	}
	if o.Adjust != nil {
		fmt.Fprintf(stderr, "rerun with  : %s\n", shown.flags())
	}
	return nil
}

// Adjust nudges a live run: DX/DY move the image by that many percent of
// the free space, ScaleSteps grows (positive) or shrinks (negative) it.
type Adjust struct {
	DX, DY     float64
	ScaleSteps int
}

// liveState is where a live run wants the image, in rerunnable terms.
type liveState struct {
	XPct, YPct, Scale float64
}

func initialLive(img image.Image, placed image.Rectangle, o Options) liveState {
	s := liveState{XPct: 50, YPct: 50, Scale: o.Scale}
	if s.Scale <= 0 {
		s.Scale = math.Round(float64(placed.Dx())/float64(img.Bounds().Dx())*100) / 100
	}
	if o.XSet {
		s.XPct = startPct(o.X, placed.Min.X, o.CanvasW, placed.Dx())
	}
	if o.YSet {
		s.YPct = startPct(o.Y, placed.Min.Y, o.CanvasH, placed.Dy())
	}
	return s
}

func startPct(p Pos, px, canvas, size int) float64 {
	if p.IsPct {
		return clampPct(p.Pct)
	}
	if canvas == size {
		return 50
	}
	return clampPct(float64(px) * 100 / float64(canvas-size))
}

func (s liveState) apply(a Adjust) liveState {
	s.XPct = clampPct(s.XPct + a.DX)
	s.YPct = clampPct(s.YPct + a.DY)
	for ; a.ScaleSteps > 0; a.ScaleSteps-- {
		s.Scale = stepScale(s.Scale, 1)
	}
	for ; a.ScaleSteps < 0; a.ScaleSteps++ {
		s.Scale = stepScale(s.Scale, -1)
	}
	return s
}

func (s liveState) drawOpts(nearest bool) drawOpts {
	return drawOpts{
		Scale:   s.Scale,
		X:       Pos{Pct: s.XPct, IsPct: true},
		Y:       Pos{Pct: s.YPct, IsPct: true},
		XSet:    true,
		YSet:    true,
		Nearest: nearest,
	}
}

func (s liveState) flags() string {
	return fmt.Sprintf("-x %s%% -y %s%% -scale %s", fmtNum(s.XPct), fmtNum(s.YPct), fmtNum(s.Scale))
}

func clampPct(v float64) float64 {
	return math.Round(math.Min(100, math.Max(0, v))*10) / 10
}

// stepScale moves to the next scale on a 0.1 grid below 1x and a 0.25 grid
// from 1x up, so repeated presses land on readable values.
func stepScale(s float64, dir int) float64 {
	const eps = 1e-9
	var next float64
	if dir > 0 {
		step := 0.1
		if s >= 1-eps {
			step = 0.25
		}
		next = math.Floor(s/step+eps)*step + step
	} else {
		step := 0.1
		if s > 1+eps {
			step = 0.25
		}
		next = math.Ceil(s/step-eps)*step - step
	}
	return math.Max(0.1, math.Round(next*100)/100)
}

func fmtNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func describe(r image.Rectangle) string {
	return fmt.Sprintf("%dx%d at (%d,%d)", r.Dx(), r.Dy(), r.Min.X, r.Min.Y)
}

// ParsePrefix accepts a prefix such as "2001:db8:b317:a000::" or
// "2001:db8:b317:a000::/64" and returns its first 64 bits.
func ParsePrefix(s string) ([8]byte, error) {
	var out [8]byte
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		plen, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return out, fmt.Errorf("bad prefix length %q", s[i+1:])
		}
		if plen > 64 {
			return out, fmt.Errorf("prefix must be a /64 or shorter (got /%d)", plen)
		}
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return out, fmt.Errorf("%q is not a valid IPv6 address", s)
	}
	if ip.To4() != nil {
		return out, errors.New("that looks like an IPv4 address")
	}
	copy(out[:], ip.To16()[:8])
	return out, nil
}

// Pos is a canvas coordinate on one axis: either a pixel offset of the
// top-left corner, or a percentage of the free space (canvas minus image),
// so 0% is flush with the start edge, 100% with the far edge, 50% centred.
type Pos struct {
	Px    int
	Pct   float64
	IsPct bool
}

// ParsePos accepts a pixel value such as "1700" or a percentage such as "25%".
func ParsePos(s string) (Pos, error) {
	s = strings.TrimSpace(s)
	if v, ok := strings.CutSuffix(s, "%"); ok {
		pct, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
			return Pos{}, fmt.Errorf("bad percentage %q", s)
		}
		return Pos{Pct: pct, IsPct: true}, nil
	}
	px, err := strconv.Atoi(s)
	if err != nil {
		return Pos{}, fmt.Errorf("bad position %q: want pixels (e.g. 1700) or a percentage (e.g. 25%%)", s)
	}
	return Pos{Px: px}, nil
}

// Resolve returns the pixel offset of a block of the given size on an axis
// of length canvas.
func (p Pos) Resolve(canvas, size int) int {
	if !p.IsPct {
		return p.Px
	}
	return int(math.Round(p.Pct / 100 * float64(canvas-size)))
}

// drawOpts controls how the source image is resized and placed on the
// canvas.
type drawOpts struct {
	Fit        string
	Scale      float64
	RW, RH     int
	X, Y       Pos
	XSet, YSet bool
	Nearest    bool
}

// renderCanvas decodes the image and renders it into an NRGBA buffer that
// matches the canvas resolution. NRGBA keeps colours un-premultiplied,
// so the RGB and A bytes can be sent to the canvas as-is.
func renderCanvas(src image.Image, w, h int, o drawOpts) (*image.NRGBA, image.Rectangle, error) {
	r, err := layout(src.Bounds(), w, h, o)
	if err != nil {
		return nil, r, err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	scalerFor(o.Nearest).Scale(dst, r, src, src.Bounds(), xdraw.Src, nil)
	return dst, r, nil
}

// Layer is one image of a composed scene, positioned in percent of the
// free space like -x 50% -y 50%.
type Layer struct {
	Img        image.Image
	XPct, YPct float64
	Scale      float64
	Nearest    bool
}

// Compose draws layers in order (later ones on top) onto one transparent
// w×h canvas and returns it with each layer's placement.
func Compose(w, h int, layers []Layer) (*image.NRGBA, []image.Rectangle, error) {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	rects, err := ComposeInto(dst, w, h, layers)
	if err != nil {
		return nil, nil, err
	}
	return dst, rects, nil
}

// ComposeInto is Compose into an existing buffer: dst is cleared and its
// bounds select which part of the w×h canvas gets drawn, so a buffer
// covering just the layers can be reused for every frame.
func ComposeInto(dst *image.NRGBA, w, h int, layers []Layer) ([]image.Rectangle, error) {
	clearNRGBA(dst)
	rects := make([]image.Rectangle, len(layers))
	for i, l := range layers {
		r, err := LayerRect(w, h, l)
		if err != nil {
			return nil, err
		}
		scalerFor(l.Nearest).Scale(dst, r, l.Img, l.Img.Bounds(), xdraw.Over, nil)
		rects[i] = r
	}
	return rects, nil
}

// clearNRGBA makes every pixel transparent, row by row so SubImages work.
func clearNRGBA(img *image.NRGBA) {
	w := 4 * img.Rect.Dx()
	for y := 0; y < img.Rect.Dy(); y++ {
		clear(img.Pix[y*img.Stride : y*img.Stride+w])
	}
}

// LayerRect is where a layer lands on a w×h canvas.
func LayerRect(w, h int, l Layer) (image.Rectangle, error) {
	return layout(l.Img.Bounds(), w, h, drawOpts{
		Scale: l.Scale,
		X:     Pos{Pct: l.XPct, IsPct: true},
		Y:     Pos{Pct: l.YPct, IsPct: true},
		XSet:  true,
		YSet:  true,
	})
}

func scalerFor(nearest bool) xdraw.Scaler {
	if nearest {
		return xdraw.NearestNeighbor
	}
	return xdraw.CatmullRom
}

// layout works out where an image with bounds sb lands on a w×h canvas.
func layout(sb image.Rectangle, w, h int, o drawOpts) (image.Rectangle, error) {
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return image.Rectangle{}, errors.New("image has zero size")
	}

	// Work out the target size. Explicit -rw/-rh win, then -scale,
	// then the -fit strategy.
	var tw, th int
	switch {
	case o.RW > 0 || o.RH > 0:
		switch {
		case o.RW > 0 && o.RH > 0:
			tw, th = o.RW, o.RH
		case o.RW > 0:
			tw = o.RW
			th = max(1, int(math.Round(float64(sh)*float64(tw)/float64(sw))))
		default:
			th = o.RH
			tw = max(1, int(math.Round(float64(sw)*float64(th)/float64(sh))))
		}
	case o.Scale > 0:
		tw = max(1, int(math.Round(float64(sw)*o.Scale)))
		th = max(1, int(math.Round(float64(sh)*o.Scale)))
	default:
		switch strings.ToLower(o.Fit) {
		case "stretch":
			tw, th = w, h
		case "fit":
			s := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
			tw = max(1, int(math.Round(float64(sw)*s)))
			th = max(1, int(math.Round(float64(sh)*s)))
		case "none":
			tw, th = sw, sh
		case "":
			tw, th = sw, sh
		default:
			return image.Rectangle{}, fmt.Errorf("unknown -fit %q (use fit, stretch or none)", o.Fit)
		}
	}

	// Position: use -x/-y if given, otherwise centre.
	px, py := o.X.Resolve(w, tw), o.Y.Resolve(h, th)
	if !o.XSet {
		px = (w - tw) / 2
	}
	if !o.YSet {
		py = (h - th) / 2
	}
	return image.Rect(px, py, px+tw, py+th), nil
}

// buildTargets packs every visible pixel into a uint64:
//
//	bits 63..48 X | 47..32 Y | 31..24 B | 23..16 G | 15..8 R | 7..0 A
func buildTargets(img *image.NRGBA, minAlpha int) []uint64 {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([]uint64, 0, w*h/4)
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			r, g, b, a := row[x*4], row[x*4+1], row[x*4+2], row[x*4+3]
			if int(a) < minAlpha {
				continue
			}
			out = append(out, uint64(x)<<48|uint64(y)<<32|uint64(b)<<24|uint64(g)<<16|uint64(r)<<8|uint64(a))
		}
	}
	return out
}

func shuffleTargets(rng *rand.Rand, t []uint64) {
	rng.Shuffle(len(t), func(i, j int) { t[i], t[j] = t[j], t[i] })
}

// shiftTargets moves every pixel by (dx,dy), wrapping around the canvas.
func shiftTargets(t []uint64, dx, dy, w, h int) {
	for i, v := range t {
		x := ((int(v>>48)&0xFFFF)+dx)%w + w
		y := ((int(v>>32)&0xFFFF)+dy)%h + h
		t[i] = (v & 0xFFFFFFFF) | uint64(x%w)<<48 | uint64(y%h)<<32
	}
}

// moveTargets translates every pixel by (dx,dy) without wrapping. The
// caller must keep the result inside the canvas (see targetBounds).
func moveTargets(t []uint64, dx, dy int) {
	for i, v := range t {
		x := (int(v>>48)&0xFFFF + dx) & 0xFFFF
		y := (int(v>>32)&0xFFFF + dy) & 0xFFFF
		t[i] = (v & 0xFFFFFFFF) | uint64(x)<<48 | uint64(y)<<32
	}
}

// stepBounce advances t by (vx,vy), reflecting the velocity off the canvas
// edges, and returns the new velocity.
func stepBounce(t []uint64, vx, vy, w, h int) (int, int) {
	minX, maxX, minY, maxY := targetBounds(t)
	if minX+vx < 0 || maxX+vx >= w {
		vx = -vx
	}
	if minY+vy < 0 || maxY+vy >= h {
		vy = -vy
	}
	moveTargets(t, vx, vy)
	return vx, vy
}

// shiftHue / hueShifted rotate the hue of pixel colours.
//
// hueShifted returns t with its colour rotated; the hue advances from
// hueStart to hueStart+hueDelta continuously across the whole pass, so
// the sweep is already visible while a pass is being sent. Alpha and the
// position bits are untouched.
func hueShifted(t uint64, gi, total int, hueStart, hueDelta float64) uint64 {
	deg := hueStart + hueDelta*float64(gi)/float64(total)
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	r := int(t>>8) & 0xFF
	g := int(t>>16) & 0xFF
	b := int(t>>24) & 0xFF
	nr, ng, nb := rotateHue(r, g, b, deg*math.Pi/180)
	return (t & 0xFFFFFFFF000000FF) | uint64(nb)<<24 | uint64(ng)<<16 | uint64(nr)<<8
}

// rotateHue returns new (r,g,b) with the hue rotated by rad radians.
func rotateHue(r, g, b int, rad float64) (int, int, int) {
	rf := float64(r) / 255
	gf := float64(g) / 255
	bf := float64(b) / 255
	max := math.Max(rf, math.Max(gf, bf))
	min := math.Min(rf, math.Min(gf, bf))
	l := (max + min) / 2
	var h, s float64
	if max == min {
		h, s = 0, 0
	} else {
		d := max - min
		switch max {
		case rf:
			h = math.Mod((gf-bf)/d, 6)
		case gf:
			h = (bf-rf)/d + 2
		default:
			h = (rf-gf)/d + 4
		}
		h *= 60 // convertible to degrees
		if h < 0 {
			h += 360
		}
		s = d / (1 - math.Abs(2*l-1))
	}

	h = math.Mod(h+rad*180/math.Pi, 360)
	if h < 0 {
		h += 360
	}

	c := max * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := max - c
	var nr, ng, nb float64
	switch {
	case h < 60:
		nr, ng, nb = c, x, 0
	case h < 120:
		nr, ng, nb = x, c, 0
	case h < 180:
		nr, ng, nb = 0, c, x
	case h < 240:
		nr, ng, nb = 0, x, c
	case h < 300:
		nr, ng, nb = x, 0, c
	default:
		nr, ng, nb = c, 0, x
	}
	return int(math.Round((nr + m) * 255)), int(math.Round((ng + m) * 255)), int(math.Round((nb + m) * 255))
}

// targetBounds returns the bounding box of all pixels in t.
func targetBounds(t []uint64) (minX, maxX, minY, maxY int) {
	minX, minY = math.MaxInt, math.MaxInt
	maxX, maxY = math.MinInt, math.MinInt
	for _, v := range t {
		x := int(v>>48) & 0xFFFF
		y := int(v>>32) & 0xFFFF
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}
	return
}

func buildPacket() []byte {
	m := icmp.Message{
		Type: ipv6.ICMPTypeEchoRequest,
		Code: 0,
		Body: &icmp.Echo{ID: 1, Seq: 1},
	}
	b, err := m.Marshal(nil) // checksum left for the kernel to fill in
	if err != nil {
		panic(err)
	}
	return b
}

func runWorker(ctx context.Context, id int, prefix [8]byte, targets []uint64, start, total int, packet []byte, hueStart, hueDelta float64, network, bind string, lim *limiter, stderr io.Writer, sent, failed *atomic.Int64) {
	conn, err := icmp.ListenPacket(network, bind)
	if err != nil {
		fmt.Fprintf(stderr, "\nworker %d: listen %s: %v\n", id, network, err)
		return
	}
	defer conn.Close()

	var buf [16]byte
	copy(buf[:8], prefix[:])
	dst := &net.UDPAddr{IP: buf[:]}

	var reported, consecutive int
	for i := range targets {
		if i&1023 == 0 && ctx.Err() != nil {
			return
		}
		tv := targets[i]
		if hueDelta > 0 {
			tv = hueShifted(tv, start+i, total, hueStart, hueDelta)
		}
		binary.BigEndian.PutUint64(buf[8:], tv)
		if _, err := conn.WriteTo(packet, dst); err != nil {
			failed.Add(1)
			consecutive++
			if reported < 5 {
				fmt.Fprintf(stderr, "\nworker %d: send: %v\n", id, err)
				reported++
			}
			if consecutive > 64 { // no route / no buffer: don't spin the CPU
				time.Sleep(10 * time.Millisecond)
				consecutive = 0
			}
			continue
		}
		consecutive = 0
		sent.Add(1)
		if lim != nil && lim.wait(ctx) != nil {
			return
		}
	}
}

func printStats(ctx context.Context, stderr io.Writer, sent, failed *atomic.Int64) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last, peak int64
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := sent.Load()
				rate := cur - last
				peak = max(peak, rate)
				elapsed := time.Since(start).Seconds()
				fmt.Fprintf(stderr, "\r sent=%-12d rate=%-10d/s peak=%-10d/s failed=%-6d avg=%.0f/s",
					cur, rate, peak, failed.Load(), float64(cur)/elapsed)
				last = cur
			}
		}
	}()
}

func formatTarget(prefix [8]byte, t uint64) string {
	var b [16]byte
	copy(b[:8], prefix[:])
	binary.BigEndian.PutUint64(b[8:], t)
	return fmt.Sprintf("%x:%x:%x:%x:%x:%x:%x:%x",
		b[0:2], b[2:4], b[4:6], b[6:8], b[8:10], b[10:12], b[12:14], b[14:16])
}

// limiter is a token-bucket rate limiter. A background goroutine refills
// tokens on a fine-grained ticker and workers take one token per packet.
// Call close to stop the refill goroutine.
type limiter struct {
	tokens chan struct{}
	done   chan struct{}
}

func newLimiter(pps int64) *limiter {
	burst := max(1, int(pps/10))
	l := &limiter{tokens: make(chan struct{}, burst), done: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		perTick := float64(pps) / 1000
		acc := 0.0
		for {
			select {
			case <-l.done:
				return
			case <-ticker.C:
				acc += perTick
				n := int(acc)
				acc -= float64(n)
				for i := 0; i < n; i++ {
					select {
					case l.tokens <- struct{}{}:
					default: // bucket full, drop the token
					}
				}
			}
		}
	}()
	return l
}

func (l *limiter) close() {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
}

func (l *limiter) wait(ctx context.Context) error {
	select {
	case <-l.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
