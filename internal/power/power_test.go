package power

import "testing"

func TestEventFor(t *testing.T) {
	for typ, want := range map[uint32]Event{0x4: Suspend, 0x7: Resume, 0x12: Resume} {
		if ev, ok := eventFor(typ); !ok || ev != want {
			t.Errorf("type %#x → %v %v", typ, ev, ok)
		}
	}
	for _, typ := range []uint32{0x0, 0xA, 0x8013, 0x9} { // battery, power setting change and the like
		if _, ok := eventFor(typ); ok {
			t.Errorf("type %#x treated as sleep or wake", typ)
		}
	}
}
