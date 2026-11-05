package trust

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/sign"
)

func listWith(entries ...string) []byte {
	return []byte(`{"product":"cr-agent","keys":[` + strings.Join(entries, ",") + `]}`)
}

func entryFor(k *sign.Key, status string) string {
	return `{"key_id":"` + k.ID.String() + `","public_key":"` + sign.EncodePublicKey(k.Public) + `","status":"` + status + `"}`
}

func realLooking(b byte) *sign.Key {
	k, _ := sign.KeyFromSeed(bytes.Repeat([]byte{b}, 32))
	return k
}

func TestParse(t *testing.T) {
	a, b := realLooking(1), realLooking(2)
	keys, err := Parse(listWith(entryFor(a, "current"), entryFor(b, "next")))
	if err != nil || len(keys) != 2 || keys[0].ID != a.ID || keys[1].Status != "next" {
		t.Fatalf("%v %v", keys, err)
	}
	if k, ok := Lookup(keys, b.ID); !ok || !k.Public.Equal(b.Public) {
		t.Fatal("lookup")
	}
	if _, ok := Lookup(keys, realLooking(3).ID); ok {
		t.Fatal("found a key that is not listed")
	}
	for name, raw := range map[string][]byte{
		"not json":         []byte("{"),
		"another product":  []byte(`{"product":"other","keys":[]}`),
		"key id mismatch":  listWith(`{"key_id":"0000000000000000","public_key":"` + sign.EncodePublicKey(a.Public) + `","status":"current"}`),
		"not a public key": listWith(`{"key_id":"` + a.ID.String() + `","public_key":"AAAA","status":"current"}`),
		"unknown status":   listWith(entryFor(a, "trusted")),
		"listed twice":     listWith(entryFor(a, "current"), entryFor(a, "next")),
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The rule a release must pass: at least one key that can sign, and never the test key.
func TestProductionRule(t *testing.T) {
	a := realLooking(1)
	cases := []struct {
		name string
		list []byte
		want error
	}{
		{"empty", listWith(), ErrNoProductionKey},
		{"only a retired key", listWith(entryFor(a, "retired")), ErrNoProductionKey},
		{"the test key alone", listWith(entryFor(sign.ThrowawayKey(), "current")), ErrTestKeyInList},
		{"the test key beside a real one", listWith(entryFor(a, "current"), entryFor(sign.ThrowawayKey(), "next")), ErrTestKeyInList},
		{"one production key", listWith(entryFor(a, "current")), nil},
	}
	for _, c := range cases {
		keys, err := Parse(c.list)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := checkProduction(keys); !errors.Is(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The compiled-in list, whatever it holds today: it parses, it never names the test key, and
// CheckProduction says exactly whether it can ship.
func TestCompiledInList(t *testing.T) {
	keys, err := Production()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if IsThrowaway(k) {
			t.Fatal("protocol/release-keys.json names the throwaway test key")
		}
	}
	err = CheckProduction()
	if len(keys) == 0 && !errors.Is(err, ErrNoProductionKey) {
		t.Fatalf("an empty list passed the production check: %v", err)
	}
	if len(keys) > 0 && err != nil {
		t.Fatalf("the production list is not fit to ship: %v", err)
	}
}
