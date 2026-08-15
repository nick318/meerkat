//! The window's root: the connections screen, or a workspace open on one
//! database.
//!
//! Only one of the two is alive at a time. Leaving a workspace drops it,
//! and with it the connection pool behind it, so a closed database keeps
//! no sockets open.

use crate::connections::{Connections, ConnectionsEvent};
use crate::shell::{Shell, ShellEvent, Target};
use gpui::{
    App, Context, Entity, FocusHandle, Focusable, Subscription, Window, div, prelude::*,
};

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
        let screen = cx.new(|cx| Connections::new(cx));
        let subscription = cx.subscribe_in(&screen, window, Self::on_connections_event);
        window.focus(&screen.focus_handle(cx), cx);
        Self { screen: Screen::Connections(screen), _subscription: subscription }
    }

    fn workspace(target: Target, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let workspace = cx.new(|cx| Shell::new(target, cx));
        let subscription = cx.subscribe_in(&workspace, window, Self::on_shell_event);
        window.focus(&workspace.focus_handle(cx), cx);
        Self { screen: Screen::Workspace(workspace), _subscription: subscription }
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
        let ShellEvent::Close = event;
        *self = Self::connections(window, cx);
        cx.notify();
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
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        div().size_full().child(match &self.screen {
            Screen::Connections(screen) => screen.clone().into_any_element(),
            Screen::Workspace(workspace) => workspace.clone().into_any_element(),
        })
    }
}
