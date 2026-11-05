// Package sign is the release signature of the CR agent (rulings B18 and B19): an Ed25519 key derived
// from a passphrase with Argon2id, and signatures in minisign's format over manifest.json.
//
// Nothing here stores a key. The private key exists only in memory, in the offline signing tool,
// for the moment it signs.
package sign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/text/unicode/norm"
)

// The derivation is frozen (ruling B18). A rotated passphrase uses the salt "…-v2".
const (
	Salt        = "cr-ai-release-signing-v1"
	argonTime   = 4
	argonMemory = 1 << 20 // KiB: 1 GiB
	argonThread = 4
	keyIDDomain = "cr-ai-keyid-v1"
)

// Normalize is norm() of ruling B18: NFC, lowercase, words split on whitespace and joined with one
// space, no leading or trailing whitespace. Two people typing the same words get the same key.
func Normalize(passphrase string) string {
	s := strings.ToLower(norm.NFC.String(passphrase))
	return strings.Join(strings.Fields(s), " ")
}

// DeriveSeed is the Ed25519 seed for a passphrase. It takes a gigabyte of memory and a few seconds
// by design: every guess at the passphrase costs that much.
func DeriveSeed(passphrase string) []byte {
	return argon2.IDKey([]byte(Normalize(passphrase)), []byte(Salt), argonTime, argonMemory, argonThread, ed25519.SeedSize)
}

// KeyID is the first 8 bytes of BLAKE2b-256("cr-ai-keyid-v1" ‖ public key).
type KeyID [8]byte

func (k KeyID) String() string { return hex.EncodeToString(k[:]) }

// ParseKeyID reads 16 hex characters.
func ParseKeyID(s string) (KeyID, error) {
	var k KeyID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(k) {
		return k, errors.New("key id must be 16 hex characters")
	}
	copy(k[:], b)
	return k, nil
}

// IDOf computes a public key's id.
func IDOf(pk ed25519.PublicKey) KeyID {
	h, _ := blake2b.New256(nil)
	h.Write([]byte(keyIDDomain))
	h.Write(pk)
	var k KeyID
	copy(k[:], h.Sum(nil))
	return k
}

// Key is a signing key in memory.
type Key struct {
	private ed25519.PrivateKey
	Public  ed25519.PublicKey
	ID      KeyID
}

// KeyFromSeed builds the key pair of a seed.
func KeyFromSeed(seed []byte) (*Key, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("seed must be 32 bytes")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pk := priv.Public().(ed25519.PublicKey)
	return &Key{private: priv, Public: pk, ID: IDOf(pk)}, nil
}

// Wipe overwrites the private key.
func (k *Key) Wipe() {
	for i := range k.private {
		k.private[i] = 0
	}
}

// minisign's algorithm tags: "Ed" in a public key, "ED" in a signature made over the BLAKE2b-512
// hash of the message (the only kind this project makes or accepts).
var (
	algPublic    = []byte("Ed")
	algPrehashed = []byte("ED")
)

// EncodePublicKey is minisign's public key line: base64("Ed" ‖ key id ‖ public key).
func EncodePublicKey(pk ed25519.PublicKey) string {
	id := IDOf(pk)
	return base64.StdEncoding.EncodeToString(append(append(append([]byte{}, algPublic...), id[:]...), pk...))
}

// DecodePublicKey reads a minisign public key line and checks that its key id is the one this
// project derives from the key.
func DecodePublicKey(s string) (ed25519.PublicKey, KeyID, error) {
	var id KeyID
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 2+8+ed25519.PublicKeySize || !bytes.Equal(b[:2], algPublic) {
		return nil, id, errors.New("not a minisign Ed25519 public key")
	}
	copy(id[:], b[2:10])
	pk := ed25519.PublicKey(append([]byte(nil), b[10:]...))
	if IDOf(pk) != id {
		return nil, id, errors.New("the key id does not belong to the public key")
	}
	return pk, id, nil
}

// Signature is a parsed .minisig file.
type Signature struct {
	KeyID          KeyID
	Sig            []byte // Ed25519 over BLAKE2b-512(message)
	TrustedComment string
	GlobalSig      []byte // Ed25519 over Sig ‖ TrustedComment
}

const (
	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "
)

// Sign makes a .minisig file for message. The trusted comment is covered by the signature.
func (k *Key) Sign(message []byte, trustedComment string) ([]byte, error) {
	if strings.ContainsAny(trustedComment, "\r\n") {
		return nil, errors.New("the trusted comment must be one line")
	}
	hash := blake2b.Sum512(message)
	sig := ed25519.Sign(k.private, hash[:])
	global := ed25519.Sign(k.private, append(append([]byte{}, sig...), trustedComment...))
	line := append(append(append([]byte{}, algPrehashed...), k.ID[:]...), sig...)
	var b strings.Builder
	b.WriteString(untrustedPrefix + "signature from the cr-agent release key " + k.ID.String() + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(line) + "\n")
	b.WriteString(trustedPrefix + trustedComment + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(global) + "\n")
	return []byte(b.String()), nil
}

// ParseSignature reads a .minisig file.
func ParseSignature(file []byte) (*Signature, error) {
	if len(file) > 4096 {
		return nil, errors.New("signature file too large")
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(string(file), "\r\n"), "\r\n", "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], untrustedPrefix) || !strings.HasPrefix(lines[2], trustedPrefix) {
		return nil, errors.New("not a minisign signature file")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != 2+8+ed25519.SignatureSize {
		return nil, errors.New("bad signature line")
	}
	if !bytes.Equal(raw[:2], algPrehashed) {
		return nil, errors.New("only prehashed (ED) signatures are accepted")
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return nil, errors.New("bad global signature line")
	}
	s := &Signature{Sig: raw[10:], TrustedComment: strings.TrimPrefix(lines[2], trustedPrefix), GlobalSig: global}
	copy(s.KeyID[:], raw[2:10])
	return s, nil
}

// ErrBadSignature is returned for any signature that does not verify.
var ErrBadSignature = errors.New("signature does not verify")

// Verify checks a parsed signature over message with a public key, in the order of ruling B19:
// key id, signature, global signature. The caller then compares the trusted comment with the manifest.
func (s *Signature) Verify(pk ed25519.PublicKey, message []byte) error {
	id := IDOf(pk)
	if subtle.ConstantTimeCompare(id[:], s.KeyID[:]) != 1 {
		return fmt.Errorf("%w: made with another key", ErrBadSignature)
	}
	hash := blake2b.Sum512(message)
	if !ed25519.Verify(pk, hash[:], s.Sig) {
		return ErrBadSignature
	}
	if !ed25519.Verify(pk, append(append([]byte{}, s.Sig...), s.TrustedComment...), s.GlobalSig) {
		return fmt.Errorf("%w: the trusted comment was changed", ErrBadSignature)
	}
	return nil
}
