package lru

import "testing"

func TestEviction(t *testing.T) {
	c := New[string, int](10)
	c.Put("a", 1, 4)
	c.Put("b", 2, 4)
	c.Get("a") // a is now the most recent
	c.Put("c", 3, 4)
	if _, ok := c.Get("b"); ok {
		t.Error("b should be evicted first")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("a was used recently")
	}
	c.Grow("a", 10)
	if c.Bytes() > 10 {
		if _, ok := c.Get("c"); ok {
			t.Error("growing past the budget evicts the rest")
		}
	}
}
