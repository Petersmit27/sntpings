package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"sntping/pinger"
)

const editHelp = "tab/shift-tab or 1-9: select   arrows: move 1%   shift+arrows: 10%   ctrl/alt+arrows: 0.1%\n" +
	"+/-: scale   [ ]: scale -/+ 0.01   , .: GIF slower/faster   n: nearest on/off   t: bring to top   x x: remove\n" +
	"q: quit (the scene keeps running)"

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyTab
	keyBackTab
)

type keyPress struct {
	k     key
	shift bool
	fine  bool
	r     byte
}

// livePusher keeps one ssh session per server open and streams each new
// scene to it as a single JSON line, so a held arrow key is cheap.
type livePusher struct {
	ins   []io.WriteCloser
	procs []*exec.Cmd
}

func startPusher() (*livePusher, error) {
	p := &livePusher{}
	script := "cd " + remoteDir + " && while IFS= read -r l; do printf '%s\\n' \"$l\" > scene.json.tmp && mv scene.json.tmp scene.json; done"
	for _, h := range hosts {
		c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-T", h, script)
		in, err := c.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := c.Start(); err != nil {
			return nil, err
		}
		p.ins = append(p.ins, in)
		p.procs = append(p.procs, c)
	}
	return p, nil
}

func (p *livePusher) push(s pinger.Scene) int {
	data, _ := json.Marshal(s)
	data = append(data, '\n')
	ok := 0
	for _, in := range p.ins {
		if _, err := in.Write(data); err == nil {
			ok++
		}
	}
	return ok
}

func (p *livePusher) close() {
	for _, in := range p.ins {
		in.Close()
	}
	for _, c := range p.procs {
		c.Wait()
	}
}

func cmdEdit() error {
	s, err := loadScene()
	if err != nil {
		return err
	}
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("edit needs a terminal")
	}
	raw := *old
	raw.Iflag &^= unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)

	pusher, err := startPusher()
	if err != nil {
		return err
	}
	defer pusher.close()

	keys := make(chan keyPress)
	go readKeys(os.Stdin, keys)
	rates := make(chan []string, 1)
	go pollRates(rates)

	sel, armedRemove := 0, false
	msg := "connected to " + fmt.Sprint(len(hosts)) + " servers"
	var rateLines []string
	draw := func() { drawEdit(s, sel, msg, rateLines) }
	draw()

	for {
		select {
		case rateLines = <-rates:
			draw()
			continue
		case kp, ok := <-keys:
			if !ok || kp.r == 'q' || kp.r == 'Q' || kp.r == 0x03 || kp.r == 0x04 {
				fmt.Print("\r\n")
				return nil
			}
			changed, note := applyKey(&s, &sel, &armedRemove, kp)
			if changed {
				if err := s.Save(sceneFile); err != nil {
					msg = "save failed: " + err.Error()
				} else {
					msg = fmt.Sprintf("pushed to %d/%d servers at %s", pusher.push(s), len(hosts), time.Now().Format("15:04:05"))
				}
			}
			if note != "" {
				msg = note
			}
			draw()
		}
	}
}

// applyKey changes the scene or selection for one key press and reports
// whether the scene changed.
func applyKey(s *pinger.Scene, sel *int, armedRemove *bool, kp keyPress) (bool, string) {
	n := len(s.Images)
	wasArmed := *armedRemove
	*armedRemove = false
	if n == 0 {
		return false, "scene is empty; add images with: pingctl add <image>"
	}
	*sel = min(*sel, n-1)
	img := &s.Images[*sel]
	step := 1.0
	switch {
	case kp.fine:
		step = 0.1
	case kp.shift:
		step = 10
	}
	switch {
	case kp.k == keyTab:
		*sel = (*sel + 1) % n
	case kp.k == keyBackTab:
		*sel = (*sel + n - 1) % n
	case kp.r >= '1' && kp.r <= '9':
		if i := int(kp.r - '1'); i < n {
			*sel = i
		}
	case kp.k == keyLeft:
		img.X = pinger.ClampPct(img.X - step)
		return true, ""
	case kp.k == keyRight:
		img.X = pinger.ClampPct(img.X + step)
		return true, ""
	case kp.k == keyUp:
		img.Y = pinger.ClampPct(img.Y - step)
		return true, ""
	case kp.k == keyDown:
		img.Y = pinger.ClampPct(img.Y + step)
		return true, ""
	case kp.r == '+' || kp.r == '=':
		img.Scale = pinger.StepScale(img.Scale, 1)
		return true, ""
	case kp.r == '-' || kp.r == '_':
		img.Scale = pinger.StepScale(img.Scale, -1)
		return true, ""
	case kp.r == ']':
		img.Scale = fineScale(img.Scale, 1)
		return true, ""
	case kp.r == '[':
		img.Scale = fineScale(img.Scale, -1)
		return true, ""
	case kp.r == ',' || kp.r == '<' || kp.r == '.' || kp.r == '>':
		dir := 1
		if kp.r == ',' || kp.r == '<' {
			dir = -1
		}
		note := ""
		if img.FrameMs > 0 {
			note = "cleared -frame-ms so the speed takes effect"
		}
		img.FrameMs = 0
		img.Speed = speedField(pinger.StepSpeed(img.Speed, dir))
		return true, note
	case kp.r == 'n' || kp.r == 'N':
		img.Nearest = !img.Nearest
		return true, ""
	case kp.r == 't' || kp.r == 'T':
		moved := *img
		s.Images = append(append(s.Images[:*sel:*sel], s.Images[*sel+1:]...), moved)
		*sel = n - 1
		return true, ""
	case kp.r == 'x' || kp.r == 'X':
		if !wasArmed {
			*armedRemove = true
			return false, "press x again to remove " + img.Name
		}
		name := img.Name
		s.Images = append(s.Images[:*sel], s.Images[*sel+1:]...)
		*sel = max(0, min(*sel, len(s.Images)-1))
		return true, "removed " + name
	}
	return false, ""
}

func drawEdit(s pinger.Scene, sel int, msg string, rates []string) {
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString("pingctl edit\n" + editHelp + "\n\n")
	if len(s.Images) == 0 {
		b.WriteString("  (scene is empty)\n")
	}
	for i, img := range s.Images {
		mark := "  "
		if i == sel {
			mark = "> "
		}
		nearest := ""
		if img.Nearest {
			nearest = "nearest"
		}
		fmt.Fprintf(&b, "%s%d %-24s -x %-6s -y %-6s -scale %-5s %-11s %-8s %s\n",
			mark, i+1, img.Name, num(img.X)+"%", num(img.Y)+"%", num(img.Scale), size(img), nearest, timing(img))
	}
	b.WriteString("\n" + msg + "\n")
	for i, r := range rates {
		fmt.Fprintf(&b, "%d %s\n", i+1, firstLine(r))
	}
	os.Stdout.WriteString(b.String())
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func pollRates(out chan<- []string) {
	for {
		lines := statusLines()
		select {
		case out <- lines:
		default:
		}
		time.Sleep(3 * time.Second)
	}
}

func readKeys(r io.Reader, out chan<- keyPress) {
	defer close(out)
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case '\t':
			out <- keyPress{k: keyTab}
		case 0x1b:
			if kp, ok := readEscape(br); ok {
				out <- kp
			}
		default:
			out <- keyPress{r: b}
		}
	}
}

// readEscape decodes ESC [ A..D (arrows), ESC [ 1 ; <mod> A..D (modified
// arrows: 2 shift, 3 alt, 5 ctrl, 6 ctrl+shift, 7 ctrl+alt) and ESC [ Z
// (shift+tab). Ctrl and alt both mean a fine step, since some desktops
// grab ctrl+arrow.
func readEscape(br *bufio.Reader) (keyPress, bool) {
	if b, err := br.ReadByte(); err != nil || b != '[' {
		return keyPress{}, false
	}
	c, err := br.ReadByte()
	if err != nil {
		return keyPress{}, false
	}
	kp := keyPress{}
	if c == '1' {
		semi, err1 := br.ReadByte()
		mod, err2 := br.ReadByte()
		last, err3 := br.ReadByte()
		if err1 != nil || err2 != nil || err3 != nil || semi != ';' {
			return keyPress{}, false
		}
		kp.shift = mod == '2'
		kp.fine = mod == '3' || mod == '5' || mod == '6' || mod == '7'
		c = last
	}
	switch c {
	case 'A':
		kp.k = keyUp
	case 'B':
		kp.k = keyDown
	case 'C':
		kp.k = keyRight
	case 'D':
		kp.k = keyLeft
	case 'Z':
		kp.k = keyBackTab
	default:
		return keyPress{}, false
	}
	return kp, true
}

// fineScale nudges the scale by 0.01, never below 0.01.
func fineScale(s float64, dir int) float64 {
	return math.Max(0.01, math.Round(s*100+float64(dir))/100)
}
