package pipe

import (
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// Dedup unit tests: memory LRU + conflict translation.

func TestDedupCheckRemember(t *testing.T) {
	d := NewDedup(3)
	if _, ok := d.Check(1, 2, "a"); ok {
		t.Fatal("empty dedup hit")
	}
	d.Remember(1, 2, "a", dedupResult{MsgID: 10, Seq: 3})
	r, ok := d.Check(1, 2, "a")
	if !ok || r.MsgID != 10 || r.Seq != 3 {
		t.Fatalf("hit: %+v %v", r, ok)
	}
	// different sender: no hit
	if _, ok := d.Check(1, 3, "a"); ok {
		t.Fatal("sender mixed into key failed")
	}
	// LRU eviction at capacity 3
	d.Remember(1, 2, "b", dedupResult{})
	d.Remember(1, 2, "c", dedupResult{})
	d.Remember(1, 2, "d", dedupResult{}) // evicts oldest
	if _, ok := d.Check(1, 2, "a"); ok {
		t.Fatal("capacity eviction failed")
	}
	if _, ok := d.Check(1, 2, "d"); !ok {
		t.Fatal("newest evicted")
	}
}

func TestDedupTranslateConflict(t *testing.T) {
	d := NewDedup(10)
	row := &store.HotMsg{MsgID: 77, Cid: 1, Seq: 5, SenderID: 2, ClientMsgID: "x", SentAt: 9}
	// no memory, row supplied: adopt the row
	r, ok := d.TranslateConflict(1, 2, "x", row)
	if !ok || r.MsgID != 77 || r.Seq != 5 {
		t.Fatalf("translate: %+v %v", r, ok)
	}
	// now memory remembers it
	if _, ok := d.Check(1, 2, "x"); !ok {
		t.Fatal("translate did not remember")
	}
	// no memory, no row: miss
	if _, ok := d.TranslateConflict(9, 9, "z", nil); ok {
		t.Fatal("nil row must miss")
	}
}
