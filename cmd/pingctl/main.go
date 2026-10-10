// Command pingctl manages the scene that pingd draws on every server: the
// local scene.json is the source of truth and every change is pushed to all
// servers, where pingd picks it up within a fraction of a second.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"sntping/pinger"
)

const (
	remoteDir = "/root/pings"
	prefix    = "2001:610:5ea:221e::"
	sceneFile = "scene.json"
)

var hosts = []string{
	"root@pings.smittenfeld.nl",
	"root@pings2.smittenfeld.nl",
	"root@pings3.smittenfeld.nl",
	"root@pings4.smittenfeld.nl",
	"root@pings5.smittenfeld.nl",
}

const usage = `usage: pingctl <command> [flags]

  list                                   show the scene
  add <image> [-name n] [-x 50] [-y 50] [-scale 1] [-nearest] [-speed 1] [-frame-ms N]
                                         upload an image and add it on top
  set <name> [-x] [-y] [-scale] [-nearest=true|false] [-speed] [-frame-ms N] [-top]
                                         change an image
  rm <name>                              remove an image
  edit                                   steer the scene live with the keyboard
  push                                   upload scene.json and its images again
  status                                 pingd state and rate per server
  deploy                                 build pingd and (re)install it on every server

x and y are percentages of the free space: 0 = left/top, 100 = right/bottom.
Animated GIFs play on every server in sync. -speed 0.5 plays at half speed
(2 = double), keeping the GIF's own rhythm; -frame-ms shows every frame that
long instead and overrides -speed (0 = off).
Each server sends only its own share of the pixels (split by position), so
together they cover the scene once. Servers: PINGCTL_HOSTS (comma separated)
overrides the built-in list; push afterwards to re-split, e.g. without a dead
server.`

func main() {
	if h := os.Getenv("PINGCTL_HOSTS"); h != "" {
		hosts = strings.Split(h, ",")
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "list", "ls":
		err = cmdList()
	case "add":
		err = cmdAdd(args)
	case "set":
		err = cmdSet(args)
	case "rm", "remove":
		err = cmdRemove(args)
	case "edit":
		err = cmdEdit()
	case "push":
		err = cmdPush()
	case "status":
		err = cmdStatus()
	case "deploy":
		err = cmdDeploy()
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// loadScene reads the local scene and stamps it with the current server
// list, so every push tells each pingd which share of the pixels is its own.
func loadScene() (pinger.Scene, error) {
	s, err := pinger.LoadScene(sceneFile)
	s.Hosts = hostnames()
	return s, err
}

// hostnames turns "root@pings2.smittenfeld.nl" into "pings2", the name the
// server itself reports.
func hostnames() []string {
	names := make([]string, len(hosts))
	for i, h := range hosts {
		_, h, _ = strings.Cut(h, "@")
		if h == "" {
			h = hosts[i]
		}
		names[i], _, _ = strings.Cut(h, ".")
	}
	return names
}

// commit saves the scene locally and pushes it to every server.
func commit(s pinger.Scene) error {
	if err := s.Save(sceneFile); err != nil {
		return err
	}
	return pushScene(s)
}

func cmdList() error {
	s, err := loadScene()
	if err != nil {
		return err
	}
	if len(s.Images) == 0 {
		fmt.Println("scene is empty; add an image with: pingctl add <image>")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tNAME\tFILE\tX\tY\tSCALE\tSIZE\tNEAREST\tFRAMES")
	for i, img := range s.Images {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s%%\t%s%%\t%s\t%s\t%v\t%s\n", i+1, img.Name, img.File,
			num(img.X), num(img.Y), num(img.Scale), size(img), img.Nearest, frames(img))
	}
	tw.Flush()
	fmt.Println("(later images are drawn on top)")
	return nil
}

func cmdAdd(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: pingctl add <image> [-name n] [-x 50] [-y 50] [-scale 1] [-nearest] [-speed 1] [-frame-ms N]")
	}
	path := args[0]
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "name of the image in the scene (default: file name)")
	x := fs.String("x", "50", "horizontal position, percent")
	y := fs.String("y", "50", "vertical position, percent")
	scale := fs.Float64("scale", 1, "scale factor")
	nearest := fs.Bool("nearest", false, "nearest-neighbour scaling (crisp pixels)")
	frameMs := fs.Int("frame-ms", 0, "animated GIFs: show each frame this many ms (0 = the GIF's timing)")
	speed := fs.Float64("speed", 1, "animated GIFs: playback speed, 0.5 = half speed")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *frameMs < 0 {
		return fmt.Errorf("-frame-ms cannot be negative")
	}
	if *speed <= 0 {
		return fmt.Errorf("-speed must be greater than 0")
	}
	xp, err := parsePct(*x)
	if err != nil {
		return err
	}
	yp, err := parsePct(*y)
	if err != nil {
		return err
	}
	if *scale <= 0 {
		return fmt.Errorf("-scale must be greater than 0")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := imageSize(abs); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	s, err := loadScene()
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	n = uniqueName(s, n)
	if err := uploadFiles(abs); err != nil {
		return err
	}
	s.Images = append(s.Images, pinger.SceneImage{
		Name: n, File: filepath.Base(abs), Src: abs,
		X: xp, Y: yp, Scale: *scale, Nearest: *nearest, FrameMs: *frameMs, Speed: speedField(*speed),
	})
	if err := commit(s); err != nil {
		return err
	}
	fmt.Printf("added %s\n", n)
	return nil
}

func cmdSet(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: pingctl set <name> [-x] [-y] [-scale] [-nearest=true|false] [-speed] [-frame-ms N] [-top]")
	}
	s, err := loadScene()
	if err != nil {
		return err
	}
	i := s.Find(args[0])
	if i < 0 {
		return fmt.Errorf("no image called %q (see pingctl list)", args[0])
	}
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	x := fs.String("x", "", "horizontal position, percent")
	y := fs.String("y", "", "vertical position, percent")
	scale := fs.Float64("scale", 0, "scale factor")
	nearest := fs.Bool("nearest", false, "nearest-neighbour scaling")
	frameMs := fs.Int("frame-ms", 0, "animated GIFs: show each frame this many ms (0 = the GIF's timing)")
	speed := fs.Float64("speed", 1, "animated GIFs: playback speed, 0.5 = half speed")
	top := fs.Bool("top", false, "draw this image on top of the others")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	img := s.Images[i]
	var errs []error
	fs.Visit(func(f *flag.Flag) {
		var err error
		switch f.Name {
		case "x":
			img.X, err = parsePct(*x)
		case "y":
			img.Y, err = parsePct(*y)
		case "scale":
			if *scale <= 0 {
				err = fmt.Errorf("-scale must be greater than 0")
			}
			img.Scale = *scale
		case "nearest":
			img.Nearest = *nearest
		case "frame-ms":
			if *frameMs < 0 {
				err = fmt.Errorf("-frame-ms cannot be negative")
			}
			img.FrameMs = *frameMs
		case "speed":
			if *speed <= 0 {
				err = fmt.Errorf("-speed must be greater than 0")
			}
			img.Speed = speedField(*speed)
		}
		if err != nil {
			errs = append(errs, err)
		}
	})
	if len(errs) > 0 {
		return errs[0]
	}
	s.Images[i] = img
	if *top {
		s.Images = append(append(s.Images[:i:i], s.Images[i+1:]...), img)
	}
	return commit(s)
}

func cmdRemove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: pingctl rm <name>")
	}
	s, err := loadScene()
	if err != nil {
		return err
	}
	i := s.Find(args[0])
	if i < 0 {
		return fmt.Errorf("no image called %q (see pingctl list)", args[0])
	}
	s.Images = append(s.Images[:i], s.Images[i+1:]...)
	return commit(s)
}

func cmdPush() error {
	s, err := loadScene()
	if err != nil {
		return err
	}
	var files []string
	for _, img := range s.Images {
		if img.Src != "" {
			files = append(files, img.Src)
		}
	}
	if len(files) > 0 {
		if err := uploadFiles(files...); err != nil {
			return err
		}
	}
	return pushScene(s)
}

type remoteStatus struct {
	Updated time.Time `json:"updated"`
	Shard   string    `json:"shard"`
	Rate    int64     `json:"rate"`
	Sent    int64     `json:"sent"`
	Pixels  int       `json:"pixels"`
	Images  []string  `json:"images"`
	Errors  []string  `json:"errors"`
}

func cmdStatus() error {
	for i, line := range statusLines() {
		fmt.Printf("%d %-28s %s\n", i+1, hosts[i], line)
	}
	return nil
}

func statusLines() []string {
	out := make([]string, len(hosts))
	onEachHost(func(i int, h string) error {
		raw, err := ssh(h, nil, "systemctl is-active pingd; cat "+remoteDir+"/status.json 2>/dev/null")
		active, js, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
		if err != nil && active == "" {
			out[i] = "unreachable: " + err.Error()
			return nil
		}
		if active != "active" {
			out[i] = "pingd " + active
			return nil
		}
		var st remoteStatus
		if json.Unmarshal([]byte(js), &st) != nil {
			out[i] = "pingd active, no status yet"
			return nil
		}
		shard := ""
		if st.Shard != "" {
			shard = "share " + st.Shard + "  "
		}
		line := fmt.Sprintf("%7d pps  %8d px/pass  %s%s", st.Rate, st.Pixels, shard, strings.Join(st.Images, ", "))
		if age := time.Since(st.Updated); age > 10*time.Second {
			line += fmt.Sprintf("  (status %s old)", age.Round(time.Second))
		}
		for _, e := range st.Errors {
			line += "\n    error: " + e
		}
		out[i] = line
		return nil
	})
	return out
}

func cmdDeploy() error {
	if _, err := os.Stat("go.mod"); err != nil {
		return fmt.Errorf("run deploy from the project directory (needs go.mod)")
	}
	tmp, err := os.MkdirTemp("", "pingctl")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "pingd")
	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o", bin, "./cmd/pingd")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building pingd: %w", err)
	}
	unit := filepath.Join(tmp, "pingd.service")
	if err := os.WriteFile(unit, []byte(fmt.Sprintf(`[Unit]
Description=SNTpings image daemon
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=%[1]s
ExecStart=%[1]s/pingd -prefix %[2]s
Environment=GOMEMLIMIT=300MiB
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, remoteDir, prefix)), 0o644); err != nil {
		return err
	}

	s, err := loadScene()
	if err != nil {
		return err
	}
	if err := s.Save(sceneFile); err != nil {
		return err
	}
	files := []string{bin, unit, sceneFile}
	for _, img := range s.Images {
		if img.Src != "" {
			files = append(files, img.Src)
		}
	}
	if err := uploadFiles(files...); err != nil {
		return err
	}
	err = onEachHost(func(_ int, h string) error {
		_, err := ssh(h, nil, "cd "+remoteDir+" && mv pingd.service /etc/systemd/system/pingd.service"+
			" && (tmux kill-session -t pings 2>/dev/null; pkill -x sntping; true)"+
			" && systemctl daemon-reload && systemctl enable pingd >/dev/null 2>&1 && systemctl restart pingd")
		return err
	})
	if err != nil {
		return err
	}
	time.Sleep(4 * time.Second)
	return cmdStatus()
}

// pushScene writes the scene to every server; pingd notices the new file.
func pushScene(s pinger.Scene) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return onEachHost(func(_ int, h string) error {
		_, err := ssh(h, data, "cat > "+remoteDir+"/scene.json.tmp && mv "+remoteDir+"/scene.json.tmp "+remoteDir+"/scene.json")
		return err
	})
}

// uploadFiles copies files to every server. rsync skips files that are
// already there, so only new or changed ones cost time; big ones are
// announced so a long upload does not look like a hang.
func uploadFiles(paths ...string) error {
	var total int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	verbose := total > 2<<20
	if verbose {
		fmt.Printf("uploading %.1f MB to %d servers (up to %.0f MB in total)...\n",
			float64(total)/(1<<20), len(hosts), float64(total)*float64(len(hosts))/(1<<20))
	}
	return onEachHost(func(_ int, h string) error {
		start := time.Now()
		args := append([]string{"-az", "--no-owner", "--no-group", "--mkpath"}, paths...)
		args = append(args, h+":"+remoteDir+"/")
		out, err := exec.Command("rsync", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("rsync to %s: %v: %s", h, err, bytes.TrimSpace(out))
		}
		if verbose {
			fmt.Printf("  %s done in %s\n", h, time.Since(start).Round(time.Second))
		}
		return nil
	})
}

func ssh(host string, stdin []byte, command string) ([]byte, error) {
	c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %v: %s", host, err, bytes.TrimSpace(errOut.Bytes()))
	}
	return out.Bytes(), nil
}

// onEachHost runs fn for every server in parallel and returns the first error.
func onEachHost(fn func(i int, host string) error) error {
	errs := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(i, h)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func parsePct(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("bad position %q: want a percentage such as 47.5", s)
	}
	return pinger.ClampPct(v), nil
}

func uniqueName(s pinger.Scene, base string) string {
	name := base
	for n := 2; s.Find(name) >= 0; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

func imageSize(path string) (image.Point, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Point{}, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return image.Point{}, err
	}
	return image.Pt(cfg.Width, cfg.Height), nil
}

func size(img pinger.SceneImage) string {
	p, err := imageSize(img.Src)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%dx%d", max(1, int(float64(p.X)*img.Scale+0.5)), max(1, int(float64(p.Y)*img.Scale+0.5)))
}

func frames(img pinger.SceneImage) string {
	data, err := os.ReadFile(img.Src)
	if err != nil {
		return "?"
	}
	a, err := pinger.DecodeAnimation(data)
	if err != nil {
		return "?"
	}
	if a.Len() == 1 {
		return "-"
	}
	if img.FrameMs > 0 {
		return fmt.Sprintf("%d x %dms", a.Len(), img.FrameMs)
	}
	var total time.Duration
	for _, d := range a.Delays {
		total += d
	}
	if img.Speed > 0 && img.Speed != 1 {
		total = time.Duration(float64(total) / img.Speed)
		return fmt.Sprintf("%d, %s loop at %sx", a.Len(), total.Round(10*time.Millisecond), num(img.Speed))
	}
	return fmt.Sprintf("%d, %s loop", a.Len(), total.Round(10*time.Millisecond))
}

// speedField stores normal speed as the zero value, keeping scene files tidy.
func speedField(v float64) float64 {
	if v == 1 {
		return 0
	}
	return v
}

// timing describes how an image's animation is paced, for the edit screen.
func timing(img pinger.SceneImage) string {
	switch {
	case img.FrameMs > 0:
		return fmt.Sprintf("%dms/frame", img.FrameMs)
	case img.Speed > 0 && img.Speed != 1:
		return num(img.Speed) + "x"
	}
	return ""
}

func num(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
