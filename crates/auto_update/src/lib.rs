//! Self-update: poll the channel's feed, download what is newer, and lay
//! it over the running bundle, ready for a restart.
//!
//! One updater for the whole app, held as a global entity — two windows
//! must not race two rsyncs over one bundle. It exists only on a
//! distributed channel: a `local` build has nothing an update could
//! replace, so `init` returns `None` and the UI shows nothing.
//!
//! Nothing here restarts the app. The updater gets as far as `Ready` and
//! stops; the restart is the user's, from the pill in the UI, because an
//! app that relaunches itself under an open transaction would be making
//! the close dialog's decision without asking.
//!
//! The check and the install run on tokio through `gpui_tokio`, the same
//! bridge every database call uses, and replies carry a generation the
//! way a tab's queries do: a manual check racing the hourly one must not
//! apply twice.

mod install;
mod manifest;

pub use manifest::{Manifest, Update, update_available};

use anyhow::Result;
use gpui::{App, AppContext as _, Context, Entity, Global, Task};
use std::time::Duration;

/// The first check waits this out, so it never competes with the app
/// opening a connection.
const START_DELAY: Duration = Duration::from_secs(30);

/// Both channels poll hourly. The feed is a static file behind a CDN;
/// nothing is gained by asking faster.
const POLL_INTERVAL: Duration = Duration::from_secs(60 * 60);

#[derive(Debug, Clone, PartialEq)]
pub enum Status {
    Idle,
    Checking,
    /// Downloading and installing, one word: the UI says "updating…" for
    /// the whole of it, and the split earned nobody anything.
    Updating,
    /// Installed over the bundle; a restart picks it up.
    Ready {
        version: String,
        sha: Option<String>,
    },
    /// Only a manual check lands here. The hourly one fails quietly back
    /// to `Idle`, because offline is normal and not news.
    Errored {
        error: String,
    },
}

/// Which build a `Ready` status is offering. It is what a dismissal is
/// keyed by, so an update the user waved away stays waved away and the
/// *next* one announces itself.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Ready {
    pub version: String,
    pub sha: Option<String>,
}

pub struct AutoUpdater {
    status: Status,
    /// Bumped per check; a reply carrying an older value is dropped.
    generation: u64,
    /// The build whose toast the user closed. **In memory on purpose**:
    /// closing the toast says "not now", not "never" — a restart is the
    /// one thing that update was waiting for, so a run of the app that
    /// still has it pending says so again. Persisting it would turn one
    /// dismissal into silence for ever.
    dismissed: Option<Ready>,
    _poll: Task<()>,
}

struct GlobalAutoUpdater(Entity<AutoUpdater>);

impl Global for GlobalAutoUpdater {}

/// Build the updater and start the hourly loop — on a distributed
/// channel. On `Local` this does nothing and answers `None`.
pub fn init(cx: &mut App) -> Option<Entity<AutoUpdater>> {
    if !release_channel::channel().polls_for_updates() {
        return None;
    }
    let updater = cx.new(AutoUpdater::new);
    cx.set_global(GlobalAutoUpdater(updater.clone()));
    Some(updater)
}

impl AutoUpdater {
    /// The global updater, absent on a local build.
    pub fn try_global(cx: &App) -> Option<Entity<Self>> {
        cx.try_global::<GlobalAutoUpdater>()
            .map(|global| global.0.clone())
    }

    fn new(cx: &mut Context<Self>) -> Self {
        let poll = cx.spawn(async move |this, cx| {
            cx.background_executor().timer(START_DELAY).await;
            loop {
                let alive = this
                    .update(cx, |this, cx| this.check(Trigger::Automatic, cx))
                    .is_ok();
                if !alive {
                    return;
                }
                cx.background_executor().timer(POLL_INTERVAL).await;
            }
        });

        Self {
            status: Status::Idle,
            generation: 0,
            dismissed: None,
            _poll: poll,
        }
    }

    pub fn status(&self) -> &Status {
        &self.status
    }

    /// The update worth announcing, or `None`. It is `Some` only while an
    /// install is `Ready` **and** the user has not closed that one's
    /// toast: the footer's line still says so either way, because a line
    /// the user went looking for is not the same as one that arrives.
    pub fn announcement(&self) -> Option<Ready> {
        let Status::Ready { version, sha } = &self.status else {
            return None;
        };
        let ready = Ready {
            version: version.clone(),
            sha: sha.clone(),
        };
        announces(&ready, self.dismissed.as_ref()).then_some(ready)
    }

    /// Close the toast for whatever is ready now. A later install carries
    /// a different build, so it announces itself in its turn.
    pub fn dismiss(&mut self, cx: &mut Context<Self>) {
        let Status::Ready { version, sha } = &self.status else {
            return;
        };
        self.dismissed = Some(Ready {
            version: version.clone(),
            sha: sha.clone(),
        });
        cx.notify();
    }

    /// Start a check unless one is already under way. `Ready` also stays
    /// put: the update is on disk, and the only thing left is a restart.
    pub fn check(&mut self, trigger: Trigger, cx: &mut Context<Self>) {
        match self.status {
            Status::Checking | Status::Updating | Status::Ready { .. } => return,
            Status::Idle | Status::Errored { .. } => {}
        }

        let channel = release_channel::channel();
        self.generation += 1;
        let generation = self.generation;
        self.status = Status::Checking;
        cx.notify();

        let url = install::feed_url(channel);
        let task = gpui_tokio::Tokio::spawn(cx, async move {
            let manifest = install::fetch_manifest(&url).await?;
            manifest::update_available(
                channel,
                release_channel::version(),
                release_channel::commit_sha(),
                &manifest,
            )
        });

        cx.spawn(async move |this, cx| {
            let outcome = flatten(task.await);
            this.update(cx, |this, cx| {
                this.apply_check(generation, outcome, trigger, cx)
            })
            .ok();
        })
        .detach();
    }

    fn apply_check(
        &mut self,
        generation: u64,
        outcome: Result<Option<Update>, String>,
        trigger: Trigger,
        cx: &mut Context<Self>,
    ) {
        if generation != self.generation {
            return;
        }
        match outcome {
            Ok(Some(update)) => self.install(generation, update, trigger, cx),
            Ok(None) => {
                self.status = Status::Idle;
                cx.notify();
            }
            Err(error) => self.fail(error, trigger, cx),
        }
    }

    fn install(
        &mut self,
        generation: u64,
        update: Update,
        trigger: Trigger,
        cx: &mut Context<Self>,
    ) {
        let app_path = match cx.app_path() {
            Ok(path) => path,
            Err(error) => {
                self.fail(format!("no app path: {error}"), trigger, cx);
                return;
            }
        };

        self.status = Status::Updating;
        cx.notify();

        let version = update.version.clone();
        let sha = update.sha.clone();
        let task = gpui_tokio::Tokio::spawn(cx, async move {
            install::download_and_install(update, app_path).await
        });

        cx.spawn(async move |this, cx| {
            let outcome = flatten(task.await);
            this.update(cx, |this, cx| {
                if generation != this.generation {
                    return;
                }
                match outcome {
                    Ok(()) => this.status = Status::Ready { version, sha },
                    Err(error) => {
                        this.fail(error, trigger, cx);
                        return;
                    }
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
    }

    /// A manual check owes the user its error; the hourly one owes them
    /// quiet — offline is the ordinary case it fails in.
    fn fail(&mut self, error: String, trigger: Trigger, cx: &mut Context<Self>) {
        eprintln!("auto-update: {error}");
        self.status = match trigger {
            Trigger::Manual => Status::Errored { error },
            Trigger::Automatic => Status::Idle,
        };
        cx.notify();
    }
}

/// Whether a ready build is still worth a toast. A plain function over
/// the two, so the rule is argued with in a test rather than in a
/// running window: the same build stays closed, and anything else — a
/// new version, the same version at a new commit, or nothing dismissed
/// yet — is news.
pub fn announces(ready: &Ready, dismissed: Option<&Ready>) -> bool {
    dismissed != Some(ready)
}

/// Who asked for the check. Decides only what an error does.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Trigger {
    Automatic,
    Manual,
}

/// Collapse the JoinError and the inner error into one string for
/// display, the shape `shell.rs` uses for its own tokio replies.
fn flatten<T>(outcome: Result<Result<T>, impl std::fmt::Display>) -> Result<T, String> {
    match outcome {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(error)) => Err(format!("{error:#}")),
        Err(error) => Err(error.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ready(version: &str, sha: Option<&str>) -> Ready {
        Ready {
            version: version.to_string(),
            sha: sha.map(str::to_string),
        }
    }

    /// Closing the toast silences that one build and nothing else.
    #[test]
    fn a_dismissal_covers_one_build() {
        let one = ready("0.1.0", Some("2342a00"));

        assert!(announces(&one, None), "nothing dismissed yet");
        assert!(
            !announces(&one, Some(&one)),
            "this is the one that was closed"
        );

        // The dev channel rarely bumps the version, so the commit is what
        // says a second update landed. It gets its own toast.
        let next = ready("0.1.0", Some("beefcaf"));
        assert!(announces(&next, Some(&one)));

        // And the public channel moves by version.
        assert!(announces(
            &ready("0.2.0", None),
            Some(&ready("0.1.0", None))
        ));
    }
}
