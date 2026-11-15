package autoupdate

import (
	"encoding/json"
	"testing"

	"github.com/nick318/meerkat/internal/releasechannel"
)

func strPtr(s string) *string { return &s }

func manifest(version string, sha *string) *Manifest {
	assets := map[string]Asset{}
	assets[AssetKey()] = Asset{
		Url:    "https://example.test/a.tar.gz",
		Sha256: "aa",
	}
	return &Manifest{
		Version: version,
		Sha:     sha,
		Assets:  assets,
	}
}

func available(t *testing.T, channel releasechannel.Channel, installed string, sha *string, m *Manifest) bool {
	t.Helper()
	update, err := UpdateAvailable(channel, installed, sha, m)
	if err != nil {
		t.Fatal(err)
	}
	return update != nil
}

func TestPublicMovesByVersion(t *testing.T) {
	m := manifest("0.2.0", nil)
	if !available(t, releasechannel.Public, "0.1.0", nil, m) {
		t.Fatal("0.1.0 was not offered 0.2.0")
	}
	if available(t, releasechannel.Public, "0.2.0", nil, m) {
		t.Fatal("0.2.0 was offered 0.2.0")
	}
	if available(t, releasechannel.Public, "0.3.0", nil, m) {
		t.Fatal("0.3.0 was offered 0.2.0")
	}
}

func TestDevMovesByCommitEvenOnTheSameVersion(t *testing.T) {
	m := manifest("0.1.0", strPtr("bbb2222"))
	if !available(t, releasechannel.Dev, "0.1.0", strPtr("aaa1111"), m) {
		t.Fatal("a new commit was not offered")
	}
	if available(t, releasechannel.Dev, "0.1.0", strPtr("bbb2222"), m) {
		t.Fatal("the same commit was offered")
	}
}

func TestADevBuildThatDoesNotKnowItsCommitTakesTheDownload(t *testing.T) {
	m := manifest("0.1.0", strPtr("bbb2222"))
	if !available(t, releasechannel.Dev, "0.1.0", nil, m) {
		t.Fatal("a build without a commit was not offered")
	}
}

func TestADevFeedWithoutACommitFallsBackToTheVersion(t *testing.T) {
	m := manifest("0.2.0", nil)
	if !available(t, releasechannel.Dev, "0.1.0", strPtr("aaa1111"), m) {
		t.Fatal("a newer version was not offered")
	}
	m = manifest("0.1.0", nil)
	if available(t, releasechannel.Dev, "0.1.0", strPtr("aaa1111"), m) {
		t.Fatal("the same version was offered")
	}
}

func TestNoAssetForThisPlatformIsUpToDateNotAnError(t *testing.T) {
	m := &Manifest{
		Version: "9.9.9",
		Sha:     nil,
		Assets:  map[string]Asset{},
	}
	if available(t, releasechannel.Public, "0.1.0", nil, m) {
		t.Fatal("a feed with no asset for this platform offered one")
	}
}

func TestALocalBuildIsNeverOfferedAnything(t *testing.T) {
	m := manifest("9.9.9", strPtr("bbb2222"))
	if available(t, releasechannel.Local, "0.1.0", nil, m) {
		t.Fatal("a local build was offered an update")
	}
}

func TestGarbageVersionsAreErrorsNotUpdates(t *testing.T) {
	m := manifest("not-a-version", nil)
	if _, err := UpdateAvailable(releasechannel.Public, "0.1.0", nil, m); err == nil {
		t.Fatal("a garbage version was not an error")
	}
}

func TestTheFeedParses(t *testing.T) {
	text := `{
            "version": "0.2.0",
            "sha": "abc1234",
            "assets": {
                "macos-aarch64": { "url": "https://example.test/m.tar.gz", "sha256": "deadbeef" }
            }
        }`
	var m Manifest
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.2.0" {
		t.Fatalf("version = %q", m.Version)
	}
	if m.Sha == nil || *m.Sha != "abc1234" {
		t.Fatalf("sha = %v", m.Sha)
	}
	if _, ok := m.Assets["macos-aarch64"]; !ok {
		t.Fatal("no macos-aarch64 asset")
	}
}
