# GPUI → mygo: the mapping sheet

Every UI agent of the port (SQL editor, results grid, the app shell, the
connections screen, the palette) reads this instead of rediscovering
mygo. It covers every GPUI construct the Rust UI crates use (counted with
`rg` over `crates/meerkat crates/sql_editor crates/results_grid
crates/ui`), the mygo construct that replaces it, and a short Go example.

- mygo as pinned: `github.com/egoist/mygo v0.9.2-0.20261115180743-31bf65e6ec29`,
  source in `~/go/pkg/mod/github.com/egoist/mygo@v0.9.2-0.20261115180743-31bf65e6ec29`
  (`docs/ui/*.md` are worth reading; `ui/` is the package). Citations below
  are `file:line` in that tree.
- **Every Go example here compiles**: each one is a function in
  `cmd/meerkat-spike/snippets_test.go` (vetted and run as
  `TestSnippetsBuild`), and the eight open points of the handoff are tests
  in `cmd/meerkat-spike/points_test.go`. Change a snippet there first.
- The foundation already ported: `internal/theme` (tokens, GPUI `Hsla`),
  `internal/uikit` (title bar, buttons, keycaps, glyphs, icons, `TextField`,
  overlay `Scrollbar`, `FindBar`, `Blink`, plus the port's helpers: the key
  dispatcher `keys.go`, GPUI animations `motion.go`, bundled fonts
  `fonts.go`).

## 0. The model, in five lines

GPUI's `render` rebuilds an element tree every frame from entity state, and
so does mygo's view function. The differences that matter:

1. There are no entities. State is plain Go structs kept by the app; a view
   is a method that builds elements from them (`func (s *Shell) view(c *ui.Context)`).
2. An element is built **into the current parent** as its constructor runs
   (`ui.Row(c)`), and children are built inside `.Children(func(){…})`.
   A `ui.Element` lives one build pass: never store one in state.
3. Input is polled where the element is built (`el.Clicked()`,
   `el.Shortcut(…)`, `el.Hovered()`), or taken as it comes with
   `el.HandleInput(fn)` for pointer drags and raw keys.
4. Everything runs on the main thread. A goroutine hands results back with
   `win.Update(func(){…})`, which runs on main and draws a frame.
5. A frame follows input by itself: there is no `cx.notify()` after an edit
   made from input. Only state changed from outside (a goroutine, a timer)
   needs `win.Update` / `win.Invalidate` / `c.Invalidate()`.

## 1. The `div()` styling chain

| GPUI | mygo |
|---|---|
| `div()` (block) | `ui.Box(c)` or `ui.Column(c)` — a column whose children stretch |
| `div().flex()` / `.flex_row()` | `ui.Row(c)` — **a mygo Row centres its children vertically by default; GPUI's flex row stretched them.** Where the Rust row had no `items_*`, add `.AlignItems(ui.Stretch)` |
| `.flex_col()` | `ui.Column(c)` |
| `.flex_none()` | `.Shrink(0)` (mygo's default grow is 0) |
| `.flex_1()` | `.Grow(1).Basis(0)` |
| `.flex_grow()` / `.flex_shrink_0()` | `.Grow(1)` / `.Shrink(0)` |
| `.items_center()` `.items_start()` `.items_end()` | `.AlignItems(ui.Center / ui.Start / ui.End)` |
| `.justify_center()` `.justify_end()` `.justify_between()` | `.Justify(ui.Center / ui.End / ui.SpaceBetween)` |
| `.gap(px(8))` | `.Gap(8)` |
| `.p(px(4))` `.px(px(10))` `.py(px(7))` | `.Padding(4)`, `.PaddingX(10)`, `.PaddingY(7)`; `.Padding(top, right, bottom, left)` CSS order; `.Padding(v, h)` |
| `.pl(px(78))` | `.Padding(0, 0, 0, 78)` |
| `.m*` | `.Margin(…)`, `.MarginX`, `.MarginY` |
| `.w(px(30))` `.h(px(17))` `.size(px(6))` | `.Width(30)`, `.Height(17)`, `.Size(6, 6)` |
| `.min_w(px(0))` `.max_w(..)` | `.MinWidth(0)`, `.MaxWidth(..)` |
| `.w_full()` `.h_full()` `.size_full()` | `.FillWidth()`, `.FillHeight()`, `.Fill()` |
| `.border_1()` `.border_color(c)` | `.Border(1, c)` or `.BorderWidth(1).BorderColor(c)` |
| `.border_b_1()` `.border_b_2()` | `.BorderWidth(0, 0, 1, 0)` (top, right, bottom, left); `border_1().border_b_2()` is `.BorderWidth(1, 1, 2, 1)` |
| `.rounded(px(7))` `.rounded_full()` `.rounded_t(px(3))` | `.Radius(7)`, `.Radius(size/2)`, `.Radius(3, 3, 0, 0)` (TL, TR, BR, BL) |
| `.bg(c)` | `.Background(c.Color())` — every colour is a `theme` token (`theme.Hsla`), painted through `.Color()` |
| `.shadow(vec![BoxShadow::new(x, y, c).blur_radius(b).spread_radius(s)])` | `.Shadow(x, y, 2*b, s, c)` — **GPUI's blur is a sigma, mygo's is CSS's, twice the sigma** (`internal/gpu/gpu.go:549`). Use `uikit.BoxShadow` / `uikit.WithShadows`, which double it |
| `.text_size(px(12))` | `.FontSize(12)` — inherited by the `ui.Text` inside, as in GPUI |
| `.text_color(c)` `.font_weight(FontWeight::SEMIBOLD)` | `.TextColor(c.Color())`, `.FontWeight(600)` (MEDIUM 500) |
| `.font_family(MONO_FONT_FAMILY)` | `.Font(theme.MonoFontFamily)` |
| `.line_height(px(15))` | `.FixedLineHeight(15)` (`.LineHeight(m)` is a multiple) |
| `.child("text")` | `ui.Text(c, "text")` inside `.Children` |
| `.child(entity)` (a sub-view) | `entity.View(c)` inside `.Children` |
| `.children(iter)` | a `for` loop inside `.Children` |
| `.truncate()` on a cell | `.SingleLine()` on the `ui.Text` (ellipsis); `.NoWrap()` keeps it whole |
| `.overflow_hidden()` | `.Clip()` (`.ClipX()`, `.ClipY()`) |
| `.absolute().top(..).left(..)` | `.Absolute().Top(..).Left(..)` — relative to the **parent**, always |
| `.relative()` | nothing: an absolute child is always placed in its parent |
| `.cursor_pointer()` `.cursor(CursorStyle::IBeam)` | `.Cursor(ui.CursorPointer)`, `.Cursor(ui.CursorText)`; resize: `ui.CursorResizeEW` / `ui.CursorResizeNS` |
| `.hover(\|s\| s.bg(c))` | `if el.Hovered() { el.Background(c) }` |
| `.opacity(o)` | `.Opacity(o)` |
| `.id("x")` | `c.Key("x")` passed to the constructor: `ui.Row(c.Key("x"))` |
| `.when(cond, \|d\| …)` `.map(\|d\| …)` | a Go `if` on the element value |
| `.occlude()` | nothing — see §10 |

```go
func snippetDiv(c *ui.Context, name string, active bool) {
	colors := &theme.Current().Colors
	row := ui.Row(c.Key(name)). // div().id(name).flex()
		Shrink(0).             // .flex_none()
		AlignItems(ui.Center). // .items_center()
		Gap(8).Padding(6, 14). // .gap(px(8)).py(px(6)).px(px(14))
		BorderWidth(0, 0, 1, 0).BorderColor(colors.Hairline.Color()). // .border_b_1()
		Background(colors.Panel.Color()).
		FontSize(12).TextColor(colors.TextMuted.Color()).Cursor(ui.CursorPointer)
	if row.Hovered() { // .hover(|s| s.bg(colors.window))
		row.Background(colors.Window.Color())
	}
	row.Children(func() { // .child(name)
		ui.Text(c, name).SingleLine() // a cell's .truncate()
		if active {                   // .when(active, |d| d.child(..))
			uikit.StatusDot(c, colors.Ok)
		}
	})
}
```

Styles may be chained after `.Children` too; the element is laid out once
the build ends. A uikit builder that returned a GPUI `Div` for the caller
to finish returns the `ui.Element` already in the tree; the caller keeps
chaining (`uikit.ToolbarButtonBare(c).Width(80).Children(…)`).

## 2. `uniform_list` and its scroll handle

`ui.List(c, &state, n, func(i int){…})` builds only the rows in view (plus
a few). Give every row the same `Height` and it is GPUI's uniform list.
Sideways: `.RowWidth(w)` makes rows at least `w` wide and the list then
scrolls both ways (verified: `TestPoint6ListScrollsBothWaysByTheProgram`).
The offset is a `ui.ScrollState` given with `.TrackScroll(&st)`
(`ui/scroll.go:69`): `st.X`, `st.Y` are how far the content has travelled
(**positive**, where GPUI's `ScrollHandle::offset()` ran negative),
`st.MaxX`, `st.MaxY` how far it can go; set `X`/`Y` to scroll the
container in the next frame. Rows by index: `state.ScrollTo(ix, ui.Start)`,
`state.ScrollIntoView(ix)`, `state.Visible()` (`ui/list.go:358-420`).

```go
type sidebar struct {
	rows   []string
	list   ui.ListState
	scroll ui.ScrollState
}

func (s *sidebar) snippetUniformList(c *ui.Context) {
	ui.List(c.Key("catalog"), &s.list, len(s.rows), func(i int) {
		ui.Row(c).Height(26).Children(func() { ui.Text(c, s.rows[i]).SingleLine() })
	}).Grow(1).TrackScroll(&s.scroll).Scrollbars(false)
	// handle.scroll_to_item(ix, ScrollStrategy::Top):
	// s.list.ScrollTo(ix, ui.Start); s.list.ScrollIntoView(ix)
}
```

- `.Scrollbars(false)` turns mygo's own bars off for a container that
  wears `uikit.Scrollbar` (the Rust overlay bar). `uikit.ScrollHandle`
  wraps the `ScrollState` plus the viewport `Bounds` the bar needs; write
  `h.Bounds = list.Bounds()` after building the list.
- A list keeps its place by row; a list that shows another set of rows
  (another result) wants `state = ui.ListState{}` and `st = ui.ScrollState{}`.
- Plain scrolling containers: `ui.Scroll` (down), `ui.ScrollHorizontal`,
  `ui.ScrollBoth`, all taking `TrackScroll`.
- `GridState::reveal` / autoscroll per frame: set `st.X`/`st.Y` and, while
  a drag holds the pointer past an edge, call `c.AnimationFrame()` in each
  frame that moved (one step per frame, as the Rust).

## 3. `deferred(..)` (and `anchored`)

`ui.Overlay(c, func(){…})` builds elements in the layer above everything
else (the palette, the close dialog, the value card, the column-find
popover, the update toast). Place with `.Absolute()` in window DIPs, or
next to an element with `.AttachTo(target, at, self)`. `el.PressedOutside()`
is GPUI's `on_mouse_down_out`. Paint order among overlays is build order:
build the toast first, then palette / card / dialog, as the Rust painted it.

```go
func snippetDeferred(c *ui.Context, open *bool) {
	button := uikit.ToolbarButton(c, "column")
	if button.Clicked() {
		*open = !*open
	}
	if *open {
		ui.Overlay(c, func() { // deferred(..): painted after everything
			panel := uikit.Card(c).AttachTo(button, ui.AnchorBottomLeft, ui.AnchorTopLeft).Padding(8)
			if panel.PressedOutside() { // on_mouse_down_out
				*open = false
			}
			panel.Children(func() { ui.Text(c, "search columns") })
		})
	}
}
```

`ui.Modal(c, &open, fn)` / `.Modal()` keep the focus in a dialog and make
what is behind it inert; Escape closes the overlay on top unless the focus
takes it (the Rust dialogs handle ⎋ themselves: bind it in the dialog's
key table, see §8).

## 4. `canvas(prepaint, paint)`

A `Box` with a size and `.Draw(func(p *ui.Painter, r ui.Rect){…})`. `r` is
the element's box in window DIPs. Painter: `Fill(r, c, radius)` (=
`paint_quad(fill(..))`), `Stroke`, `Line`, `FillPath` / `StrokePath`
(= `paint_path`), `Clip(r, radius, fn)` (= `with_content_mask`),
`Glyphs`, `RichText`, `MeasureText`, `Transform`, `Visible()`
(`ui/paint.go:1594-1842`). `.DrawOver` paints after the children.
mygo's `ui.Path` has `MoveTo/LineTo/QuadTo/CubeTo/Close/Circle` but no
arc; `ui.ParsePath` reads SVG data with arcs (`uikit/icon.go` does that for
GPUI's `arc_to`). GPUI's stroked paths have flat ends and mitred joins;
mygo's are round unless `path.Cap(ui.ButtCap).Join(ui.MiterJoin)`.

```go
func snippetCanvas(c *ui.Context, color theme.Hsla) ui.Element {
	return ui.Box(c).Size(8, 10).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
		var path ui.Path
		path.MoveTo(r.X, r.Y).LineTo(r.X, r.Y+r.H).LineTo(r.X+r.W, r.Y+r.H/2).Close()
		p.FillPath(&path, color.Color()) // window.paint_path(path, color)
	})
}
```

A `Draw` may run more than once a frame and in frames that only paint
(the view did not run). Keep it a function of state; writing back
idempotent results (the text field's scroll, last layout) is fine and is
what `uikit.TextField` does.

## 5. A custom `Element` (`request_layout` / `prepaint` / `paint`)

- `request_layout` → the element's size in the build: `.Width`, `.Height`,
  `.FillWidth`, or children that give it one.
- `prepaint` → the top of the `Draw` callback (it gets the laid-out box), or
  the build when it needs no layout.
- `paint` → the rest of `Draw`.
- `window.insert_hitbox` + `hitbox.is_hovered` → `el.Hovered()`;
  mouse listeners → `el.HandleInput` (§9).
- State the Rust kept on the element between prepaint and an event
  (`PrepaintState`) → `ui.Local(el, "key", init)` on the element, or the
  owner's struct (see `uikit/scrollbar.go`).
- Only what is on screen: `p.Visible()` is the clip in window DIPs;
  `ui.List` virtualises rows by itself.

```go
type gutter struct{ lines int }

func (g *gutter) snippetElement(c *ui.Context) ui.Element {
	const lineHeight = 20
	font := ui.Font{Family: theme.MonoFontFamily, Size: 12}
	return ui.Box(c).Width(40).Height(float32(g.lines*lineHeight)). // request_layout
		Draw(func(p *ui.Painter, r ui.Rect) { // prepaint + paint
			vis := p.Visible() // only what is on screen (the clip)
			for i := 0; i < g.lines; i++ {
				top := r.Y + float32(i*lineHeight)
				if top+lineHeight < vis.Y || top > vis.Y+vis.H {
					continue
				}
				glyphs := ui.Shape(fmt.Sprint(i+1), font)
				p.Glyphs(glyphs, r.X+4, top+15, theme.Current().Colors.LineNumber.Color())
			}
		})
}
```

### Text in a custom element

- `window.text_system().shape_line(text, size, runs)` → `ui.Shape(text,
  ui.Font{Family, Size, Weight})` (`ui/glyphs.go:92`): glyphs in visual
  order with `X`, `Advance`, `Cluster` (a **rune** index, not a byte
  index) and `Runes`. It caches nothing: keep the glyphs of a line across
  frames, keyed by its text. Paint with `p.Glyphs(glyphs, x, baseline, c)`.
- Baseline: GPUI centres the glyphs in the line height —
  `baseline = top + (lineHeight - ascent - descent)/2 + ascent`, with
  `font.Metrics()` (`text_system/line.rs:353`).
- `ShapedLine::x_for_index` / `closest_index_for_x` (byte offsets): port
  them over the glyphs mapped back to byte offsets; `uikit/text_field.go`
  (`shapedLine`) is a line-for-line copy of GPUI's.
- **Fixed advance** (the editor and the grid): `ui.Shape("M", mono)[0].Advance`
  is the cell width; JetBrains Mono gives every glyph that advance and
  `glyph.X == float32(glyph.Cluster)*advance` (verified:
  `TestPoint2FixedAdvanceMono`). Place glyphs at `Cluster*advance` to be
  exact under ligatures; expand tabs before shaping (`display::Expanded`).
- Coloured runs, backgrounds, underlines (`TextRun`): `p.RichText(x, y, 0,
  spans...)` with `ui.Span{Text, Font, Size, Color, Background,
  WavyUnderline, Underline, Strikethrough, DecorationColor, Weight}`
  (`ui/richtext.go:15-80`, `:325`), or glyphs plus `p.Fill` quads
  (selection, `find_hit`, washes) painted *before* the glyphs.
- IME underline: GPUI draws it at `baseline + descent*0.618`.

## 6. `EntityInputHandler`

The owner's struct implements `ui.TextInputClient`
(`ui/text_input_client.go:39`) and the element gets
`.HandleTextInput(client)`, which also makes it focusable with the I-beam.
Ranges are UTF-16, as in GPUI.

| GPUI `EntityInputHandler` | `ui.TextInputClient` |
|---|---|
| `text_for_range(range, &mut actual)` | `TextForRange(r) (string, actual)` |
| `selected_text_range` | `Selection() TextInputSelection` (Range, Reversed) |
| `marked_text_range` | `MarkedRange() (TextInputRange, bool)` |
| `unmark_text` | `UnmarkText()` |
| `replace_text_in_range(Option<range>, text)` | `ReplaceText(*TextInputRange, text)` (nil = marked, else selection) |
| `replace_and_mark_text_in_range(.., Option<sel>)` | `SetMarkedText(*TextInputRange, text, selected)` (selected relative to the new text, always given) |
| `bounds_for_range(range, element_bounds)` | `BoundsForRange(r) (Rect, actual, ok)` — **element-relative**, no bounds argument: keep the last paint's line box |
| `character_index_for_point(point)` | `IndexForPoint(p) (int, bool)` — element-relative |
| `window.handle_input(&focus, ElementInputHandler::new(bounds, entity))` | nothing per frame: `.HandleTextInput(client)` on the element |

```go
func snippetInputHandler(c *ui.Context, client ui.TextInputClient, onInput func(ui.InputEvent) bool) ui.Element {
	return ui.Box(c).HandleTextInput(client).HandleInput(onInput).Cursor(ui.CursorText)
}

var _ ui.TextInputClient = (*uikit.TextField)(nil)
```

Typed text and IME arrive through the client. **Keys do not**: bind them
in the element's key table (§8). Let `HandleInput` take the pointer only
and return false for keys, text and `InputCommand` — an Edit-menu command
(Copy, Paste, Select All, Undo) then comes back as its key, ⌘C, to the key
table (`ui/input.go:1463-1485`, `commandKey`). `uikit.TextField` is the
worked example.

## 7. `FocusHandle`

A `ui.Handle` in the owner's struct, bound with `.Bind(&h)` on a
`.Focusable()` element (elements with `HandleTextInput` are focusable
already).

| GPUI | mygo |
|---|---|
| `cx.focus_handle()` | a `ui.Handle` field |
| `.track_focus(&handle)` | `.Focusable().Bind(&handle)`; add `.TabStop(false)` for a pane that holds keys but is not a Tab stop |
| `window.focus(&handle)` | `handle.Focus()` (main thread; waits while the control is hidden) |
| `handle.is_focused(window)` | `handle.Focused(c)` — works before the control is built |
| `handle.contains_focused(window, cx)` | `handle.FocusWithin(c)` |
| `Focusable::focus_handle` | a method returning `*ui.Handle` (`TextField.FocusHandle()`) |
| `window.blur()` / focus nowhere | `handle.CancelFocus()`; a click on nothing focusable clears the focus |

```go
type pane struct {
	focus ui.Handle // cx.focus_handle()
}

func (p *pane) snippetFocus(c *ui.Context) {
	el := ui.Column(c).Focusable().TabStop(false).Bind(&p.focus) // .track_focus(&handle)
	if p.focus.Focused(c) { // handle.is_focused(window)
		el.Background(theme.Current().Colors.Raised.Color())
	}
	if p.focus.FocusWithin(c) { // handle.contains_focused(window, cx)
		el.Border(1, theme.Current().Colors.AccentFill.Color())
	}
	// window.focus(&handle):
	// p.focus.Focus()
}
```

A press focuses the innermost focusable element under it
(`ui/input.go:549-575`). A Handle is bound per window; with several windows
query it with that window's `c`.

## 8. Key contexts, actions, bindings — `uikit` dispatcher

**mygo resolves keys the way GPUI does: deepest focused context first.**
`Element.Shortcut(mods, key)` registers a key on an element; a key goes to
the innermost element *of the focus chain* that registered it, then to the
overlay on top, then to the window's `c.Shortcut` (`ui/input.go:1284-1360`
`routeKeys`/`routeKey`, `:1053` focus chain, `:1894` "the innermost element
handling a key gets it"). Verified: `TestPoint3DeepestContextWins` — ⌘⏎
bound in a palette, in the shell around it and on the window goes to
whichever is deepest around the focus, ⎋ bound by the field and the shell
goes to the field, and the window binding fires with no focus.

`uikit/keys.go` is the one dispatcher, so the Rust tables port line for
line:

| GPUI | Go |
|---|---|
| `actions!(ns, [A, B])` | `type xAction int` + consts |
| `KeyBinding::new("cmd-shift-left", A, Some(CTX))` | `uikit.Bind("cmd-shift-left", aAction)` in a `[]uikit.KeyBinding[xAction]` table |
| `cx.bind_keys(table)` in `main` | nothing: the table is dispatched where the context is |
| `.key_context(CTX)` on an element | that element: `uikit.DispatchKeys(el, table)` in **every** frame it is built |
| `.on_action(cx.listener(Self::a))` | `switch` on the action `DispatchKeys` returned |
| binding with no context / one that must fire with no focus | `uikit.DispatchWindowKeys(c, table)` — last in line |
| `KeyBinding` matched deepest | automatic |

```go
type gridAction int

const (
	gridMoveDown gridAction = iota
	gridTickRow
	gridCopy
)

var gridKeys = []uikit.KeyBinding[gridAction]{ // KeyBinding::new(.., Some(GRID_KEY_CONTEXT))
	uikit.Bind("down", gridMoveDown),
	uikit.Bind("space", gridTickRow),
	uikit.Bind("cmd-c", gridCopy),
}

func snippetKeys(c *ui.Context, focus *ui.Handle, perform func(gridAction)) {
	grid := ui.Box(c).Focusable().Bind(focus) // .key_context(GRID_KEY_CONTEXT).track_focus(..)
	if action, ok := uikit.DispatchKeys(grid, gridKeys); ok {
		perform(action) // .on_action(cx.listener(Self::move_down)) …
	}
}
```

Rules (also in the `keys.go` header):

1. A Rust context element → the Go element that `DispatchKeys` runs on;
   the focus must be inside it, as in GPUI. The shell's root element is
   `.Focusable().TabStop(false).Bind(&shell.focus)` and takes the focus
   when nothing inside it has it, as the Rust shell div did.
2. The root context's ⌘W and ⌘N, and anything the Rust bound with no
   context: `DispatchWindowKeys`.
3. **Never handle a bindable key in `HandleInput` when an ancestor binds
   it**: mygo gives `HandleInput` only keys no Shortcut in the focus chain
   or the window claims (`ui/input.go:1118`), so the ancestor would win.
   Put every key a widget answers in its own table: the innermost table
   wins.
4. Dispatch every frame: routing reads the registrations of the previous
   frame (`regs`).
5. A bare printable key (`space`) bound *around* a focused text-taking
   element fires **and** types: the darwin backend gives the key to the
   input method whatever the shortcuts said
   (`internal/darwin/surface.go:1367-1412`). The Rust design never does
   this (the find strip is a sibling of its owner's context); keep it so.
6. One key per element per frame (`ui/input.go:1288`); a second key for the
   same element waits a frame. Fast typing is not lost.
7. Keystroke strings: modifiers `cmd ctrl alt shift`, keys `a`–`z`,
   `0`–`9`, `enter escape tab space backspace delete home end pageup
   pagedown left right up down f1`–`f12` and `- = , . / ; ' [ ] \ \``.
   `cmd` is `ui.Cmd` (Command on macOS). Every keystroke the Rust binds
   parses.
8. ⌘C/⌘V/⌘X/⌘A/⌘Z/⇧⌘Z: the macOS Edit menu takes them first and sends a
   command; a focused element that does not take the command gets the key
   back (rule above), so bind them in the table like any other key.

## 9. `on_mouse_down` / `on_mouse_move` / `on_mouse_up` / `on_click`

| GPUI | mygo |
|---|---|
| `.on_click(…)` | `if el.Clicked() {…}` inline, or `el.OnClick(fn)` (runs after the build — use it when the action changes what is being built) |
| `event.click_count() == 2` on click | `el.DoubleClicked()`; in `HandleInput`, `ev.Clicks` |
| `event.modifiers.shift` | `ev.Mods&ui.Shift != 0`; at a click `el.ClickModifiers()`; now `c.Modifiers()` |
| `.on_mouse_down(MouseButton::Left, …)` | `HandleInput` with `ev.Kind == ui.InputPointerDown && ev.Button == 0`; return true to **take the press** |
| `.on_mouse_move(…)` | `ui.InputPointerMove` — `ev.Button` is the button held, `-1` for a hover move |
| `.on_mouse_up(…)` / `.on_mouse_up_out(…)` | `ui.InputPointerUp` — delivered to the element that took the press wherever the pointer is |
| `.on_mouse_down_out(…)` | `el.PressedOutside()` |
| `window.on_mouse_event` (the `DragSurface` / `pane_drag_surface` / `peek_drag_surface` / scrollbar pattern) | the element that took the press gets **every move and the release until the button comes up, wherever the pointer goes** — mygo's implicit pointer capture (`ui/input.go:474-485`, `:580`, `:759`) |
| `window.set_window_cursor_style` during a drag | the pressed element's `.Cursor(…)` holds for the whole press (`ui/runtime.go:1867`) |
| `cx.stop_propagation()` | return true from `HandleInput` |
| `ev.position` | `ev.X`, `ev.Y` relative to the element's box (negative or past its size when outside) |

```go
type drag struct {
	anchor  *ui.Point
	clicked int
}

func (d *drag) snippetMouse(c *ui.Context) ui.Element {
	return ui.Box(c).Fill().HandleInput(func(ev ui.InputEvent) bool {
		switch ev.Kind {
		case ui.InputPointerDown: // on_mouse_down(MouseButton::Left, ..)
			if ev.Button != 0 {
				return false
			}
			d.anchor, d.clicked = &ui.Point{X: ev.X, Y: ev.Y}, ev.Clicks // event.click_count
			return true // takes the moves and the release, anywhere
		case ui.InputPointerMove: // on_mouse_move; ev.Button < 0 is a hover
			return d.anchor != nil && ev.Button == 0
		case ui.InputPointerUp: // on_mouse_up, and on_mouse_up_out
			d.anchor = nil
			return true
		}
		return false
	})
}
```

Verified: `TestPoint5PointerCaptureDuringADrag` — a press on one element,
moves over a second element and to the window's corner, and the release
there all reach the first; the second hears nothing. Notes:

- `HandleInput` sits on the innermost element in the hit chain that has
  one; it gets the press **before** a clickable child, so return false for
  presses it does not want (`ui/input.go:576-585`).
- Autoscroll while the pointer is held still past an edge: no events
  arrive, so step the scroll from the view and call `c.AnimationFrame()`
  while it moves, as the Rust asked for the next frame.
- A move that arrives with no button held (`ev.Button < 0`) ends a drag,
  as in the Rust.

## 10. `.occlude()`

Nothing to do. mygo hit-tests the topmost element painted under the
pointer and gives the press to the innermost *interactive* element of that
chain (`ui/input.go:549-560`), so a strip, a card or a scrim that is
painted over something already keeps the press from it. The reverse —
letting presses through a painted element — is `.PassThrough()`. A scrim
that must swallow clicks without acting: give it `.Focusable().TabStop(false)`
or an `OnClick` that does nothing.

## 11. `with_animation` / `repeat_synced` / `reduce_motion`

`uikit.AnimationDelta(c, el, id, anim)` returns GPUI's eased delta for
element `el`, and the caller styles the element with it as the Rust
closure did. `uikit.NewAnimation(d)`, `.Repeat()`, `.RepeatSynced()`,
`.WithEasing(f)`; easings `uikit.Linear`, `Quadratic`, `EaseInOut`,
`EaseOutQuint()`, `Bounce(f)`, `PulsatingBetween(lo, hi)` — GPUI's own
formulas (`uikit/motion.go`). Colour mixing is `theme.Hsla.Blend` /
`.Opacity`, GPUI's arithmetic.

```go
func snippetAnimation(c *ui.Context, running bool, landed int) {
	colors := &theme.Current().Colors
	button := uikit.AccentButtonBare(c)
	if running {
		breath := uikit.NewAnimation(1600 * time.Millisecond).RepeatSynced().WithEasing(uikit.PulsatingBetween(0, 1))
		delta := uikit.AnimationDelta(c, button, "breath", breath)
		button.Background(colors.AccentFill.Blend(colors.OnAccent.Opacity(0.18 * delta)).Color())
	} else if landed > 0 {
		settle := uikit.NewAnimation(420 * time.Millisecond).WithEasing(uikit.EaseInOut)
		delta := uikit.AnimationDelta(c, button, landed, settle) // the id carries `landed`
		button.Background(colors.AccentFill.Blend(colors.OnAccent.Opacity(0.18 * (1 - delta))).Color())
	}
}
```

- **A new id restarts it**, as a new GPUI element id did (the settle keyed
  on `QueryTab::landed`, the test word keyed on `testing`).
- **`repeat_synced`**: the phase is `c.Now()` modulo the period from the
  Unix epoch — one phase for every element in every window, so a rebuild
  never restarts the breath. mygo's own `el.Loop` is **not** synced (it
  starts at the element's first build, `ui/widgets.go:71`); do not use it
  for the breath.
- **Reduce motion** (`c.Preferences().ReduceMotion`, `ui/preferences.go:7`):
  a oneshot is drawn at its end, a repeating one at its start, and no
  frames are asked for — GPUI's rule. Verified:
  `TestPoint8ReduceMotionAndSyncedPhase`.
- While an animation runs, `AnimationDelta` calls `c.AnimationFrame()`;
  nothing else is needed. Motion of your own: compute from `c.Now()` and
  call `c.AnimationFrame()` each frame that moves, or `c.After(d)` for the
  next step of something that changes in steps.
- Don't reach for mygo's `AnimateTo`/springs in the port: they ease
  *values* to targets, a different model from the delta the Rust code and
  its CLAUDE.md reasons are written in.

## 12. `cx.notify()`

| GPUI | mygo |
|---|---|
| `cx.notify()` after an edit from input | nothing: a frame follows the input |
| `cx.notify()` in a spawned reply | `win.Update(func(){…})` (runs on main, then draws) |
| a timer loop that repaints | `c.After(d)` from the view (one frame at that time), or a goroutine + `win.Invalidate()` |
| `window.refresh()` | `win.Invalidate()` / `c.Invalidate()` |
| `window.on_next_frame(f)` | `c.Invalidate()` and do `f` in the next build |

## 13. `cx.spawn` + `gpui_tokio::Tokio::spawn` → goroutine + `win.Update`

The view needs its `*mygo.Window`: keep it in the window's root state
(`mygo.NewWindow` returns it before the first frame runs). Keep every
generation counter exactly as the Rust has it.

```go
type tab struct {
	generation uint64
	rows       []string
	err        error
}

func snippetSpawn(win *mygo.Window, t *tab, query func(context.Context) ([]string, error)) {
	t.generation++ // bump before the request
	generation := t.generation
	go func() {
		rows, err := query(context.Background()) // the tokio task
		win.Update(func() { // this.update(cx, |this, cx| { ..; cx.notify() })
			if t.generation != generation {
				return // a stale reply
			}
			t.rows, t.err = rows, err
		})
	}()
}
```

- `flatten()` (JoinError then the inner Result) becomes one `error`.
- `.detach()` is the default: a goroutine is never aborted by dropping a
  handle. Where the Rust relied on `Tokio::spawn` aborting on drop
  (nothing in the app does, it detaches), pass a `context.Context` and
  cancel it.
- **⌘Q waits for its cancels** (CLAUDE.md "Closing something that is still
  running"): in the quit path, run the cancels with a `sync.WaitGroup`
  before letting the quit go on — `App.Run` returns and the process ends.
- `mygo.RunOnMain(f)` runs on main without a frame (for a poll that only
  sometimes changes state); then `win.Invalidate()` if it did.
- `cx.background_executor().timer(d)` → `time.AfterFunc(d, func(){ win.Update(…) })`
  outside the view, `c.After(d)` inside it.

## 14. `Entity`, `cx.new`, `emit`, `subscribe`, `observe`

| GPUI | Go |
|---|---|
| `Entity<T>`, `cx.new(\|cx\| T::new(..))` | `*T` kept in the owner's struct |
| `entity.read(cx)` / `entity.update(cx, \|t, cx\| ..)` | plain field access / method calls on main |
| `cx.entity()` / `WeakEntity` | the pointer; for "the reply's target may be gone", check a generation or a `closed` flag |
| `impl EventEmitter<E>` + `cx.emit(e)` | a listener slice on the emitter, called synchronously (`TextField.Subscribe`) |
| `cx.subscribe(&e, \|this, e, ev, cx\| ..)` / `subscribe_in` | `e.Subscribe(func(src *T, ev E){ … })` |
| `cx.observe(&e, ..)` | nothing: the view reads the state every frame |
| `cx.set_global(T)` / `cx.global::<T>()` | a package value (`theme.SetGlobal`, `theme.Current()`); the updater is a process-wide struct |
| `impl Render for T` | `func (t *T) View(c *ui.Context) ui.Element` |

```go
func snippetSubscribe(field *uikit.TextField, submit func()) {
	field.Subscribe(func(_ *uikit.TextField, e uikit.TextFieldEvent) { // cx.subscribe_in(&field, ..)
		if e == uikit.TextFieldSubmitted {
			submit()
		}
	})
}
```

GPUI delivered `emit` after the current update finished; Go listeners run
inside the call that emitted. A listener that would re-enter the emitter in
a way that matters should defer its work (set a flag the view reads, or
`el.OnClick`-style post-build action).

## 15. Clipboard

```go
func snippetClipboard(c *ui.Context, csv string) string {
	c.WriteClipboard(csv)    // cx.write_to_clipboard(ClipboardItem::new_string(csv))
	return c.ReadClipboard() // cx.read_from_clipboard().and_then(|i| i.text())
}
```

Outside a view: `mygo.Clipboard.WriteText(s)` / `ReadText()`, or keep
`c.Services()` (`ReadClipboard`, `WriteClipboard`, `Invalidate`) for
callbacks between builds. A `ui.Tester` keeps its own clipboard
(`tt.Clipboard()`). `ReadClipboard` returns "" for "no text", which GPUI
told apart from an empty string.

## 16. `ScrollHandle` / `UniformListScrollHandle`

| GPUI | mygo |
|---|---|
| `ScrollHandle::new()` | a `ui.ScrollState` field (or `uikit.ScrollHandle`, which adds the viewport) |
| `.track_scroll(&handle)` / `uniform_list(..).track_scroll(handle)` | `.TrackScroll(&st)` on `ui.Scroll*` or `ui.List` |
| `handle.offset()` (negative) | `Point{st.X, st.Y}` (**positive** distance travelled) |
| `handle.set_offset(p)` | `st.X, st.Y = -p.x, -p.y` |
| `handle.max_offset()` | `Point{st.MaxX, st.MaxY}` (set by the frame) |
| `handle.bounds()` | the container's `Bounds()` after building it (`uikit.ScrollHandle.Bounds`) |
| `UniformListScrollHandle::scroll_to_item(ix, strategy)` | `listState.ScrollTo(ix, ui.Start/ui.Center/ui.End)` / `ScrollIntoView(ix)` |
| `UniformListScrollHandle.0.borrow().base_handle` | the same `ScrollState` given to the list |
| smooth scroll | `st.ScrollTo(x, y, ui.Smooth)` — the Rust never animates scrolls; don't |

## 17. Window options and the title bar

```go
func snippetWindow(view func(*ui.Context)) *mygo.Window {
	return mygo.NewWindow(mygo.WindowOptions{
		Title:                "Meerkat",
		Width:                1360,
		Height:               880,
		TitleBarStyle:        mygo.TitleBarHidden,        // appears_transparent: true
		TrafficLightPosition: &mygo.Point{X: 14, Y: 15}, // traffic_light_position
		Content:              ui.View(view),             // a new Root per window
	})
}
```

- `app_owns_titlebar_drag` + `start_window_move` + `TitleDrag::claim` →
  `uikit.TitleBar(c, id)` (`.DragWindow()`): the window drags only when the
  innermost interactive element under the press is the bar, so tabs and
  buttons in it (anything clickable or focusable) take their own presses
  (`ui/input.go:549-560`, `:602`); a double click on the bar zooms
  (`titlebar_double_click`). `c.TitleBar()` gives the traffic lights'
  room; the Rust used the fixed `TRAFFIC_LIGHTS_WIDTH` 78, kept.
- `window.viewport_size()` → `c.Size()`. `window.is_fullscreen()` →
  `win.IsFullScreen()`. `WindowBounds` / cascade → `win.SetPosition`,
  `WindowOptions.X/Y` (the Rust cascades by `WINDOW_CASCADE`; do it with
  `X/Y` from the previous window's `Position()`).
- `window.remove_window()` → `win.Destroy()` (no `OnClose`), or
  `win.Close()` (asks `OnClose`).
- The root element: `c.Root().Background(colors.Window.Color()).Font(theme.UiFontFamily)`
  — the screens' roots set the chrome font, as in Rust.

## 18. Fonts

`uikit.RegisterFonts()` once in `main` before the first window (it
replaces `add_fonts`; the TTFs are embedded from `internal/uikit/fonts/`,
copies of `crates/meerkat/assets/fonts/`). Each face is registered under
its family name, so `.Font(theme.UiFontFamily).FontWeight(600)` picks
IBM Plex Sans SemiBold and `.Font(theme.MonoFontFamily)` JetBrains Mono
(`ui/window.go:37-43`; docs/ui/text.md "Fonts"). A Tester draws them too.
Verified: `TestPoint1TwoBundledFamilies` (both families registered, mono
reported monospaced, SemiBold wider than Regular).

## 19. Several windows, the close guard, ⌘Q, restart

- **⌘N**: `mygo.NewWindow` again with `ui.View(newRoot.view)` — each window
  its own state (a `Content` may serve several windows, each with its own
  element state, `ui/window.go:33-35`). `mygo.Windows()` lists them
  (`window.go:825`), `cx.windows()`.
- **`on_window_should_close`** → `win.OnClose(func(e *mygo.CloseEvent))`;
  `e.PreventDefault()` answers no (`window.go:1975`, `events.go:21`); the
  dialog closes the window later with `win.Close()` once the guard says
  yes (or `win.Destroy()`, which skips `OnClose`).
- **⌘Q** → `mygo.App.Quit()`, which runs `OnBeforeQuit` (preventable,
  `app.go:577`) and then every window's `OnClose`; `OnQuitCanceled`
  (`app.go:594`) is the "stay" that clears every window's `quitting` flag.
  Port `main::quit`'s walk into `OnBeforeQuit`: the first window with
  something at stake puts its dialog up and the quit is prevented;
  agreeing re-runs `mygo.App.Quit()`. `e.SessionEnding` (logout) cannot be
  prevented.
- **`cx.restart()`** → `mygo.App.Relaunch()` (`login.go:102`): the same
  quit sequence (cancelable by the same listeners), then the app starts
  again. `restart_to_update` is the ⌘Q walk with `Relaunch` as the last
  word.
- **`cx.on_window_closed` quitting when the list is empty** → mygo's
  default: an app quits when its last window closes, unless
  `OnWindowAllClosed` has a listener (`docs/app.md:70`).
- **`cx.quit()`** → `mygo.App.Quit()`.

```go
func snippetLifecycle(win *mygo.Window, guard func() bool, walk func() bool, cancelled func()) {
	win.OnClose(func(e *mygo.CloseEvent) { // on_window_should_close
		if !guard() {
			e.PreventDefault() // answer "no"; the dialog closes it later with win.Close()
		}
	})
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) { // ⌘Q → main::quit
		if !e.SessionEnding && !walk() {
			e.PreventDefault()
		}
	})
	mygo.App.OnQuitCanceled(cancelled) // ShellEvent::QuitCancelled
	// cx.restart(): mygo.App.Relaunch(); cx.quit(): mygo.App.Quit()
	// cx.windows(): mygo.Windows()
}
```

These need a real window; they are compiled (`snippets_test.go`,
`cmd/meerkat-spike/main.go`) but **not exercised** in a test.

## 20. Timers and the caret

```go
func snippetTimer(c *ui.Context, started time.Time) {
	if elapsed := c.Now().Sub(started); elapsed < time.Second {
		c.After(time.Second - elapsed) // the TIMER_DELAY: one frame when the pill is due
	} else {
		c.After(100 * time.Millisecond) // start_timer's 100 ms repaint
	}
}
```

- The caret: `uikit.Blink` + `uikit.TrackBlinkFocus(owner, focused, c)` in
  the owner's view every frame, `uikit.RestartBlink(owner)` after every
  edit and motion; paint the caret while `owner.BlinkState().On()`. It
  asks one frame per half cycle with `c.After`, none while unfocused
  (`TestTheCaretBlinksByTheFrameClock`).
- Tests: never sleep; `tt.Advance(d)` moves a Tester's clock and runs the
  frames due.

## 21. Testing a view

`ui.NewTester(view, w, h)`: `tt.Click("label")`, `tt.ClickAt(x, y)`,
`tt.Press/Move/Release(x, y)`, `tt.Key(mods, key)`, `tt.Type(text)`,
`tt.Frame()`, `tt.Advance(d)`, `tt.Find(text)`, `tt.Texts()`,
`tt.Clipboard()`, `tt.Image()` (an `*image.RGBA` — `png.Encode` it and
look at it). `ui.NewTesterWith(view, w, h, ui.TesterOptions{Preferences:
ui.Preferences{ReduceMotion: true}})`. Call `uikit.RegisterFonts()` in
`TestMain` so text is set in the real fonts. Focus a control with its
handle (`h.Focus(); tt.Frame()`).

## 22. The eight open points of the handoff, answered

| # | Point | Answer | Evidence |
|---|---|---|---|
| 1 | Bundled TTFs, two families | `ui.RegisterFont(data, family)`, each weight under its family; `uikit.RegisterFonts()` | `ui/window.go:37-43`; `TestPoint1TwoBundledFamilies` |
| 2 | Fixed-advance mono measurement | `ui.Shape("M", mono)[0].Advance`; every JetBrains Mono glyph has it and sits at `Cluster*advance` | `ui/glyphs.go:60-104`; `TestPoint2FixedAdvanceMono` |
| 3 | Key scoping between nested elements | Native: innermost element of the focus chain that registered the key, then the top overlay, then the window — GPUI's deepest-context-wins. One dispatcher, `uikit/keys.go`, ports the binding tables; rules in §8 | `ui/input.go:1053`, `:1284-1360`, `:1118`, `:1894`; `TestPoint3DeepestContextWins` |
| 4 | Several windows; close hook that can say no; quit interception; restart | `NewWindow` per root; `win.OnClose` + `PreventDefault`; `App.OnBeforeQuit` + `OnQuitCanceled`; `App.Relaunch`; app quits with its last window | `window.go:517`, `:825`, `:898`, `:1975`; `events.go:21-34`; `app.go:191`, `:577`, `:594`; `login.go:102`; `docs/app.md:70` — compiled, not run |
| 5 | Window-level pointer capture during a drag | Implicit: the element whose `HandleInput` took the press gets every move and the release, anywhere; its cursor holds | `ui/input.go:474-485`, `:576-585`, `:759`; `ui/runtime.go:1867`; `TestPoint5PointerCaptureDuringADrag` |
| 6 | Virtualized list scrolling both ways, programmatic offset | `ui.List(..).RowWidth(w).TrackScroll(&st)`; set `st.X`/`st.Y` (positive); `ListState.ScrollTo/ScrollIntoView/Visible` | `ui/list_builder.go:48`, `ui/scroll.go:5-19`, `:69`, `:484`, `ui/list.go:358-420`; `TestPoint6ListScrollsBothWaysByTheProgram` |
| 7 | Wavy underline, per-range highlight in painted text | `p.RichText` spans with `WavyUnderline`+`DecorationColor`, `Background`; or `p.Fill` before `p.Glyphs` | `ui/richtext.go:15-80`, `:325`; `TestPoint7HighlightAndWavyUnderline` |
| 8 | `reduce_motion`, app-clock phase-locked animation | `uikit.AnimationDelta` with `RepeatSynced()` (phase from `c.Now()` mod period) and GPUI's reduce-motion rule; mygo's `Loop` is not synced | `ui/preferences.go:7-16`, `ui/context.go:497-533`, `ui/widgets.go:71-85`; `TestPoint8ReduceMotionAndSyncedPhase` |

## 23. Build notes for agents

- `go.sum` at the repo root lacks two modules mygo's `ui` needs
  (`github.com/go-text/typesetting v0.3.5`, `golang.org/x/image v0.46.0`).
  Until the final `go mod tidy`, build UI packages with a scratch copy:
  `cp go.mod go.sum $S/; GOTOOLCHAIN=go1.27.3 go get -modfile=$S/go.mod
  github.com/egoist/mygo/ui@v0.9.2-0.20261115180743-31bf65e6ec29`, then
  `go test -modfile=$S/go.mod …`. The repo's own files stay untouched.
- `CGO_ENABLED=0` builds and tests all of this, Tester included.
