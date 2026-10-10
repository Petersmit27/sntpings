package pinger

import (
	"image"
	"image/color"
	"testing"
)

func pack(x, y, r, g, b, a int) uint64 {
	return uint64(x)<<48 | uint64(y)<<32 | uint64(b)<<24 | uint64(g)<<16 | uint64(r)<<8 | uint64(a)
}

func unpack(v uint64) (x, y, r, g, b, a int) {
	return int(v>>48) & 0xFFFF, int(v>>32) & 0xFFFF, int(v>>8) & 0xFF, int(v>>16) & 0xFF, int(v>>24) & 0xFF, int(v) & 0xFF
}

func TestFormatTargetMatchesDocs(t *testing.T) {
	prefix, err := ParsePrefix("2001:db8:b317:a000::/64")
	if err != nil {
		t.Fatal(err)
	}
	got := formatTarget(prefix, pack(25, 25, 0xFF, 0xD1, 0x00, 0xFF))
	want := "2001:0db8:b317:a000:0019:0019:00d1:ffff"
	if got != want {
		t.Fatalf("formatTarget = %s, want %s", got, want)
	}
}

func TestMoveTargetsKeepsColour(t *testing.T) {
	ts := []uint64{pack(10, 20, 1, 2, 3, 4)}
	moveTargets(ts, 5, -7)
	x, y, r, g, b, a := unpack(ts[0])
	if x != 15 || y != 13 {
		t.Fatalf("moved to (%d,%d), want (15,13)", x, y)
	}
	if r != 1 || g != 2 || b != 3 || a != 4 {
		t.Fatalf("colour changed: %d %d %d %d", r, g, b, a)
	}
}

func TestStepBounceStaysInBounds(t *testing.T) {
	const w, h = 100, 80
	ts := make([]uint64, 0, 8*8)
	for y := 30; y < 38; y++ {
		for x := 40; x < 48; x++ {
			ts = append(ts, pack(x, y, 255, 255, 255, 255))
		}
	}
	vx, vy := 4, 4
	for i := 0; i < 10000; i++ {
		vx, vy = stepBounce(ts, vx, vy, w, h)
		minX, maxX, minY, maxY := targetBounds(ts)
		if minX < 0 || minY < 0 || maxX >= w || maxY >= h {
			t.Fatalf("escaped canvas at step %d: box (%d,%d)-(%d,%d)", i, minX, minY, maxX, maxY)
		}
	}
	// The box must actually have moved away from its start.
	minX, _, minY, _ := targetBounds(ts)
	if minX == 40 && minY == 30 {
		t.Fatal("image never moved")
	}
}

func TestStepBounceReflects(t *testing.T) {
	ts := []uint64{pack(0, 0, 0, 0, 0, 255), pack(10, 10, 0, 0, 0, 255)}
	vx, vy := -4, -4
	vx, vy = stepBounce(ts, vx, vy, 100, 100)
	if vx != 4 || vy != 4 {
		t.Fatalf("velocity not reflected: (%d,%d), want (4,4)", vx, vy)
	}
}

func near(t *testing.T, got, want, tol int, what string) {
	t.Helper()
	if got < want-tol || got > want+tol {
		t.Fatalf("%s = %d, want %d (+/-%d)", what, got, want, tol)
	}
}

func TestRotateHue(t *testing.T) {
	r, g, b := rotateHue(255, 0, 0, 0)
	near(t, r, 255, 2, "red@0 r")
	near(t, g, 0, 2, "red@0 g")
	near(t, b, 0, 2, "red@0 b")

	r, g, b = rotateHue(255, 0, 0, 120*3.141592653589793/180)
	near(t, r, 0, 3, "red@120 r")
	near(t, g, 255, 3, "red@120 g")
	near(t, b, 0, 3, "red@120 b")

	r, g, b = rotateHue(255, 0, 0, 240*3.141592653589793/180)
	near(t, r, 0, 3, "red@240 r")
	near(t, g, 0, 3, "red@240 g")
	near(t, b, 255, 3, "red@240 b")
}

func TestHueShiftedKeepsPositionAndAlpha(t *testing.T) {
	target := pack(10, 20, 255, 0, 0, 200)

	// hueDelta 0 (hue disabled) must return the target unchanged.
	if got := hueShifted(target, 0, 1, 0, 0); got != target {
		t.Fatalf("hue disabled changed the target: %x", got)
	}

	// Sweep 0..120 across 2 pixels: pixel 0 sits at 0deg (red), pixel 1
	// sits at 60deg (yellow).
	first := hueShifted(target, 0, 2, 0, 120)
	x, y, r, g, b, a := unpack(first)
	if x != 10 || y != 20 || a != 200 {
		t.Fatalf("first pixel position/alpha changed: (%d,%d) a=%d", x, y, a)
	}
	near(t, r, 255, 2, "first r")
	near(t, g, 0, 2, "first g")
	near(t, b, 0, 2, "first b")

	second := hueShifted(target, 1, 2, 0, 120)
	x, y, r, g, b, a = unpack(second)
	if x != 10 || y != 20 || a != 200 {
		t.Fatalf("second pixel position/alpha changed: (%d,%d) a=%d", x, y, a)
	}
	near(t, r, 255, 3, "second r")
	near(t, g, 255, 3, "second g")
	near(t, b, 0, 3, "second b")
}

func TestParsePos(t *testing.T) {
	tests := []struct {
		in   string
		want Pos
	}{
		{"1700", Pos{Px: 1700}},
		{"-20", Pos{Px: -20}},
		{"25%", Pos{Pct: 25, IsPct: true}},
		{" 33.5 % ", Pos{Pct: 33.5, IsPct: true}},
	}
	for _, tt := range tests {
		got, err := ParsePos(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParsePos(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "abc", "%", "12.5", "x%", "NaN%"} {
		if _, err := ParsePos(bad); err == nil {
			t.Errorf("ParsePos(%q) succeeded, want error", bad)
		}
	}
}

func TestPosResolve(t *testing.T) {
	tests := []struct {
		pos          Pos
		canvas, size int
		want         int
	}{
		{Pos{Px: 1700}, 3840, 448, 1700},
		{Pos{Pct: 0, IsPct: true}, 3840, 448, 0},
		{Pos{Pct: 50, IsPct: true}, 3840, 448, 1696},
		{Pos{Pct: 100, IsPct: true}, 3840, 448, 3392},
		{Pos{Pct: 80, IsPct: true}, 2160, 424, 1389},
	}
	for _, tt := range tests {
		if got := tt.pos.Resolve(tt.canvas, tt.size); got != tt.want {
			t.Errorf("%+v.Resolve(%d, %d) = %d, want %d", tt.pos, tt.canvas, tt.size, got, tt.want)
		}
	}
}

func TestRenderCanvasPercentPlacement(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	_, placement, err := renderCanvas(src, 100, 50, drawOpts{
		Scale: 2,
		X:     Pos{Pct: 100, IsPct: true},
		Y:     Pos{Pct: 50, IsPct: true},
		XSet:  true,
		YSet:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "20x20 at (80,15)"; describe(placement) != want {
		t.Fatalf("placement = %q, want %q", placement, want)
	}
}

func TestRenderCanvasNearestKeepsSourceColours(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	red := color.NRGBA{R: 255, A: 255}
	src.SetNRGBA(0, 0, red)
	src.SetNRGBA(1, 1, red)

	dst, _, err := renderCanvas(src, 8, 8, drawOpts{Scale: 4, Nearest: true})
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			got := dst.NRGBAAt(x, y)
			want := color.NRGBA{}
			if (x < 4) == (y < 4) {
				want = red
			}
			if got != want {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestStepScale(t *testing.T) {
	up := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.25, 1.5}
	for i := 0; i+1 < len(up); i++ {
		if got := stepScale(up[i], 1); got != up[i+1] {
			t.Errorf("stepScale(%v, up) = %v, want %v", up[i], got, up[i+1])
		}
		if got := stepScale(up[i+1], -1); got != up[i] {
			t.Errorf("stepScale(%v, down) = %v, want %v", up[i+1], got, up[i])
		}
	}
	if got := stepScale(1.1, 1); got != 1.25 {
		t.Errorf("stepScale(1.1, up) = %v, want 1.25", got)
	}
	if got := stepScale(0.1, -1); got != 0.1 {
		t.Errorf("stepScale(0.1, down) = %v, want 0.1", got)
	}
}

func TestLiveStateApply(t *testing.T) {
	s := liveState{XPct: 55, YPct: 95, Scale: 1}
	s = s.apply(Adjust{DX: 1, DY: 10, ScaleSteps: 2})
	if s != (liveState{XPct: 56, YPct: 100, Scale: 1.5}) {
		t.Fatalf("apply = %+v", s)
	}
	if got, want := s.flags(), "-x 56% -y 100% -scale 1.5"; got != want {
		t.Fatalf("flags = %q, want %q", got, want)
	}
}

func TestInitialLiveFromPixels(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	placed := image.Rect(45, 0, 65, 20)
	s := initialLive(src, placed, Options{CanvasW: 110, CanvasH: 50, XSet: true, X: Pos{Px: 45}})
	if s != (liveState{XPct: 50, YPct: 50, Scale: 2}) {
		t.Fatalf("initialLive = %+v", s)
	}
}

func TestComposeLaterLayersOnTop(t *testing.T) {
	red := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	blue := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			red.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			blue.SetNRGBA(x, y, color.NRGBA{B: 255, A: 255})
		}
	}
	dst, rects, err := Compose(10, 4, []Layer{
		{Img: red, XPct: 0, YPct: 0, Scale: 1, Nearest: true},
		{Img: blue, XPct: 50, YPct: 0, Scale: 1, Nearest: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rects[1] != image.Rect(3, 0, 7, 4) {
		t.Fatalf("blue placed at %v", rects[1])
	}
	for x, want := range []color.NRGBA{
		{R: 255, A: 255}, {R: 255, A: 255}, {R: 255, A: 255},
		{B: 255, A: 255}, {B: 255, A: 255}, {B: 255, A: 255}, {B: 255, A: 255},
		{}, {}, {},
	} {
		if got := dst.NRGBAAt(x, 1); got != want {
			t.Errorf("pixel %d = %v, want %v", x, got, want)
		}
	}
}

func TestSceneSaveLoad(t *testing.T) {
	path := t.TempDir() + "/scene.json"
	if s, err := LoadScene(path); err != nil || len(s.Images) != 0 {
		t.Fatalf("missing scene = %+v, %v", s, err)
	}
	want := Scene{Images: []SceneImage{{Name: "a", File: "a.png", X: 47.5, Y: 81, Scale: 0.4, Nearest: true}}}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadScene(path)
	if err != nil || len(got.Images) != 1 || got.Images[0] != want.Images[0] {
		t.Fatalf("LoadScene = %+v, %v", got, err)
	}
	if got.Find("a") != 0 || got.Find("b") != -1 {
		t.Fatal("Find")
	}
}

func TestClearNRGBASubImageLeavesOutsideAlone(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	clearNRGBA(img.SubImage(image.Rect(1, 1, 3, 3)).(*image.NRGBA))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			inside := x >= 1 && x < 3 && y >= 1 && y < 3
			if got := img.NRGBAAt(x, y).A; (got == 0) != inside {
				t.Fatalf("pixel (%d,%d) alpha %d, inside=%v", x, y, got, inside)
			}
		}
	}
}

func TestStepSpeed(t *testing.T) {
	for _, tt := range []struct {
		from float64
		dir  int
		want float64
	}{
		{0, -1, 0.75}, {1, -1, 0.75}, {1, 1, 1.25}, {0.5, -1, 0.33}, {0.05, -1, 0.05},
		{4, 1, 4}, {0.6, -1, 0.5}, {0.6, 1, 0.75},
	} {
		if got := StepSpeed(tt.from, tt.dir); got != tt.want {
			t.Errorf("StepSpeed(%v, %d) = %v, want %v", tt.from, tt.dir, got, tt.want)
		}
	}
}

func TestShardOfSpreadsEvenly(t *testing.T) {
	const total = 5
	counts := make([]int, total)
	for y := 1000; y < 1100; y++ {
		for x := 2000; x < 2100; x++ {
			counts[ShardOf(x, y, total)]++
		}
	}
	for i, c := range counts {
		if c < 1800 || c > 2200 {
			t.Errorf("shard %d got %d of 10000 pixels, want about 2000", i, c)
		}
	}
	if ShardOf(5, 7, 1) != 0 || ShardOf(5, 7, 0) != 0 {
		t.Error("a single server must own every pixel")
	}
}

func TestSceneShard(t *testing.T) {
	s := Scene{Hosts: []string{"pings", "pings2", "pings3"}}
	if i, n, ok := s.Shard("pings2"); i != 1 || n != 3 || !ok {
		t.Errorf("pings2 = %d/%d listed=%v", i, n, ok)
	}
	if i, n, ok := s.Shard("other"); i != 0 || n != 1 || ok {
		t.Errorf("unlisted host = %d/%d listed=%v, want the whole scene, not listed", i, n, ok)
	}
	if i, n, ok := (Scene{}).Shard("pings"); i != 0 || n != 1 || !ok {
		t.Errorf("no hosts = %d/%d listed=%v", i, n, ok)
	}
}
