// Package trust is the list of public keys a release signature may come from (ruling B20). The list
// is protocol/release-keys.json, compiled into the agent and the launcher.
//
// Until the owner derives the real key there is no production key. A debug build therefore also
// trusts the throwaway test key; a release build trusts only the list, and refuses to start while
// the list is empty or names the test key (Check).
package trust

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"

	releasekeys "github.com/cheetahrunout/cr-agent/protocol"

	"github.com/cheetahrunout/cr-agent/internal/sign"
)

// Key is one trusted public key.
type Key struct {
	ID     sign.KeyID
	Public ed25519.PublicKey
	Status string // current | next | retired
}

type entry struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Status    string `json:"status"`
	Added     string `json:"added,omitempty"`
	Note      string `json:"note,omitempty"`
}

type file struct {
	Product string  `json:"product"`
	Comment string  `json:"comment,omitempty"`
	Keys    []entry `json:"keys"`
}

// Errors of Check.
var (
	ErrNoProductionKey = errors.New("the release key list is empty: no production key has been added to protocol/release-keys.json")
	ErrTestKeyInList   = errors.New("the release key list names the throwaway test key")
)

// Parse reads a release-keys.json. Every entry must be a minisign public key whose key id is the
// one stated.
func Parse(b []byte) ([]Key, error) {
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("release-keys.json: %w", err)
	}
	if f.Product != "cr-agent" {
		return nil, errors.New("release-keys.json: product must be cr-agent")
	}
	seen := map[sign.KeyID]bool{}
	keys := make([]Key, 0, len(f.Keys))
	for _, e := range f.Keys {
		pk, id, err := sign.DecodePublicKey(e.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("release-keys.json: %w", err)
		}
		if id.String() != e.KeyID {
			return nil, fmt.Errorf("release-keys.json: key id %q does not match its public key", e.KeyID)
		}
		switch e.Status {
		case "current", "next", "retired":
		default:
			return nil, fmt.Errorf("release-keys.json: status %q", e.Status)
		}
		if seen[id] {
			return nil, errors.New("release-keys.json: a key is listed twice")
		}
		seen[id] = true
		keys = append(keys, Key{ID: id, Public: pk, Status: e.Status})
	}
	return keys, nil
}

func throwaway() Key {
	k := sign.ThrowawayKey()
	return Key{ID: k.ID, Public: k.Public, Status: "current"}
}

// IsThrowaway reports whether a key is the test key.
func IsThrowaway(k Key) bool { return k.ID.String() == sign.ThrowawayKeyID }

// checkProduction is the rule for a list a release may ship with.
func checkProduction(keys []Key) error {
	signing := 0
	for _, k := range keys {
		if IsThrowaway(k) {
			return ErrTestKeyInList
		}
		if k.Status != "retired" {
			signing++
		}
	}
	if signing == 0 {
		return ErrNoProductionKey
	}
	return nil
}

// Production is the compiled-in list.
func Production() ([]Key, error) { return Parse(releasekeys.JSON) }

// CheckProduction reports whether the compiled-in list is fit for a release: at least one key that
// can sign, and no test key. The release workflow runs it before it builds anything.
func CheckProduction() error {
	keys, err := Production()
	if err != nil {
		return err
	}
	return checkProduction(keys)
}

// Check is called at the start of the agent and the launcher. A release build fails it until a
// production key is listed; a debug build passes as long as the list parses.
func Check() error {
	if debugTrust {
		_, err := Production()
		return err
	}
	return CheckProduction()
}

// Keys are the keys this build accepts signatures from: retired keys are not among them.
func Keys() []Key {
	keys, err := Production()
	if err != nil {
		keys = nil
	}
	out := make([]Key, 0, len(keys)+1)
	for _, k := range keys {
		if k.Status != "retired" {
			out = append(out, k)
		}
	}
	if debugTrust {
		out = append(out, throwaway())
	}
	return out
}

// Lookup finds a key by id among keys.
func Lookup(keys []Key, id sign.KeyID) (Key, bool) {
	for _, k := range keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// Debug reports whether this build trusts the throwaway key.
func Debug() bool { return debugTrust }
