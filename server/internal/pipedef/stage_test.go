package pipedef

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

func mkFrame(seq uint32, trace string) *proto.Frame {
	return &proto.Frame{
		Seq:  seq,
		Kind: seq,
		Meta: &proto.FrameMeta{TraceID: trace},
	}
}

func TestTeeStagePassesAndCopies(t *testing.T) {
	in := make(FrameCh, 4)
	out := make(FrameCh, 4)
	var mu sync.Mutex
	var copies []*proto.Frame

	tee := &TeeStage{InOut{In: in, Out: out}, func(f *proto.Frame) {
		mu.Lock()
		copies = append(copies, f)
		mu.Unlock()
	}}
	if tee.Name() != "tee" {
		t.Errorf("Name = %q", tee.Name())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tee.Run(ctx)

	in <- mkFrame(1, "t1")
	in <- mkFrame(2, "t2")
	close(in)

	got := 0
	for f := range out {
		if int(f.Seq) != got+1 {
			t.Errorf("out of order: %v", f.Seq)
		}
		got++
	}
	if got != 2 {
		t.Fatalf("forwarded %d frames, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(copies) != 2 {
		t.Fatalf("copied %d frames, want 2", len(copies))
	}
}

func TestTeeStageWriterSink(t *testing.T) {
	in := make(FrameCh, 1)
	out := make(FrameCh, 1)
	var sb strings.Builder
	tee := &TeeStage{InOut{In: in, Out: out}, &sb}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tee.Run(ctx)

	in <- mkFrame(7, "trace-abc")
	close(in)
	<-out
	if !strings.Contains(sb.String(), "trace-abc") {
		t.Errorf("writer sink should contain trace id, got %q", sb.String())
	}
}

func TestEchoStageBackAndForward(t *testing.T) {
	in := make(FrameCh, 1)
	out := make(FrameCh, 1)
	back := make(FrameCh, 3)
	echo := &EchoStage{InOut{In: in, Out: out}, back}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go echo.Run(ctx)

	in <- mkFrame(5, "tx")
	fb := <-back
	fo := <-out
	if fb.Seq != 5 || fo.Seq != 5 {
		t.Errorf("echo mismatch: back=%v out=%v", fb.Seq, fo.Seq)
	}
	if echo.Name() != "echo" {
		t.Errorf("Name = %q", echo.Name())
	}
}

func TestStageContextCancel(t *testing.T) {
	in := make(FrameCh)
	out := make(FrameCh)
	tee := &TeeStage{InOut{In: in, Out: out}, nil}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tee.Run(ctx) }()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Run on cancel = %v, want context.Canceled", err)
	}
}
