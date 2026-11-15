// Package settings is user settings: a JSON file the user can edit by hand
// (Zed pattern). Unknown keys are ignored; missing keys fall back to
// defaults.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Settings is the whole file. Every field is optional in the JSON
// (`#[serde(default)]` on the struct): a missing key keeps its value from
// [Default].
type Settings struct {
	// "dark" or "light".
	Theme string `json:"theme"`
	// Default row cap for query results.
	FetchLimit uint32 `json:"fetch_limit"`
}

// Default is `impl Default for Settings`.
func Default() Settings {
	return Settings{
		Theme:      "dark",
		FetchLimit: 10_000,
	}
}

// Load reads the settings file, or the defaults when there is none.
func Load() (Settings, error) {
	path, err := SettingsPath()
	if err != nil {
		return Settings{}, err
	}
	if _, err := os.Stat(path); err != nil {
		// `Path::exists` is false on any error, not only on a missing file.
		return Default(), nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	loaded := Default()
	if err := json.Unmarshal(text, &loaded); err != nil {
		return Settings{}, err
	}
	return loaded, nil
}

// Save writes the settings file, pretty-printed, making its directory
// first.
func (s Settings) Save() error {
	path, err := SettingsPath()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	text, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, text, 0o666)
}

// SettingsPath is `<user config dir>/meerkat/settings.json`.
func SettingsPath() (string, error) {
	// `dirs::config_dir` answers `None` with no reason, so the reason Go gives
	// is dropped and the message is the Rust context alone.
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("no user config directory")
	}
	return filepath.Join(dir, "meerkat", "settings.json"), nil
}
