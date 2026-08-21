//! Fetch and install one update, on the tokio side.
//!
//! Everything here is plain async Rust with no `gpui` in sight, so it can
//! run inside `gpui_tokio::Tokio::spawn` the way every database call does.
//!
//! The install is Zed's: extract the new `.app` next to the download, then
//! `rsync -a --delete` its contents over the running bundle. Overwriting a
//! running binary is fine on macOS — the old executable lives on unlinked
//! until the process exits — and rsync in place means there is never a
//! moment with no app on disk, which a remove-and-rename would have.
//!
//! The archive is checked against the feed's sha256 before anything is
//! unpacked: it crosses a CDN the app does not run.

use crate::manifest::{Manifest, Update};
use anyhow::{Context as _, Result, bail, ensure};
use sha2::{Digest as _, Sha256};
use std::path::{Path, PathBuf};

/// The one HTTP client. A bare GET with a plain user agent — no ids, no
/// query, no cookies: nothing in a request says who is asking.
fn http() -> Result<reqwest::Client> {
    Ok(reqwest::Client::builder()
        .user_agent(format!("meerkat-updater/{}", release_channel::version()))
        .build()?)
}

/// Where the feed lives. `MEERKAT_UPDATE_URL` overrides it, which is how
/// an update is tested end to end against a local file server.
pub fn feed_url(channel: release_channel::Channel) -> String {
    match std::env::var("MEERKAT_UPDATE_URL") {
        Ok(url) => url,
        Err(_) => format!(
            "https://raw.githubusercontent.com/nick318/meerkat/updates/{}.json",
            channel.dev_name()
        ),
    }
}

pub async fn fetch_manifest(url: &str) -> Result<Manifest> {
    let response = http()?.get(url).send().await?;
    ensure!(
        response.status().is_success(),
        "the feed answered {} for {url}",
        response.status()
    );
    let body = response.bytes().await?;
    serde_json::from_slice(&body).context("the feed did not parse as a manifest")
}

/// Download the archive, verify it, unpack it, and rsync the new bundle
/// over `app_path` — the running `.app`.
pub async fn download_and_install(update: Update, app_path: PathBuf) -> Result<()> {
    ensure!(
        app_path.extension().is_some_and(|e| e == "app"),
        "not running from an .app bundle ({}), nothing to update",
        app_path.display()
    );

    let staging = tempfile::Builder::new()
        .prefix("meerkat-update")
        .tempdir()
        .context("could not create a staging directory")?;

    let archive = staging.path().join("update.tar.gz");
    download_verified(&update, &archive).await?;

    let extracted = staging.path().join("extracted");
    tokio::fs::create_dir(&extracted).await?;
    run(
        tokio::process::Command::new("tar")
            .arg("-xzf")
            .arg(&archive)
            .arg("-C")
            .arg(&extracted),
        "tar",
    )
    .await?;

    let new_app = single_app_in(&extracted).await?;

    // Trailing slash on the source: rsync copies the *contents* of the
    // new bundle into the running one, whatever either is named.
    let mut source = new_app.into_os_string();
    source.push("/");
    run(
        tokio::process::Command::new("rsync")
            .arg("-a")
            .arg("--delete")
            .arg(&source)
            .arg(&app_path),
        "rsync",
    )
    .await?;

    Ok(())
}

async fn download_verified(update: &Update, target: &Path) -> Result<()> {
    let response = http()?.get(&update.url).send().await?;
    ensure!(
        response.status().is_success(),
        "the download answered {} for {}",
        response.status(),
        update.url
    );
    let body = response.bytes().await?;

    let digest = format!("{:x}", Sha256::digest(&body));
    ensure!(
        digest.eq_ignore_ascii_case(&update.sha256),
        "checksum mismatch: the feed says {}, the download is {digest}",
        update.sha256
    );

    tokio::fs::write(target, &body).await?;
    Ok(())
}

/// The archive holds exactly one `.app`; its name changes with the
/// channel, so it is found rather than assumed.
async fn single_app_in(dir: &Path) -> Result<PathBuf> {
    let mut apps = Vec::new();
    let mut entries = tokio::fs::read_dir(dir).await?;
    while let Some(entry) = entries.next_entry().await? {
        if entry.path().extension().is_some_and(|e| e == "app") {
            apps.push(entry.path());
        }
    }
    match apps.as_slice() {
        [app] => Ok(app.clone()),
        [] => bail!("the update archive held no .app"),
        _ => bail!("the update archive held more than one .app"),
    }
}

async fn run(command: &mut tokio::process::Command, name: &str) -> Result<()> {
    let output = command
        .output()
        .await
        .with_context(|| format!("could not run {name}"))?;
    ensure!(
        output.status.success(),
        "{name} failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    Ok(())
}
