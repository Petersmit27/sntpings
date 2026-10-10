package pinger

import (
	"context"
	"io"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

func TestSenderKeepsSendingAcrossSwaps(t *testing.T) {
	c, err := icmp.ListenPacket("udp6", "::1")
	if err != nil {
		t.Skipf("no ping sockets here: %v", err)
	}
	c.Close()

	// With an all-zero prefix, target 1 is ::1, so nothing leaves the machine.
	s := NewSender([8]byte{}, Options{Workers: 2, PPS: 2000, Bind: "::1", Stderr: io.Discard})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	s.SetTargets([]uint64{1, 1, 1})
	time.Sleep(150 * time.Millisecond)
	first := s.Sent.Load()
	if first == 0 {
		t.Fatal("nothing sent")
	}

	s.SetTargets([]uint64{1})
	time.Sleep(150 * time.Millisecond)
	if s.Sent.Load() <= first {
		t.Fatal("stopped sending after swapping targets")
	}

	s.SetTargets(nil)
	time.Sleep(150 * time.Millisecond)
	idle := s.Sent.Load()
	time.Sleep(150 * time.Millisecond)
	if s.Sent.Load() != idle {
		t.Fatal("kept sending with no targets")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if s.Failed.Load() != 0 {
		t.Fatalf("%d sends failed", s.Failed.Load())
	}
}
