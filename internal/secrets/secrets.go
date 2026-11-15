// Package secrets keeps database passwords in the OS keychain (macOS
// Keychain, Windows Credential Manager, Secret Service on Linux), keyed by
// profile id. Passwords never touch the profiles database or settings
// files.
//
// The Rust build used the `keyring` crate (v3, `apple-native`), which on
// macOS keeps each password as a generic password of the login keychain
// with the service `dev.meerkat` and the profile id as the account. mygo's
// Secrets module keeps its secrets the same way — a generic password of
// the file-based login keychain, `kSecAttrService` the service and
// `kSecAttrAccount` the key — so `Service(service)` reads and writes the
// very items the Rust build left behind, and an existing user keeps every
// saved password.
package secrets

import (
	"errors"
	"unicode/utf8"

	"github.com/egoist/mygo"
)

const service = "dev.meerkat"

// store is the keychain under the Rust build's service name.
func store() mygo.SecretsModule {
	return mygo.Secrets.Service(service)
}

// SetPassword keeps password for the profile, replacing what it had.
func SetPassword(profileID, password string) error {
	return store().Set(profileID, []byte(password))
}

// GetPassword is the profile's password; false when none is kept.
func GetPassword(profileID string) (string, bool, error) {
	password, err := store().Get(profileID)
	switch {
	case err == nil:
		// keyring hands back a `String`, and refuses an item that is
		// not UTF-8 rather than mangling it.
		if !utf8.Valid(password) {
			return "", false, errors.New("Password cannot be UTF-8 encoded")
		}
		return string(password), true, nil
	case errors.Is(err, mygo.ErrSecretNotFound):
		return "", false, nil
	default:
		return "", false, err
	}
}

// DeletePassword forgets the profile's password. Forgetting one that was
// never kept is not an error.
func DeletePassword(profileID string) error {
	err := store().Delete(profileID)
	if err == nil || errors.Is(err, mygo.ErrSecretNotFound) {
		return nil
	}
	return err
}
