// Package autoupdate is self-update: poll the channel's feed, download
// what is newer, and lay it over the running bundle, ready for a restart.
//
// One updater for the whole app, held as a global — two windows must not
// race two rsyncs over one bundle. It exists only on a distributed
// channel: a `local` build has nothing an update could replace, so `Init`
// returns nil and the UI shows nothing.
//
// Nothing here restarts the app. The updater gets as far as `Ready` and
// stops; the restart is the user's, from the toast in the UI, because an
// app that relaunches itself under an open transaction would be making
// the close dialog's decision without asking.
//
// The check and the install run on goroutines, the way every database
// call does, and replies carry a generation the way a tab's queries do: a
// manual check racing the hourly one must not apply twice.
//
// The Rust build held the updater as a GPUI global entity and repainted
// through `cx.notify()`. Here it is a plain struct guarded by a mutex, and
// every change calls the `onChange` func the app hands to `Init` — which
// the app wires to a redraw of its windows. `onChange` is called with no
// lock held, from whichever goroutine made the change.
package autoupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nick318/meerkat/internal/releasechannel"
)

// startDelay is what the first check waits out, so it never competes with
// the app opening a connection.
const startDelay = 30 * time.Second

// pollInterval: both channels poll hourly. The feed is a static file
// behind a CDN; nothing is gained by asking faster.
const pollInterval = 60 * 60 * time.Second

// Status is where the updater is. One of StatusIdle, StatusChecking,
// StatusUpdating, StatusReady or StatusErrored.
type Status interface {
	isStatus()
}

// StatusIdle: nothing under way.
type StatusIdle struct{}

// StatusChecking: the feed has been asked.
type StatusChecking struct{}

// StatusUpdating is downloading and installing, one word: the UI says
// "updating…" for the whole of it, and the split earned nobody anything.
type StatusUpdating struct{}

// StatusReady is installed over the bundle; a restart picks it up.
type StatusReady struct {
	Version string
	Sha     *string
}

// StatusErrored: only a manual check lands here. The hourly one fails
// quietly back to `StatusIdle`, because offline is normal and not news.
type StatusErrored struct {
	Error string
}

func (StatusIdle) isStatus()     {}
func (StatusChecking) isStatus() {}
func (StatusUpdating) isStatus() {}
func (StatusReady) isStatus()    {}
func (StatusErrored) isStatus()  {}

// Ready is which build a `StatusReady` is offering. It is what a dismissal
// is keyed by, so an update the user waved away stays waved away and the
// *next* one announces itself.
type Ready struct {
	Version string
	Sha     *string
}

// Equal is Rust's derived `PartialEq`: the version and the commit, by
// value.
func (r *Ready) Equal(other *Ready) bool {
	if r == nil || other == nil {
		return r == other
	}
	if r.Version != other.Version {
		return false
	}
	if r.Sha == nil || other.Sha == nil {
		return r.Sha == nil && other.Sha == nil
	}
	return *r.Sha == *other.Sha
}

// AutoUpdater is the one updater.
type AutoUpdater struct {
	mu     sync.Mutex
	status Status
	// generation is bumped per check; a reply carrying an older value is
	// dropped.
	generation uint64
	// dismissed is the build whose toast the user closed. **In memory on
	// purpose**: closing the toast says "not now", not "never" — a
	// restart is the one thing that update was waiting for, so a run of
	// the app that still has it pending says so again. Persisting it
	// would turn one dismissal into silence for ever.
	dismissed *Ready
	// onChange is `cx.notify()`: the app's redraw.
	onChange func()
	// appPath is `cx.app_path()`: the running bundle.
	appPath func() (string, error)
	// stop ends the hourly loop — Rust's `_poll` task, which ended when
	// the entity was dropped.
	stop     chan struct{}
	stopOnce sync.Once
}

// global is `GlobalAutoUpdater`.
var global atomic.Pointer[AutoUpdater]

// Init builds the updater and starts the hourly loop — on a distributed
// channel. On `Local` this does nothing and answers nil. onChange is
// called after every change of what the updater would paint; it may be
// nil.
func Init(onChange func()) *AutoUpdater {
	if !releasechannel.Current().PollsForUpdates() {
		return nil
	}
	updater := newAutoUpdater(onChange)
	global.Store(updater)
	return updater
}

// TryGlobal is the global updater, nil on a local build.
func TryGlobal() *AutoUpdater {
	return global.Load()
}

func newAutoUpdater(onChange func()) *AutoUpdater {
	u := &AutoUpdater{
		status:     StatusIdle{},
		generation: 0,
		dismissed:  nil,
		onChange:   onChange,
		appPath:    AppPath,
		stop:       make(chan struct{}),
	}
	go func() {
		select {
		case <-time.After(startDelay):
		case <-u.stop:
			return
		}
		for {
			u.Check(TriggerAutomatic)
			select {
			case <-time.After(pollInterval):
			case <-u.stop:
				return
			}
		}
	}()
	return u
}

// Close ends the hourly loop. Rust's loop ended when the entity was
// dropped; a Go value has to be told.
func (u *AutoUpdater) Close() {
	u.stopOnce.Do(func() { close(u.stop) })
}

// notify is `cx.notify()`. Call it with no lock held.
func (u *AutoUpdater) notify() {
	if u.onChange != nil {
		u.onChange()
	}
}

// Status is where the updater is now.
func (u *AutoUpdater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	if ready, ok := u.status.(StatusReady); ok {
		return StatusReady{Version: ready.Version, Sha: cloneString(ready.Sha)}
	}
	return u.status
}

// Announcement is the update worth announcing, or nil. It is non-nil only
// while an install is `Ready` **and** the user has not closed that one's
// toast: the footer's line still says so either way, because a line the
// user went looking for is not the same as one that arrives.
func (u *AutoUpdater) Announcement() *Ready {
	u.mu.Lock()
	defer u.mu.Unlock()
	status, ok := u.status.(StatusReady)
	if !ok {
		return nil
	}
	ready := &Ready{
		Version: status.Version,
		Sha:     cloneString(status.Sha),
	}
	if !Announces(ready, u.dismissed) {
		return nil
	}
	return ready
}

// Dismiss closes the toast for whatever is ready now. A later install
// carries a different build, so it announces itself in its turn.
func (u *AutoUpdater) Dismiss() {
	u.mu.Lock()
	status, ok := u.status.(StatusReady)
	if !ok {
		u.mu.Unlock()
		return
	}
	u.dismissed = &Ready{
		Version: status.Version,
		Sha:     cloneString(status.Sha),
	}
	u.mu.Unlock()
	u.notify()
}

// Check starts a check unless one is already under way. `Ready` also
// stays put: the update is on disk, and the only thing left is a restart.
func (u *AutoUpdater) Check(trigger Trigger) {
	u.mu.Lock()
	switch u.status.(type) {
	case StatusChecking, StatusUpdating, StatusReady:
		u.mu.Unlock()
		return
	case StatusIdle, StatusErrored:
	}

	channel := releasechannel.Current()
	u.generation++
	generation := u.generation
	u.status = StatusChecking{}
	u.mu.Unlock()
	u.notify()

	url := FeedURL(channel)
	go func() {
		update, err := func() (*Update, error) {
			manifest, err := FetchManifest(context.Background(), url)
			if err != nil {
				return nil, err
			}
			var installedSha *string
			if sha, ok := releasechannel.CommitSHA(); ok {
				installedSha = &sha
			}
			return UpdateAvailable(
				channel,
				releasechannel.Version(),
				installedSha,
				manifest,
			)
		}()
		u.applyCheck(generation, update, flatten(err), trigger)
	}()
}

// applyCheck lands a check. errText is nil when the check worked.
func (u *AutoUpdater) applyCheck(generation uint64, update *Update, errText *string, trigger Trigger) {
	u.mu.Lock()
	if generation != u.generation {
		u.mu.Unlock()
		return
	}
	switch {
	case errText == nil && update != nil:
		u.mu.Unlock()
		u.install(generation, *update, trigger)
	case errText == nil:
		u.status = StatusIdle{}
		u.mu.Unlock()
		u.notify()
	default:
		u.failLocked(*errText, trigger)
		u.mu.Unlock()
		u.notify()
	}
}

func (u *AutoUpdater) install(generation uint64, update Update, trigger Trigger) {
	appPath, err := u.appPath()
	u.mu.Lock()
	if generation != u.generation {
		u.mu.Unlock()
		return
	}
	if err != nil {
		u.failLocked(fmt.Sprintf("no app path: %v", err), trigger)
		u.mu.Unlock()
		u.notify()
		return
	}

	u.status = StatusUpdating{}
	u.mu.Unlock()
	u.notify()

	version := update.Version
	sha := cloneString(update.Sha)
	go func() {
		errText := flatten(DownloadAndInstall(context.Background(), update, appPath))
		u.mu.Lock()
		if generation != u.generation {
			u.mu.Unlock()
			return
		}
		if errText == nil {
			u.status = StatusReady{Version: version, Sha: sha}
		} else {
			u.failLocked(*errText, trigger)
		}
		u.mu.Unlock()
		u.notify()
	}()
}

// failLocked: a manual check owes the user its error; the hourly one owes
// them quiet — offline is the ordinary case it fails in. Called with the
// lock held; the caller notifies once it has let go.
func (u *AutoUpdater) failLocked(errText string, trigger Trigger) {
	fmt.Fprintf(os.Stderr, "auto-update: %s\n", errText)
	switch trigger {
	case TriggerManual:
		u.status = StatusErrored{Error: errText}
	default:
		u.status = StatusIdle{}
	}
}

// Announces says whether a ready build is still worth a toast. A plain
// function over the two, so the rule is argued with in a test rather than
// in a running window: the same build stays closed, and anything else — a
// new version, the same version at a new commit, or nothing dismissed yet
// — is news.
func Announces(ready *Ready, dismissed *Ready) bool {
	return dismissed == nil || !dismissed.Equal(ready)
}

// Trigger is who asked for the check. Decides only what an error does.
type Trigger int

const (
	TriggerAutomatic Trigger = iota
	TriggerManual
)

// String is the variant name, as Rust's derived Debug prints it.
func (t Trigger) String() string {
	switch t {
	case TriggerAutomatic:
		return "Automatic"
	case TriggerManual:
		return "Manual"
	}
	return fmt.Sprintf("Trigger(%d)", int(t))
}

// flatten collapses an error into one string for display, the shape
// `shell.rs` uses for its own replies. nil when there was no error. (Rust
// also folded tokio's `JoinError` in; a goroutine has none.)
func flatten(err error) *string {
	if err == nil {
		return nil
	}
	text := err.Error()
	return &text
}

// AppPath is `cx.app_path()`: the `.app` bundle the running binary sits
// in (`…/Meerkat.app/Contents/MacOS/meerkat`), or — for a bare binary,
// as `NSBundle.mainBundle.bundlePath` answers — the folder holding it.
func AppPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	macOS := filepath.Dir(exe)
	contents := filepath.Dir(macOS)
	bundle := filepath.Dir(contents)
	if filepath.Base(macOS) == "MacOS" && filepath.Base(contents) == "Contents" && hasAppExtension(bundle) {
		return bundle, nil
	}
	return macOS, nil
}
