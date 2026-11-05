package sign

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	want := "correct horse battery staple café"
	for _, in := range []string{
		"correct horse battery staple café",
		"  Correct   HORSE battery\tstaple\n CAFÉ  ",
		"correct horse battery staple café",     // NFD: e + combining acute
		"CORRECT HORSE battery staple CAFÉ\r\n", // no-break space, CRLF
	} {
		if got := Normalize(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	if Normalize("correct horse battery staple cafe") == want {
		t.Error("a different passphrase normalised to the same string")
	}
}

// The golden vector of ruling B18: passphrase → seed → public key → key id, with the frozen
// parameters. If this test ever fails, every installed agent would stop trusting new releases.
func TestGoldenDerivation(t *testing.T) {
	seed := DeriveSeed(ThrowawayPassphrase) // the real Argon2id run: 1 GiB, 4 passes
	if got := hex.EncodeToString(seed); got != ThrowawaySeedHex {
		t.Fatalf("seed %s, want %s", got, ThrowawaySeedHex)
	}
	// Typed differently, the same words give the same key.
	if !bytes.Equal(DeriveSeed("  CR-Agent  Throwaway TEST key:   never used for a RELEASE\n"), seed) {
		t.Fatal("normalisation is not applied before the derivation")
	}
	k, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if EncodePublicKey(k.Public) != ThrowawayPublicKey || k.ID.String() != ThrowawayKeyID {
		t.Fatalf("public key %s, key id %s", EncodePublicKey(k.Public), k.ID)
	}
	pk, id, err := DecodePublicKey(ThrowawayPublicKey)
	if err != nil || !pk.Equal(k.Public) || id != k.ID {
		t.Fatalf("decode: %v", err)
	}
	// A mistyped passphrase is another key (the tool then refuses to sign: it is not in the trust list).
	other, _ := KeyFromSeed(DeriveSeed(ThrowawayPassphrase + "s"))
	if other.ID == k.ID {
		t.Fatal("a typo gave the same key")
	}
}

// The golden signature: Ed25519 is deterministic, so the file is stable byte for byte. CI also
// verifies it with the reference minisign program.
func TestGoldenSignature(t *testing.T) {
	msg, err := os.ReadFile("testdata/golden-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	k := ThrowawayKey()
	const comment = "product=cr-agent version=0.1.1 release_seq=2"
	got, err := k.Sign(msg, comment)
	if err != nil {
		t.Fatal(err)
	}
	golden := "testdata/golden-manifest.json.minisig"
	want, err := os.ReadFile(golden)
	if os.IsNotExist(err) && os.Getenv("CRAGENT_WRITE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		want = got
	} else if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("signature changed:\n%s\nwant:\n%s", got, want)
	}
	s, err := ParseSignature(want)
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID.String() != ThrowawayKeyID || s.TrustedComment != comment {
		t.Fatalf("parsed %+v", s)
	}
	if err := s.Verify(k.Public, msg); err != nil {
		t.Fatal(err)
	}
	// Windows line endings in the signature file are accepted.
	if s2, err := ParseSignature(bytes.ReplaceAll(want, []byte("\n"), []byte("\r\n"))); err != nil || s2.Verify(k.Public, msg) != nil {
		t.Fatalf("CRLF signature file: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	msg := []byte(`{"version":"0.1.1"}`)
	k := ThrowawayKey()
	file, _ := k.Sign(msg, "product=cr-agent version=0.1.1 release_seq=2")
	s, _ := ParseSignature(file)

	// One flipped byte of the message.
	bad := append([]byte(nil), msg...)
	bad[5] ^= 1
	if err := s.Verify(k.Public, bad); !errors.Is(err, ErrBadSignature) {
		t.Errorf("flipped message byte: %v", err)
	}
	// A changed trusted comment (a version swapped under a valid signature).
	s2 := *s
	s2.TrustedComment = "product=cr-agent version=9.9.9 release_seq=2"
	if err := s2.Verify(k.Public, msg); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed trusted comment: %v", err)
	}
	// Another key.
	other, _ := KeyFromSeed(bytes.Repeat([]byte{7}, 32))
	if err := s.Verify(other.Public, msg); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other key: %v", err)
	}
	// The other key's signature carrying this key's id.
	forged, _ := other.Sign(msg, "product=cr-agent version=0.1.1 release_seq=2")
	fs, _ := ParseSignature(forged)
	fs.KeyID = k.ID
	if err := fs.Verify(k.Public, msg); !errors.Is(err, ErrBadSignature) {
		t.Errorf("forged key id: %v", err)
	}
	// Flipped signature bytes.
	s3 := *s
	s3.Sig = append([]byte(nil), s.Sig...)
	s3.Sig[10] ^= 0x80
	if err := s3.Verify(k.Public, msg); !errors.Is(err, ErrBadSignature) {
		t.Errorf("flipped signature: %v", err)
	}

	// Malformed files.
	lines := strings.Split(strings.TrimSpace(string(file)), "\n")
	for name, f := range map[string]string{
		"empty":             "",
		"three lines":       strings.Join(lines[:3], "\n"),
		"five lines":        string(file) + "extra\n",
		"no trusted prefix": strings.Join([]string{lines[0], lines[1], "comment: x", lines[3]}, "\n"),
		"bad base64":        strings.Join([]string{lines[0], "!!!", lines[2], lines[3]}, "\n"),
		"short signature":   strings.Join([]string{lines[0], "RUQ=", lines[2], lines[3]}, "\n"),
		"legacy algorithm":  strings.Join([]string{lines[0], "RWQ" + lines[1][3:], lines[2], lines[3]}, "\n"),
		"huge":              strings.Repeat("x", 5000),
	} {
		if _, err := ParseSignature([]byte(f)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := k.Sign(msg, "two\nlines"); err == nil {
		t.Error("a trusted comment with a line break was signed")
	}
	// A public key whose id was edited does not decode.
	if _, _, err := DecodePublicKey("RWTeMD/IbDD426IUjZ5Gbxa0wyzrEADObWy6lmuAxmsr8kiOOSVcq7Yl"); err == nil {
		t.Error("a public key with a foreign key id was accepted")
	}
	k2 := ThrowawayKey()
	k2.Wipe()
	if k2.private[0] != 0 || k2.private[31] != 0 {
		t.Error("Wipe left key bytes")
	}
}
