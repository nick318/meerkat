// Package theme holds the color tokens for the whole app. Components take
// colors from here, never from literals, so themes stay swappable (Zed
// `theme` crate pattern).
//
// Default theme: "warm paper", second edition. IBM Plex Sans for the
// chrome, JetBrains Mono for everything the database said or the user
// typed, hairline rules, one ochre accent. The first edition was the comp
// at `docs/design/Meerkat.dc.html`; the second is the "Meerkat facelift"
// canvas, direction A.
//
// **Text must read at 4.5:1 or better against the paper.** The first
// edition's greys and its ochre carried row counts, captions and SQL
// keywords at 2–3.6:1. So ink that says something is `TextMuted` or
// darker; `TextFaint` is for marks that repeat what something else
// already says. The accent is two tokens for the same reason: `Accent`
// fills and strokes, `AccentDeep` is the accent as *text*.
//
// Go port: the tokens stay GPUI's `Hsla`, converted from the same hex
// values by GPUI's own arithmetic (see color.go), because the run button's
// breath and the pulses mix them with `Hsla.Blend` and `Hsla.Opacity`.
// `Hsla.Color` turns a token into mygo's `ui.Color` where it is painted.
package theme

import "sync"

// Appearance is whether the theme is light or dark.
type Appearance int

const (
	Light Appearance = iota
	Dark
)

// String names the appearance as Rust's Debug did.
func (a Appearance) String() string {
	if a == Dark {
		return "Dark"
	}
	return "Light"
}

// Theme is the app's look: its appearance and its color tokens.
type Theme struct {
	Appearance Appearance
	Colors     ThemeColors
}

// ThemeColors are the color tokens.
type ThemeColors struct {
	// Desktop behind the window content (outermost ground).
	Canvas Hsla
	// Main window surface.
	Window Hsla
	// Sidebar, toolbars, table headers.
	Panel Hsla
	// Cards and inputs sitting on a panel.
	Elevated Hsla
	// The one surface lifted off the paper: an active tab, a keycap, the
	// lit chip of a segmented control, a floating card. White, so a raised
	// thing reads as nearer than the paper under it.
	Raised Hsla
	// A recessed well: the track of a segmented control, the NULL chip in a
	// cell. Darker than `Panel`, so a thing sitting in it reads as set in.
	Sunk Hsla
	// Standard border.
	Border Hsla
	// Stronger border (grid header rules, window edge).
	BorderStrong Hsla
	// Faintest rule between data rows.
	Hairline Hsla
	// Headings and primary values.
	Text Hsla
	// Body/data text.
	TextBody Hsla
	// Secondary text (inactive tabs, buttons).
	TextSecondary Hsla
	// Muted captions and meta text.
	TextMuted Hsla
	// Faintest text: NULL cells, disabled, hints.
	TextFaint Hsla
	// Line numbers in the query editor gutter. Sits between `TextFaint`
	// and the rules, so the numbers recede behind the SQL.
	LineNumber Hsla
	// The single ochre accent, for fills, strokes and icons. As text it is
	// 3.6:1 on the paper, which is why `AccentDeep` exists.
	Accent Hsla
	// The fill of the one primary button in a view (run, commit, connect,
	// save). Deeper than `Accent`, so `OnAccent` reads on it at 5.2:1.
	AccentFill Hsla
	// Ink on `AccentFill`.
	OnAccent Hsla
	// Deeper accent for emphasized values and hover. Also the SQL
	// keyword colour in the query editor.
	AccentDeep Hsla
	// String and number literals in the query editor.
	SyntaxLiteral Hsla
	// Names the connected database actually has: schemas, tables, views
	// and columns. Ink against the ochre keywords, so a query reads as
	// commands in warm tones over the schema in cool ones.
	SyntaxIdentifier Hsla
	// Selected row / active list item background. In the results grid it
	// is what a *ticked* row wears, whole.
	Selection Hsla
	// The wash behind the line the SQL editor's caret is on. A shade off
	// the paper and no more: it says which line, and must not compete
	// with the selection or a statement's own wash.
	ActiveLine Hsla
	// The wash under a range of cells in the results grid. Lighter than
	// `Selection`, because three marks have to stay apart at a glance in
	// the same warm family: the cursor's own cell (`MatchStrong`), the
	// range around it (this), and a ticked row (`Selection`).
	RangeSurface Hsla
	// The wash behind the part of a name the ⌘K palette matched. The only
	// place in the app where text carries a background of its own.
	MatchWash Hsla
	// The same wash on the selected palette row, which already sits on
	// `Selection` and needs a deeper mark to stay visible.
	MatchStrong Hsla
	// The same wash inside a palette row for a run that failed, so the
	// mark stays warm against `ErrorSurface`.
	MatchError Hsla
	// What the ⌘F line found, in the SQL editor and in the results grid.
	// A butter yellow rather than another warm tan: the grid already
	// carries three marks in the ochre family — the cursor, the range and
	// a ticked row — and a hit has to read apart from all three at a
	// glance. `FindHit` is every hit; `FindCurrent` is the one the walk
	// stands on, deep enough to be found again after a scroll.
	FindHit     Hsla
	FindCurrent Hsla
	// The scrim the ⌘K palette lays over the workspace. Carries alpha:
	// the screen behind it must stay readable.
	Overlay Hsla
	// The drop shadow under a floating surface (the palette). Carries
	// alpha.
	Shadow Hsla
	// The shadow a raised thing *on* the paper casts — an active tab, the
	// lit chip of a segmented control. A pixel of it, not a float.
	ShadowSoft Hsla
	// Environment tags: prod warns in clay, staging holds the middle in
	// sand, dev rests in green. Each carries four tones from the comp:
	// the ring (dots, badges, the window frame), the inner hairline just
	// inside that frame, the surface wash (selected chip, framed top
	// bar), and the text that sits on the wash.
	EnvProd           Hsla
	EnvProdInner      Hsla
	EnvProdSurface    Hsla
	EnvProdText       Hsla
	EnvStaging        Hsla
	EnvStagingInner   Hsla
	EnvStagingSurface Hsla
	EnvStagingText    Hsla
	EnvDev            Hsla
	EnvDevInner       Hsla
	EnvDevSurface     Hsla
	EnvDevText        Hsla
	// The read-only mark on a session that has no server to be read-only
	// against: a grey badge, drained of the green a live read-only
	// session wears. Read-only and read-write themselves borrow the dev
	// and prod families, so only this third state needs tones of its own.
	ModeOffSurface Hsla
	ModeOffBorder  Hsla
	ModeOffText    Hsla
	// The run timer while a statement is in flight: a warm pill with a
	// dot, in the accent's family rather than a warning's. A query that
	// is merely slow is not yet a problem. Its text is `AccentDeep`.
	RunningSurface Hsla
	RunningBorder  Hsla
	RunningMark    Hsla
	// A caption on `RunningSurface`: the transaction bar's second line,
	// which says what an open transaction holds. `TextMuted` is grey and
	// goes muddy over that warm wash, so the accent family needs a muted
	// tone of its own — the only one in the app.
	AccentMuted Hsla
	// A keycap sitting *inside* a filled button (the run button's ⌘⏎).
	// Both carry alpha, because the cap has to work over the ochre fill
	// and over the clay one without a tone of its own for each.
	KeyOnFillSurface Hsla
	KeyOnFillBorder  Hsla
	// A statement that changes the **shape** of the database — a
	// `CREATE`, `ALTER`, `DROP` or `TRUNCATE` — and the report of what it
	// changed. The family is a cool teal, on purpose: every other tone
	// on the query screen is warm, and a shape change is the one event
	// whose answer is not on screen afterwards, so it has to read apart
	// from a run that merely worked. The comp calls it the DDL family.
	// `Ddl` is the ring, the chip and the running rail; `DdlInner` the
	// queued rail and the edges; `DdlSurface` the wash behind the
	// statement's lines and the panel; `DdlText` the ink on that wash;
	// `DdlMuted` a caption on it; `DdlGutter` and `DdlNumber` the
	// gutter's own tint and its line numbers; `DdlDone` the rail once
	// the statement has landed.
	Ddl        Hsla
	DdlInner   Hsla
	DdlSurface Hsla
	DdlText    Hsla
	DdlMuted   Hsla
	DdlGutter  Hsla
	DdlNumber  Hsla
	DdlDone    Hsla
	// A column or a table the schema report says appeared: the row's
	// wash, the `+`, its type and the note beside it.
	DeltaAddSurface Hsla
	DeltaAdd        Hsla
	DeltaAddType    Hsla
	DeltaAddText    Hsla
	// A column or a table the report says is gone. The `−` and the name
	// borrow `EnvProd` and `Error`; only the wash and the type need
	// tones of their own.
	DeltaDropSurface Hsla
	DeltaDropType    Hsla
	// The statistic beside a statement that worked: a soft green, so the
	// number reads as a note rather than a badge. `Ok` is the tick.
	OkMuted Hsla
	// Success / connected.
	Ok Hsla
	// The wash under a statement's own statistic, and under a green note.
	// Its ink is `EnvDevText`.
	OkSurface Hsla
	// A connection that is saved but not open: the sand dot.
	Idle Hsla
	// Error text.
	Error Hsla
	// Secondary text inside an error row: the reason, the retry link.
	ErrorSecondary Hsla
	// Muted meta text inside an error row ("last seen 3d ago").
	ErrorFaint Hsla
	// The dot marking a connection that is down.
	ErrorMark Hsla
	// Error row/card background.
	ErrorSurface Hsla
	// Error border.
	ErrorBorder Hsla
	// The meerkat mark: head, ears, eyes and muzzle. Only the mark uses
	// these; nothing else in the app is drawn from them.
	MarkFace   Hsla
	MarkEars   Hsla
	MarkInk    Hsla
	MarkMuzzle Hsla
}

// WarmPaper is the default theme.
func WarmPaper() Theme {
	return Theme{
		Appearance: Light,
		Colors: ThemeColors{
			Canvas:            rgb(0xEDEAE3).ToHsla(),
			Window:            rgb(0xFBFAF7).ToHsla(),
			Panel:             rgb(0xF5F3EE).ToHsla(),
			Elevated:          rgb(0xFBFAF7).ToHsla(),
			Raised:            rgb(0xFFFFFF).ToHsla(),
			Sunk:              rgb(0xECE9E2).ToHsla(),
			Border:            rgb(0xE3DED3).ToHsla(),
			BorderStrong:      rgb(0xD9D3C7).ToHsla(),
			Hairline:          rgb(0xEEEAE2).ToHsla(),
			Text:              rgb(0x211F1B).ToHsla(),
			TextBody:          rgb(0x2E2B26).ToHsla(),
			TextSecondary:     rgb(0x55514A).ToHsla(),
			TextMuted:         rgb(0x6F6A60).ToHsla(),
			TextFaint:         rgb(0x958E82).ToHsla(),
			LineNumber:        rgb(0xB3AC9F).ToHsla(),
			Accent:            rgb(0xB4762F).ToHsla(),
			AccentFill:        rgb(0x9A5F1F).ToHsla(),
			OnAccent:          rgb(0xFFFFFF).ToHsla(),
			AccentDeep:        rgb(0x8A5519).ToHsla(),
			SyntaxLiteral:     rgb(0x5C7A4E).ToHsla(),
			SyntaxIdentifier:  rgb(0x3F5A6B).ToHsla(),
			Selection:         rgb(0xF3E7D3).ToHsla(),
			ActiveLine:        rgb(0xF6F3EC).ToHsla(),
			RangeSurface:      rgb(0xF8F1E3).ToHsla(),
			MatchWash:         rgb(0xEFE3CC).ToHsla(),
			MatchStrong:       rgb(0xEBD3A8).ToHsla(),
			MatchError:        rgb(0xF2DDD1).ToHsla(),
			FindHit:           rgb(0xF6E8B1).ToHsla(),
			FindCurrent:       rgb(0xEBCB67).ToHsla(),
			Overlay:           rgba(0x34302A38).ToHsla(),
			Shadow:            rgba(0x211F1B66).ToHsla(),
			ShadowSoft:        rgba(0x211F1B1F).ToHsla(),
			EnvProd:           rgb(0xB4552A).ToHsla(),
			EnvProdInner:      rgb(0xE8C6B2).ToHsla(),
			EnvProdSurface:    rgb(0xFBEFE8).ToHsla(),
			EnvProdText:       rgb(0x8E4A2A).ToHsla(),
			EnvStaging:        rgb(0xC79B2E).ToHsla(),
			EnvStagingInner:   rgb(0xEBDCAE).ToHsla(),
			EnvStagingSurface: rgb(0xFBF6E7).ToHsla(),
			EnvStagingText:    rgb(0x7A6420).ToHsla(),
			EnvDev:            rgb(0x5C8A4E).ToHsla(),
			EnvDevInner:       rgb(0xCBDEC2).ToHsla(),
			EnvDevSurface:     rgb(0xF1F7EE).ToHsla(),
			EnvDevText:        rgb(0x3F6032).ToHsla(),
			ModeOffSurface:    rgb(0xF2F0EA).ToHsla(),
			ModeOffBorder:     rgb(0xE2DDD2).ToHsla(),
			ModeOffText:       rgb(0xA59F93).ToHsla(),
			RunningSurface:    rgb(0xFDF8EE).ToHsla(),
			RunningBorder:     rgb(0xDCCFB4).ToHsla(),
			RunningMark:       rgb(0xC98B3E).ToHsla(),
			AccentMuted:       rgb(0xA08544).ToHsla(),
			KeyOnFillSurface:  rgba(0xFFFFFF29).ToHsla(),
			KeyOnFillBorder:   rgba(0xFFFFFF57).ToHsla(),
			Ddl:               rgb(0x4A7A6B).ToHsla(),
			DdlInner:          rgb(0xCFE0D6).ToHsla(),
			DdlSurface:        rgb(0xF3F7F4).ToHsla(),
			DdlText:           rgb(0x3F7263).ToHsla(),
			DdlMuted:          rgb(0x7E8C82).ToHsla(),
			DdlGutter:         rgb(0xEEF4F0).ToHsla(),
			DdlNumber:         rgb(0xA9BBB0).ToHsla(),
			DdlDone:           rgb(0x6F9C8B).ToHsla(),
			DeltaAddSurface:   rgb(0xE9F1E7).ToHsla(),
			DeltaAdd:          rgb(0x4E7A44).ToHsla(),
			DeltaAddType:      rgb(0x5C6B60).ToHsla(),
			DeltaAddText:      rgb(0x4E6152).ToHsla(),
			DeltaDropSurface:  rgb(0xFAEDE5).ToHsla(),
			DeltaDropType:     rgb(0xA0765F).ToHsla(),
			OkMuted:           rgb(0x4F7A40).ToHsla(),
			Ok:                rgb(0x4F7A40).ToHsla(),
			OkSurface:         rgb(0xE8F1E3).ToHsla(),
			Idle:              rgb(0xCFC8B8).ToHsla(),
			Error:             rgb(0x8E4A2A).ToHsla(),
			ErrorSecondary:    rgb(0xA9694A).ToHsla(),
			ErrorFaint:        rgb(0xC0A192).ToHsla(),
			ErrorMark:         rgb(0xC08A6A).ToHsla(),
			ErrorSurface:      rgb(0xFCF7F4).ToHsla(),
			ErrorBorder:       rgb(0xE4D3C9).ToHsla(),
			MarkFace:          rgb(0xC98B3E).ToHsla(),
			MarkEars:          rgb(0xB4762F).ToHsla(),
			MarkInk:           rgb(0x3B3325).ToHsla(),
			MarkMuzzle:        rgb(0x7A5522).ToHsla(),
		},
	}
}

// UiFontFamily is the chrome's font: labels, buttons, titles, captions.
// Bundled in the app's assets (uikit.RegisterFonts registers it).
const UiFontFamily = "IBM Plex Sans"

// MonoFontFamily is everything the database said or the user typed: SQL,
// names, values, counts, timings. Also bundled. The SQL editor and the
// grid measure columns by a fixed advance, so they must set it themselves.
const MonoFontFamily = "JetBrains Mono"

// The type scale. Every font size in the app is one of these six, by the
// rule the colours follow: a size is a token, never a literal in view
// code. Both families use the scale, but **not at the same step for the
// same weight on screen**: JetBrains Mono's x-height is near 0.55em and
// IBM Plex Sans's near 0.52em, so mono 13 reads as large as sans 14. Where
// the two sit side by side — a dialog's list of names with its chips, its
// times and its footer — the sans goes one step above what it would be
// alone, or the sans reads a size smaller than the number says.
//
// The steps are whole pixels a visible step apart. Half sizes (10.5,
// 11.5, 12.5) read as no step at all, and on a 1x display they render
// softer than the whole sizes around them. Nothing is below `SizeCaption`:
// 10pt is the smallest text Apple's macOS guidelines call legible, and
// the smallest text here is often the only place a key is written down.
//
// The body is 12 rather than the platform's 13: a database viewer has to
// show many rows, and JetBrains Mono's tall x-height reads well at 12.
const (
	// SizeCaption: key hints, counts, chips, a lane's number — text that
	// labels something else.
	SizeCaption float32 = 10.
	// SizeMeta: the second line of a row, statistics, section headings,
	// summaries beside a title.
	SizeMeta float32 = 11.
	// SizeBody: buttons, list rows, the grid, the SQL editor, text fields.
	SizeBody float32 = 12.
	// SizeEmphasis: titles of a pane or a dialog, the tabs, a dialog's
	// question.
	SizeEmphasis float32 = 13.
	// SizeInput: the palette's search line, a size up from the list it
	// filters, because it is the dialog's whole point. The `×` that
	// closes a tab or empties a line is drawn at it too: a glyph that
	// small is a target, and 14 is what makes it one.
	SizeInput float32 = 14.
	// SizeTitle: the one screen heading, over the connections list.
	SizeTitle float32 = 20.
)

// The GPUI global. Rust installed it with `cx.set_global` in `main` and
// read it with `theme(cx)`; here it is a package value, set once at start.
var (
	globalMu sync.RWMutex
	global   *Theme
)

// SetGlobal installs t as the app's theme (Rust: `cx.set_global(theme)`).
func SetGlobal(t Theme) {
	globalMu.Lock()
	global = &t
	globalMu.Unlock()
}

// Current reads the current theme from the app (Rust: `theme(cx)`). A Go
// test that never called SetGlobal gets the warm paper rather than a
// panic, so a view can be built in a ui.Tester without the app's `main`.
func Current() *Theme {
	globalMu.RLock()
	t := global
	globalMu.RUnlock()
	if t != nil {
		return t
	}
	globalMu.Lock()
	defer globalMu.Unlock()
	if global == nil {
		paper := WarmPaper()
		global = &paper
	}
	return global
}
