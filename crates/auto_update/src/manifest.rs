//! The update feed: one static JSON file per channel, and the rule that
//! reads it.
//!
//! The feed is a file on a CDN, never an endpoint — an endpoint could be
//! taught to log who asked, and the whole design promises nobody counts.
//! The client sends a bare GET with a plain user agent and nothing else.
//!
//! Two channels, two rules for "is that newer". The public channel moves
//! by version, so semver answers. The dev channel ships often from `main`
//! and rarely bumps the version, so two dev builds of one version are
//! told apart by their commit — the rule Zed's nightly channel uses.

use anyhow::{Context as _, Result};
use release_channel::Channel;
use semver::Version;
use serde::Deserialize;
use std::collections::HashMap;

/// One channel's feed. `sha` is what the dev rule compares; the public
/// rule never reads it.
#[derive(Debug, Clone, Deserialize)]
pub struct Manifest {
    pub version: String,
    #[serde(default)]
    pub sha: Option<String>,
    pub assets: HashMap<String, Asset>,
}

/// One downloadable build. The checksum is required: the archive crosses
/// a CDN the app does not run, so the app checks what arrived.
#[derive(Debug, Clone, Deserialize)]
pub struct Asset {
    pub url: String,
    pub sha256: String,
}

/// What a check decided to fetch.
#[derive(Debug, Clone, PartialEq)]
pub struct Update {
    pub version: String,
    pub sha: Option<String>,
    pub url: String,
    pub sha256: String,
}

/// The key this build looks itself up under in `assets` —
/// `macos-aarch64`, straight from the compiler's own names.
pub fn asset_key() -> String {
    format!("{}-{}", std::env::consts::OS, std::env::consts::ARCH)
}

/// Whether the manifest offers something newer than this build, and what
/// to download if so. `Ok(None)` says up to date — including "no build
/// for this platform", which is a gap in the feed and not an error the
/// user can act on.
pub fn update_available(
    channel: Channel,
    installed_version: &str,
    installed_sha: Option<&str>,
    manifest: &Manifest,
) -> Result<Option<Update>> {
    let newer = match channel {
        // A local build is never offered an update; the caller should not
        // have asked.
        Channel::Local => false,
        // Dev moves by commit. A build that does not know its own commit
        // takes the download: it cannot prove it is current.
        Channel::Dev => match (&manifest.sha, installed_sha) {
            (Some(fetched), Some(installed)) => fetched != installed,
            (Some(_), None) => true,
            // A dev feed without a commit cannot be compared by one;
            // fall back to the version.
            (None, _) => is_version_newer(installed_version, &manifest.version)?,
        },
        Channel::Public => is_version_newer(installed_version, &manifest.version)?,
    };

    if !newer {
        return Ok(None);
    }

    let Some(asset) = manifest.assets.get(&asset_key()) else {
        return Ok(None);
    };

    Ok(Some(Update {
        version: manifest.version.clone(),
        sha: manifest.sha.clone(),
        url: asset.url.clone(),
        sha256: asset.sha256.clone(),
    }))
}

fn is_version_newer(installed: &str, fetched: &str) -> Result<bool> {
    let installed: Version = installed
        .parse()
        .with_context(|| format!("invalid installed version {installed:?}"))?;
    let fetched: Version = fetched
        .parse()
        .with_context(|| format!("invalid version {fetched:?} in the feed"))?;
    Ok(fetched > installed)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest(version: &str, sha: Option<&str>) -> Manifest {
        let mut assets = HashMap::new();
        assets.insert(
            asset_key(),
            Asset { url: "https://example.test/a.tar.gz".into(), sha256: "aa".into() },
        );
        Manifest { version: version.into(), sha: sha.map(Into::into), assets }
    }

    #[test]
    fn public_moves_by_version() {
        let m = manifest("0.2.0", None);
        assert!(update_available(Channel::Public, "0.1.0", None, &m).unwrap().is_some());
        assert!(update_available(Channel::Public, "0.2.0", None, &m).unwrap().is_none());
        assert!(update_available(Channel::Public, "0.3.0", None, &m).unwrap().is_none());
    }

    #[test]
    fn dev_moves_by_commit_even_on_the_same_version() {
        let m = manifest("0.1.0", Some("bbb2222"));
        assert!(update_available(Channel::Dev, "0.1.0", Some("aaa1111"), &m).unwrap().is_some());
        assert!(update_available(Channel::Dev, "0.1.0", Some("bbb2222"), &m).unwrap().is_none());
    }

    #[test]
    fn a_dev_build_that_does_not_know_its_commit_takes_the_download() {
        let m = manifest("0.1.0", Some("bbb2222"));
        assert!(update_available(Channel::Dev, "0.1.0", None, &m).unwrap().is_some());
    }

    #[test]
    fn a_dev_feed_without_a_commit_falls_back_to_the_version() {
        let m = manifest("0.2.0", None);
        assert!(update_available(Channel::Dev, "0.1.0", Some("aaa1111"), &m).unwrap().is_some());
        let m = manifest("0.1.0", None);
        assert!(update_available(Channel::Dev, "0.1.0", Some("aaa1111"), &m).unwrap().is_none());
    }

    #[test]
    fn no_asset_for_this_platform_is_up_to_date_not_an_error() {
        let m = Manifest {
            version: "9.9.9".into(),
            sha: None,
            assets: HashMap::new(),
        };
        assert!(update_available(Channel::Public, "0.1.0", None, &m).unwrap().is_none());
    }

    #[test]
    fn a_local_build_is_never_offered_anything() {
        let m = manifest("9.9.9", Some("bbb2222"));
        assert!(update_available(Channel::Local, "0.1.0", None, &m).unwrap().is_none());
    }

    #[test]
    fn garbage_versions_are_errors_not_updates() {
        let m = manifest("not-a-version", None);
        assert!(update_available(Channel::Public, "0.1.0", None, &m).is_err());
    }

    #[test]
    fn the_feed_parses() {
        let json = r#"{
            "version": "0.2.0",
            "sha": "abc1234",
            "assets": {
                "macos-aarch64": { "url": "https://example.test/m.tar.gz", "sha256": "deadbeef" }
            }
        }"#;
        let m: Manifest = serde_json::from_str(json).unwrap();
        assert_eq!(m.version, "0.2.0");
        assert_eq!(m.sha.as_deref(), Some("abc1234"));
        assert!(m.assets.contains_key("macos-aarch64"));
    }
}
