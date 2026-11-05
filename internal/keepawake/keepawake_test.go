package keepawake

import (
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	a := New()
	if a.On() {
		t.Fatal("on at start")
	}
	for _, v := range []bool{true, true, false, true, false, false} {
		a.Set(v)
		if a.On() != v {
			t.Fatalf("On() = %v after Set(%v)", a.On(), v)
		}
	}
	time.Sleep(20 * time.Millisecond) // let the owning thread apply the last value
}
