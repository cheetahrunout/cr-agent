// Package store keeps the agent's local config and state under %LOCALAPPDATA%\cr-agent:
//
//	config.json          hub URL, machine id, slug, key id (public part), model folder, bounded config
//	state\*.bin          secrets, each a DPAPI blob for the current user (machine key, HF token, approvals)
//	state\*.json         plain state (the model index)
//	logs\agent.log       JSON lines
//
// Secrets are never returned by anything that logs, and no method formats them into an error.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Config is config.json. Nothing in it is secret.
type Config struct {
	Hub       string `json:"hub,omitempty"`        // wss://<host>/agent, already validated when stored
	MachineID string `json:"machine_id,omitempty"` // minted by the hub at pairing
	Slug      string `json:"slug,omitempty"`
	KeyID     string `json:"key_id,omitempty"`     // public part of the machine key
	ModelsDir string `json:"models_dir,omitempty"` // empty = <home>\models
	// EngineReadFolder is the model folder the host allowed the engine's sandbox to read, by a click.
	EngineReadFolder string `json:"engine_read_folder,omitempty"`
	// EngineGrants lists the paths the agent itself added the sandbox's read entry to, so that it can
	// take those away and never touches an entry the host made.
	EngineGrants  []string `json:"engine_grants,omitempty"`
	LocalPause    bool     `json:"local_pause,omitempty"`
	KeepAwake     bool     `json:"keep_awake,omitempty"`
	IdleUnloadMin int      `json:"idle_unload_min,omitempty"`
	GPUSelect     string   `json:"gpu_select,omitempty"`
	// EngineLowIntegrity is the host's own click in the tray: the AppContainer does not work on this PC,
	// and the host chose low integrity instead. The hub cannot set it. There is no setting below it: an older
	// version's engine_normal_integrity is not read, and that host starts again from the AppContainer.
	EngineLowIntegrity bool `json:"engine_low_integrity,omitempty"`
}

// Names of the secrets and state files.
const (
	SecretMachineKey  = "machine-key"
	SecretHFToken     = "hf-token"
	SecretApprovals   = "approvals"
	SecretModelHashes = "model-hashes"
	StateModels       = "models"
)

// ErrNotFound is returned for a secret or state file that does not exist.
var ErrNotFound = errors.New("not found")

// Protector encrypts secrets at rest. The Windows implementation is DPAPI for the current user.
type Protector interface {
	Protect(name string, plain []byte) ([]byte, error)
	Unprotect(name string, blob []byte) ([]byte, error)
}

// Store is the agent's home folder.
type Store struct {
	dir string
	p   Protector

	mu  sync.Mutex
	cfg Config
}

// DefaultDir is %LOCALAPPDATA%\cr-agent (the user cache dir elsewhere).
func DefaultDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "cr-agent"), nil
}

// Open creates the folder layout and reads config.json. A corrupt config is an error, never silently reset:
// resetting would drop the pairing.
func Open(dir string) (*Store, error) { return OpenWith(dir, defaultProtector()) }

// OpenWith is Open with a given protector (tests).
func OpenWith(dir string, p Protector) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{abs, filepath.Join(abs, "state"), filepath.Join(abs, "logs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: abs, p: p}
	b, err := os.ReadFile(s.configPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.cfg); err != nil {
			return nil, fmt.Errorf("config.json is damaged: %w", err)
		}
	}
	return s, nil
}

// Dir is the home folder.
func (s *Store) Dir() string { return s.dir }

// LogPath is the agent log.
func (s *Store) LogPath() string { return filepath.Join(s.dir, "logs", "agent.log") }

func (s *Store) configPath() string { return filepath.Join(s.dir, "config.json") }

// Config returns a copy of the config.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Update changes the config and writes it atomically.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	fn(&next)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.configPath(), b); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// ModelsDir is the configured model folder, or <home>\models.
func (s *Store) ModelsDir() string {
	if d := s.Config().ModelsDir; d != "" {
		return d
	}
	return filepath.Join(s.dir, "models")
}

var reName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)

func (s *Store) statePath(name, ext string) (string, error) {
	if !reName.MatchString(name) {
		return "", errors.New("bad state name")
	}
	return filepath.Join(s.dir, "state", name+ext), nil
}

// WriteSecret protects plain and stores it.
func (s *Store) WriteSecret(name string, plain []byte) error {
	path, err := s.statePath(name, ".bin")
	if err != nil {
		return err
	}
	blob, err := s.p.Protect(name, plain)
	if err != nil {
		return fmt.Errorf("protect %s: %w", name, err)
	}
	return writeAtomic(path, blob)
}

// ReadSecret returns the secret, ErrNotFound when absent. A blob that cannot be opened (another user,
// another PC, damaged) is an error that names the secret but never its content.
func (s *Store) ReadSecret(name string) ([]byte, error) {
	path, err := s.statePath(name, ".bin")
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.p.Unprotect(name, blob)
	if err != nil {
		return nil, fmt.Errorf("unprotect %s: %w", name, err)
	}
	return plain, nil
}

// DeleteSecret removes a secret; a missing one is not an error.
func (s *Store) DeleteSecret(name string) error {
	path, err := s.statePath(name, ".bin")
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WriteJSON stores plain state.
func (s *Store) WriteJSON(name string, v any) error {
	path, err := s.statePath(name, ".json")
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// ReadJSON reads plain state, ErrNotFound when absent.
func (s *Store) ReadJSON(name string, v any) error {
	path, err := s.statePath(name, ".json")
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// MachineKey returns crm_<key_id>_<secret>, or ErrNotFound when the PC is not paired.
func (s *Store) MachineKey() (string, error) {
	b, err := s.ReadSecret(SecretMachineKey)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SavePairing stores the key and the pairing facts together.
func (s *Store) SavePairing(hub, machineID, slug, keyID, key string) error {
	if err := s.WriteSecret(SecretMachineKey, []byte(key)); err != nil {
		return err
	}
	return s.Update(func(c *Config) { c.Hub, c.MachineID, c.Slug, c.KeyID = hub, machineID, slug, keyID })
}

// Unpair forgets the key. The machine id is kept so a later pairing can offer "Replace key" (ruling B1).
func (s *Store) Unpair() error {
	if err := s.DeleteSecret(SecretMachineKey); err != nil {
		return err
	}
	return s.Update(func(c *Config) { c.KeyID = "" })
}

// Paired reports whether a machine key is stored.
func (s *Store) Paired() bool {
	path, _ := s.statePath(SecretMachineKey, ".bin")
	_, err := os.Stat(path)
	return err == nil
}

// writeAtomic writes to a temp file in the same folder and renames it over the target.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
