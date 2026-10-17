package lru

import (
	"fmt"
	"sync"
	"testing"
)

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Add("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 { // a is now the most recent
		t.Fatalf("a = %v, %v", v, ok)
	}
	c.Add("c", 3) // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	c.Add("a", 10) // updates in place
	if v, _ := c.Get("a"); v != 10 {
		t.Fatalf("a = %v after update", v)
	}
	c.Remove("a")
	c.Remove("missing")
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be gone")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c = %v, %v", v, ok)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := New[string, int](100)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				k := fmt.Sprint(i % 150)
				c.Add(k, i)
				c.Get(k)
				if i%7 == 0 {
					c.Remove(k)
				}
			}
		}(g)
	}
	wg.Wait()
	if n := c.ll.Len(); n > 100 || n != len(c.m) {
		t.Fatalf("cache holds %d entries (map %d), max 100", n, len(c.m))
	}
}
