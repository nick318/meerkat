//! Color tokens for the whole app. Components take colors from here,
//! never from literals, so themes stay swappable (Zed `theme` crate pattern).
//!
//! Default theme: "warm paper" light — JetBrains Mono throughout, hairline
//! 1px rules, single ochre accent. From the Meerkat design comp.

use gpui::{Hsla, rgb};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Appearance {
    Light,
    Dark,
}

#[derive(Debug, Clone)]
pub struct Theme {
    pub appearance: Appearance,
    pub colors: ThemeColors,
}

#[derive(Debug, Clone)]
pub struct ThemeColors {
    /// Desktop behind the window content (outermost ground).
    pub canvas: Hsla,
    /// Main window surface.
    pub window: Hsla,
    /// Sidebar, toolbars, table headers.
    pub panel: Hsla,
    /// Cards and inputs sitting on a panel.
    pub elevated: Hsla,
    /// Standard border.
    pub border: Hsla,
    /// Stronger border (grid header rules, window edge).
    pub border_strong: Hsla,
    /// Faintest rule between data rows.
    pub hairline: Hsla,
    /// Headings and primary values.
    pub text: Hsla,
    /// Body/data text.
    pub text_body: Hsla,
    /// Secondary text (inactive tabs, buttons).
    pub text_secondary: Hsla,
    /// Muted captions and meta text.
    pub text_muted: Hsla,
    /// Faintest text: NULL cells, disabled, hints.
    pub text_faint: Hsla,
    /// Line numbers in the query editor gutter. Sits between `text_faint`
    /// and the rules, so the numbers recede behind the SQL.
    pub line_number: Hsla,
    /// The single ochre accent.
    pub accent: Hsla,
    /// Deeper accent for emphasized values and hover. Also the SQL
    /// keyword colour in the query editor.
    pub accent_deep: Hsla,
    /// String and number literals in the query editor.
    pub syntax_literal: Hsla,
    /// Selected row / active list item background.
    pub selection: Hsla,
    /// Success / connected.
    pub ok: Hsla,
    /// Error text.
    pub error: Hsla,
    /// Error row/card background.
    pub error_surface: Hsla,
    /// Error border.
    pub error_border: Hsla,
}

impl Theme {
    pub fn warm_paper() -> Self {
        Self {
            appearance: Appearance::Light,
            colors: ThemeColors {
                canvas: rgb(0xEDEAE3).into(),
                window: rgb(0xFBFAF7).into(),
                panel: rgb(0xF7F5F0).into(),
                elevated: rgb(0xFBFAF7).into(),
                border: rgb(0xEAE6DC).into(),
                border_strong: rgb(0xE5E1D8).into(),
                hairline: rgb(0xF1EFE8).into(),
                text: rgb(0x211F1B).into(),
                text_body: rgb(0x2E2B26).into(),
                text_secondary: rgb(0x55514A).into(),
                text_muted: rgb(0x8A857C).into(),
                text_faint: rgb(0xB0AAA0).into(),
                line_number: rgb(0xC6C0B4).into(),
                accent: rgb(0xB4762F).into(),
                accent_deep: rgb(0x8E5A1E).into(),
                syntax_literal: rgb(0x5C7A4E).into(),
                selection: rgb(0xF0E5D2).into(),
                ok: rgb(0x5C8A4E).into(),
                error: rgb(0x8E4A2A).into(),
                error_surface: rgb(0xFCF7F4).into(),
                error_border: rgb(0xE4D3C9).into(),
            },
        }
    }
}

/// UI font for the whole app; bundled in the `meerkat` crate assets.
pub const FONT_FAMILY: &str = "JetBrains Mono";

impl gpui::Global for Theme {}

/// Read the current theme from the app context.
pub fn theme(cx: &gpui::App) -> &Theme {
    cx.global::<Theme>()
}
