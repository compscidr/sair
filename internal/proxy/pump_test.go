package proxy

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// chanStream is an in-memory byteStream: in is what the peer sent us, out is
// what we sent. Closing `in` models the peer's half-close.
type chanStream struct {
	in      chan []byte
	out     chan []byte
	mu      sync.Mutex
	closed  bool
	sendErr error
}

func newChanStream() *chanStream {
	return &chanStream{in: make(chan []byte, 16), out: make(chan []byte, 16)}
}

func (c *chanStream) SendBytes(p []byte) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	c.out <- append([]byte(nil), p...)
	return nil
}
func (c *chanStream) RecvBytes() ([]byte, error) {
	p, ok := <-c.in
	if !ok {
		return nil, io.EOF
	}
	return p, nil
}
func (c *chanStream) CloseSend() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.out)
	}
	return nil
}
func (c *chanStream) sentClosed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }

func drain(ch chan []byte) string {
	var s string
	for p := range ch {
		s += string(p)
	}
	return s
}

func TestPumpCopiesBothWaysAndForwardsHalfClose(t *testing.T) {
	a, b := newChanStream(), newChanStream()
	done := make(chan error, 1)
	go func() { done <- pumpStreams(a, b) }()

	a.in <- []byte("hello ")
	a.in <- []byte("from a")
	b.in <- []byte("reply from b")
	close(a.in) // a's peer half-closed: b must see CloseSend, but b→a keeps flowing
	waitFor(t, "b half-closed", b.sentClosed)
	b.in <- []byte(" and more")
	close(b.in)

	if err := <-done; err != nil {
		t.Fatalf("pump: %v", err)
	}
	if got := drain(b.out); got != "hello from a" {
		t.Errorf("a→b got %q", got)
	}
	if got := drain(a.out); got != "reply from b and more" {
		t.Errorf("b→a got %q (half-close must not stop the other direction)", got)
	}
	if !a.sentClosed() {
		t.Errorf("a was not half-closed after b ended")
	}
}

func TestPumpReturnsFirstRealError(t *testing.T) {
	a, b := newChanStream(), newChanStream()
	b.sendErr = errors.New("peer gone")
	done := make(chan error, 1)
	go func() { done <- pumpStreams(a, b) }()
	a.in <- []byte("x") // a→b send fails
	close(a.in)
	close(b.in)
	select {
	case err := <-done:
		if err == nil || err.Error() != "peer gone" {
			t.Errorf("want the send error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump did not return after a send error")
	}
}
