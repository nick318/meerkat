// Package releasechannel says which release channel this build belongs to,
// and the identity a build carries: version and commit.
//
// The channel is baked in at link time (`-ldflags -X …channel=`), which
// only the bundle script sets. A plain `go build` therefore
// always produces a `Local` build, and a `Local` build never polls for
// updates — there is nothing an update could replace. The commit rides in
// the same way, through `commitSHA`, because two dev builds of
// the same version differ only by their commit.
//
// In Go both ride in through the linker rather than the environment:
//
//	go build -ldflags "-X github.com/nick318/meerkat/internal/releasechannel.channel=dev \
//	  -X github.com/nick318/meerkat/internal/releasechannel.commitSHA=<sha>"
//
// A plain `go build` sets neither, which is the `Local` build.
//
// This crate must not know about `gpui` (see the dependency direction in
// CLAUDE.md): the updater and the UI read these values, the values do not
// reach for the UI.
package releasechannel

import "fmt"

// Set by `-ldflags -X` at link time; empty when nothing was set. They are
// `option_env!("MEERKAT_CHANNEL")` and `option_env!("MEERKAT_COMMIT_SHA")`
// in the Rust, with the empty string standing for `None`.
var (
	channel   string
	commitSHA string
)

// version is the version of this build, and the one place it is written:
// `scripts/bundle-mac.sh` reads it from here for the bundle's
// Info.plist.
const version = "0.1.11"

// Channel is where a build came from, and so where its updates come from.
type Channel int

const (
	// Local is a plain `go build` on somebody's machine. Never polls for
	// updates.
	Local Channel = iota
	// Dev is the moving channel: built from `main`, replaced often.
	Dev
	// Public is the stable channel: built from a version tag.
	Public
)

// InvalidChannel is the channel string was none of `dev` and `public`.
type InvalidChannel struct{}

func (InvalidChannel) Error() string {
	return "invalid channel"
}

// ParseChannel is what a distributed build may set `MEERKAT_CHANNEL` to.
// `local` is deliberately not accepted: local is what a build is when
// nothing was set, never something to ask for — a CI job that asked for it
// by name would ship a build that cannot update itself.
//
// A nil `name` is `None`: nothing was set.
func ParseChannel(name *string) (Channel, error) {
	switch {
	case name == nil:
		return Local, nil
	case *name == "dev":
		return Dev, nil
	case *name == "public":
		return Public, nil
	default:
		return 0, InvalidChannel{}
	}
}

// Current is the channel this binary was built for. (Rust: `channel()`;
// the Go name `Channel` is the type's.)
func Current() Channel {
	var name *string
	if channel != "" {
		name = &channel
	}
	parsed, err := ParseChannel(name)
	if err != nil {
		panic(fmt.Sprintf(
			"invalid MEERKAT_CHANNEL %q: use dev or public, or unset it for a local build",
			channel,
		))
	}
	return parsed
}

// Version is the version of this build.
func Version() string {
	return version
}

// CommitSHA is the commit this binary was built from, when CI said. Two dev
// builds of the same version are told apart by nothing else.
func CommitSHA() (string, bool) {
	return commitSHA, commitSHA != ""
}

// PollsForUpdates reports whether a build on this channel looks for updates
// at all.
func (c Channel) PollsForUpdates() bool {
	return c != Local
}

// DevName is the name the feed, the CI and the UI all use for this channel.
func (c Channel) DevName() string {
	switch c {
	case Local:
		return "local"
	case Dev:
		return "dev"
	case Public:
		return "public"
	}
	return ""
}

// DisplayName is what the app calls itself on this channel. The dev app is
// named apart so the two can sit in /Applications side by side.
func (c Channel) DisplayName() string {
	switch c {
	case Local:
		return "Meerkat (local)"
	case Dev:
		return "Meerkat Dev"
	case Public:
		return "Meerkat"
	}
	return ""
}
