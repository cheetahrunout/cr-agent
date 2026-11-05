package sign

import "encoding/hex"

// The throwaway test key. Its passphrase is public on purpose: it exists for the golden vectors,
// the tests and the local dry run. Debug builds trust it; a release build refuses to start if it
// is in the trust list. It must never sign a release.
const (
	ThrowawayPassphrase = "cr-agent throwaway test key: never used for a release"
	ThrowawaySeedHex    = "3481349b02e3f66b2d5fde75c287a566de47110b8ac392f97a4d97a68489c916"
	ThrowawayPublicKey  = "RWTeMD/IbDD425IUjZ5Gbxa0wyzrEADObWy6lmuAxmsr8kiOOSVcq7Yl"
	ThrowawayKeyID      = "de303fc86c30f8db"
)

// ThrowawayKey returns the test key from its stored seed (no Argon2 run).
func ThrowawayKey() *Key {
	seed, err := hex.DecodeString(ThrowawaySeedHex)
	if err != nil {
		panic(err)
	}
	k, err := KeyFromSeed(seed)
	if err != nil {
		panic(err)
	}
	return k
}
