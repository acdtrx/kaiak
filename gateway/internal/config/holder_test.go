package config

import (
	"sync"
	"testing"
)

func TestHolderIsEmptyUntilTheFirstSwap(t *testing.T) {
	var h Holder
	if h.Loaded() || h.Current() != nil {
		t.Fatal("zero Holder reports a snapshot")
	}
	s := parseFixture(t, "minimal.json")
	h.Swap(s)
	if !h.Loaded() || h.Current() != s {
		t.Fatal("swapped snapshot is not current")
	}
}

func TestReaderKeepsItsSnapshotAcrossASwap(t *testing.T) {
	var h Holder
	first := parseFixture(t, "full.json")
	h.Swap(first)

	taken := make(chan struct{})
	swapped := make(chan struct{})
	var wg sync.WaitGroup
	var sawBefore, sawAfter *Snapshot
	wg.Go(func() {
		// A request takes the snapshot once at its start...
		snapshot := h.Current()
		sawBefore = snapshot
		close(taken)
		<-swapped
		// ...and keeps using it after a reload swapped in another.
		sawAfter = snapshot
	})

	<-taken
	second := parseFixture(t, "minimal.json")
	h.Swap(second)
	close(swapped)
	wg.Wait()

	if sawBefore != first || sawAfter != first {
		t.Error("the request's snapshot changed under it")
	}
	if _, ok := sawAfter.Models["qwen3-32b"]; !ok {
		t.Error("the held snapshot lost its content")
	}
	if h.Current() != second {
		t.Error("new requests do not see the new snapshot")
	}
}
