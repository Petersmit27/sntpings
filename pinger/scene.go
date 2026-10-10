package pinger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Scene is the set of images a pingd server keeps on the canvas. Images are
// drawn in order, so later ones cover earlier ones where they overlap.
// Hosts lists the servers by hostname; each sends only its share of the
// pixels, so together they cover the scene once instead of each sending it
// all.
type Scene struct {
	Images []SceneImage `json:"images"`
	Hosts  []string     `json:"hosts,omitempty"`
}

// SceneImage places one image file. X and Y are percentages of the free
// space (0 = left/top edge, 100 = right/bottom edge), like -x 50% -y 50%.
// Animated GIFs play, with the frame picked from the wall clock.
type SceneImage struct {
	Name    string  `json:"name"`
	File    string  `json:"file"`
	Src     string  `json:"src,omitempty"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Scale   float64 `json:"scale"`
	Nearest bool    `json:"nearest,omitempty"`
	FrameMs int     `json:"frame_ms,omitempty"` // animated GIFs: show every frame this long instead of the GIF's own timing
	Speed   float64 `json:"speed,omitempty"`    // animated GIFs: playback speed, 0.5 = half speed; 0 means 1
}

// LoadScene reads a scene file; a missing file is an empty scene.
func LoadScene(path string) (Scene, error) {
	var s Scene
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes the scene atomically, so a reader never sees half a file.
func (s Scene) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scene-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Shard returns this host's position among the scene's hosts and how many
// there are. A host that is not listed, or a scene without hosts, gets the
// whole scene (index 0 of 1).
func (s Scene) Shard(hostname string) (index, total int, listed bool) {
	for i, h := range s.Hosts {
		if h == hostname {
			return i, len(s.Hosts), true
		}
	}
	return 0, 1, len(s.Hosts) == 0
}

// ShardOf assigns canvas pixel (x, y) to one of total servers. The hash
// scatters neighbouring pixels across servers, and depends only on the
// position, so a pixel stays with the same server when its colour changes.
func ShardOf(x, y, total int) int {
	if total <= 1 {
		return 0
	}
	h := uint32(x)*0x9E3779B1 ^ uint32(y)*0x85EBCA77
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	return int(h % uint32(total))
}

// Find returns the index of the image called name, or -1.
func (s Scene) Find(name string) int {
	for i, img := range s.Images {
		if img.Name == name {
			return i
		}
	}
	return -1
}

// StepScale returns the next scale up (dir > 0) or down (dir < 0) on the
// same grid the -live keys use.
func StepScale(s float64, dir int) float64 {
	return stepScale(s, dir)
}

// SpeedSteps are the playback speeds pingctl edit steps through.
var SpeedSteps = []float64{0.05, 0.1, 0.2, 0.25, 0.33, 0.5, 0.75, 1, 1.25, 1.5, 2, 3, 4}

// StepSpeed returns the next playback speed up (dir > 0) or down (dir < 0).
func StepSpeed(speed float64, dir int) float64 {
	if speed <= 0 {
		speed = 1
	}
	if dir > 0 {
		for _, v := range SpeedSteps {
			if v > speed+1e-9 {
				return v
			}
		}
		return SpeedSteps[len(SpeedSteps)-1]
	}
	for i := len(SpeedSteps) - 1; i >= 0; i-- {
		if SpeedSteps[i] < speed-1e-9 {
			return SpeedSteps[i]
		}
	}
	return SpeedSteps[0]
}

// ClampPct limits a position to 0-100% in 0.1% steps.
func ClampPct(v float64) float64 {
	return clampPct(v)
}
