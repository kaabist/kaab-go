package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"
)

// Where the CLI keeps its profiles:
//
//	~/.config/kaab/config.toml        profiles: name, base URL, panel URL, an organisation note,
//	                                   and where each mode's key was put (never a key)
//	OS keychain, service "kaab"       one entry per profile and mode: "<profile>:live", "<profile>:test"
//	~/.config/kaab/credentials.toml   the fallback when there is no keychain (0600)
//
// KAAB_CONFIG_DIR moves the directory; KAAB_API_KEY and KAAB_BASE_URL override any profile.

const (
	keyringService  = "kaab"
	defaultProfile  = "default"
	defaultPanelURL = "https://app.kaab.ist"
)

// Profile is one named set of credentials.
type Profile struct {
	BaseURL  string `toml:"base_url,omitempty"`
	PanelURL string `toml:"panel_url,omitempty"`
	// Org is a note to tell profiles apart ("acme production"); the key decides the organisation.
	Org string `toml:"org,omitempty"`
	// Storage is where each mode's key was put: "keyring" or "file". `auth status` shows it; a
	// lookup tries the keychain, then the file, whatever it says.
	Storage StorageByMode `toml:"storage,omitempty"`
	// Modes lists the modes a key is stored for ("live", "test").
	Modes []string `toml:"modes,omitempty"`
}

// StorageByMode maps a mode ("live", "test") to where its key was put.
type StorageByMode map[string]string

// legacyStorage holds the string form older CLIs wrote (one storage for the whole profile) until
// load spreads it over the profile's modes.
const legacyStorage = "*"

// UnmarshalTOML reads the per-mode table and the older single string.
func (s *StorageByMode) UnmarshalTOML(data any) error {
	out := StorageByMode{}
	switch v := data.(type) {
	case string:
		out[legacyStorage] = v
	case map[string]any:
		for mode, where := range v {
			text, ok := where.(string)
			if !ok {
				return fmt.Errorf("storage.%s is not a string", mode)
			}
			out[mode] = text
		}
	default:
		return fmt.Errorf("storage is a %T, not a table", data)
	}
	*s = out
	return nil
}

// String is "live:keyring,test:file", in mode order.
func (s StorageByMode) String() string {
	modes := make([]string, 0, len(s))
	for mode := range s {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	parts := make([]string, 0, len(modes))
	for _, mode := range modes {
		parts = append(parts, mode+":"+s[mode])
	}
	return strings.Join(parts, ",")
}

// Config is config.toml.
type Config struct {
	Current  string              `toml:"current,omitempty"`
	Profiles map[string]*Profile `toml:"profiles,omitempty"`
}

type fileCredentials struct {
	Keys map[string]string `toml:"keys"`
}

// secretStore keeps keys; the OS keychain, or a test double.
type secretStore interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Set(service, user, secret string) error { return keyring.Set(service, user, secret) }
func (osKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

func configDir(getenv func(string) string) (string, error) {
	if dir := getenv("KAAB_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "kaab"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kaab"), nil
}

type store struct {
	dir     string
	secrets secretStore
}

func (s *store) configPath() string      { return filepath.Join(s.dir, "config.toml") }
func (s *store) credentialsPath() string { return filepath.Join(s.dir, "credentials.toml") }

func (s *store) load() (*Config, error) {
	cfg := &Config{Profiles: map[string]*Profile{}}
	data, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", s.configPath(), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	for _, p := range cfg.Profiles {
		if where, ok := p.Storage[legacyStorage]; ok {
			delete(p.Storage, legacyStorage)
			for _, mode := range p.Modes {
				p.Storage[mode] = where
			}
		}
	}
	return cfg, nil
}

func writeTOML(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), mode)
}

func (s *store) save(cfg *Config) error { return writeTOML(s.configPath(), cfg, 0o600) }

func (s *store) readFileKeys() (*fileCredentials, error) {
	creds := &fileCredentials{Keys: map[string]string{}}
	data, err := os.ReadFile(s.credentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(data), creds); err != nil {
		return nil, err
	}
	if creds.Keys == nil {
		creds.Keys = map[string]string{}
	}
	return creds, nil
}

func entry(profile, mode string) string { return profile + ":" + mode }

// putKey stores key in the keychain, or in credentials.toml when there is none (or forceFile);
// it answers where.
func (s *store) putKey(profile, mode, key string, forceFile bool) (string, error) {
	if !forceFile {
		if err := s.secrets.Set(keyringService, entry(profile, mode), key); err == nil {
			return "keyring", nil
		}
	}
	creds, err := s.readFileKeys()
	if err != nil {
		return "", err
	}
	creds.Keys[entry(profile, mode)] = key
	if err := writeTOML(s.credentialsPath(), creds, 0o600); err != nil {
		return "", err
	}
	return "file", nil
}

// getKey tries the keychain, then the file: modes of one profile can be kept in different places
// (a live key in the keychain, a test key put in the file with --file-storage).
func (s *store) getKey(profile, mode string) (string, error) {
	if key, err := s.secrets.Get(keyringService, entry(profile, mode)); err == nil {
		return key, nil
	}
	creds, err := s.readFileKeys()
	if err != nil {
		return "", err
	}
	if key := creds.Keys[entry(profile, mode)]; key != "" {
		return key, nil
	}
	return "", fmt.Errorf("no %s key stored for profile %q: run `kaab auth login`", mode, profile)
}

func (s *store) deleteKeys(profile string) error {
	for _, mode := range []string{"live", "test"} {
		_ = s.secrets.Delete(keyringService, entry(profile, mode))
	}
	creds, err := s.readFileKeys()
	if err != nil {
		return err
	}
	for _, mode := range []string{"live", "test"} {
		delete(creds.Keys, entry(profile, mode))
	}
	if len(creds.Keys) == 0 {
		err := os.Remove(s.credentialsPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeTOML(s.credentialsPath(), creds, 0o600)
}

// keyMode is "live" or "test" from a key's prefix.
func keyMode(key string) (string, error) {
	switch {
	case strings.HasPrefix(key, "sk_live_"):
		return "live", nil
	case strings.HasPrefix(key, "sk_test_"):
		return "test", nil
	}
	return "", errors.New("an API key starts with sk_live_ or sk_test_")
}

func sortedProfiles(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasMode(p *Profile, mode string) bool {
	for _, m := range p.Modes {
		if m == mode {
			return true
		}
	}
	return false
}
