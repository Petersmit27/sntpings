// Command pingd keeps every image of a scene file on the SNTpings canvas,
// pinging them continuously, and re-renders whenever the scene file or one
// of its images changes. Animated GIFs play, with the frame picked from the
// wall clock so every server shows the same frame. pingctl edits the scene
// from a laptop.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"sntping/pinger"
)

// minFrameTime caps how often the picture changes. A pass over a scene takes
// far longer than a typical GIF frame, so faster frames would never land on
// the canvas anyway; frames in between are skipped, keeping real-time speed.
// Switches happen on wall-clock multiples, so every server skips the same
// frames.
const minFrameTime = 250 * time.Millisecond

type sceneInfo struct {
	Shard  string   `json:"shard,omitempty"`
	Pixels int      `json:"pixels"`
	Images []string `json:"images"`
	Errors []string `json:"errors,omitempty"`
}

type status struct {
	Updated time.Time `json:"updated"`
	Rate    int64     `json:"rate"`
	Sent    int64     `json:"sent"`
	sceneInfo
}

type decoded struct {
	mod  time.Time
	size int64
	anim *pinger.Animation
}

type layer struct {
	name    string
	key     string // identifies the decoded file, for the scaled-frame cache
	anim    *pinger.Animation
	delays  []time.Duration
	x, y    float64
	scale   float64
	nearest bool
}

type daemon struct {
	canvasW, canvasH int
	minAlpha         int
	sender           *pinger.Sender
	hostname         string
	info             atomic.Pointer[sceneInfo]
	rng              *rand.Rand
	scaled           map[string]*scaledAnim
}

func main() {
	var (
		prefixStr = flag.String("prefix", "", "IPv6 /64 prefix to ping [required]")
		dir       = flag.String("dir", ".", "directory holding scene.json and the images")
		canvasW   = flag.Int("w", 3840, "canvas width in pixels")
		canvasH   = flag.Int("h", 2160, "canvas height in pixels")
		minAlpha  = flag.Int("min-alpha", 16, "skip pixels whose alpha is below this value (0-255)")
		workers   = flag.Int("workers", 0, "number of sender goroutines / sockets (0 = 2x CPU cores)")
		pps       = flag.Int("pps", 0, "packets-per-second ceiling (0 = unlimited)")
		poll      = flag.Duration("poll", 250*time.Millisecond, "how often to check the scene for changes")
	)
	flag.Parse()
	log.SetFlags(0)

	prefix, err := pinger.ParsePrefix(*prefixStr)
	if err != nil {
		log.Fatalf("invalid -prefix: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := &daemon{
		canvasW:  *canvasW,
		canvasH:  *canvasH,
		minAlpha: *minAlpha,
		sender:   pinger.NewSender(prefix, pinger.Options{Workers: *workers, PPS: *pps, Stderr: log.Writer()}),
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	d.hostname, _ = os.Hostname()
	d.info.Store(&sceneInfo{})
	go d.reportStatus(ctx, filepath.Join(*dir, "status.json"))
	go d.sender.Run(ctx)

	cache := map[string]decoded{}
	scenePath := filepath.Join(*dir, "scene.json")
	for ctx.Err() == nil {
		scene, err := pinger.LoadScene(scenePath)
		if err != nil {
			log.Printf("scene: %v", err)
		}
		layers, errs := loadLayers(*dir, scene, cache)
		stamp := fingerprint(scenePath, *dir, scene)

		sceneCtx, cancel := context.WithCancel(ctx)
		go func() {
			waitForChange(sceneCtx, *poll, func() string { return fingerprint(scenePath, *dir, mustLoad(scenePath)) }, stamp)
			cancel()
		}()
		d.runScene(sceneCtx, scene, layers, errs)
		cancel()
	}
}

// runScene keeps the sender fed with the scene's pixels until ctx ends,
// swapping in the next frame whenever an animated layer's frame changes.
// The sender never stops, so a frame change costs no sending time.
func (d *daemon) runScene(ctx context.Context, scene pinger.Scene, layers []layer, errs []string) {
	shard, total, listed := scene.Shard(d.hostname)
	if !listed {
		errs = append(errs, fmt.Sprintf("host %q is not in the scene's hosts %v, sending the whole scene", d.hostname, scene.Hosts))
	}
	p, scaled, perrs := newPlan(d.canvasW, d.canvasH, d.minAlpha, shard, total, layers, d.scaled)
	d.scaled = scaled
	errs = append(errs, perrs...)

	frames := make([]int, len(layers))
	first := p.targets(frames)
	d.logScene(layers, p.rects, errs, len(first), fmt.Sprintf("%d/%d", shard+1, total))
	d.publish(first)
	if !p.animated() {
		<-ctx.Done()
		return
	}

	shown := fmt.Sprint(frames)
	for {
		now := time.Now()
		var next time.Time
		for i, l := range layers {
			f, until := pinger.FrameAt(l.delays, now)
			frames[i] = f
			if !until.IsZero() && (next.IsZero() || until.Before(next)) {
				next = until
			}
		}
		if key := fmt.Sprint(frames); key != shown {
			d.publish(p.targets(frames))
			shown = key
		}
		if next.Sub(now) < minFrameTime {
			next = now.Truncate(minFrameTime).Add(minFrameTime)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

// publish shuffles a fresh target list and hands it to the sender.
func (d *daemon) publish(targets []uint64) {
	d.rng.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	d.sender.SetTargets(targets)
}

func (d *daemon) logScene(layers []layer, rects []image.Rectangle, errs []string, pixels int, shard string) {
	names := make([]string, len(layers))
	for i, l := range layers {
		names[i] = l.name
		anim := ""
		if l.anim.Len() > 1 {
			names[i] = fmt.Sprintf("%s (%d frames)", l.name, l.anim.Len())
			anim = fmt.Sprintf(", %d frames", l.anim.Len())
		}
		if i < len(rects) {
			r := rects[i]
			log.Printf("image %-20s %dx%d at (%d,%d)%s", l.name, r.Dx(), r.Dy(), r.Min.X, r.Min.Y, anim)
		}
	}
	for _, e := range errs {
		log.Printf("error: %s", e)
	}
	log.Printf("scene: %d images, shard %s, %d pixels per pass", len(layers), shard, pixels)
	d.info.Store(&sceneInfo{Shard: shard, Pixels: pixels, Images: names, Errors: errs})
}

func mustLoad(path string) pinger.Scene {
	s, _ := pinger.LoadScene(path)
	return s
}

// loadLayers decodes the scene's images. An image used twice gets its own
// decoder, because an animation hands out frames in a shared buffer.
func loadLayers(dir string, scene pinger.Scene, cache map[string]decoded) ([]layer, []string) {
	var layers []layer
	var errs []string
	inUse := map[string]bool{}
	for _, si := range scene.Images {
		path := filepath.Join(dir, filepath.Base(si.File))
		dec, err := loadFrames(path, cache, inUse[path])
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", si.Name, err))
			continue
		}
		inUse[path] = true
		delays := playDelays(dec.anim.Delays, si)
		scale := si.Scale
		if scale <= 0 {
			scale = 1
		}
		layers = append(layers, layer{
			name: si.Name, key: fmt.Sprintf("%s|%d|%d", path, dec.mod.UnixNano(), dec.size), anim: dec.anim, delays: delays,
			x: si.X, y: si.Y, scale: scale, nearest: si.Nearest,
		})
	}
	return layers, errs
}

// playDelays applies an image's -frame-ms or -speed to its GIF timing.
func playDelays(base []time.Duration, si pinger.SceneImage) []time.Duration {
	if len(base) <= 1 || (si.FrameMs <= 0 && (si.Speed <= 0 || si.Speed == 1)) {
		return base
	}
	out := make([]time.Duration, len(base))
	for i, d := range base {
		if si.FrameMs > 0 {
			out[i] = time.Duration(si.FrameMs) * time.Millisecond
		} else {
			out[i] = time.Duration(float64(d) / si.Speed)
		}
	}
	return out
}

func loadFrames(path string, cache map[string]decoded, fresh bool) (decoded, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return decoded{}, err
	}
	if c, ok := cache[path]; ok && !fresh && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return decoded{}, err
	}
	anim, err := pinger.DecodeAnimation(data)
	if err != nil {
		return decoded{}, err
	}
	c := decoded{mod: fi.ModTime(), size: fi.Size(), anim: anim}
	if !fresh {
		cache[path] = c
	}
	return c, nil
}

// fingerprint changes whenever the scene file or any image it uses changes.
func fingerprint(scenePath, dir string, scene pinger.Scene) string {
	fp := statStamp(scenePath)
	for _, si := range scene.Images {
		fp += "|" + statStamp(filepath.Join(dir, filepath.Base(si.File)))
	}
	return fp
}

func statStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

func waitForChange(ctx context.Context, every time.Duration, current func() string, stamp string) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if current() != stamp {
				return
			}
		}
	}
}

// reportStatus writes status.json every two seconds and logs the rate every
// minute, so pingctl status and journalctl both show what is going on.
func (d *daemon) reportStatus(ctx context.Context, path string) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	last, lastLog := d.sender.Sent.Load(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := d.sender.Sent.Load()
			s := status{Updated: now, Rate: (cur - last) / 2, Sent: cur, sceneInfo: *d.info.Load()}
			last = cur
			if data, err := json.Marshal(s); err == nil {
				tmp := path + ".tmp"
				if os.WriteFile(tmp, data, 0o644) == nil {
					os.Rename(tmp, path)
				}
			}
			if now.Sub(lastLog) >= time.Minute {
				log.Printf("rate %d pps, %d pixels per pass, %d sent", s.Rate, s.Pixels, s.Sent)
				lastLog = now
			}
		}
	}
}
