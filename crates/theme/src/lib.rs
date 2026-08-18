//! Color tokens for the whole app. Components take colors from here,
//! never from literals, so themes stay swappable (Zed `theme` crate pattern).
//!
//! Default theme: "warm paper" light — JetBrains Mono throughout, hairline
//! 1px rules, single ochre accent. From the Meerkat design comp.

use gpui::{Hsla, rgb, rgba};

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
    /// Names the connected database actually has: schemas, tables, views
    /// and columns. Ink against the ochre keywords, so a query reads as
    /// commands in warm tones over the schema in cool ones.
    pub syntax_identifier: Hsla,
    /// Selected row / active list item background.
    pub selection: Hsla,
    /// The wash behind the part of a name the ⌘K palette matched. The only
    /// place in the app where text carries a background of its own.
    pub match_wash: Hsla,
    /// The same wash on the selected palette row, which already sits on
    /// `selection` and needs a deeper mark to stay visible.
    pub match_strong: Hsla,
    /// The same wash inside a palette row for a run that failed, so the
    /// mark stays warm against `error_surface`.
    pub match_error: Hsla,
    /// The scrim the ⌘K palette lays over the workspace. Carries alpha:
    /// the screen behind it must stay readable.
    pub overlay: Hsla,
    /// The drop shadow under a floating surface (the palette). Carries
    /// alpha; nothing else in the app is raised off the paper.
    pub shadow: Hsla,
    /// Environment tag dots and chips: prod warns in warm clay, staging
    /// holds the middle in gold, dev rests in green. The `_surface`
    /// partner is the selected chip's wash behind that dot.
    pub env_prod: Hsla,
    pub env_prod_surface: Hsla,
    pub env_staging: Hsla,
    pub env_staging_surface: Hsla,
    pub env_dev: Hsla,
    pub env_dev_surface: Hsla,
    /// Success / connected.
    pub ok: Hsla,
    /// A connection that is saved but not open: the sand dot.
    pub idle: Hsla,
    /// Error text.
    pub error: Hsla,
    /// Secondary text inside an error row: the reason, the retry link.
    pub error_secondary: Hsla,
    /// Muted meta text inside an error row ("last seen 3d ago").
    pub error_faint: Hsla,
    /// The dot marking a connection that is down.
    pub error_mark: Hsla,
    /// Error row/card background.
    pub error_surface: Hsla,
    /// Error border.
    pub error_border: Hsla,
    /// The meerkat mark: head, ears, eyes and muzzle. Only the mark uses
    /// these; nothing else in the app is drawn from them.
    pub mark_face: Hsla,
    pub mark_ears: Hsla,
    pub mark_ink: Hsla,
    pub mark_muzzle: Hsla,
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
                syntax_identifier: rgb(0x3F5A6B).into(),
                selection: rgb(0xF0E5D2).into(),
                match_wash: rgb(0xEFE3CC).into(),
                match_strong: rgb(0xE7CFA3).into(),
                match_error: rgb(0xF2DDD1).into(),
                overlay: rgba(0x34302A38).into(),
                shadow: rgba(0x211F1B66).into(),
                env_prod: rgb(0xA9694A).into(),
                env_prod_surface: rgb(0xF7EAE1).into(),
                env_staging: rgb(0xC9A44A).into(),
                env_staging_surface: rgb(0xF5EDD8).into(),
                env_dev: rgb(0x5C8A4E).into(),
                env_dev_surface: rgb(0xEBF1E6).into(),
                ok: rgb(0x5C8A4E).into(),
                idle: rgb(0xCFC8B8).into(),
                error: rgb(0x8E4A2A).into(),
                error_secondary: rgb(0xA9694A).into(),
                error_faint: rgb(0xC0A192).into(),
                error_mark: rgb(0xC08A6A).into(),
                error_surface: rgb(0xFCF7F4).into(),
                error_border: rgb(0xE4D3C9).into(),
                mark_face: rgb(0xC98B3E).into(),
                mark_ears: rgb(0xB4762F).into(),
                mark_ink: rgb(0x3B3325).into(),
                mark_muzzle: rgb(0x7A5522).into(),
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
