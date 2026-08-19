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
    /// Environment tags: prod warns in clay, staging holds the middle in
    /// sand, dev rests in green. Each carries four tones from the comp:
    /// the ring (dots, badges, the window frame), the inner hairline just
    /// inside that frame, the surface wash (selected chip, framed top
    /// bar), and the text that sits on the wash.
    pub env_prod: Hsla,
    pub env_prod_inner: Hsla,
    pub env_prod_surface: Hsla,
    pub env_prod_text: Hsla,
    pub env_staging: Hsla,
    pub env_staging_inner: Hsla,
    pub env_staging_surface: Hsla,
    pub env_staging_text: Hsla,
    pub env_dev: Hsla,
    pub env_dev_inner: Hsla,
    pub env_dev_surface: Hsla,
    pub env_dev_text: Hsla,
    /// The read-only mark on a session that has no server to be read-only
    /// against: a grey badge, drained of the green a live read-only
    /// session wears. Read-only and read-write themselves borrow the dev
    /// and prod families, so only this third state needs tones of its own.
    pub mode_off_surface: Hsla,
    pub mode_off_border: Hsla,
    pub mode_off_text: Hsla,
    /// The run timer while a statement is in flight: a warm pill with a
    /// dot, in the accent's family rather than a warning's. A query that
    /// is merely slow is not yet a problem. Its text is `accent_deep`.
    pub running_surface: Hsla,
    pub running_border: Hsla,
    pub running_mark: Hsla,
    /// A keycap sitting *inside* a filled button (the run button's ⌘⏎).
    /// Both carry alpha, because the cap has to work over the ochre fill
    /// and over the clay one without a tone of its own for each.
    pub key_on_fill_surface: Hsla,
    pub key_on_fill_border: Hsla,
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
                env_prod: rgb(0xB4552A).into(),
                env_prod_inner: rgb(0xE8C6B2).into(),
                env_prod_surface: rgb(0xFBEFE8).into(),
                env_prod_text: rgb(0x8E4A2A).into(),
                env_staging: rgb(0xC79B2E).into(),
                env_staging_inner: rgb(0xEBDCAE).into(),
                env_staging_surface: rgb(0xFBF6E7).into(),
                env_staging_text: rgb(0x7A6420).into(),
                env_dev: rgb(0x5C8A4E).into(),
                env_dev_inner: rgb(0xCBDEC2).into(),
                env_dev_surface: rgb(0xF1F7EE).into(),
                env_dev_text: rgb(0x3F6032).into(),
                mode_off_surface: rgb(0xF2F0EA).into(),
                mode_off_border: rgb(0xE2DDD2).into(),
                mode_off_text: rgb(0xA59F93).into(),
                running_surface: rgb(0xFDF8EE).into(),
                running_border: rgb(0xDCCFB4).into(),
                running_mark: rgb(0xC98B3E).into(),
                key_on_fill_surface: rgba(0xFFFFFF29).into(),
                key_on_fill_border: rgba(0xFFFFFF57).into(),
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
