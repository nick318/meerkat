//! The window's root: the connections screen, or a workspace open on one
//! database.
//!
//! Only one of the two is alive at a time. Leaving a workspace drops it,
//! and with it the connection pool behind it, so a closed database keeps
//! no sockets open.

use crate::connections::{Connections, ConnectionsEvent};
use crate::shell::{Close, Shell, ShellEvent, StopTask, Target};
use gpui::{
    App, Context, Entity, FocusHandle, Focusable, KeyBinding, Subscription, Window, actions, div,
    prelude::*,
};

actions!(root, [CloseWindow]);

/// The context ⌘W falls back to. It sits *outside* the shell's, so a window
/// with a tab strip answers ⌘W in the strip — GPUI gives a keystroke to the
/// binding that matched deepest — and a window with no strip at all answers
/// it here.
pub const KEY_CONTEXT: &str = "Root";

pub fn key_bindings() -> Vec<KeyBinding> {
    vec![KeyBinding::new("cmd-w", CloseWindow, Some(KEY_CONTEXT))]
}

pub struct Root {
    screen: Screen,
    /// Dropped with the screen it belongs to.
    _subscription: Subscription,
}

enum Screen {
    Connections(Entity<Connections>),
    Workspace(Entity<Shell>),
}

impl Root {
    /// Open on a database when the command line named one, and on the
    /// connections screen otherwise.
    pub fn new(target: Option<Target>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        match target {
            Some(target) => Self::workspace(target, window, cx),
            None => Self::connections(window, cx),
        }
    }

    fn connections(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let screen = cx.new(|cx| Connections::new(window, cx));
        let subscription = cx.subscribe_in(&screen, window, Self::on_connections_event);
        window.focus(&screen.focus_handle(cx), cx);
        Self { screen: Screen::Connections(screen), _subscription: subscription }
    }

    fn workspace(target: Target, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let workspace = cx.new(|cx| Shell::new(target, window, cx));
        let subscription = cx.subscribe_in(&workspace, window, Self::on_shell_event);
        window.focus(&workspace.focus_handle(cx), cx);
        Self { screen: Screen::Workspace(workspace), _subscription: subscription }
    }

    /// Write an open workspace's tabs back to the local file. The window
    /// closing and ⌘Q both drop the shell without going through the
    /// "‹ connections" way out, so they call this first.
    pub fn remember(&self, cx: &App) {
        if let Screen::Workspace(workspace) = &self.screen {
            workspace.read(cx).remember_tabs(cx);
        }
    }

    /// Ask the workspace whether this **window** may close. `false` means
    /// it put a question on screen instead, and closes the window itself if
    /// the user says so — the close button ends every tab of this window at
    /// once, so it may not skip the guard the tab strip goes through.
    pub fn guard_close_window(&self, window: &mut Window, cx: &mut Context<Self>) -> bool {
        self.guard(Close::Window, window, cx)
    }

    /// Ask the workspace whether the **app** may go. ⌘Q ends every window,
    /// so each one is asked in turn; `false` means this one is asking the
    /// user, and the quit carries on from there.
    pub fn guard_quit(&self, window: &mut Window, cx: &mut Context<Self>) -> bool {
        self.guard(Close::Quit, window, cx)
    }

    /// ⌘W with nothing on screen to close. The connections screen holds no
    /// tabs, so the gesture that would take one takes the **window** — the
    /// same reading as ⌘W on the last tab of a strip, and the reading a
    /// browser gives a window with one tab left.
    fn on_close_window(&mut self, _: &CloseWindow, window: &mut Window, cx: &mut Context<Self>) {
        if !self.guard_close_window(window, cx) {
            return;
        }
        self.remember(cx);
        window.remove_window();
    }

    fn guard(&self, what: Close, window: &mut Window, cx: &mut Context<Self>) -> bool {
        match &self.screen {
            Screen::Workspace(workspace) => {
                workspace.update(cx, |shell, cx| shell.guard_close(what, window, cx))
            }
            // The connections screen holds no runs.
            Screen::Connections(_) => true,
        }
    }

    /// Forget that this window agreed to a quit. The quit was refused at
    /// another window's dialog, so nothing was agreed to after all.
    pub fn forget_quit(&self, cx: &mut Context<Self>) {
        if let Screen::Workspace(workspace) = &self.screen {
            workspace.update(cx, |shell, _cx| shell.forget_quit());
        }
    }

    /// Ask the server to give up every run in this window, on the way to a
    /// quit. The requests are handed back rather than detached, because
    /// quitting drops the tokio runtime and a request that has not left yet
    /// never leaves.
    pub fn stop_runs(&self, cx: &mut Context<Self>) -> Vec<StopTask> {
        match &self.screen {
            Screen::Workspace(workspace) => {
                workspace.update(cx, |shell, cx| shell.cancel_runs(Close::Quit, cx))
            }
            Screen::Connections(_) => Vec::new(),
        }
    }

    fn on_connections_event(
        &mut self,
        _screen: &Entity<Connections>,
        event: &ConnectionsEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let ConnectionsEvent::Open(profile) = event;
        *self = Self::workspace(Target::Profile(profile.clone()), window, cx);
        cx.notify();
    }

    fn on_shell_event(
        &mut self,
        _workspace: &Entity<Shell>,
        event: &ShellEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            ShellEvent::Close => {
                *self = Self::connections(window, cx);
                cx.notify();
            }
            // The window the user answered has agreed; the others have not
            // been asked. So the quit starts again and walks the whole list,
            // and this window now answers yes without asking twice.
            //
            // It runs on the next tick rather than from here: the quit
            // updates every window, this one included, and this callback is
            // already inside that update.
            ShellEvent::Quit => cx
                .spawn(async move |_, cx| {
                    cx.update(|cx| crate::quit(cx));
                })
                .detach(),
            // Same reason for the next tick: the walk updates every window,
            // and this callback is inside one of those updates.
            ShellEvent::QuitCancelled => cx
                .spawn(async move |_, cx| {
                    cx.update(|cx| crate::quit_cancelled(cx));
                })
                .detach(),
        }
    }
}

impl Focusable for Root {
    fn focus_handle(&self, cx: &App) -> FocusHandle {
        match &self.screen {
            Screen::Connections(screen) => screen.focus_handle(cx),
            Screen::Workspace(workspace) => workspace.focus_handle(cx),
        }
    }
}

impl Render for Root {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .key_context(KEY_CONTEXT)
            .on_action(cx.listener(Self::on_close_window))
            .size_full()
            .child(match &self.screen {
                Screen::Connections(screen) => screen.clone().into_any_element(),
                Screen::Workspace(workspace) => workspace.clone().into_any_element(),
            })
    }
}
