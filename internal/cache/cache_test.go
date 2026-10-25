package cache

import (
	"testing"
	"time"
)

func TestTTLExpiresAndInvalidates(t *testing.T) {
	cache := NewTTL[int](10 * time.Second)
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }
	loads := 0
	load := func() int { loads++; return loads }

	if value, at := cache.Get("a", load); value != 1 || !at.Equal(now) {
		t.Fatalf("first get = %d at %v", value, at)
	}
	now = now.Add(9 * time.Second)
	if value, _ := cache.Get("a", load); value != 1 {
		t.Fatalf("get inside TTL = %d, want cached 1", value)
	}
	if value, _ := cache.Get("b", load); value != 2 {
		t.Fatalf("other key = %d, want new load 2", value)
	}
	now = now.Add(time.Second)
	if value, _ := cache.Get("a", load); value != 3 {
		t.Fatalf("get after TTL = %d, want 3", value)
	}
	cache.Invalidate("a")
	if value, _ := cache.Get("a", load); value != 4 {
		t.Fatalf("get after invalidate = %d, want 4", value)
	}
	cache.Reset()
	if value, _ := cache.Get("b", load); value != 5 {
		t.Fatalf("get after reset = %d, want 5", value)
	}
}

func TestTTLSlowKeyDoesNotBlockOtherKeys(t *testing.T) {
	cache := NewTTL[int](time.Minute)
	release := make(chan struct{})
	started := make(chan struct{})
	go cache.Get("slow", func() int {
		close(started)
		<-release
		return 1
	})
	<-started
	defer close(release)
	done := make(chan int)
	go func() {
		value, _ := cache.Get("fast", func() int { return 2 })
		done <- value
	}()
	select {
	case value := <-done:
		if value != 2 {
			t.Fatalf("fast key = %d", value)
		}
	case <-time.After(time.Second):
		t.Fatal("fast key waited for slow key")
	}
}
