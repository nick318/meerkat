package autoupdate

// The update feed: one static JSON file per channel, and the rule that
// reads it.
//
// The feed is a file on a CDN, never an endpoint — an endpoint could be
// taught to log who asked, and the whole design promises nobody counts.
// The client sends a bare GET with a plain user agent and nothing else.
//
// Two channels, two rules for "is that newer". The public channel moves
// by version, so semver answers. The dev channel ships often from `main`
// and rarely bumps the version, so two dev builds of one version are
// told apart by their commit — the rule Zed's nightly channel uses.

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/nick318/meerkat/internal/releasechannel"
)

// Manifest is one channel's feed. `Sha` is what the dev rule compares; the
// public rule never reads it.
type Manifest struct {
	Version string `json:"version"`
	// Sha is `#[serde(default)] Option<String>`.
	Sha    *string          `json:"sha"`
	Assets map[string]Asset `json:"assets"`
}

// UnmarshalJSON keeps serde's rule that `version` and `assets` must be
// there: a feed without them did not parse.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version *string           `json:"version"`
		Sha     *string           `json:"sha"`
		Assets  *map[string]Asset `json:"assets"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Version == nil {
		return errors.New("missing field `version`")
	}
	if raw.Assets == nil {
		return errors.New("missing field `assets`")
	}
	*m = Manifest{Version: *raw.Version, Sha: raw.Sha, Assets: *raw.Assets}
	return nil
}

// Asset is one downloadable build. The checksum is required: the archive
// crosses a CDN the app does not run, so the app checks what arrived.
type Asset struct {
	Url    string `json:"url"`
	Sha256 string `json:"sha256"`
}

// UnmarshalJSON keeps serde's rule that both fields must be there.
func (a *Asset) UnmarshalJSON(data []byte) error {
	var raw struct {
		Url    *string `json:"url"`
		Sha256 *string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Url == nil {
		return errors.New("missing field `url`")
	}
	if raw.Sha256 == nil {
		return errors.New("missing field `sha256`")
	}
	*a = Asset{Url: *raw.Url, Sha256: *raw.Sha256}
	return nil
}

// Update is what a check decided to fetch.
type Update struct {
	Version string
	Sha     *string
	Url     string
	Sha256  string
}

// AssetKey is the key this build looks itself up under in `assets` —
// `macos-aarch64`, straight from the compiler's own names. The names are
// Rust's (`std::env::consts::OS` and `ARCH`), because the feed was keyed
// by the Rust build and the Go build reads the same feed.
func AssetKey() string {
	return fmt.Sprintf("%s-%s", rustOS(runtime.GOOS), rustArch(runtime.GOARCH))
}

// rustOS is `std::env::consts::OS` for a GOOS.
func rustOS(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	default:
		return goos
	}
}

// rustArch is `std::env::consts::ARCH` for a GOARCH.
func rustArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	case "386":
		return "x86"
	default:
		return goarch
	}
}

// UpdateAvailable says whether the manifest offers something newer than
// this build, and what to download if so. `(nil, nil)` says up to date —
// including "no build for this platform", which is a gap in the feed and
// not an error the user can act on.
func UpdateAvailable(
	channel releasechannel.Channel,
	installedVersion string,
	installedSha *string,
	manifest *Manifest,
) (*Update, error) {
	var newer bool
	switch channel {
	// A local build is never offered an update; the caller should not
	// have asked.
	case releasechannel.Local:
		newer = false
	// Dev moves by commit. A build that does not know its own commit
	// takes the download: it cannot prove it is current.
	case releasechannel.Dev:
		switch {
		case manifest.Sha != nil && installedSha != nil:
			newer = *manifest.Sha != *installedSha
		case manifest.Sha != nil && installedSha == nil:
			newer = true
		// A dev feed without a commit cannot be compared by one;
		// fall back to the version.
		default:
			var err error
			if newer, err = isVersionNewer(installedVersion, manifest.Version); err != nil {
				return nil, err
			}
		}
	case releasechannel.Public:
		var err error
		if newer, err = isVersionNewer(installedVersion, manifest.Version); err != nil {
			return nil, err
		}
	}

	if !newer {
		return nil, nil
	}

	asset, ok := manifest.Assets[AssetKey()]
	if !ok {
		return nil, nil
	}

	return &Update{
		Version: manifest.Version,
		Sha:     cloneString(manifest.Sha),
		Url:     asset.Url,
		Sha256:  asset.Sha256,
	}, nil
}

func isVersionNewer(installed, fetched string) (bool, error) {
	installedV, err := parseVersion(installed)
	if err != nil {
		return false, fmt.Errorf("invalid installed version %q: %w", installed, err)
	}
	fetchedV, err := parseVersion(fetched)
	if err != nil {
		return false, fmt.Errorf("invalid version %q in the feed: %w", fetched, err)
	}
	return compareVersions(fetchedV, installedV) > 0, nil
}

// parseVersion is `semver::Version::from_str`: `major.minor.patch`, all
// three, with an optional pre-release and build, and no leading `v`. It
// answers the `v`-prefixed form `golang.org/x/mod/semver` compares.
func parseVersion(text string) (string, error) {
	v := "v" + text
	if !semver.IsValid(v) {
		return "", errors.New("not a semantic version")
	}
	// x/mod accepts `v1` and `v1.2` as shorthands; the semver crate does
	// not. Canonical fills them in, so a shorthand reads differently.
	if semver.Canonical(v) != strings.TrimSuffix(v, semver.Build(v)) {
		return "", errors.New("a version needs major, minor and patch")
	}
	return v, nil
}

// compareVersions is the semver crate's `Ord for Version`: precedence
// first, as x/mod compares, and build metadata last, as x/mod does not.
func compareVersions(a, b string) int {
	if c := semver.Compare(a, b); c != 0 {
		return c
	}
	return compareBuild(
		strings.TrimPrefix(semver.Build(a), "+"),
		strings.TrimPrefix(semver.Build(b), "+"),
	)
}

// compareBuild is the semver crate's `Ord for BuildMetadata`.
func compareBuild(lhs, rhs string) int {
	if lhs == rhs {
		return 0
	}
	left := strings.Split(lhs, ".")
	right := strings.Split(rhs, ".")
	for i, l := range left {
		if i >= len(right) {
			return 1
		}
		r := right[i]
		var ordering int
		switch lDigits, rDigits := allASCIIDigits(l), allASCIIDigits(r); {
		case lDigits && rDigits:
			// 0 < 00 < 1 < 01 < 001 < 2 < 02 < 002 < 10
			lv := strings.TrimLeft(l, "0")
			rv := strings.TrimLeft(r, "0")
			ordering = cmpInt(len(lv), len(rv))
			if ordering == 0 {
				ordering = strings.Compare(lv, rv)
			}
			if ordering == 0 {
				ordering = cmpInt(len(l), len(r))
			}
		case lDigits && !rDigits:
			return -1
		case !lDigits && rDigits:
			return 1
		default:
			ordering = strings.Compare(l, r)
		}
		if ordering != 0 {
			return ordering
		}
	}
	if len(right) == len(left) {
		return 0
	}
	return -1
}

func allASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}
