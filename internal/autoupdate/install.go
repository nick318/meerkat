package autoupdate

// Fetch and install one update, off the UI's goroutine.
//
// Everything here is plain Go with no UI in sight, so it can run on a
// goroutine the way every database call does.
//
// The install is Zed's: extract the new `.app` next to the download, then
// `rsync -a --delete` its contents over the running bundle. Overwriting a
// running binary is fine on macOS — the old executable lives on unlinked
// until the process exits — and rsync in place means there is never a
// moment with no app on disk, which a remove-and-rename would have.
//
// The archive is checked against the feed's sha256 before anything is
// unpacked: it crosses a CDN the app does not run.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nick318/meerkat/internal/releasechannel"
)

// httpClient is the one HTTP client. A bare GET with a plain user agent — no
// ids, no query, no cookies: nothing in a request says who is asking.
// Compression is off because Go would otherwise add an
// `Accept-Encoding` the Rust client never sent.
func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:              http.ProxyFromEnvironment,
			DisableCompression: true,
			ForceAttemptHTTP2:  true,
		},
	}
}

// get is the bare GET, with the user agent and nothing else.
func get(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", fmt.Sprintf("meerkat-updater/%s", releasechannel.Version()))
	return httpClient().Do(request)
}

// statusText is reqwest's `Display` for a status: the code and its
// canonical reason.
func statusText(code int) string {
	reason := http.StatusText(code)
	if reason == "" {
		reason = "<unknown status code>"
	}
	return fmt.Sprintf("%d %s", code, reason)
}

func isSuccess(code int) bool {
	return code >= 200 && code < 300
}

// FeedURL is where the feed lives. `MEERKAT_UPDATE_URL` overrides it,
// which is how an update is tested end to end against a local file server.
func FeedURL(channel releasechannel.Channel) string {
	if url, ok := os.LookupEnv("MEERKAT_UPDATE_URL"); ok {
		return url
	}
	return fmt.Sprintf(
		"https://raw.githubusercontent.com/nick318/meerkat/updates/%s.json",
		channel.DevName(),
	)
}

// FetchManifest reads one channel's feed.
func FetchManifest(ctx context.Context, url string) (*Manifest, error) {
	response, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if !isSuccess(response.StatusCode) {
		return nil, fmt.Errorf("the feed answered %s for %s", statusText(response.StatusCode), url)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("the feed did not parse as a manifest: %w", err)
	}
	return &manifest, nil
}

// DownloadAndInstall downloads the archive, verifies it, unpacks it, and
// rsyncs the new bundle over `appPath` — the running `.app`.
func DownloadAndInstall(ctx context.Context, update Update, appPath string) error {
	if !hasAppExtension(appPath) {
		return fmt.Errorf("not running from an .app bundle (%s), nothing to update", appPath)
	}

	staging, err := os.MkdirTemp("", "meerkat-update")
	if err != nil {
		return fmt.Errorf("could not create a staging directory: %w", err)
	}
	// `tempfile::TempDir` removes itself when dropped.
	defer os.RemoveAll(staging)

	archive := filepath.Join(staging, "update.tar.gz")
	if err := downloadVerified(ctx, &update, archive); err != nil {
		return err
	}

	extracted := filepath.Join(staging, "extracted")
	if err := os.Mkdir(extracted, 0o777); err != nil {
		return err
	}
	if err := run(exec.CommandContext(ctx, "tar", "-xzf", archive, "-C", extracted), "tar"); err != nil {
		return err
	}

	newApp, err := singleAppIn(extracted)
	if err != nil {
		return err
	}

	// Trailing slash on the source: rsync copies the *contents* of the
	// new bundle into the running one, whatever either is named.
	source := newApp + "/"
	return run(exec.CommandContext(ctx, "rsync", "-a", "--delete", source, appPath), "rsync")
}

func downloadVerified(ctx context.Context, update *Update, target string) error {
	response, err := get(ctx, update.Url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if !isSuccess(response.StatusCode) {
		return fmt.Errorf("the download answered %s for %s", statusText(response.StatusCode), update.Url)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}

	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	if !eqIgnoreASCIICase(digest, update.Sha256) {
		return fmt.Errorf("checksum mismatch: the feed says %s, the download is %s", update.Sha256, digest)
	}

	return os.WriteFile(target, body, 0o666)
}

// singleAppIn finds the one `.app` the archive holds; its name changes
// with the channel, so it is found rather than assumed.
func singleAppIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var apps []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if hasAppExtension(path) {
			apps = append(apps, path)
		}
	}
	switch len(apps) {
	case 1:
		return apps[0], nil
	case 0:
		return "", errors.New("the update archive held no .app")
	default:
		return "", errors.New("the update archive held more than one .app")
	}
}

// hasAppExtension is `path.extension() == Some("app")`: the part of the
// file name after its last dot, where a name that only starts with a dot
// has no extension.
func hasAppExtension(path string) bool {
	name := filepath.Base(path)
	dot := strings.LastIndex(name, ".")
	return dot > 0 && name[dot+1:] == "app"
}

func run(command *exec.Cmd, name string) error {
	var stderr strings.Builder
	command.Stderr = &stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		return fmt.Errorf("%s failed: %s", name, strings.ToValidUTF8(stderr.String(), "�"))
	default:
		return fmt.Errorf("could not run %s: %w", name, err)
	}
}

// eqIgnoreASCIICase is Rust's `str::eq_ignore_ascii_case`: only ASCII
// letters fold, unlike `strings.EqualFold`.
func eqIgnoreASCIICase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
