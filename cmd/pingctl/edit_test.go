package main

import (
	"bufio"
	"strings"
	"testing"

	"sntping/pinger"
)

func scene() pinger.Scene {
	return pinger.Scene{Images: []pinger.SceneImage{
		{Name: "a", X: 50, Y: 50, Scale: 1},
		{Name: "b", X: 10, Y: 10, Scale: 0.4},
	}}
}

func TestApplyKeyMovesAndScalesSelected(t *testing.T) {
	s, sel, armed := scene(), 0, false
	applyKey(&s, &sel, &armed, keyPress{k: keyTab})
	applyKey(&s, &sel, &armed, keyPress{k: keyRight, shift: true})
	applyKey(&s, &sel, &armed, keyPress{k: keyUp})
	applyKey(&s, &sel, &armed, keyPress{r: '+'})
	applyKey(&s, &sel, &armed, keyPress{r: 'n'})
	if got := s.Images[1]; got.X != 20 || got.Y != 9 || got.Scale != 0.5 || !got.Nearest {
		t.Fatalf("b = %+v", got)
	}
	if s.Images[0] != scene().Images[0] {
		t.Fatalf("a changed: %+v", s.Images[0])
	}
}

func TestApplyKeyRemoveNeedsTwoPresses(t *testing.T) {
	s, sel, armed := scene(), 0, false
	if changed, _ := applyKey(&s, &sel, &armed, keyPress{r: 'x'}); changed || len(s.Images) != 2 {
		t.Fatal("first x removed the image")
	}
	if changed, _ := applyKey(&s, &sel, &armed, keyPress{r: 'x'}); !changed || len(s.Images) != 1 || s.Images[0].Name != "b" {
		t.Fatalf("second x: %+v", s.Images)
	}
	applyKey(&s, &sel, &armed, keyPress{r: 'x'})
	applyKey(&s, &sel, &armed, keyPress{k: keyLeft})
	if changed, _ := applyKey(&s, &sel, &armed, keyPress{r: 'x'}); changed {
		t.Fatal("x after another key should only re-arm")
	}
}

func TestApplyKeyBringToTop(t *testing.T) {
	s, sel, armed := scene(), 0, false
	applyKey(&s, &sel, &armed, keyPress{r: 't'})
	if s.Images[1].Name != "a" || sel != 1 {
		t.Fatalf("order %s,%s sel %d", s.Images[0].Name, s.Images[1].Name, sel)
	}
}

func TestReadKeys(t *testing.T) {
	ch := make(chan keyPress, 16)
	readKeys(bufio.NewReader(strings.NewReader("\t\x1b[Z\x1b[C\x1b[1;2A\x1b[1;5D\x1b[1;3Bq")), ch)
	var got []keyPress
	for kp := range ch {
		got = append(got, kp)
	}
	want := []keyPress{{k: keyTab}, {k: keyBackTab}, {k: keyRight}, {k: keyUp, shift: true}, {k: keyLeft, fine: true}, {k: keyDown, fine: true}, {r: 'q'}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestApplyKeyFineSteps(t *testing.T) {
	s, sel, armed := scene(), 0, false
	applyKey(&s, &sel, &armed, keyPress{k: keyRight, fine: true})
	applyKey(&s, &sel, &armed, keyPress{k: keyRight, fine: true})
	applyKey(&s, &sel, &armed, keyPress{k: keyUp, fine: true})
	applyKey(&s, &sel, &armed, keyPress{r: ']'})
	applyKey(&s, &sel, &armed, keyPress{r: ']'})
	applyKey(&s, &sel, &armed, keyPress{r: '['})
	if got := s.Images[0]; got.X != 50.2 || got.Y != 49.9 || got.Scale != 1.01 {
		t.Fatalf("a = %+v", got)
	}
}

func TestFineScaleFloor(t *testing.T) {
	if got := fineScale(0.01, -1); got != 0.01 {
		t.Fatalf("fineScale(0.01, down) = %v", got)
	}
	if got := fineScale(0.4, 1); got != 0.41 {
		t.Fatalf("fineScale(0.4, up) = %v", got)
	}
}

func TestApplyKeySpeed(t *testing.T) {
	s, sel, armed := scene(), 0, false
	s.Images[0].FrameMs = 400
	_, note := applyKey(&s, &sel, &armed, keyPress{r: ','})
	if got := s.Images[0]; got.Speed != 0.75 || got.FrameMs != 0 || note == "" {
		t.Fatalf("after ',': %+v note %q", got, note)
	}
	applyKey(&s, &sel, &armed, keyPress{r: '>'})
	if got := s.Images[0].Speed; got != 0 {
		t.Fatalf("back to normal speed should store 0, got %v", got)
	}
	applyKey(&s, &sel, &armed, keyPress{r: '.'})
	if got := s.Images[0].Speed; got != 1.25 {
		t.Fatalf("after '.': speed %v", got)
	}
}

func TestHostnames(t *testing.T) {
	old := hosts
	defer func() { hosts = old }()
	hosts = []string{"root@pings.smittenfeld.nl", "root@pings2.smittenfeld.nl", "pings3", "admin@10.0.0.4"}
	got := hostnames()
	want := []string{"pings", "pings2", "pings3", "10"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hostnames() = %v, want %v", got, want)
		}
	}
}
