// sntping draws an image on the SNTpings canvas by sending ICMPv6 echo
// requests to addresses that encode the pixel coordinate and colour.
//
// See README.md for the full guide.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"image"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"sntping/pinger"
)

func main() {
	var (
		imgPath   = flag.String("image", "", "path to the image to draw (png/jpeg/gif/bmp/tiff/webp) [required]")
		prefixStr = flag.String("prefix", "", "IPv6 /64 prefix to ping, e.g. 2001:db8:b317:a000:: [required]")
		canvasW   = flag.Int("w", 3840, "canvas width in pixels")
		canvasH   = flag.Int("h", 2160, "canvas height in pixels")
		fit       = flag.String("fit", "fit", "map image onto the canvas: fit | stretch | none")
		scale     = flag.Float64("scale", 0, "resize factor, e.g. 0.5 or 2 (overrides -fit)")
		resizeW   = flag.Int("rw", 0, "resize image to this width in pixels (overrides -scale)")
		resizeH   = flag.Int("rh", 0, "resize image to this height in pixels (overrides -fit)")
		live      = flag.Bool("live", false, "move (arrows) and scale (+/-) the image while it runs; prints the flags to rerun with")
		nearest   = flag.Bool("nearest", false, "resize with nearest-neighbour (crisp pixels) instead of Catmull-Rom (smooth)")
		minAlpha  = flag.Int("min-alpha", 16, "skip pixels whose alpha is below this value (0-255)")
		shuffle   = flag.Bool("shuffle", false, "send pixels in random order (noise) instead of scan lines")
		seed      = flag.Int64("seed", 0, "random seed for -shuffle (0 = time-based)")
		kitchen   = flag.Bool("kitchen-sink", false, "enable everything: shuffle + loop + drifting image")
		dvd       = flag.Bool("dvd", false, "bounce the image around the canvas, DVD screensaver style")
		dvdSpeed  = flag.Int("dvd-speed", 4, "pixels the image moves per pass with -dvd")
		hue       = flag.Float64("hue", 0, "sweep the hue by this many degrees across each pass (implies -loop)")
		workers   = flag.Int("workers", 0, "number of sender goroutines / sockets (0 = 2x CPU cores)")
		pps       = flag.Int("pps", 0, "packets-per-second ceiling (0 = unlimited)")
		loop      = flag.Bool("loop", false, "keep redrawing the image until interrupted")
		interval  = flag.Duration("interval", 0, "pause between passes when -loop is set (0 = none)")
		dry       = flag.Bool("dry", false, "print the destination addresses instead of sending")
		maxPkts   = flag.Int("max", 0, "max packets sent per pass (0 = all pixels), handy for testing")
		bind      = flag.String("bind", "::", "local IPv6 address to bind the sockets to")
		network   = flag.String("socket", "udp6", "socket type: udp6 (unprivileged) or ip6:ipv6-icmp (needs CAP_NET_RAW)")
	)
	var posX, posY pinger.Pos
	flag.Func("x", "left edge on the canvas: pixels, or a percentage of the free space such as 25% (default: centered)", posFlag(&posX))
	flag.Func("y", "top edge on the canvas: pixels, or a percentage of the free space such as 80% (default: centered)", posFlag(&posY))
	flag.Parse()

	var xSet, ySet bool
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "x":
			xSet = true
		case "y":
			ySet = true
		}
	})

	if *imgPath == "" || *prefixStr == "" {
		fmt.Fprintln(os.Stderr, "error: both -image and -prefix are required")
		flag.Usage()
		os.Exit(2)
	}
	if *canvasW <= 0 || *canvasH <= 0 || *canvasW > 0xFFFF || *canvasH > 0xFFFF {
		fatalf("canvas must be between 1x1 and 65535x65535")
	}
	if *scale < 0 || *resizeW < 0 || *resizeH < 0 {
		fatalf("-scale, -rw and -rh cannot be negative")
	}

	if *live && (*dvd || *kitchen || *dry) {
		fatalf("-live cannot be combined with -dvd, -kitchen-sink or -dry")
	}

	prefix, err := pinger.ParsePrefix(*prefixStr)
	if err != nil {
		fatalf("invalid -prefix: %v", err)
	}

	f, err := os.Open(*imgPath)
	if err != nil {
		fatalf("loading image: %v", err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		fatalf("decoding image: %v", err)
	}

	fmt.Fprintf(os.Stderr, "prefix      : %s\n", *prefixStr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var adjust chan pinger.Adjust
	var oldTerm *unix.Termios
	if *live {
		oldTerm, _ = setRaw(int(os.Stdin.Fd()))
		adjust = make(chan pinger.Adjust, 64)
		go readAdjustments(os.Stdin, adjust, stop)
	}

	err = pinger.Run(ctx, src, prefix, pinger.Options{
		CanvasW:     *canvasW,
		CanvasH:     *canvasH,
		Fit:         *fit,
		Scale:       *scale,
		RW:          *resizeW,
		RH:          *resizeH,
		X:           posX,
		Y:           posY,
		XSet:        xSet,
		YSet:        ySet,
		Nearest:     *nearest,
		Adjust:      adjust,
		MinAlpha:    *minAlpha,
		Workers:     *workers,
		PPS:         *pps,
		Loop:        *loop,
		Interval:    *interval,
		Shuffle:     *shuffle,
		Seed:        *seed,
		KitchenSink: *kitchen,
		DVD:         *dvd,
		DVDSpeed:    *dvdSpeed,
		Hue:         *hue,
		MaxPkts:     *maxPkts,
		Bind:        *bind,
		Network:     *network,
		Dry:         *dry,
	})
	if oldTerm != nil {
		unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, oldTerm)
	}
	if err != nil {
		fatalf("%v", err)
	}
}

// readAdjustments turns key presses into adjustments until q, ctrl-c,
// ctrl-d or end of input, then calls quit.
func readAdjustments(r io.Reader, out chan<- pinger.Adjust, quit func()) {
	defer quit()
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case 'q', 'Q', 0x03, 0x04:
			return
		case '+', '=':
			out <- pinger.Adjust{ScaleSteps: 1}
		case '-', '_':
			out <- pinger.Adjust{ScaleSteps: -1}
		case 0x1b:
			if a, ok := readArrow(br); ok {
				out <- a
			}
		}
	}
}

// readArrow decodes the rest of an arrow key escape sequence: ESC [ A..D,
// or ESC [ 1 ; 2 A..D for shift+arrow.
func readArrow(br *bufio.Reader) (pinger.Adjust, bool) {
	if b, err := br.ReadByte(); err != nil || b != '[' {
		return pinger.Adjust{}, false
	}
	c, err := br.ReadByte()
	if err != nil {
		return pinger.Adjust{}, false
	}
	step := 1.0
	if c == '1' {
		semi, err1 := br.ReadByte()
		mod, err2 := br.ReadByte()
		key, err3 := br.ReadByte()
		if err1 != nil || err2 != nil || err3 != nil || semi != ';' {
			return pinger.Adjust{}, false
		}
		if mod == '2' {
			step = 10
		}
		c = key
	}
	switch c {
	case 'A':
		return pinger.Adjust{DY: -step}, true
	case 'B':
		return pinger.Adjust{DY: step}, true
	case 'C':
		return pinger.Adjust{DX: step}, true
	case 'D':
		return pinger.Adjust{DX: -step}, true
	}
	return pinger.Adjust{}, false
}

// setRaw switches a terminal to unbuffered, unechoed input while leaving
// output processing alone, so the status lines still render normally.
func setRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return old, nil
}

func posFlag(dst *pinger.Pos) func(string) error {
	return func(s string) error {
		p, err := pinger.ParsePos(s)
		if err != nil {
			return err
		}
		*dst = p
		return nil
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
