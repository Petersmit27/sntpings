package pinger

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
)

// Sender keeps a pool of long-lived ping sockets sending the current target
// list over and over until its context ends. SetTargets swaps the list
// without pausing: each worker picks the new one up within a few hundred
// packets, so frame changes never stop the flow.
type Sender struct {
	Sent, Failed atomic.Int64

	prefix [8]byte
	o      Options
	cur    atomic.Pointer[[]uint64]

	errMu      sync.Mutex
	errLast    time.Time
	errPending int
}

// errorEvery limits how often send errors are logged; an outage would
// otherwise print one line per worker per failure.
const errorEvery = 10 * time.Second

// NewSender prepares a sender; Workers, PPS, Network, Bind and Stderr of o
// are used.
func NewSender(prefix [8]byte, o Options) *Sender {
	if o.Network == "" {
		o.Network = "udp6"
	}
	if o.Bind == "" {
		o.Bind = "::"
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Workers <= 0 {
		o.Workers = max(1, 2*runtime.NumCPU())
	}
	return &Sender{prefix: prefix, o: o}
}

// SetTargets publishes a new target list. The sender only reads it, so the
// caller must not modify t afterwards.
func (s *Sender) SetTargets(t []uint64) {
	s.cur.Store(&t)
}

// Run sends until ctx ends. Each worker walks every Workers-th target, so
// together they cover the whole list once per pass.
func (s *Sender) Run(ctx context.Context) {
	var lim *limiter
	if s.o.PPS > 0 {
		lim = newLimiter(int64(s.o.PPS))
		defer lim.close()
	}
	var wg sync.WaitGroup
	for id := 0; id < s.o.Workers; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, id, lim)
		}()
	}
	wg.Wait()
}

func (s *Sender) worker(ctx context.Context, id int, lim *limiter) {
	var conn *icmp.PacketConn
	for {
		var err error
		if conn, err = icmp.ListenPacket(s.o.Network, s.o.Bind); err == nil {
			break
		}
		fmt.Fprintf(s.o.Stderr, "worker %d: listen %s: %v\n", id, s.o.Network, err)
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
	defer conn.Close()

	packet := buildPacket()
	var buf [16]byte
	copy(buf[:8], s.prefix[:])
	dst := &net.UDPAddr{IP: buf[:]}
	nw := s.o.Workers

	var sent, failed int64
	flush := func() {
		s.Sent.Add(sent)
		s.Failed.Add(failed)
		sent, failed = 0, 0
	}
	defer flush()

	var consecutive int
	for ctx.Err() == nil {
		tp := s.cur.Load()
		if tp == nil || len(*tp) == 0 {
			if !sleepCtx(ctx, 50*time.Millisecond) {
				return
			}
			continue
		}
		t := *tp
		for n, i := 0, id; i < len(t); n, i = n+1, i+nw {
			if n&255 == 0 {
				flush()
				if ctx.Err() != nil || s.cur.Load() != tp {
					break
				}
			}
			binary.BigEndian.PutUint64(buf[8:], t[i])
			if _, err := conn.WriteTo(packet, dst); err != nil {
				failed++
				consecutive++
				s.logError(err)
				if consecutive > 64 { // no route / no buffer: don't spin the CPU
					sleepCtx(ctx, 10*time.Millisecond)
					consecutive = 0
				}
				continue
			}
			consecutive = 0
			sent++
			if lim != nil && lim.wait(ctx) != nil {
				return
			}
		}
	}
}

func (s *Sender) logError(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	s.errPending++
	if time.Since(s.errLast) < errorEvery {
		return
	}
	fmt.Fprintf(s.o.Stderr, "send: %d errors since last report, latest: %v\n", s.errPending, err)
	s.errLast, s.errPending = time.Now(), 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
