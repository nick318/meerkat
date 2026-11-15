package uikit

// A single-line text field for forms: the connection name, the URL, a
// password.
//
// Same shape as `sqleditor`, cut down to one line: the field owns the
// string and implements mygo's `ui.TextInputClient` (GPUI's
// `EntityInputHandler`), so the platform delivers typed text and IME
// edits, and a `Draw` callback shapes and paints the line. One line means
// no rows to map, but it does need a horizontal scroll offset, so a long
// URL keeps the caret in view.
//
// Offsets are byte offsets into the value and always sit on a character
// boundary.
//
// Go port:
//
//   - GPUI's entity is the `*TextField` the owner keeps in its state;
//     `Render::render` is `View`, built in every frame.
//   - GPUI's `cx.emit(TextFieldEvent)` and the owner's `cx.subscribe` are
//     `Subscribe`: a listener the field calls on the main thread.
//   - The `actions!` and their key bindings, scoped to the `TextField` key
//     context, are one table dispatched on the field's element with
//     `DispatchKeys`, so a key the field binds beats the same key bound by
//     anything around it, as the deepest context did in GPUI.
//   - The custom `FieldElement` is the field element's own `Draw`; its
//     `prepaint` (shaping and the scroll) runs inside that callback, which
//     also writes back what the Rust's `paint` wrote back.

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

// TextFieldAction is one of the field's actions (Rust: the `text_field`
// actions).
type TextFieldAction int

const (
	TextFieldBackspace TextFieldAction = iota
	TextFieldCancel
	TextFieldCopySelection
	TextFieldCutSelection
	TextFieldDelete
	TextFieldDeleteToBeginningOfLine
	TextFieldDeleteToEndOfLine
	TextFieldDeleteToNextWordEnd
	TextFieldDeleteToPreviousWordStart
	TextFieldMoveLeft
	TextFieldMoveRight
	TextFieldMoveToBeginningOfLine
	TextFieldMoveToEndOfLine
	TextFieldMoveToNextWordEnd
	TextFieldMoveToPreviousWordStart
	TextFieldNextField
	TextFieldPaste
	TextFieldSelectAll
	TextFieldSelectLeft
	TextFieldSelectRight
	TextFieldSelectToBeginningOfLine
	TextFieldSelectToEndOfLine
	TextFieldSelectToNextWordEnd
	TextFieldSelectToPreviousWordStart
	TextFieldSubmit
)

// textFieldKeyContext is the Rust `KEY_CONTEXT`. In the port the context is
// the field's element; the name stays for the docs and the inspector.
const textFieldKeyContext = "TextField"

// textFieldFontSize and textFieldLineHeight: 12px text on a 18px line, the
// design's form rhythm.
const (
	textFieldFontSize   float32 = 12.
	textFieldLineHeight float32 = 18.
)

// lineSpacing is the same 12-on-18 rhythm, as a ratio, for a `bare` field
// that sets its own text size.
const lineSpacing = textFieldLineHeight / textFieldFontSize

// caretMargin keeps this much room to the right of the caret, so the
// character being typed is never flush against the field's edge.
const caretMargin float32 = 2.

// mask is what a masked field paints in place of every character.
const mask = '•'

// TextFieldKeyBindings are the key bindings of every text field. The Rust
// bound them once at startup, scoped to the field's key context, so they
// never shadow the app's own bindings while a field has focus; here the
// field dispatches them on its own element, which is the same scope.
func TextFieldKeyBindings() []KeyBinding[TextFieldAction] {
	return []KeyBinding[TextFieldAction]{
		Bind("backspace", TextFieldBackspace),
		Bind("delete", TextFieldDelete),
		Bind("alt-backspace", TextFieldDeleteToPreviousWordStart),
		Bind("alt-delete", TextFieldDeleteToNextWordEnd),
		Bind("cmd-backspace", TextFieldDeleteToBeginningOfLine),
		Bind("cmd-delete", TextFieldDeleteToEndOfLine),
		Bind("left", TextFieldMoveLeft),
		Bind("right", TextFieldMoveRight),
		Bind("alt-left", TextFieldMoveToPreviousWordStart),
		Bind("alt-right", TextFieldMoveToNextWordEnd),
		Bind("cmd-left", TextFieldMoveToBeginningOfLine),
		Bind("cmd-right", TextFieldMoveToEndOfLine),
		Bind("home", TextFieldMoveToBeginningOfLine),
		Bind("end", TextFieldMoveToEndOfLine),
		// The emacs pair macOS honours in every text field.
		Bind("ctrl-a", TextFieldMoveToBeginningOfLine),
		Bind("ctrl-e", TextFieldMoveToEndOfLine),
		Bind("shift-left", TextFieldSelectLeft),
		Bind("shift-right", TextFieldSelectRight),
		Bind("alt-shift-left", TextFieldSelectToPreviousWordStart),
		Bind("alt-shift-right", TextFieldSelectToNextWordEnd),
		Bind("cmd-shift-left", TextFieldSelectToBeginningOfLine),
		Bind("cmd-shift-right", TextFieldSelectToEndOfLine),
		Bind("shift-home", TextFieldSelectToBeginningOfLine),
		Bind("shift-end", TextFieldSelectToEndOfLine),
		Bind("cmd-a", TextFieldSelectAll),
		Bind("cmd-c", TextFieldCopySelection),
		Bind("cmd-x", TextFieldCutSelection),
		Bind("cmd-v", TextFieldPaste),
		Bind("enter", TextFieldSubmit),
		Bind("tab", TextFieldNextField),
		Bind("escape", TextFieldCancel),
	}
}

// textFieldKeys is the table, parsed once.
var textFieldKeys = TextFieldKeyBindings()

// TextFieldEvent is what a field tells the form around it.
type TextFieldEvent int

const (
	// TextFieldChanged: the value changed.
	TextFieldChanged TextFieldEvent = iota
	// TextFieldSubmitted: Enter, the form should do whatever its primary
	// button does. (Rust: `TextFieldEvent::Submit`.)
	TextFieldSubmitted
	// TextFieldNextFieldEvent: Tab, the form should focus the next field.
	// (Rust: `TextFieldEvent::NextField`.)
	TextFieldNextFieldEvent
	// TextFieldCancelled: Escape, the form should close. (Rust:
	// `TextFieldEvent::Cancel`.)
	TextFieldCancelled
)

// String names the event as Rust's Debug did.
func (e TextFieldEvent) String() string {
	switch e {
	case TextFieldChanged:
		return "Changed"
	case TextFieldSubmitted:
		return "Submit"
	case TextFieldNextFieldEvent:
		return "NextField"
	case TextFieldCancelled:
		return "Cancel"
	}
	return "TextFieldEvent(?)"
}

// textRange is Rust's `Range<usize>` of byte offsets, end exclusive.
type textRange struct{ start, end int }

func (r textRange) isEmpty() bool { return r.start >= r.end }

// TextField is the field's state, kept by its owner across frames.
type TextField struct {
	focusHandle ui.Handle
	content     string
	placeholder string
	// A password field paints dots and never copies its value out.
	masked bool
	// A field with no chrome of its own, at this text size (nil for the
	// framed field). The palette's search line sits in a header that
	// already draws the border and the surface, so the field must add
	// neither.
	bare *float32
	// What the field would finish the value with, painted faint after it
	// and never part of the value. Whoever owns the field decides what
	// that is and what accepts it; the field only shows it.
	ghost             string
	selectedRange     textRange
	selectionReversed bool
	markedRange       *textRange
	// How far the line is scrolled left, so a long value keeps the caret
	// in view. Written by the paint after every frame.
	scroll     float32
	lastLayout *shapedLine
	// The line's box, relative to the field's element: the content box
	// inside the padding and the border.
	lastBounds  *ui.Rect
	isSelecting bool
	// Where the caret is in its blink.
	blink Blink

	listeners []func(*TextField, TextFieldEvent)
}

// BlinkState is the field's half of `Blinking`.
func (f *TextField) BlinkState() *Blink { return &f.blink }

// NewTextField is the Rust `TextField::new(placeholder, cx)`.
func NewTextField(placeholder string) *TextField {
	return &TextField{
		placeholder: placeholder,
		blink:       NewBlink(),
	}
}

// Masked paints dots in place of the value, for a password.
func (f *TextField) Masked() *TextField {
	f.masked = true
	return f
}

// Bare drops the border, the surface and the padding, and shapes the line
// at `fontSize`. For a field that sits inside a surface of its own — the
// palette's search line.
func (f *TextField) Bare(fontSize float32) *TextField {
	f.bare = &fontSize
	return f
}

// Subscribe adds a listener for the field's events (GPUI's
// `cx.subscribe(&field, …)`). Listeners run on the main thread, in the
// order they were added.
func (f *TextField) Subscribe(fn func(*TextField, TextFieldEvent)) {
	f.listeners = append(f.listeners, fn)
}

func (f *TextField) emit(e TextFieldEvent) {
	for _, fn := range f.listeners {
		fn(f, e)
	}
}

// FocusHandle is the field's focus identity (GPUI's `Focusable`):
// `field.FocusHandle().Focus()` is `window.focus(&field.focus_handle(cx))`.
func (f *TextField) FocusHandle() *ui.Handle { return &f.focusHandle }

func (f *TextField) Text() string { return f.content }

func (f *TextField) Trimmed() string { return strings.TrimFunc(f.content, unicode.IsSpace) }

func (f *TextField) IsEmpty() bool { return f.Trimmed() == "" }

func (f *TextField) SetText(text string) {
	f.content = text
	end := len(f.content)
	f.selectedRange = textRange{end, end}
	f.selectionReversed = false
	f.markedRange = nil
	f.emit(TextFieldChanged)
	f.touched()
}

func (f *TextField) Clear() { f.SetText("") }

// SetGhost shows what the value would be finished with, faint and after
// the caret. Pass an empty string to take it away.
//
// It is never part of the value: nothing here accepts it, reads it back or
// lets the caret into it. A masked field never shows one — a password must
// not be guessed at on screen.
func (f *TextField) SetGhost(ghost string) {
	if f.ghost == ghost {
		return
	}
	f.ghost = ghost
}

// touched puts the caret back on show. Every edit and every motion goes
// through here. (The redraw the Rust asked for with `cx.notify()` comes by
// itself: mygo draws a frame after the input that caused the edit.)
func (f *TextField) touched() {
	RestartBlink(f)
}

// --- selection -------------------------------------------------------

func (f *TextField) cursorOffset() int {
	if f.selectionReversed {
		return f.selectedRange.start
	}
	return f.selectedRange.end
}

func (f *TextField) moveTo(offset int) {
	offset = clampOffset(f.content, offset)
	f.selectedRange = textRange{offset, offset}
	f.selectionReversed = false
	f.touched()
}

func (f *TextField) selectTo(offset int) {
	offset = clampOffset(f.content, offset)
	if f.selectionReversed {
		f.selectedRange.start = offset
	} else {
		f.selectedRange.end = offset
	}
	if f.selectedRange.end < f.selectedRange.start {
		f.selectionReversed = !f.selectionReversed
		f.selectedRange = textRange{f.selectedRange.end, f.selectedRange.start}
	}
	f.touched()
}

func (f *TextField) deleteTo(offset int) {
	if f.selectedRange.isEmpty() {
		if offset == f.cursorOffset() {
			return
		}
		f.selectTo(offset)
	}
	f.replaceTextInRange(nil, "")
}

func (f *TextField) selected() string {
	r := clampRange(f.content, f.selectedRange)
	return f.content[r.start:r.end]
}

// --- actions ---------------------------------------------------------

// perform runs an action, as the Rust's `on_action` listeners did.
func (f *TextField) perform(c *ui.Context, action TextFieldAction) {
	switch action {
	case TextFieldBackspace:
		f.deleteTo(motion.previousBoundary(f.content, f.cursorOffset()))
	case TextFieldDelete:
		f.deleteTo(motion.nextBoundary(f.content, f.cursorOffset()))
	case TextFieldDeleteToPreviousWordStart:
		f.deleteTo(motion.previousWordStart(f.content, f.cursorOffset()))
	case TextFieldDeleteToNextWordEnd:
		f.deleteTo(motion.nextWordEnd(f.content, f.cursorOffset()))
	case TextFieldDeleteToBeginningOfLine:
		f.deleteTo(0)
	case TextFieldDeleteToEndOfLine:
		f.deleteTo(len(f.content))
	case TextFieldMoveLeft:
		if f.selectedRange.isEmpty() {
			f.moveTo(motion.previousBoundary(f.content, f.cursorOffset()))
		} else {
			f.moveTo(f.selectedRange.start)
		}
	case TextFieldMoveRight:
		if f.selectedRange.isEmpty() {
			f.moveTo(motion.nextBoundary(f.content, f.cursorOffset()))
		} else {
			f.moveTo(f.selectedRange.end)
		}
	case TextFieldMoveToPreviousWordStart:
		f.moveTo(motion.previousWordStart(f.content, f.cursorOffset()))
	case TextFieldMoveToNextWordEnd:
		f.moveTo(motion.nextWordEnd(f.content, f.cursorOffset()))
	case TextFieldMoveToBeginningOfLine:
		f.moveTo(0)
	case TextFieldMoveToEndOfLine:
		f.moveTo(len(f.content))
	case TextFieldSelectLeft:
		f.selectTo(motion.previousBoundary(f.content, f.cursorOffset()))
	case TextFieldSelectRight:
		f.selectTo(motion.nextBoundary(f.content, f.cursorOffset()))
	case TextFieldSelectToPreviousWordStart:
		f.selectTo(motion.previousWordStart(f.content, f.cursorOffset()))
	case TextFieldSelectToNextWordEnd:
		f.selectTo(motion.nextWordEnd(f.content, f.cursorOffset()))
	case TextFieldSelectToBeginningOfLine:
		f.selectTo(0)
	case TextFieldSelectToEndOfLine:
		f.selectTo(len(f.content))
	case TextFieldSelectAll:
		f.SelectEverything()
	case TextFieldCopySelection:
		f.copy(c)
	case TextFieldCutSelection:
		f.cut(c)
	case TextFieldPaste:
		f.paste(c)
	case TextFieldSubmit:
		f.emit(TextFieldSubmitted)
	case TextFieldNextField:
		f.emit(TextFieldNextFieldEvent)
	case TextFieldCancel:
		f.emit(TextFieldCancelled)
	}
}

// SelectEverything marks the whole value, as ⌘A does. A key that puts the
// focus on a field the user may have typed into before wants this: the
// next character replaces what is there rather than being appended to it.
func (f *TextField) SelectEverything() {
	f.moveTo(0)
	f.selectTo(len(f.content))
}

func (f *TextField) copy(c *ui.Context) {
	// A password never leaves the field.
	if f.masked {
		return
	}
	if selected := f.selected(); selected != "" {
		c.WriteClipboard(selected)
	}
}

func (f *TextField) cut(c *ui.Context) {
	if f.masked {
		return
	}
	if selected := f.selected(); selected != "" {
		c.WriteClipboard(selected)
		f.replaceTextInRange(nil, "")
	}
}

func (f *TextField) paste(c *ui.Context) {
	text := c.ReadClipboard()
	if text == "" {
		return
	}
	// One line: a pasted URL with a stray newline lands as one value.
	text = strings.NewReplacer("\n", "", "\r", "").Replace(text)
	f.replaceTextInRange(nil, text)
}

// --- mouse -----------------------------------------------------------

// onInput takes the pointer, as the Rust's mouse listeners did. Keys,
// text and edit commands are left alone (false): the keys go to the
// field's key table, the text to the TextInputClient, and an Edit-menu
// command comes back as its key (mygo ui/input.go:1463-1485,
// `commandKey`).
func (f *TextField) onInput(ev ui.InputEvent) bool {
	switch ev.Kind {
	case ui.InputPointerDown:
		if ev.Button != 0 {
			return false
		}
		f.onMouseDown(ev)
		return true
	case ui.InputPointerUp:
		f.isSelecting = false
		return ev.Button == 0
	case ui.InputPointerMove:
		if f.isSelecting {
			f.selectTo(f.offsetForPosition(ui.Point{X: ev.X, Y: ev.Y}))
			return true
		}
	}
	return false
}

func (f *TextField) onMouseDown(ev ui.InputEvent) {
	offset := f.offsetForPosition(ui.Point{X: ev.X, Y: ev.Y})
	switch {
	case ev.Clicks <= 1 && ev.Mods&ui.Shift != 0:
		f.selectTo(offset)
	case ev.Clicks <= 1:
		f.isSelecting = true
		f.moveTo(offset)
	case ev.Clicks == 2:
		word := motion.wordAt(f.content, offset)
		f.moveTo(word.start)
		f.selectTo(word.end)
	default:
		f.moveTo(0)
		f.selectTo(len(f.content))
	}
}

// --- offsets ---------------------------------------------------------

// displayText is what the paint draws: the value, or one dot per
// character.
func (f *TextField) displayText() string {
	if f.masked {
		return strings.Repeat(string(mask), utf8.RuneCountInString(f.content))
	}
	return f.content
}

// toDisplay maps a value offset into the painted string. The two agree
// character by character, so count characters across.
func (f *TextField) toDisplay(offset int) int {
	if !f.masked {
		return offset
	}
	offset = clampOffset(f.content, offset)
	return utf8.RuneCountInString(f.content[:offset]) * utf8.RuneLen(mask)
}

// fromDisplay is the inverse: a painted offset, back in the value.
func (f *TextField) fromDisplay(offset int) int {
	if !f.masked {
		return clampOffset(f.content, offset)
	}
	characters := offset / utf8.RuneLen(mask)
	n := 0
	for ix := range f.content {
		if n == characters {
			return ix
		}
		n++
	}
	return len(f.content)
}

func (f *TextField) offsetForPosition(position ui.Point) int {
	if f.lastBounds == nil || f.lastLayout == nil {
		return f.cursorOffset()
	}
	x := position.X - f.lastBounds.X + f.scroll
	return f.fromDisplay(f.lastLayout.closestIndexForX(x))
}

func (f *TextField) offsetFromUTF16(target int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range f.content {
		if utf16n >= target {
			break
		}
		utf16n += utf16Len(ch)
		utf8n += utf8.RuneLen(ch)
	}
	return utf8n
}

func (f *TextField) offsetToUTF16(target int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range f.content {
		if utf8n >= target {
			break
		}
		utf8n += utf8.RuneLen(ch)
		utf16n += utf16Len(ch)
	}
	return utf16n
}

func (f *TextField) rangeToUTF16(r textRange) ui.TextInputRange {
	return ui.TextInputRange{Start: f.offsetToUTF16(r.start), End: f.offsetToUTF16(r.end)}
}

func (f *TextField) rangeFromUTF16(r ui.TextInputRange) textRange {
	return textRange{f.offsetFromUTF16(r.Start), f.offsetFromUTF16(r.End)}
}

// utf16Len is Rust's char::len_utf16.
func utf16Len(ch rune) int {
	if ch >= 0x10000 {
		return 2
	}
	return 1
}

// --- ui.TextInputClient (GPUI's EntityInputHandler) -------------------

// TextForRange is `text_for_range`.
func (f *TextField) TextForRange(rangeUTF16 ui.TextInputRange) (string, ui.TextInputRange) {
	r := clampRange(f.content, f.rangeFromUTF16(rangeUTF16))
	return f.content[r.start:r.end], f.rangeToUTF16(r)
}

// Selection is `selected_text_range`.
func (f *TextField) Selection() ui.TextInputSelection {
	return ui.TextInputSelection{Range: f.rangeToUTF16(f.selectedRange), Reversed: f.selectionReversed}
}

// MarkedRange is `marked_text_range`.
func (f *TextField) MarkedRange() (ui.TextInputRange, bool) {
	if f.markedRange == nil {
		return ui.TextInputRange{}, false
	}
	return f.rangeToUTF16(*f.markedRange), true
}

// UnmarkText is `unmark_text`.
func (f *TextField) UnmarkText() { f.markedRange = nil }

// ReplaceText is `replace_text_in_range`.
func (f *TextField) ReplaceText(rangeUTF16 *ui.TextInputRange, newText string) {
	var r *textRange
	if rangeUTF16 != nil {
		v := f.rangeFromUTF16(*rangeUTF16)
		r = &v
	}
	f.replaceTextInRange(r, newText)
}

// replaceTextInRange is the Rust's `replace_text_in_range` with the UTF-16
// range already brought into the value; nil is the marked range, else the
// selection.
func (f *TextField) replaceTextInRange(byteRange *textRange, newText string) {
	var target textRange
	switch {
	case byteRange != nil:
		target = *byteRange
	case f.markedRange != nil:
		target = *f.markedRange
	default:
		target = f.selectedRange
	}
	r := clampRange(f.content, target)

	f.content = f.content[:r.start] + newText + f.content[r.end:]
	cursor := r.start + len(newText)
	f.selectedRange = textRange{cursor, cursor}
	f.selectionReversed = false
	f.markedRange = nil
	f.emit(TextFieldChanged)
	f.touched()
}

// SetMarkedText is `replace_and_mark_text_in_range`.
func (f *TextField) SetMarkedText(rangeUTF16 *ui.TextInputRange, newText string, newSelectedRangeUTF16 ui.TextInputRange) {
	var target textRange
	switch {
	case rangeUTF16 != nil:
		target = f.rangeFromUTF16(*rangeUTF16)
	case f.markedRange != nil:
		target = *f.markedRange
	default:
		target = f.selectedRange
	}
	r := clampRange(f.content, target)

	f.content = f.content[:r.start] + newText + f.content[r.end:]
	if newText != "" {
		f.markedRange = &textRange{r.start, r.start + len(newText)}
	} else {
		f.markedRange = nil
	}
	// The platform reports this selection relative to `newText`, so
	// convert it inside that string and then shift it into the value.
	start := r.start + utf16ToByte(newText, newSelectedRangeUTF16.Start)
	end := r.start + utf16ToByte(newText, newSelectedRangeUTF16.End)
	f.selectedRange = clampRange(f.content, textRange{start, end})
	f.emit(TextFieldChanged)
	f.touched()
}

// BoundsForRange is `bounds_for_range`, relative to the field's element.
func (f *TextField) BoundsForRange(rangeUTF16 ui.TextInputRange) (ui.Rect, ui.TextInputRange, bool) {
	line, bounds := f.lastLayout, f.lastBounds
	if line == nil || bounds == nil {
		return ui.Rect{}, rangeUTF16, false
	}
	r := clampRange(f.content, f.rangeFromUTF16(rangeUTF16))
	from := min(f.toDisplay(r.start), line.len())
	to := min(f.toDisplay(r.end), line.len())
	left := bounds.X + line.xForIndex(from) - f.scroll
	right := bounds.X + line.xForIndex(to) - f.scroll
	return ui.Rect{X: left, Y: bounds.Y, W: right - left, H: bounds.H}, rangeUTF16, true
}

// IndexForPoint is `character_index_for_point`.
func (f *TextField) IndexForPoint(point ui.Point) (int, bool) {
	return f.offsetToUTF16(f.offsetForPosition(point)), true
}

// --- view --------------------------------------------------------------

func (f *TextField) fontSize() float32 {
	if f.bare != nil {
		return *f.bare
	}
	return textFieldFontSize
}

func (f *TextField) lineHeight() float32 {
	if f.bare != nil {
		return *f.bare * lineSpacing
	}
	return textFieldLineHeight
}

// inset is how far the line sits inside the field's box: the padding and
// the 1px border of a framed field, nothing for a bare one.
func (f *TextField) inset() (x, y float32) {
	if f.bare != nil {
		return 0, 0
	}
	return 10 + 1, 7 + 1
}

// View builds the field (Rust: `Render::render`). Call it in every frame
// the field is on screen.
func (f *TextField) View(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors
	focused := f.focusHandle.Focused(c)
	// Focus belongs to the window, and this is the one place that has
	// one, so the blink is started and stopped from the edge here rather
	// than from a focus listener the constructor cannot install.
	TrackBlinkFocus(f, focused, c)

	el := ui.Box(c).
		Bind(&f.focusHandle).
		HandleTextInput(f).
		HandleInput(f.onInput).
		Cursor(ui.CursorText).
		FillWidth()
	if f.bare != nil {
		// A bare field draws nothing of its own: the surface around it
		// already carries the border and the padding.
		el.FontSize(*f.bare).FixedLineHeight(*f.bare * lineSpacing)
	} else {
		border, surface := colors.Border, colors.Elevated
		if focused {
			// The focused field is the one wearing the accent; every
			// other border on the screen stays a hairline.
			border, surface = colors.AccentFill, colors.Raised
		}
		el.Padding(7, 10).
			Border(1, border.Color()).
			Radius(7).
			Background(surface.Color()).
			FontSize(textFieldFontSize).
			FixedLineHeight(textFieldLineHeight)
	}
	// What is typed here is a name, a URL or a query — the kind of text
	// the app sets in monospace everywhere else.
	el.Font(theme.MonoFontFamily).TextColor(colors.TextBody.Color())

	// The `TextField` key context.
	if action, ok := DispatchKeys(el, textFieldKeys); ok {
		f.perform(c, action)
	}

	el.Draw(func(p *ui.Painter, r ui.Rect) { f.paint(p, r, focused, colors) })
	// The line's own box: as wide as the field, one line tall (the Rust
	// `FieldElement::request_layout`).
	return el.Children(func() { ui.Box(c).FillWidth().Height(f.lineHeight()) })
}

// paint is the Rust `FieldElement`: prepaint (shape, settle the scroll,
// place the selection, the caret and the ghost) and paint (clip to the
// line's box, draw, write back what the next input reads).
func (f *TextField) paint(p *ui.Painter, box ui.Rect, focused bool, colors *theme.ThemeColors) {
	ix, iy := f.inset()
	bounds := ui.Rect{X: box.X + ix, Y: box.Y + iy, W: box.W - 2*ix, H: f.lineHeight()}
	font := ui.Font{Family: theme.MonoFontFamily, Size: f.fontSize()}

	placeholder := f.content == ""
	text := f.displayText()
	color := colors.TextBody
	if placeholder {
		text = f.placeholder
		color = colors.TextFaint
	}
	line := f.shape(text, font)

	// Keep the caret inside the field: scroll only as far as it must.
	var cursorX float32
	if !placeholder {
		cursorX = line.xForIndex(min(f.toDisplay(f.cursorOffset()), line.len()))
	}
	width := bounds.W
	scroll := f.scroll
	if cursorX-scroll > width-caretMargin {
		scroll = cursorX - width + caretMargin
	}
	if cursorX < scroll {
		scroll = cursorX
	}
	overflow := max(line.width+caretMargin-width, 0)
	scroll = min(max(scroll, 0), overflow)

	originX := bounds.X - scroll
	metrics := font.Metrics()
	// GPUI centres the glyphs in the line height (text_system/line.rs,
	// `padding_top`).
	baseline := bounds.Y + (bounds.H-metrics.Ascent-metrics.Descent)/2 + metrics.Ascent

	// The line is scrolled, not cut: a value wider than the field is
	// shaped whole and painted from `-scroll`, so without a clip the part
	// past either edge lands on the padding and the border. The element's
	// own box includes the padding the text must not reach, so the paint
	// clips to the line's box here.
	p.Clip(bounds, 0, func() {
		if !placeholder && !f.selectedRange.isEmpty() {
			from := min(f.toDisplay(f.selectedRange.start), line.len())
			to := min(f.toDisplay(f.selectedRange.end), line.len())
			left := originX + line.xForIndex(from)
			p.Fill(ui.Rect{X: left, Y: bounds.Y, W: originX + line.xForIndex(to) - left, H: bounds.H}, colors.Selection.Color(), 0)
		}

		p.Glyphs(line.glyphs, originX, baseline, color.Color())
		// Underline the text the IME is still composing: the whole run,
		// as the Rust's single run did.
		if f.markedRange != nil && !placeholder {
			y := baseline + metrics.Descent*0.618
			p.Fill(ui.Rect{X: originX, Y: y, W: line.width, H: 1}, color.Color(), 0)
		}

		// The ghost hangs off the end of the value, not off the caret, so
		// it stays put while the caret walks back through the text. It is
		// shaped after the scroll is settled and never widens it: the
		// value is what has to stay in view, and a long suggestion must
		// not push it out.
		if !placeholder && !f.masked && f.ghost != "" {
			ghost := shapeLine(f.ghost, font)
			p.Glyphs(ghost.glyphs, originX+line.width, baseline, colors.TextFaint.Color())
		}

		// The off half of the blink simply has no caret to paint.
		if focused && f.blink.On() {
			p.Fill(ui.Rect{X: cursorX + originX, Y: bounds.Y, W: 1.5, H: bounds.H}, colors.Accent.Color(), 0)
		}
	})

	rel := ui.Rect{X: ix, Y: iy, W: bounds.W, H: bounds.H}
	f.lastLayout = line
	f.lastBounds = &rel
	f.scroll = scroll
}

// shape shapes the painted line, reusing the last one while nothing it
// depends on changed: mygo's `Shape` caches nothing, and the paint may
// run more than once a frame.
func (f *TextField) shape(text string, font ui.Font) *shapedLine {
	if l := f.lastLayout; l != nil && l.text == text && l.font == font {
		return l
	}
	return shapeLine(text, font)
}

// shapedLine is GPUI's `ShapedLine` for one run of one font: the glyphs
// and the byte offset each starts at.
type shapedLine struct {
	text   string
	font   ui.Font
	glyphs []ui.Glyph
	// index is the byte offset of each glyph's cluster (GPUI's
	// `glyph.index`).
	index []int
	width float32
}

func shapeLine(text string, font ui.Font) *shapedLine {
	l := &shapedLine{text: text, font: font}
	if text == "" {
		return l
	}
	l.glyphs = ui.Shape(text, font)
	byteOf := make([]int, 0, len(text)+1)
	for ix := range text {
		byteOf = append(byteOf, ix)
	}
	byteOf = append(byteOf, len(text))
	l.index = make([]int, len(l.glyphs))
	for i, g := range l.glyphs {
		l.index[i] = byteOf[min(g.Cluster, len(byteOf)-1)]
		l.width = max(l.width, g.X+g.Advance)
	}
	return l
}

func (l *shapedLine) len() int { return len(l.text) }

// xForIndex is GPUI's `x_for_index`: the x of the first glyph at or past
// the byte offset, else the line's width.
func (l *shapedLine) xForIndex(index int) float32 {
	for i, g := range l.glyphs {
		if l.index[i] >= index {
			return g.X
		}
	}
	return l.width
}

// closestIndexForX is GPUI's `closest_index_for_x`: the character
// boundary closest to x.
func (l *shapedLine) closestIndexForX(x float32) int {
	prevIndex := 0
	var prevX float32
	for i, g := range l.glyphs {
		if g.X >= x {
			if g.X-x < x-prevX {
				return l.index[i]
			}
			return prevIndex
		}
		prevIndex = l.index[i]
		prevX = g.X
	}
	if l.len() == 1 {
		if x > l.width/2. {
			return 1
		}
		return 0
	}
	return l.len()
}

// clampOffset pulls an offset onto a character boundary inside the value.
// The platform can hand back a range from one edit ago.
func clampOffset(text string, offset int) int {
	offset = min(max(offset, 0), len(text))
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	return offset
}

func clampRange(text string, r textRange) textRange {
	start := clampOffset(text, r.start)
	end := clampOffset(text, max(r.end, r.start))
	return textRange{start, end}
}

func utf16ToByte(text string, utf16Offset int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range text {
		if utf16n >= utf16Offset {
			break
		}
		utf16n += utf16Len(ch)
		utf8n += utf8.RuneLen(ch)
	}
	return min(utf8n, len(text))
}

// motion is caret motion over one line: character and word boundaries.
// The query editor has its own copy that also knows about line breaks; a
// field has none, so this one stays short. (Rust: `mod motion`; the
// methods keep its call sites, `motion.nextWordEnd(…)`.)
var motion motionFns

type motionFns struct{}

// isWordChar is what counts as part of a word. A URL is one word up to
// each punctuation mark, so ⌥← walks host, port and database in turn.
func (motionFns) isWordChar(ch rune) bool {
	return isAlphanumeric(ch) || ch == '_' || ch == '$'
}

// isAlphanumeric is Rust's char::is_alphanumeric: Alphabetic or Numeric.
func isAlphanumeric(ch rune) bool {
	return unicode.In(ch, unicode.L, unicode.Nl, unicode.Other_Alphabetic, unicode.Nd, unicode.No)
}

func (motionFns) previousBoundary(text string, offset int) int {
	if offset <= 0 {
		return 0
	}
	_, size := utf8.DecodeLastRuneInString(text[:offset])
	return offset - size
}

func (motionFns) nextBoundary(text string, offset int) int {
	if offset >= len(text) {
		return len(text)
	}
	_, size := utf8.DecodeRuneInString(text[offset:])
	return offset + size
}

func (motionFns) charBefore(text string, offset int) (rune, bool) {
	if offset <= 0 {
		return 0, false
	}
	ch, _ := utf8.DecodeLastRuneInString(text[:offset])
	return ch, true
}

func (motionFns) charAt(text string, offset int) (rune, bool) {
	if offset >= len(text) {
		return 0, false
	}
	ch, _ := utf8.DecodeRuneInString(text[offset:])
	return ch, true
}

// previousWordStart is the start of the word before `offset`, as ⌥← gives
// on macOS.
func (m motionFns) previousWordStart(text string, offset int) int {
	ix := offset
	for {
		ch, ok := m.charBefore(text, ix)
		if !ok || m.isWordChar(ch) {
			break
		}
		ix = m.previousBoundary(text, ix)
	}
	for {
		ch, ok := m.charBefore(text, ix)
		if !ok || !m.isWordChar(ch) {
			break
		}
		ix = m.previousBoundary(text, ix)
	}
	return ix
}

// nextWordEnd is the end of the word after `offset`, as ⌥→ gives on
// macOS.
func (m motionFns) nextWordEnd(text string, offset int) int {
	ix := offset
	for {
		ch, ok := m.charAt(text, ix)
		if !ok || m.isWordChar(ch) {
			break
		}
		ix = m.nextBoundary(text, ix)
	}
	for {
		ch, ok := m.charAt(text, ix)
		if !ok || !m.isWordChar(ch) {
			break
		}
		ix = m.nextBoundary(text, ix)
	}
	return ix
}

// wordAt is the word around `offset`, for a double click.
func (m motionFns) wordAt(text string, offset int) textRange {
	at, atOK := m.charAt(text, offset)
	before, beforeOK := m.charBefore(text, offset)
	inside := atOK && m.isWordChar(at) || beforeOK && m.isWordChar(before)
	if !inside {
		return textRange{offset, min(m.nextBoundary(text, offset), len(text))}
	}
	start := offset
	for {
		ch, ok := m.charBefore(text, start)
		if !ok || !m.isWordChar(ch) {
			break
		}
		start = m.previousBoundary(text, start)
	}
	end := offset
	for {
		ch, ok := m.charAt(text, end)
		if !ok || !m.isWordChar(ch) {
			break
		}
		end = m.nextBoundary(text, end)
	}
	return textRange{start, end}
}
