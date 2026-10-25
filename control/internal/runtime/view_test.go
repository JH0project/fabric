package runtime

import (
	"sync"
	"testing"
)

func TestViewOwnershipAndReplacement(t *testing.T) {
	v := &View{}
	c := Container{ID: "a", Labels: map[string]string{"owner": "original"}, Networks: map[string][]string{"mesh": {"fd00::1"}}}
	v.Upsert(c)
	c.Labels["owner"] = "caller"
	c.Networks["mesh"][0] = "fd00::2"
	s := v.Snapshot()
	s[0].Labels["owner"] = "reader"
	s[0].Networks["mesh"][0] = "fd00::3"
	if v.Snapshot()[0].Networks["mesh"][0] != "fd00::1" {
		t.Fatal("aliased network addresses")
	}
	if got := v.Snapshot()[0].Labels["owner"]; got != "original" {
		t.Fatalf("aliased labels: %s", got)
	}
	v.Replace([]Container{{ID: "z"}, {ID: "b"}})
	s = v.Snapshot()
	if len(s) != 2 || s[0].ID != "b" || s[1].ID != "z" {
		t.Fatalf("replacement/order: %+v", s)
	}
	v.Remove("b")
	v.Remove("missing")
	if len(v.Snapshot()) != 1 {
		t.Fatal("remove failed")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				v.Upsert(c)
				v.Snapshot()
				v.Replace([]Container{c})
				v.Remove("a")
			}
		}()
	}
	wg.Wait()
}
