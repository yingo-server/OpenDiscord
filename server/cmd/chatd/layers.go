package main

// layers.go: the embedded-mode layer adapter.
//
// embedded.NewBridge attaches a transport.Layer and hands back a
// transport.Peer whose Frames() channel is *the same* channel the layer's
// Deliver() is pumped from — a layer cannot both pull from peer.Frames() and
// be pushed via Deliver(). This adapter therefore implements BOTH interfaces
// over its own two channels, which is what the real layers expect:
//
//	Send   → up    → bridge fan-in        (a layer talking to the bus)
//	Deliver ← down ← bridge egress        (the bus talking to a layer)
//
// and offers the *same object* as the layer's transport.Peer. Nothing in the
// embedded package is bypassed: Attach still runs the three pumps.

import (
	"errors"
	"sync"

	"github.com/yingo-server/UFOT/server/internal/proto"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

// layerQueueCap bounds each layer's inbound queue. Under measurement a
// 1024-slot queue overflowed at ~1.2k msg/s with 2-member groups and every
// overflowed frame was lost ("layer queue full"). Fan-out writes are bursty
// by nature (one message becomes N writes), so the queue needs room for a
// burst rather than the steady rate.
const layerQueueCap = 8192

var (
	errLayerBusy   = errors.New("chatd: layer queue full")
	errLayerClosed = errors.New("chatd: layer closed")
)

type layerPeer struct {
	name string
	up   chan *proto.Frame
	down chan *proto.Frame

	mu     sync.RWMutex
	closed bool
}

var (
	_ transport.Peer  = (*layerPeer)(nil)
	_ transport.Layer = (*layerPeer)(nil)
)

func newLayerPeer(name string) *layerPeer {
	return &layerPeer{
		name: name,
		up:   make(chan *proto.Frame, layerQueueCap),
		down: make(chan *proto.Frame, layerQueueCap),
	}
}

// ---- transport.Layer ----

func (l *layerPeer) LayerName() string { return l.name }

// Deliver is the downlink entry (bridge → layer). Non-blocking under pressure:
// a full queue drops the frame and logs at debug level rather than stalling the
// bus (the bus's own fanout grace already drops slow layers).
func (l *layerPeer) Deliver(f *proto.Frame) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return errLayerClosed
	}
	select {
	case l.down <- f:
		return nil
	default:
		return errLayerBusy
	}
}

func (l *layerPeer) Outbound() <-chan *proto.Frame { return l.up }

// ---- transport.Peer ----

func (l *layerPeer) Layer() string { return l.name }

func (l *layerPeer) Send(f *proto.Frame) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return errLayerClosed
	}
	select {
	case l.up <- f:
		return nil
	default:
		return errLayerBusy
	}
}

func (l *layerPeer) Frames() <-chan *proto.Frame { return l.down }

// Close stops delivery. Channels are intentionally NOT closed: several
// goroutines (bridge pumps, forwarder, pipe) are readers/writers at this point
// and closing would panic a concurrent sender; the flag makes both directions
// fail fast while the process drains.
func (l *layerPeer) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return nil
}
