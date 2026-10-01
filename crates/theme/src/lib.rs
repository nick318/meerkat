//! Color tokens for the whole app. Components take colors from here,
//! never from literals, so themes stay swappable (Zed `theme` crate pattern).
//!
//! Default theme: "warm paper", second edition. IBM Plex Sans for the
//! chrome, JetBrains Mono for everything the database said or the user
//! typed, hairline rules, one ochre accent. The first edition was the comp
//! at `docs/design/Meerkat.dc.html`; the second is the "Meerkat facelift"
//! canvas, direction A.
//!
//! **Text must read at 4.5:1 or better against the paper.** The first
//! edition's greys and its ochre carried row counts, captions and SQL
//! keywords at 2–3.6:1. So ink that says something is `text_muted` or
//! darker; `text_faint` is for marks that repeat what something else
//! already says. The accent is two tokens for the same reason: `accent`
//! fills and strokes, `accent_deep` is the accent as *text*.

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
    /// The one surface lifted off the paper: an active tab, a keycap, the
    /// lit chip of a segmented control, a floating card. White, so a raised
    /// thing reads as nearer than the paper under it.
    pub raised: Hsla,
    /// A recessed well: the track of a segmented control, the NULL chip in a
    /// cell. Darker than `panel`, so a thing sitting in it reads as set in.
    pub sunk: Hsla,
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
    /// The single ochre accent, for fills, strokes and icons. As text it is
    /// 3.6:1 on the paper, which is why `accent_deep` exists.
    pub accent: Hsla,
    /// The fill of the one primary button in a view (run, commit, connect,
    /// save). Deeper than `accent`, so `on_accent` reads on it at 5.2:1.
    pub accent_fill: Hsla,
    /// Ink on `accent_fill`.
    pub on_accent: Hsla,
    /// Deeper accent for emphasized values and hover. Also the SQL
    /// keyword colour in the query editor.
    pub accent_deep: Hsla,
    /// String and number literals in the query editor.
    pub syntax_literal: Hsla,
    /// Names the connected database actually has: schemas, tables, views
    /// and columns. Ink against the ochre keywords, so a query reads as
    /// commands in warm tones over the schema in cool ones.
    pub syntax_identifier: Hsla,
    /// Selected row / active list item background. In the results grid it
    /// is what a *ticked* row wears, whole.
    pub selection: Hsla,
    /// The wash under a range of cells in the results grid. Lighter than
    /// `selection`, because three marks have to stay apart at a glance in
    /// the same warm family: the cursor's own cell (`match_strong`), the
    /// range around it (this), and a ticked row (`selection`).
    pub range_surface: Hsla,
    /// The wash behind the part of a name the ⌘K palette matched. The only
    /// place in the app where text carries a background of its own.
    pub match_wash: Hsla,
    /// The same wash on the selected palette row, which already sits on
    /// `selection` and needs a deeper mark to stay visible.
    pub match_strong: Hsla,
    /// The same wash inside a palette row for a run that failed, so the
    /// mark stays warm against `error_surface`.
    pub match_error: Hsla,
    /// What the ⌘F line found, in the SQL editor and in the results grid.
    /// A butter yellow rather than another warm tan: the grid already
    /// carries three marks in the ochre family — the cursor, the range and
    /// a ticked row — and a hit has to read apart from all three at a
    /// glance. `find_hit` is every hit; `find_current` is the one the walk
    /// stands on, deep enough to be found again after a scroll.
    pub find_hit: Hsla,
    pub find_current: Hsla,
    /// The scrim the ⌘K palette lays over the workspace. Carries alpha:
    /// the screen behind it must stay readable.
    pub overlay: Hsla,
    /// The drop shadow under a floating surface (the palette). Carries
    /// alpha.
    pub shadow: Hsla,
    /// The shadow a raised thing *on* the paper casts — an active tab, the
    /// lit chip of a segmented control. A pixel of it, not a float.
    pub shadow_soft: Hsla,
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
    /// A caption on `running_surface`: the transaction bar's second line,
    /// which says what an open transaction holds. `text_muted` is grey and
    /// goes muddy over that warm wash, so the accent family needs a muted
    /// tone of its own — the only one in the app.
    pub accent_muted: Hsla,
    /// A keycap sitting *inside* a filled button (the run button's ⌘⏎).
    /// Both carry alpha, because the cap has to work over the ochre fill
    /// and over the clay one without a tone of its own for each.
    pub key_on_fill_surface: Hsla,
    pub key_on_fill_border: Hsla,
    /// A statement that changes the **shape** of the database — a
    /// `CREATE`, `ALTER`, `DROP` or `TRUNCATE` — and the report of what it
    /// changed. The family is a cool teal, on purpose: every other tone
    /// on the query screen is warm, and a shape change is the one event
    /// whose answer is not on screen afterwards, so it has to read apart
    /// from a run that merely worked. The comp calls it the DDL family.
    /// `ddl` is the ring, the chip and the running rail; `ddl_inner` the
    /// queued rail and the edges; `ddl_surface` the wash behind the
    /// statement's lines and the panel; `ddl_text` the ink on that wash;
    /// `ddl_muted` a caption on it; `ddl_gutter` and `ddl_number` the
    /// gutter's own tint and its line numbers; `ddl_done` the rail once
    /// the statement has landed.
    pub ddl: Hsla,
    pub ddl_inner: Hsla,
    pub ddl_surface: Hsla,
    pub ddl_text: Hsla,
    pub ddl_muted: Hsla,
    pub ddl_gutter: Hsla,
    pub ddl_number: Hsla,
    pub ddl_done: Hsla,
    /// A column or a table the schema report says appeared: the row's
    /// wash, the `+`, its type and the note beside it.
    pub delta_add_surface: Hsla,
    pub delta_add: Hsla,
    pub delta_add_type: Hsla,
    pub delta_add_text: Hsla,
    /// A column or a table the report says is gone. The `−` and the name
    /// borrow `env_prod` and `error`; only the wash and the type need
    /// tones of their own.
    pub delta_drop_surface: Hsla,
    pub delta_drop_type: Hsla,
    /// The statistic beside a statement that worked: a soft green, so the
    /// number reads as a note rather than a badge. `ok` is the tick.
    pub ok_muted: Hsla,
    /// Success / connected.
    pub ok: Hsla,
    /// The wash under a statement's own statistic, and under a green note.
    /// Its ink is `env_dev_text`.
    pub ok_surface: Hsla,
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
                panel: rgb(0xF5F3EE).into(),
                elevated: rgb(0xFBFAF7).into(),
                raised: rgb(0xFFFFFF).into(),
                sunk: rgb(0xECE9E2).into(),
                border: rgb(0xE3DED3).into(),
                border_strong: rgb(0xD9D3C7).into(),
                hairline: rgb(0xEEEAE2).into(),
                text: rgb(0x211F1B).into(),
                text_body: rgb(0x2E2B26).into(),
                text_secondary: rgb(0x55514A).into(),
                text_muted: rgb(0x6F6A60).into(),
                text_faint: rgb(0x958E82).into(),
                line_number: rgb(0xB3AC9F).into(),
                accent: rgb(0xB4762F).into(),
                accent_fill: rgb(0x9A5F1F).into(),
                on_accent: rgb(0xFFFFFF).into(),
                accent_deep: rgb(0x8A5519).into(),
                syntax_literal: rgb(0x5C7A4E).into(),
                syntax_identifier: rgb(0x3F5A6B).into(),
                selection: rgb(0xF3E7D3).into(),
                range_surface: rgb(0xF8F1E3).into(),
                match_wash: rgb(0xEFE3CC).into(),
                match_strong: rgb(0xEBD3A8).into(),
                match_error: rgb(0xF2DDD1).into(),
                find_hit: rgb(0xF6E8B1).into(),
                find_current: rgb(0xEBCB67).into(),
                overlay: rgba(0x34302A38).into(),
                shadow: rgba(0x211F1B66).into(),
                shadow_soft: rgba(0x211F1B1F).into(),
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
                accent_muted: rgb(0xA08544).into(),
                key_on_fill_surface: rgba(0xFFFFFF29).into(),
                key_on_fill_border: rgba(0xFFFFFF57).into(),
                ddl: rgb(0x4A7A6B).into(),
                ddl_inner: rgb(0xCFE0D6).into(),
                ddl_surface: rgb(0xF3F7F4).into(),
                ddl_text: rgb(0x3F7263).into(),
                ddl_muted: rgb(0x7E8C82).into(),
                ddl_gutter: rgb(0xEEF4F0).into(),
                ddl_number: rgb(0xA9BBB0).into(),
                ddl_done: rgb(0x6F9C8B).into(),
                delta_add_surface: rgb(0xE9F1E7).into(),
                delta_add: rgb(0x4E7A44).into(),
                delta_add_type: rgb(0x5C6B60).into(),
                delta_add_text: rgb(0x4E6152).into(),
                delta_drop_surface: rgb(0xFAEDE5).into(),
                delta_drop_type: rgb(0xA0765F).into(),
                ok_muted: rgb(0x4F7A40).into(),
                ok: rgb(0x4F7A40).into(),
                ok_surface: rgb(0xE8F1E3).into(),
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

/// The chrome's font: labels, buttons, titles, captions. Bundled in the
/// `meerkat` crate's assets.
pub const UI_FONT_FAMILY: &str = "IBM Plex Sans";

/// Everything the database said or the user typed: SQL, names, values,
/// counts, timings. Also bundled. The SQL editor and the grid measure
/// columns by a fixed advance, so they must set it themselves.
pub const MONO_FONT_FAMILY: &str = "JetBrains Mono";

impl gpui::Global for Theme {}

/// Read the current theme from the app context.
pub fn theme(cx: &gpui::App) -> &Theme {
    cx.global::<Theme>()
}
