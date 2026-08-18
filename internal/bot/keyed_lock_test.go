package bot

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestKeyedLockerSerializesAndReleasesEntries(t *testing.T) {
	var locker keyedLocker[int]
	var active atomic.Int32
	var peak atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			unlock := locker.Lock(42)
			current := active.Add(1)
			if current > peak.Load() {
				peak.Store(current)
			}
			active.Add(-1)
			unlock()
		}()
	}
	wait.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak concurrent holders=%d, want 1", peak.Load())
	}
	if size := locker.Len(); size != 0 {
		t.Fatalf("idle lock entries=%d, want 0", size)
	}
}
