package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type k struct {
	tenant string
	id     string
}

func TestGetOrLoad_CachesAndCoalesces(t *testing.T) {
	c := New[k, int](time.Minute)
	var calls int64
	load := func() (int, error) { atomic.AddInt64(&calls, 1); return 42, nil }

	// First get loads; second is a hit → loader runs once.
	for i := 0; i < 3; i++ {
		v, err := c.GetOrLoad(k{"t1", "a"}, load)
		if err != nil || v != 42 {
			t.Fatalf("got (%d,%v); want (42,nil)", v, err)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("loader ran %d times; want 1 (cache hit)", got)
	}

	// Different key (same tenant) loads separately.
	c.GetOrLoad(k{"t1", "b"}, load)
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("loader ran %d times; want 2 (distinct key)", got)
	}
}

func TestGetOrLoad_Singleflight(t *testing.T) {
	c := New[k, int](time.Minute)
	var calls int64
	release := make(chan struct{})
	load := func() (int, error) {
		atomic.AddInt64(&calls, 1)
		<-release // hold the load so concurrent callers must coalesce
		return 7, nil
	}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); c.GetOrLoad(k{"t", "x"}, load) }()
	}
	// Give goroutines time to pile onto the in-flight call, then release.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("loader ran %d times under %d concurrent gets; want 1 (singleflight)", got, n)
	}
}

func TestTTL_Expires(t *testing.T) {
	c := New[k, int](20 * time.Millisecond)
	var calls int64
	load := func() (int, error) { atomic.AddInt64(&calls, 1); return 1, nil }
	c.GetOrLoad(k{"t", "x"}, load)
	c.GetOrLoad(k{"t", "x"}, load) // hit
	time.Sleep(40 * time.Millisecond)
	c.GetOrLoad(k{"t", "x"}, load) // expired → reload
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("loader ran %d times; want 2 (one reload after TTL)", got)
	}
}

func TestInvalidate_DropsEntry(t *testing.T) {
	c := New[k, int](time.Minute)
	var calls int64
	load := func() (int, error) { atomic.AddInt64(&calls, 1); return 1, nil }
	c.GetOrLoad(k{"t", "x"}, load)
	c.Invalidate(k{"t", "x"})
	c.GetOrLoad(k{"t", "x"}, load) // must reload
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("loader ran %d times; want 2 (reload after invalidate)", got)
	}
	// Invalidating an absent key is a no-op (no panic).
	c.Invalidate(k{"t", "absent"})
}

// TestInvalidate_DuringLoad_NotCached is the security-critical race: a revoke (Invalidate) that
// runs WHILE an authorize-load is in flight must NOT let the stale (pre-revoke) value be cached.
func TestInvalidate_DuringLoad_NotCached(t *testing.T) {
	c := New[k, int](time.Minute)
	var calls int64
	loadStarted := make(chan struct{})
	finishLoad := make(chan struct{})
	// Only the FIRST load does the mid-flight channel coordination; later reloads just return.
	load := func() (int, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			close(loadStarted)
			<-finishLoad // hold so we can invalidate mid-flight
		}
		return 100, nil
	}
	done := make(chan int)
	go func() { v, _ := c.GetOrLoad(k{"t", "x"}, load); done <- v }()

	<-loadStarted
	c.Invalidate(k{"t", "x"}) // revoke arrives mid-load
	close(finishLoad)
	if v := <-done; v != 100 {
		t.Fatalf("in-flight caller got %d; want its load result 100", v)
	}
	// The mid-flight-invalidated result must NOT be cached → next get reloads (calls==2).
	c.GetOrLoad(k{"t", "x"}, load)
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("loader ran %d times; want 2 (mid-flight invalidate must not cache stale)", got)
	}
}

func TestError_NotCached(t *testing.T) {
	c := New[k, int](time.Minute)
	var calls int64
	wantErr := errors.New("db down")
	failThenOK := func() (int, error) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			return 0, wantErr
		}
		return 9, nil
	}
	if _, err := c.GetOrLoad(k{"t", "x"}, failThenOK); !errors.Is(err, wantErr) {
		t.Fatalf("first get err = %v; want %v", err, wantErr)
	}
	v, err := c.GetOrLoad(k{"t", "x"}, failThenOK) // error not cached → retries
	if err != nil || v != 9 {
		t.Fatalf("second get = (%d,%v); want (9,nil) — error must not be cached", v, err)
	}
}
