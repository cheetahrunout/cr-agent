// Command signtool is the offline release signing tool of the CR agent (PLAN §13.1, rulings B18–B21).
//
//	signtool pubkey                       derive the key from the passphrase and print its public half
//	signtool pack -out <zip> -prefix versions/X -dir <dir> [files]   build a release zip (scripts/build-installer.ps1 runs this)
//	signtool manifest -dist <dir> -version X   hash a release folder into an unsigned manifest.json (scripts/build-installer.ps1 runs this)
//	signtool sign -dist <dir> -prev <manifest.json> | -first   re-hash, set release_seq, sign
//	signtool verify -dist <dir>           check a signed release folder the way an agent would
//	signtool trust-check                  fail unless the compiled-in key list can ship
//
// The passphrase is typed at the console without echo. There is no flag, environment variable or
// file for it, and it is never written anywhere. Nothing is stored: the key is derived, used and wiped.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cheetahrunout/cr-agent/internal/enginelock"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/sign"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pubkey":
		err = cmdPubkey(os.Args[2:])
	case "pack":
		err = cmdPack(os.Args[2:])
	case "manifest":
		err = cmdManifest(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "trust-check":
		err = cmdTrustCheck()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signtool:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: signtool pubkey | pack | manifest | sign | verify | trust-check   (see -h of each)")
}

// keySource returns the signing key: from the console in every build; a debug build can also be told
// to use the throwaway test key (see key_debug.go).
type keySource func(confirm bool) (*sign.Key, error)

// minPassphraseChars is the owner's rule (PLAN ruling B61). It is counted after normalising, because that is what the key is made from.
const minPassphraseChars = 10

func checkPassphrase(p string) error {
	if utf8.RuneCountInString(sign.Normalize(p)) < minPassphraseChars {
		return fmt.Errorf("the passphrase must be at least %d characters", minPassphraseChars)
	}
	return nil
}

func fromConsole(confirm bool) (*sign.Key, error) {
	p, err := readPassphrase("Release passphrase: ")
	if err != nil {
		return nil, err
	}
	if confirm {
		again, err := readPassphrase("Again, to rule out a typo: ")
		if err != nil {
			return nil, err
		}
		if sign.Normalize(again) != sign.Normalize(p) {
			return nil, errors.New("the two entries differ")
		}
	}
	if err := checkPassphrase(p); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "Deriving the key (Argon2id, 1 GiB)...")
	return sign.KeyFromSeed(sign.DeriveSeed(p))
}

func cmdPubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	src := keyFlags(fs)
	fs.Parse(args)
	key, err := src()(true)
	if err != nil {
		return err
	}
	defer key.Wipe()
	fmt.Printf("public key: %s\nkey id:     %s\n\nEntry for protocol/release-keys.json:\n", sign.EncodePublicKey(key.Public), key.ID)
	e, _ := json.MarshalIndent(map[string]string{"key_id": key.ID.String(), "public_key": sign.EncodePublicKey(key.Public),
		"status": "current", "added": time.Now().UTC().Format("2006-01-02")}, "    ", "  ")
	fmt.Printf("    %s\n", e)
	return nil
}

// cmdPack builds one release zip whose entries are the paths the manifest will list.
func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	out := fs.String("out", "", "the zip to write")
	prefix := fs.String("prefix", "", "versions/<version> or engine/<tag>")
	dir := fs.String("dir", "", "the folder whose files go into the zip")
	fs.Parse(args)
	if *out == "" || *dir == "" || !(strings.HasPrefix(*prefix, "versions/") || strings.HasPrefix(*prefix, "engine/")) {
		return errors.New("pack needs -out, -dir and -prefix versions/<version> or engine/<tag>; file names after the flags limit the zip to those files")
	}
	if err := release.Pack(*out, *prefix, *dir, fs.Args()); err != nil {
		return err
	}
	fmt.Printf("%s written\n", *out)
	return nil
}

func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	dist := fs.String("dist", "", "the release folder (zips and the installer)")
	version := fs.String("version", "", "the release version, x.y.z")
	minHub := fs.String("min-hub", "", "the oldest hub version this agent works with")
	notes := fs.String("notes", "", "release notes, one line")
	fs.Parse(args)
	if *dist == "" || !release.ValidVersion(*version) {
		return errors.New("manifest needs -dist and -version x.y.z")
	}
	pin := enginelock.Pinned()
	llama := release.LlamaCpp{Tag: pin.Tag, UpstreamZipSHA256: map[string]string{}}
	for _, a := range pin.Archives {
		llama.UpstreamZipSHA256[a.Name] = a.SHA256
	}
	// release_seq, key_id and released_at are placeholders here: the signer sets them (ruling B21).
	m, err := release.Build(*dist, release.BuildOptions{Version: *version, ReleaseSeq: 1, Protocol: protocol.Proto, MinHubVersion: *minHub,
		LlamaCpp: llama, KeyID: "0000000000000000", ReleasedAt: time.Now(), Notes: *notes})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dist, "manifest.json"), release.Marshal(m), 0o644); err != nil {
		return err
	}
	fmt.Printf("manifest.json (unsigned): version %s, %d files, %d archives\n", m.Version, len(m.Files), len(m.Archives))
	return nil
}

// signOptions are what the signer decides.
type signOptions struct {
	Dist  string
	Prev  string // the latest published manifest.json, or "" with First
	First bool
	Old   bool // write manifest.json.old.minisig (the outgoing key during a rotation, ruling B20)
	Now   time.Time
}

// previousSeq reads the release before this one. With its signature beside it, the signature is checked.
func previousSeq(o signOptions, keys []trust.Key) (seq int64, version string, err error) {
	if o.First {
		if o.Prev != "" {
			return 0, "", errors.New("-first and -prev exclude each other")
		}
		return 0, "", nil
	}
	if o.Prev == "" {
		return 0, "", errors.New("give -prev <the latest published manifest.json>, or -first for the very first release")
	}
	raw, err := os.ReadFile(o.Prev)
	if err != nil {
		return 0, "", err
	}
	var prev *release.Manifest
	if sig, serr := os.ReadFile(o.Prev + ".minisig"); serr == nil {
		if prev, err = release.Verify(raw, sig, keys); err != nil {
			return 0, "", fmt.Errorf("the previous manifest does not verify: %w", err)
		}
	} else {
		prev = &release.Manifest{}
		if err := json.Unmarshal(raw, prev); err != nil || prev.Validate() != nil {
			return 0, "", errors.New("the previous manifest is not a valid manifest")
		}
		fmt.Fprintln(os.Stderr, "warning: no signature beside the previous manifest; its sequence is taken as read")
	}
	return prev.ReleaseSeq, prev.Version, nil
}

// signDist is the signing step without the console: re-hash the folder (fail closed), set the fields
// the signer owns, refuse a key that is not in the trust list, sign, and verify the result.
func signDist(o signOptions, key *sign.Key, keys []trust.Key) (*release.Manifest, error) {
	if len(keys) == 0 {
		return nil, errors.New("this signtool was built before any key was in protocol/release-keys.json, so it can sign nothing. Pull the repository, rebuild it, and sign again. Nothing was signed")
	}
	if _, ok := trust.Lookup(keys, key.ID); !ok {
		return nil, fmt.Errorf("this passphrase gives key %s, which is not in the trust list of this tool: a typo in the passphrase? Nothing was signed", key.ID)
	}
	path := filepath.Join(o.Dist, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m release.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("manifest.json is not valid JSON")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := release.CheckDist(o.Dist, &m); err != nil {
		return nil, fmt.Errorf("the folder is not what the manifest says: %w. Nothing was signed", err)
	}
	prevSeq, prevVersion, err := previousSeq(o, keys)
	if err != nil {
		return nil, err
	}
	if !o.First && prevVersion == m.Version {
		return nil, fmt.Errorf("version %s is already published", m.Version)
	}
	sigName := "manifest.json.minisig"
	if o.Old {
		// The outgoing key signs the manifest exactly as the new key signed it: nothing is rewritten.
		sigName = "manifest.json.old.minisig"
		if m.ReleaseSeq != prevSeq+1 {
			return nil, errors.New("sign with the new key first: release_seq is not set yet")
		}
	} else {
		m.ReleaseSeq, m.KeyID, m.ReleasedAt = prevSeq+1, key.ID.String(), o.Now.UTC().Format(time.RFC3339)
		raw = release.Marshal(&m)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return nil, err
		}
	}
	sig, err := key.Sign(raw, release.TrustedComment(&m))
	if err != nil {
		return nil, err
	}
	if !o.Old {
		if _, err := release.Verify(raw, sig, keys); err != nil {
			return nil, fmt.Errorf("the fresh signature does not verify: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(o.Dist, sigName), sig, 0o644); err != nil {
		return nil, err
	}
	return &m, nil
}

func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	var o signOptions
	fs.StringVar(&o.Dist, "dist", "", "the release folder with manifest.json")
	fs.StringVar(&o.Prev, "prev", "", "the latest published manifest.json (its .minisig beside it is checked)")
	fs.BoolVar(&o.First, "first", false, "this is the very first release (release_seq 1)")
	fs.BoolVar(&o.Old, "old", false, "write manifest.json.old.minisig with the outgoing key (rotation)")
	src := keyFlags(fs)
	fs.Parse(args)
	if o.Dist == "" {
		return errors.New("sign needs -dist")
	}
	o.Now = time.Now()
	printSelf()
	key, err := src()(false)
	if err != nil {
		return err
	}
	defer key.Wipe()
	m, err := signDist(o, key, trust.Keys())
	if err != nil {
		return err
	}
	fmt.Printf("signed: version %s, release_seq %d, key %s\n", m.Version, m.ReleaseSeq, key.ID)
	if m.Notes != "" {
		fmt.Printf("notes:  %s\n", m.Notes)
	}
	fmt.Println("Done. The lead publishes it with scripts/publish-release.ps1.")
	return nil
}

// printSelf shows this program's own SHA-256 before anything is typed into it. It catches an accident
// (the wrong file, an old build) and nothing more: a bad tool would print the right number. The
// check that counts is the owner's Get-FileHash, run before this program (docs/first-release.md).
func printSelf() {
	sum, err := selfSHA()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: this tool could not hash itself:", err)
		return
	}
	fmt.Printf("this tool: sha256 %s (an accident check only; your Get-FileHash is the real one)\n", sum)
}

func selfSHA() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	sum, _, err := release.HashFile(exe)
	return sum, err
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dist := fs.String("dist", "", "the release folder")
	fs.Parse(args)
	if *dist == "" {
		return errors.New("verify needs -dist")
	}
	raw, err := os.ReadFile(filepath.Join(*dist, "manifest.json"))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(*dist, "manifest.json.minisig"))
	if err != nil {
		return err
	}
	m, err := release.Verify(raw, sig, trust.Keys())
	if err != nil {
		return err
	}
	if err := release.CheckDist(*dist, m); err != nil {
		return err
	}
	fmt.Printf("ok: version %s, release_seq %d, signed by %s, %d files in %d archives match\n", m.Version, m.ReleaseSeq, m.KeyID, len(m.Files), len(m.Archives))
	return nil
}

func cmdTrustCheck() error {
	if err := trust.CheckProduction(); err != nil {
		return err
	}
	keys, _ := trust.Production()
	fmt.Printf("ok: %d production key(s), no test key\n", len(keys))
	return nil
}
