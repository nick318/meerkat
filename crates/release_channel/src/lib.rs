//! Which release channel this build belongs to, and the identity a build
//! carries: version and commit.
//!
//! The channel is baked in at compile time from `MEERKAT_CHANNEL`, which
//! only the bundle script and CI set. A plain `cargo build` therefore
//! always produces a `Local` build, and a `Local` build never polls for
//! updates — there is nothing an update could replace. The commit rides in
//! the same way, through `MEERKAT_COMMIT_SHA`, because two dev builds of
//! the same version differ only by their commit.
//!
//! This crate must not know about `gpui` (see the dependency direction in
//! CLAUDE.md): the updater and the UI read these values, the values do not
//! reach for the UI.

/// Where a build came from, and so where its updates come from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Channel {
    /// A `cargo build` on somebody's machine. Never polls for updates.
    Local,
    /// The moving channel: built from `main`, replaced often.
    Dev,
    /// The stable channel: built from a version tag.
    Public,
}

/// The channel string was none of `dev` and `public`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidChannel;

/// What a distributed build may set `MEERKAT_CHANNEL` to. `local` is
/// deliberately not accepted: local is what a build is when nothing was
/// set, never something to ask for — a CI job that asked for it by name
/// would ship a build that cannot update itself.
pub fn parse_channel(name: Option<&str>) -> Result<Channel, InvalidChannel> {
    match name {
        None => Ok(Channel::Local),
        Some("dev") => Ok(Channel::Dev),
        Some("public") => Ok(Channel::Public),
        Some(_) => Err(InvalidChannel),
    }
}

/// The channel this binary was built for.
pub fn channel() -> Channel {
    match parse_channel(option_env!("MEERKAT_CHANNEL")) {
        Ok(channel) => channel,
        Err(InvalidChannel) => panic!(
            "invalid MEERKAT_CHANNEL {:?}: use dev or public, or unset it for a local build",
            option_env!("MEERKAT_CHANNEL").unwrap_or_default()
        ),
    }
}

/// The version out of the workspace's Cargo.toml.
pub fn version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}

/// The commit this binary was built from, when CI said. Two dev builds of
/// the same version are told apart by nothing else.
pub fn commit_sha() -> Option<&'static str> {
    option_env!("MEERKAT_COMMIT_SHA")
}

impl Channel {
    /// Whether a build on this channel looks for updates at all.
    pub fn polls_for_updates(self) -> bool {
        !matches!(self, Channel::Local)
    }

    /// The name the feed, the CI and the UI all use for this channel.
    pub fn dev_name(self) -> &'static str {
        match self {
            Channel::Local => "local",
            Channel::Dev => "dev",
            Channel::Public => "public",
        }
    }

    /// What the app calls itself on this channel. The dev app is named
    /// apart so the two can sit in /Applications side by side.
    pub fn display_name(self) -> &'static str {
        match self {
            Channel::Local => "Meerkat (local)",
            Channel::Dev => "Meerkat Dev",
            Channel::Public => "Meerkat",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nothing_set_is_a_local_build() {
        assert_eq!(parse_channel(None), Ok(Channel::Local));
    }

    #[test]
    fn the_two_distributed_channels_parse() {
        assert_eq!(parse_channel(Some("dev")), Ok(Channel::Dev));
        assert_eq!(parse_channel(Some("public")), Ok(Channel::Public));
    }

    #[test]
    fn local_cannot_be_asked_for_by_name() {
        assert_eq!(parse_channel(Some("local")), Err(InvalidChannel));
    }

    #[test]
    fn anything_else_is_refused() {
        assert_eq!(parse_channel(Some("stable")), Err(InvalidChannel));
        assert_eq!(parse_channel(Some("")), Err(InvalidChannel));
        assert_eq!(parse_channel(Some("Dev")), Err(InvalidChannel));
    }

    #[test]
    fn only_local_stays_quiet() {
        assert!(!Channel::Local.polls_for_updates());
        assert!(Channel::Dev.polls_for_updates());
        assert!(Channel::Public.polls_for_updates());
    }
}
