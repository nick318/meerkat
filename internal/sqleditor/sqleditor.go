// Package sqleditor is a small multi-line text editor for the query tab.
//
// Built on the same shape as `gpui/examples/input.rs`: the entity owns the
// buffer and implements `EntityInputHandler` so the platform delivers
// typed text and IME edits; a custom element shapes and paints the lines.
// This one is multi-line, so it keeps a shaped line per row and maps byte
// offsets to (row, column) both ways.
//
// It carries what a macOS editor is expected to have, and the keys are
// Zed's `default-macos.json` wherever this buffer has the thing the key
// names: word, subword, line, paragraph and page motion, selection for
// every motion, a goal column that ↑ and ↓ keep, line edits (indent,
// comment, move, duplicate, join, delete), undo and redo, and word and
// line selection that a drag carries on in the unit the click started.
// Colouring comes from a one-pass tokenizer that also knows the names in
// the connected database. There is no soft wrap.
//
// **Only what is on screen is shaped and laid out.** The first edition
// shaped every line and gave the gutter a row per line on every frame,
// so a pasted script of a thousand lines made each keystroke after it
// cost a thousand. The buffer keeps an index of its line starts, redone
// on each edit, and the gutter, the rail, the statistics and the text all
// paint `display::visible_rows` and nothing else.
//
// **The viewport follows the caret in `render`, before anything is
// painted**, so the frame an edit lands in is already scrolled to it. It
// used to be worked out in `paint` and take effect a frame late, which is
// a caret that is off screen for a frame after every paste.
//
// ⌘F opens a find strip over the buffer: every hit washed, the one the
// walk stands on selected, ⏎ and ⇧⏎ (or ⌘G and ⇧⌘G) stepping between
// them. The strip is `ui::find_bar`, the one the results grid wears, and
// it is painted **beside** the editor's key context rather than inside it
// — `enter` there is a line break, and would be taken from the search
// line.
//
// Offsets are byte offsets into the buffer and always sit on a character
// boundary.
//
// Go port (see docs/tasks/gpui-to-mygo.md):
//
//   - GPUI's entity is the `*SqlEditor` the owner keeps; `Render::render`
//     is `View`, built in every frame the editor is on screen.
//   - `EntityInputHandler` is mygo's `ui.TextInputClient`, on the editor's
//     element; its rectangles and points are relative to that element.
//   - The `actions!` and `key_bindings()` are tables dispatched through
//     `uikit.DispatchKeys` on the element that wore each key context: the
//     editor's element (`SqlEditor`, and `SqlEditor && completing` ahead of
//     it while the panel is open) and the find strip's (`SqlEditorFind`).
//   - The custom `EditorElement` is the text box's `Draw`; its `prepaint`
//     (shaping, quads) and `paint` both run there.
//   - `cx.emit(SqlEditorEvent::Changed)` is `Subscribe`. GPUI delivered an
//     event after the update that emitted it; here it is delivered at the
//     end of the edit (`touched`), so a listener reads the new text.
//   - The window-level drag listeners are mygo's pointer capture: the
//     editor's element takes the press, and every move and the release
//     come to it until the button comes up.
package sqleditor

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/fuzzy"
	"github.com/nick318/meerkat/internal/query"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// Range is a byte range `Start..End` into the buffer, end exclusive: the
// port of `std::ops::Range<usize>`. It has `query.Range`'s and
// `fuzzy.Range`'s shape, so either converts with `Range(r)`.
type Range struct {
	Start int
	End   int
}

// IsEmpty reports whether the range holds nothing, as `Range::is_empty`.
func (r Range) IsEmpty() bool {
	return !(r.Start < r.End)
}

// Action is one of the editor's actions (Rust: the `sql_editor` actions).
type Action int

const (
	Backspace Action = iota
	Cancel
	Copy
	ConfirmCompletion
	Cut
	DismissCompletion
	Delete
	DeleteLine
	DeleteToBeginningOfLine
	DeleteToEndOfLine
	DeleteToNextSubwordEnd
	DeleteToNextWordEnd
	DeleteToPreviousSubwordStart
	DeleteToPreviousWordStart
	DuplicateLineDown
	DuplicateLineUp
	Find
	FindNext
	FindPrevious
	Indent
	IndentLines
	JoinLines
	KillToEndOfLine
	MoveDown
	MoveLeft
	MoveLineDown
	MoveLineUp
	MovePageDown
	MovePageUp
	MoveRight
	MoveToBeginning
	MoveToBeginningOfLine
	MoveToEnd
	MoveToEndOfLine
	MoveToEndOfParagraph
	MoveToNextSubwordEnd
	MoveToNextWordEnd
	MoveToPreviousSubwordStart
	MoveToPreviousWordStart
	MoveToStartOfParagraph
	MoveUp
	Newline
	NextCompletion
	OpenCompletion
	Outdent
	Paste
	PreviousCompletion
	Redo
	ScrollCursorCenter
	SelectAll
	SelectDown
	SelectLeft
	SelectLine
	SelectNext
	SelectPageDown
	SelectPageUp
	SelectRight
	SelectToBeginning
	SelectToBeginningOfLine
	SelectToEnd
	SelectToEndOfLine
	SelectToEndOfParagraph
	SelectToNextSubwordEnd
	SelectToNextWordEnd
	SelectToPreviousSubwordStart
	SelectToPreviousWordStart
	SelectToStartOfParagraph
	SelectUp
	ToggleComment
	Transpose
	Undo
)

// ContextBinding is one of GPUI's `KeyBinding::new(keystroke, action,
// Some(context))`: the binding, and the key context it was scoped to.
type ContextBinding struct {
	Context string
	Binding uikit.KeyBinding[Action]
}

// KeyBindings are the key bindings for the editor, following Zed's macOS
// keymap. They are scoped to the editor's key contexts so they never
// shadow the app's own bindings. (In the port the editor dispatches them
// itself, on the elements that carry those contexts; the list is kept
// whole, in the Rust's order, so it can be read against Zed's.)
func KeyBindings() []ContextBinding {
	bind := func(keystroke string, action Action) ContextBinding {
		return ContextBinding{keyContext, uikit.Bind(keystroke, action)}
	}
	finding := func(keystroke string, action Action) ContextBinding {
		return ContextBinding{findKeyContext, uikit.Bind(keystroke, action)}
	}
	completing := func(keystroke string, action Action) ContextBinding {
		return ContextBinding{completingContext, uikit.Bind(keystroke, action)}
	}
	return []ContextBinding{
		// Zed's `default-macos.json`, the `Editor` context, less what this
		// buffer has no use for (multiple cursors, syntax nodes, an LSP).
		// ⌘⏎ is left to the shell: here it runs the statement.
		bind("escape", Cancel),
		bind("backspace", Backspace),
		bind("shift-backspace", Backspace),
		bind("ctrl-h", Backspace),
		bind("delete", Delete),
		bind("ctrl-d", Delete),
		bind("alt-backspace", DeleteToPreviousWordStart),
		bind("ctrl-w", DeleteToPreviousWordStart),
		bind("alt-delete", DeleteToNextWordEnd),
		bind("ctrl-alt-backspace", DeleteToPreviousSubwordStart),
		bind("ctrl-alt-h", DeleteToPreviousSubwordStart),
		bind("ctrl-alt-delete", DeleteToNextSubwordEnd),
		bind("ctrl-alt-d", DeleteToNextSubwordEnd),
		bind("cmd-backspace", DeleteToBeginningOfLine),
		bind("cmd-delete", DeleteToEndOfLine),
		bind("ctrl-k", KillToEndOfLine),
		bind("ctrl-t", Transpose),
		bind("left", MoveLeft),
		bind("ctrl-b", MoveLeft),
		bind("right", MoveRight),
		bind("ctrl-f", MoveRight),
		bind("up", MoveUp),
		bind("ctrl-p", MoveUp),
		bind("down", MoveDown),
		bind("ctrl-n", MoveDown),
		bind("pageup", MovePageUp),
		bind("pagedown", MovePageDown),
		bind("ctrl-shift-v", MovePageUp),
		bind("ctrl-v", MovePageDown),
		bind("ctrl-up", MoveToStartOfParagraph),
		bind("ctrl-down", MoveToEndOfParagraph),
		bind("alt-left", MoveToPreviousWordStart),
		bind("alt-right", MoveToNextWordEnd),
		bind("ctrl-alt-left", MoveToPreviousSubwordStart),
		bind("ctrl-alt-b", MoveToPreviousSubwordStart),
		bind("ctrl-alt-right", MoveToNextSubwordEnd),
		bind("ctrl-alt-f", MoveToNextSubwordEnd),
		bind("cmd-left", MoveToBeginningOfLine),
		bind("cmd-right", MoveToEndOfLine),
		bind("home", MoveToBeginningOfLine),
		bind("end", MoveToEndOfLine),
		bind("ctrl-a", MoveToBeginningOfLine),
		bind("ctrl-e", MoveToEndOfLine),
		bind("cmd-up", MoveToBeginning),
		bind("cmd-down", MoveToEnd),
		bind("cmd-home", MoveToBeginning),
		bind("cmd-end", MoveToEnd),
		bind("ctrl-l", ScrollCursorCenter),
		bind("shift-left", SelectLeft),
		bind("ctrl-shift-b", SelectLeft),
		bind("shift-right", SelectRight),
		bind("ctrl-shift-f", SelectRight),
		bind("shift-up", SelectUp),
		bind("ctrl-shift-p", SelectUp),
		bind("shift-down", SelectDown),
		bind("ctrl-shift-n", SelectDown),
		bind("shift-pageup", SelectPageUp),
		bind("shift-pagedown", SelectPageDown),
		bind("ctrl-shift-up", SelectToStartOfParagraph),
		bind("ctrl-shift-down", SelectToEndOfParagraph),
		bind("alt-shift-left", SelectToPreviousWordStart),
		bind("alt-shift-right", SelectToNextWordEnd),
		bind("ctrl-alt-shift-left", SelectToPreviousSubwordStart),
		bind("ctrl-alt-shift-b", SelectToPreviousSubwordStart),
		bind("ctrl-alt-shift-right", SelectToNextSubwordEnd),
		bind("ctrl-alt-shift-f", SelectToNextSubwordEnd),
		bind("cmd-shift-left", SelectToBeginningOfLine),
		bind("cmd-shift-right", SelectToEndOfLine),
		bind("shift-home", SelectToBeginningOfLine),
		bind("shift-end", SelectToEndOfLine),
		bind("ctrl-shift-a", SelectToBeginningOfLine),
		bind("ctrl-shift-e", SelectToEndOfLine),
		bind("cmd-shift-up", SelectToBeginning),
		bind("cmd-shift-down", SelectToEnd),
		bind("cmd-a", SelectAll),
		bind("cmd-l", SelectLine),
		bind("cmd-d", SelectNext),
		bind("enter", Newline),
		bind("shift-enter", Newline),
		bind("tab", Indent),
		bind("shift-tab", Outdent),
		bind("cmd-]", IndentLines),
		bind("cmd-[", Outdent),
		bind("cmd-/", ToggleComment),
		bind("alt-up", MoveLineUp),
		bind("alt-down", MoveLineDown),
		bind("alt-shift-up", DuplicateLineUp),
		bind("alt-shift-down", DuplicateLineDown),
		bind("cmd-shift-k", DeleteLine),
		bind("ctrl-j", JoinLines),
		bind("cmd-z", Undo),
		bind("cmd-shift-z", Redo),
		bind("cmd-v", Paste),
		bind("cmd-c", Copy),
		bind("cmd-x", Cut),
		bind("ctrl-space", OpenCompletion),
		// Finding text. ⌘G and ⇧⌘G step with the strip closed as well, over
		// what it last searched for — the macOS habit, and the way back to
		// a hit after ⎋ put the keys back in the buffer.
		bind("cmd-f", Find),
		bind("cmd-g", FindNext),
		bind("cmd-shift-g", FindPrevious),
		// The strip's own keys. ⏎ and ⎋ are the search line's `Submit` and
		// `Cancel`, as they are for every other search line in the app.
		finding("cmd-f", Find),
		finding("cmd-g", FindNext),
		finding("cmd-shift-g", FindPrevious),
		finding("shift-enter", FindPrevious),
		// Registered last, and scoped to the completing context, so they
		// outrank the caret bindings above only while the panel is open.
		completing("down", NextCompletion),
		completing("ctrl-n", NextCompletion),
		completing("up", PreviousCompletion),
		completing("ctrl-p", PreviousCompletion),
		completing("enter", ConfirmCompletion),
		completing("tab", ConfirmCompletion),
		completing("escape", DismissCompletion),
	}
}

const keyContext = "SqlEditor"

// completingContext is only true while the completion panel is open, so
// ↑↓/enter/tab keep their ordinary meaning the rest of the time.
const completingContext = "SqlEditor && completing"

// findKeyContext is the find strip's own context. It is not inside
// `keyContext`, so none of the editor's keys reach the search line.
const findKeyContext = "SqlEditorFind"

// The tables each context dispatches, cut out of `KeyBindings` once.
//
// GPUI ranked `SqlEditor && completing` above `SqlEditor` because the
// context matched deeper; one element carries both here, so the
// completing table goes first and the first binding a key matches wins.
//
// ⎋ with nothing selected is not the editor's (`cx.propagate()`): mygo
// has no propagation once an element claims a key, so the editor claims ⎋
// only while there is a selection to drop, and the key goes on to whoever
// wants it otherwise.
var (
	editorKeys, editorKeysNoCancel, completingEditorKeys, findKeys []uikit.KeyBinding[Action]
)

func init() {
	var completingKeys []uikit.KeyBinding[Action]
	for _, b := range KeyBindings() {
		switch b.Context {
		case keyContext:
			editorKeys = append(editorKeys, b.Binding)
			if b.Binding.Action != Cancel {
				editorKeysNoCancel = append(editorKeysNoCancel, b.Binding)
			}
		case findKeyContext:
			findKeys = append(findKeys, b.Binding)
		case completingContext:
			completingKeys = append(completingKeys, b.Binding)
		}
	}
	completingEditorKeys = append(slices.Clone(completingKeys), editorKeys...)
}

// The completion popup keeps a readable width without stretching to fit
// a long column name.
const (
	menuMinWidth float32 = 220.
	menuMaxWidth float32 = 380.
)

// menuGap is the gap between the caret's line and the popup.
const menuGap float32 = 4.

// indentUnit is what one tab inserts, matching the design comp's indented
// SQL. (Rust: `INDENT`.)
const indentUnit = "  "

// caretWidth is the caret's width. Zed's bar is two pixels, and one and a
// half was a line the eye lost against a wash.
const caretWidth float32 = 2.

// fontSize and lineHeight: 12px text on the comp's 1.75 line height.
const (
	fontSize   float32 = 12.
	lineHeight float32 = 21.
)

const gutterWidth float32 = 64.

// markWidth is the lane of the gutter that carries a statement's mark,
// left of the line numbers. Wide enough for the widest of them with air
// either side.
const markWidth float32 = 20.

// markSize is how big a mark is drawn. It is read beside 12px text at a
// glance, from the other side of the pane, so it is a shade *larger* than
// the type rather than a hint tucked under it — the first version was 8px
// and disappeared into the gutter.
const markSize float32 = 12.

// railWidth is the rail between the gutter and the text: one column of
// colour saying which lines belong to which statement, and how that
// statement ended. The mark alone sits on the statement's first line, and
// a statement is often several lines long — the rail is what says where it
// reaches to.
const railWidth float32 = 2.

// markBreath: the running mark breathes rather than spins: the run button
// already says "working" by mixing a tone in and back out, and a shape
// turning in the gutter of a text editor is a lot of movement for one 8px
// dot.
const markBreath = 1400 * time.Millisecond

// markBreathDepth is how far down the breath takes the dot. It never
// leaves the screen: a mark that blinks out is one the eye reads as gone
// rather than as busy.
const markBreathDepth float32 = 0.55

// charWidth is one character's advance. The font is monospaced, so a
// line's width is its character count times this — JetBrains Mono advances
// 0.6em, which is exactly what the text system reports for it. It sizes
// the scrollable width only, so a double-width script costs a little
// scroll travel and nothing else.
const charWidth float32 = fontSize * 0.6

const (
	textPaddingX float32 = 16.
	textPaddingY float32 = 12.
)

// undoDepth is deep enough for a long editing session, bounded so a
// runaway paste loop cannot grow the history without limit.
const undoDepth = 256

// StatementStatus is how far one statement of the last run got.
//
// A buffer is several statements and a run sends them in turn, so "did
// that work" has one answer per statement rather than one for the run.
// The gutter is where that answer belongs: the user is reading the
// statements there, and a line of prose under the grid cannot point at
// the third of five.
type StatementStatus int

const (
	// StatementQueued is sent nowhere yet: the statements ahead of it are
	// still running. The default.
	StatementQueued StatementStatus = iota
	// StatementRunning: the server has this one.
	StatementRunning
	// StatementDone: it came back.
	StatementDone
	// StatementFailed: the server refused it. It is the last statement a
	// run reaches.
	StatementFailed
	// StatementSkipped: never sent, because the statement before it
	// failed or the user stopped the run.
	StatementSkipped
)

// StatementKind is what kind of statement a line belongs to, for the one
// kind that is painted apart from the rest.
//
// A `CREATE`, `ALTER`, `DROP` or `TRUNCATE` changes something the user is
// looking at elsewhere — the sidebar, the next query's columns — and
// answers with neither rows nor a count, so it is the one statement whose
// effect is invisible from the result pane. The comp gives it a cool teal
// family of its own against the warm paper, a `DDL` chip on its first
// line, and a wash over every line it covers, so the statement that
// changes the shape of the database reads as such from across the pane.
//
// **It is read off the buffer, not off a run.** The kind is a property of
// the text — `query::ddl_verb` over `query::statement_ranges` — so a
// pasted script wears the family the moment it lands, before anything is
// sent, and the teal is a warning as much as a report. The run's marks
// are laid over it; they never decide it.
type StatementKind int

const (
	StatementPlain StatementKind = iota
	StatementDdl
)

// StatementMark is one statement of the last run, and where its text sits
// in the buffer.
type StatementMark struct {
	Range  Range
	Status StatementStatus
	// The statistic beside the statement, on its first line and at the
	// right of the pane: `6 rows · 34 ms`, `1,204 rows deleted · 61 ms`,
	// `altered · 12 ms`, `failed · 8 ms`, `not run · statement 3 failed`.
	// Whoever owns the editor decides the words; the editor paints them
	// in the status's colour. Empty paints nothing.
	Meta string
}

// Diagnostic is one thing the server refused to parse, and where it sits
// in the buffer.
//
// The editor holds these rather than the message alone, because a mark
// under the token is the whole point: a line of prose under the pane
// cannot point at the third statement of five, which is the same reason
// [StatementMark] exists.
type Diagnostic struct {
	Range   Range
	Message string
}

// SqlEditorEvent is what the editor tells whoever owns it.
//
// One event, and it is the text: a syntax check has to run again when the
// buffer changes and must not run again when the caret merely moves.
type SqlEditorEvent int

const (
	Changed SqlEditorEvent = iota
)

type SqlEditor struct {
	focusHandle ui.Handle
	// Where the buffer is scrolled down to. The gutter is inside this one,
	// so the line numbers travel with the text.
	scrollHandle uikit.ScrollHandle
	// Where it is scrolled across to. A second container, holding the text
	// alone: a long line must not push the line numbers off the left of
	// the pane, which one container over both would do.
	hScrollHandle uikit.ScrollHandle
	// Which of the two scrollbars is being dragged, if either.
	scrollDrag uikit.DragState
	content    string
	// Byte offset where each line of `content` starts. Redone by
	// `reindex` on every edit, so a row is a binary search and the line
	// count is a length rather than a walk over the buffer per frame.
	lineStarts []int
	// Which lines the buffer's shape-changing statements cover: see
	// [lineShape]. Read off the text, so it is redone with the index.
	shapeLines []shapeMark
	// Whether any line is in `shapeLines`, so a frame need not look.
	anyShape bool
	// The widest line, in columns, for the scrollable width.
	widest int
	// Whether the buffer is all ASCII, and otherwise (when it is valid
	// UTF-8) the UTF-16 offset each line starts at: the two things that
	// keep a UTF-16 offset from being a walk over the whole buffer.
	// Redone with the index.
	ascii     bool
	lineUTF16 []int
	// The column ↑ and ↓ are holding to. A walk down through a short
	// line comes back out to where it started, as it does in every
	// editor; any other motion or edit forgets it. Nil is `None`.
	goalColumn *int
	// What a drag extends by: the unit the click that started it took.
	selectMode selectMode
	// The line ⌘C copied with nothing selected, while it is still what
	// the clipboard holds: pasting it back puts a line above the caret's
	// rather than splitting the caret's line, as Zed does.
	lineClip          *string
	placeholder       string
	vocabulary        *Vocabulary
	selectedRange     Range
	selectionReversed bool
	markedRange       *Range
	undo              []snapshot
	redo              []snapshot
	// What the last edit was, so a run of typing collapses into one undo
	// step instead of one step per character.
	lastEdit   editKind
	lastLayout *editorLayout
	// The text element's box, in window DIPs.
	lastBounds  *ui.Rect
	isSelecting bool
	// Set whenever the caret moves, cleared once the viewport follows it.
	pendingAutoscroll bool
	// Candidates for the word under the caret. Empty means the panel is
	// closed, which is also what drives the `completing` key context.
	completions  []Completion
	completionIx int
	// The word the candidates would replace.
	completionRange Range
	// Escape closes the panel until the next edit, so it does not spring
	// straight back open on the next keystroke.
	completionsDismissed bool
	// The statements of the last run, in the order they were sent, and
	// how far each got. Empty means nothing has been run over this
	// buffer — or that the buffer has been edited since, which is the
	// same thing to the gutter: the ranges are byte offsets into the
	// text that was sent, and an edit moves the text out from under
	// them. So **every edit drops them**, rather than paint a tick
	// beside a line the user has since rewritten.
	statements []StatementMark
	// statementsRev counts every change to `statements`, so the gutter's
	// per-line marks can be kept from one frame to the next rather than
	// worked out again on every frame (see cachedLineMarks).
	statementsRev uint64
	// marksCache is what lineMarks last answered, for `marksContent`
	// at `marksRev`.
	marksCache   []*lineMark
	marksContent string
	marksRev     uint64
	// What the last syntax check found, if anything. Empty is both
	// "nothing is wrong" and "nobody has checked since the last edit",
	// and the editor need not tell them apart: it paints marks, and there
	// are none either way. **Every edit drops these**, for the reason it
	// drops `statements` — the ranges name text the edit has moved.
	diagnostics []Diagnostic
	// Where the caret is in its blink.
	blink uikit.Blink
	// The find strip, while it is open.
	find *findStrip
	// What the strip last searched for, kept when it closes so ⌘G can go
	// on stepping and ⌘F can open on it again.
	lastNeedle string
	// Every hit of the open strip, worked out in `render` from the buffer
	// and the line as they are this frame, for the element to paint.
	// Never kept past a frame: an edit moves the text under them.
	hits []Range
	// The strip stopped counting at `find_bar::MAX_HITS`.
	hitsCapped bool
	// The last search, kept while the buffer and the needle are the
	// ones it was made over (see searchCapped).
	hitMemo hitMemo
	// The selection the find walk put there, while it is still the
	// selection.
	//
	// **A hit is selected, and a run over a selection runs the selection**
	// — so without this, ⌘F `delete from orders` and then ⌘⏎ would send
	// `DELETE FROM orders` with the `WHERE` after it left behind. A
	// selection the user did not make is not a request to run part of the
	// buffer, so [SqlEditor.RunSource] and [SqlEditor.SelectedText]
	// read past it. Any edit or motion clears it, through `touched`.
	found *Range

	// Go port: the editor element's box in window DIPs, so a pointer
	// position (relative to that element in mygo) can be read in the
	// window's coordinates the layout is kept in.
	origin ui.Point
	// Go port: GPUI's `cx.emit` listeners, and the `Changed` waiting to be
	// told at the end of the edit that caused it.
	listeners      []func(*SqlEditor, SqlEditorEvent)
	pendingChanged bool
	// Go port: mygo's `Shape` caches nothing; the lines of the last paint
	// (and of the caret's line), by their expanded text, so a line that
	// did not change is neither tokenized nor shaped again on the next
	// frame. See layout.go.
	lines map[string]*lineCache
}

// lineCacheFor is the kept state of one expanded line, made if no frame
// kept it.
func (e *SqlEditor) lineCacheFor(text string) *lineCache {
	font := editorFont()
	if lc := e.lines[text]; lc != nil && lc.font == font {
		return lc
	}
	lc := newLineCache(text, font)
	if e.lines == nil {
		e.lines = map[string]*lineCache{}
	}
	e.lines[text] = lc
	return lc
}

// findStrip is the open find strip. What it found is not kept here — see
// `hits`.
type findStrip struct {
	query *uikit.TextField
}

// BlinkState is the editor's half of `Blinking`.
func (e *SqlEditor) BlinkState() *uikit.Blink { return &e.blink }

// Subscribe adds a listener for the editor's events (GPUI's
// `cx.subscribe(&editor, …)`). Listeners run on the main thread, in the
// order they were added.
func (e *SqlEditor) Subscribe(fn func(*SqlEditor, SqlEditorEvent)) {
	e.listeners = append(e.listeners, fn)
}

func (e *SqlEditor) emit(ev SqlEditorEvent) {
	for _, fn := range e.listeners {
		fn(e, ev)
	}
}

// selectModeKind is the unit a click selected, and so the unit a drag from
// it extends by.
type selectModeKind int

const (
	selectCharacter selectModeKind = iota
	// A double click: the word it took, which the drag never gives up.
	selectWord
	// A triple click: likewise the line.
	selectLine
)

// selectMode is Rust's `enum SelectMode`: the kind, and the range a word
// or a line mode holds.
type selectMode struct {
	kind  selectModeKind
	value Range
}

type snapshot struct {
	content           string
	selectedRange     Range
	selectionReversed bool
}

type editKind int

const (
	// editNone: nothing to join onto: the next edit always starts a new
	// undo step.
	editNone editKind = iota
	editInsert
	editDelete
)

// lineLayout is one shaped line, and the map between the buffer's bytes
// and what was shaped: a tab is one byte and several columns.
type lineLayout struct {
	// What is painted: the whole line shaped, or — for a long line on the
	// mono grid — the window of it around the clip. See layout.go.
	shaped *shapedLine
	// Where `shaped`'s x = 0 sits from the line's left edge: the window's
	// first column for a grid line, nothing otherwise.
	paintX float32
	// The expanded text, and its grid when it is placed on one: a grid
	// line's positions are arithmetic, not a walk over glyphs.
	text string
	grid *gridLine
	tabs Expanded
	// Go port: the runs the line was shaped with (GPUI's `TextRun`s),
	// in shaped-text offsets, for the paint to colour and underline. For
	// a long line, only the runs that reach the glyphs painted.
	runs []textRun
}

// displayX is where offset `d` of the expanded text sits from the line's
// left edge.
func (l *lineLayout) displayX(d int) float32 {
	if l.grid != nil {
		return l.grid.xForIndex(l.text, d)
	}
	return l.shaped.xForIndex(d)
}

// xFor is where byte `ix` of the line sits, from the line's left edge.
func (l *lineLayout) xFor(ix int) float32 {
	return l.displayX(l.tabs.ToDisplay(ix))
}

// indexForX is the byte of the line nearest to `x`.
func (l *lineLayout) indexForX(x float32) int {
	if l.grid != nil {
		return l.tabs.ToBuffer(l.grid.closestIndexForX(l.text, x))
	}
	return l.tabs.ToBuffer(l.shaped.closestIndexForX(x))
}

// editorLayout is what the last paint produced, kept so mouse hits and IME
// rectangles can be answered without shaping the text again. It holds the
// rows that were on screen and no others.
type editorLayout struct {
	// The buffer row `lines[0]` is.
	firstRow   int
	lines      []lineLayout
	lineHeight float32
}

func (l *editorLayout) line(row int) *lineLayout {
	if row < l.firstRow || row-l.firstRow >= len(l.lines) {
		return nil
	}
	return &l.lines[row-l.firstRow]
}

// rows is the rows this layout holds.
func (l *editorLayout) rows() Range {
	return Range{l.firstRow, l.firstRow + len(l.lines)}
}

// New is Rust's `SqlEditor::new(content, vocabulary, cx)`. A nil
// vocabulary is an empty one.
func New(content string, vocabulary *Vocabulary) *SqlEditor {
	if vocabulary == nil {
		vocabulary = &Vocabulary{}
	}
	content = normalize(content)
	end := len(content)
	editor := &SqlEditor{
		content:       content,
		selectMode:    selectMode{kind: selectCharacter},
		placeholder:   "select * from",
		vocabulary:    vocabulary,
		selectedRange: Range{end, end},
		lastEdit:      editNone,
		blink:         uikit.NewBlink(),
	}
	editor.reindex()
	return editor
}

// reindex works out again everything that is read off the text: where its
// lines start, which of them a shape-changing statement covers, and how
// wide the widest is. One walk per edit, in place of one per frame for
// each of them.
func (e *SqlEditor) reindex() {
	e.lineStarts = e.lineStarts[:0]
	e.lineStarts = append(e.lineStarts, 0)
	// The same walk measures the lines: one that is ASCII with no tab is
	// as many columns wide as it is long, and only the others are
	// counted a character at a time (`widestLine`'s rule).
	var high byte
	widest, start, plain := 0, 0, true
	columns := func(end int) int {
		if plain {
			return end - start
		}
		return columnOf(e.content[start:end], end-start)
	}
	for ix := 0; ix < len(e.content); ix++ {
		b := e.content[ix]
		high |= b
		switch {
		case b == '\n':
			widest = max(widest, columns(ix))
			e.lineStarts = append(e.lineStarts, ix+1)
			start, plain = ix+1, true
		case b == '\t' || b >= utf8.RuneSelf:
			plain = false
		}
	}
	widest = max(widest, columns(len(e.content)))
	// What the UTF-16 offsets the platform speaks in need: nothing for
	// an ASCII buffer, where they are the byte offsets, and otherwise
	// where each line starts in UTF-16.
	e.ascii = high < utf8.RuneSelf
	e.lineUTF16 = e.lineUTF16[:0]
	if !e.ascii && utf8.ValidString(e.content) {
		utf16n := 0
		for row, start := range e.lineStarts {
			e.lineUTF16 = append(e.lineUTF16, utf16n)
			end := len(e.content)
			if row+1 < len(e.lineStarts) {
				end = e.lineStarts[row+1]
			}
			for _, ch := range e.content[start:end] {
				utf16n += utf16Len(ch)
			}
		}
	}
	var shape []Range
	for _, r := range query.StatementRanges(e.content) {
		if _, ok := query.DdlVerbOf(e.content[r.Start:r.End]); ok {
			shape = append(shape, Range(r))
		}
	}
	e.shapeLines = lineShapeAt(e.lineStarts, shape)
	e.anyShape = len(shape) > 0
	if e.content == "" {
		e.widest = widestLine(e.placeholder)
	} else {
		e.widest = widest
	}
}

func (e *SqlEditor) rowOf(offset int) int {
	return rowForOffset(e.lineStarts, offset)
}

// lineRange is the bytes of one line, without its break.
func (e *SqlEditor) lineRange(row int) Range {
	start := 0
	if row >= 0 && row < len(e.lineStarts) {
		start = e.lineStarts[row]
	}
	end := len(e.content)
	if row+1 >= 0 && row+1 < len(e.lineStarts) {
		end = e.lineStarts[row+1] - 1
	}
	return Range{start, max(end, start)}
}

func (e *SqlEditor) line(row int) string {
	r := e.lineRange(row)
	return e.content[r.Start:r.End]
}

// visibleRows is the rows the pane is showing, with a few to spare, read
// off the scroll position the last frame left.
func (e *SqlEditor) visibleRows() Range {
	return visibleRows(
		e.scrollHandle.State.Y,
		e.scrollHandle.Bounds.H,
		textPaddingY,
		lineHeight,
		e.LineCount(),
	)
}

// autoscroll puts the viewport over the caret, **down and across**, before
// the frame is laid out — so the frame an edit lands in is the frame that
// shows it. Everything here is in the content's own coordinates: a row is
// `LINE_HEIGHT` tall whatever the window is doing.
//
// Answers whether it could: a pane nothing has measured yet has no height
// to scroll within, and `paint` asks for another frame.
//
// (mygo's scroll offsets count the distance travelled, positive, where
// GPUI's ran negative; the sign is dropped at each read and write.)
func (e *SqlEditor) autoscroll(center bool) bool {
	viewport := e.scrollHandle.Bounds
	if viewport.H <= 0. {
		return false
	}
	offset := e.cursorOffset()
	row := e.rowOf(offset)

	// The editor's padding stays visible above and below the caret,
	// so it never sits flush against the edge of the pane.
	top := lineHeight * float32(row)
	bottom := top + lineHeight + textPaddingY*2.
	at := e.scrollHandle.State.Y
	switch {
	case center:
		at = top - (viewport.H-lineHeight)/2.
	case top < at:
		at = top
	case bottom > at+viewport.H:
		at = bottom - viewport.H
	}
	e.scrollHandle.State.Y = max(at, 0.)

	width := e.hScrollHandle.Bounds.W
	if width > 0. {
		// One column of slack past the padding, so typing at the end
		// of a long line slides the view along rather than following
		// a character late.
		left := e.caretX(row, offset)
		right := left + textPaddingX*2. + charWidth
		at := e.hScrollHandle.State.X
		switch {
		case left < at:
			at = left
		case right > at+width:
			at = right - width
		}
		e.hScrollHandle.State.X = max(at, 0.)
	}
	return true
}

// caretX is how far across its line the caret is. Shaped, not counted: a
// wide character or a fallback font moves it off the column grid. The
// line is the one the paint keeps (see layout.go), so a caret moving
// along an unchanged line shapes nothing, and a long line on the grid is
// never shaped for it at all.
func (e *SqlEditor) caretX(row, offset int) float32 {
	line := e.line(row)
	start := 0
	if row < len(e.lineStarts) {
		start = e.lineStarts[row]
	}
	text, tabs := NewExpanded(line)
	ix := min(tabs.ToDisplay(min(max(offset-start, 0), len(line))), len(text))
	if ix == 0 || !isCharBoundary(text, ix) {
		return 0.
	}
	return e.lineCacheFor(text).xForIndex(ix)
}

func (e *SqlEditor) Text() string {
	return e.content
}

// SelectedText is the selected text, when there is a selection. The query
// tab runs this in place of the whole buffer.
func (e *SqlEditor) SelectedText() (string, bool) {
	r := clampRange(e.content, e.chosenRange())
	selected := trim(e.content[r.Start:r.End])
	if selected == "" {
		return "", false
	}
	return selected, true
}

// RunSource is the text a run sends and where it starts in the buffer: the
// selection when there is one, the whole buffer otherwise.
//
// The offset is what the gutter marks are worked out from. ⌘⏎ over a
// selection runs the selection alone, and its statements are still
// statements of *this* buffer — a mark on the first of them belongs on the
// line the selection starts on, not on line one.
func (e *SqlEditor) RunSource() (string, int) {
	r := clampRange(e.content, e.chosenRange())
	slice := e.content[r.Start:r.End]
	trimmed := trim(slice)
	if trimmed == "" {
		return e.content, 0
	}
	start := r.Start + (len(slice) - len(strings.TrimLeftFunc(slice, isWhitespace)))
	return trimmed, start
}

// chosenRange is the selection the user made, if any: a hit the find walk
// selected counts as none. See `found`.
func (e *SqlEditor) chosenRange() Range {
	if e.found != nil && *e.found == e.selectedRange {
		at := e.selectedRange.Start
		return Range{at, at}
	}
	return e.selectedRange
}

// SetStatements takes the statements a run is about to send, all of them
// queued. `offset` is where the run's text starts in the buffer, so the
// ranges are the buffer's own.
func (e *SqlEditor) SetStatements(ranges []Range, offset int) {
	e.statements = make([]StatementMark, 0, len(ranges))
	for _, r := range ranges {
		e.statements = append(e.statements, StatementMark{
			Range:  Range{offset + r.Start, offset + r.End},
			Status: StatementQueued,
			Meta:   "queued",
		})
	}
	e.statementsRev++
}

// SetStatementStatus says how the statement at `ix` ended, and what to
// write beside it. A run whose buffer was edited under it has no marks
// left to write on, and this says nothing rather than guessing which line
// the statement moved to.
func (e *SqlEditor) SetStatementStatus(ix int, status StatementStatus, meta string) {
	if ix < 0 || ix >= len(e.statements) {
		return
	}
	e.statements[ix].Status = status
	e.statements[ix].Meta = meta
	e.statementsRev++
}

// SetStatementMeta rewrites the statistic beside the statement at `ix` and
// nothing else. A shape-changing statement learns what it changed one
// round trip after it lands, and the line is worth updating for it.
func (e *SqlEditor) SetStatementMeta(ix int, meta string) {
	if ix < 0 || ix >= len(e.statements) {
		return
	}
	e.statements[ix].Meta = meta
}

// SkipStatementsFrom marks every statement from `ix` on as never sent,
// with `meta` saying why. A run stops at its first failure, and the
// statements behind it were not skipped by anyone's choice — they never
// left.
func (e *SqlEditor) SkipStatementsFrom(ix int, meta string) {
	for i := max(ix, 0); i < len(e.statements); i++ {
		e.statements[i].Status = StatementSkipped
		e.statements[i].Meta = meta
	}
	e.statementsRev++
}

// SetDiagnostics takes what a syntax check found. The ranges are byte
// offsets into **this** buffer; a check whose text has since been edited
// must be thrown away by the caller rather than moved, because there is
// no way to move it that is not a guess.
func (e *SqlEditor) SetDiagnostics(diagnostics []Diagnostic) {
	if slices.Equal(e.diagnostics, diagnostics) {
		return
	}
	e.diagnostics = diagnostics
}

func (e *SqlEditor) Diagnostics() []Diagnostic {
	return e.diagnostics
}

// Cursor is where the caret is, in bytes into the buffer.
func (e *SqlEditor) Cursor() int {
	return e.cursorOffset()
}

func (e *SqlEditor) ClearStatements() {
	if len(e.statements) == 0 {
		return
	}
	e.statements = e.statements[:0]
	e.statementsRev++
}

func (e *SqlEditor) LineCount() int {
	return max(len(e.lineStarts), 1)
}

// statisticsColumn is the column at the right of the pane that carries
// each statement's statistic, and the `DDL` chip beside a shape-changing
// one.
//
// It is a column of its own, outside the horizontal scroll, for the
// reason the gutter is: a long line must slide under it rather than
// carry it off the pane, and the number has to sit where the eye
// finds it — at the right edge, on the statement's first line — however
// far across the text goes. It is inside the vertical scroll, so it
// travels with its lines. Nothing with nothing run, so a buffer nobody
// has sent gives up no room to a column of nothing.
func (e *SqlEditor) statisticsColumn(c *ui.Context, marks []*lineMark, shapeLines []shapeMark, rows Range, colors *theme.ThemeColors) {
	// Nothing run and nothing shape-changing in the buffer: no column.
	if len(e.statements) == 0 && !e.anyShape {
		return
	}
	ui.Column(c).
		Shrink(0).
		MinHeight(e.scrollHandle.Bounds.H).
		PaddingY(textPaddingY).
		AlignItems(ui.End).
		Children(func() {
			rowSpacer(c, rows.Start)
			for rowIx := rows.Start; rowIx < rows.End; rowIx++ {
				var onLine *lineMark
				if rowIx < len(marks) {
					onLine = marks[rowIx]
				}
				kind := kindAt(shapeLines, rowIx)
				firstDdl := rowIx < len(shapeLines) && shapeLines[rowIx] == shapeFirst
				row := ui.Row(c.Key(rowIx)).
					Shrink(0).
					Height(lineHeight).
					FillWidth().
					AlignItems(ui.Center).
					Justify(ui.End).
					Gap(10.).
					Padding(0, 12., 0, 16.)
				// The wash reaches under the statistic too, so the
				// statement's lines read as one band from the rail to
				// the pane's edge. A shape-changing statement wears it
				// before it is run, in the queued state.
				status := StatementQueued
				if onLine != nil {
					status = onLine.status
				}
				if color, ok := washColor(kind, status, colors); ok && (onLine != nil || kind == StatementDdl) {
					row.Background(color.Color())
				}
				meta := ""
				if onLine != nil && onLine.first && onLine.ix < len(e.statements) {
					meta = e.statements[onLine.ix].Meta
				}
				row.Children(func() {
					if firstDdl {
						ddlChip(c, colors)
					}
					if meta != "" {
						landed := kind == StatementPlain && status == StatementDone
						el := ui.Box(c).
							Shrink(0).
							NoWrap().
							FontSize(11.).
							FixedLineHeight(18.).
							TextColor(metaColor(kind, status, colors).Color())
						// A statement that worked carries its number
						// on a green wash: it is the answer, and the
						// eye goes down the column looking for the
						// one that is not.
						if landed {
							el.PaddingX(7.).Radius(5.).Background(colors.OkSurface.Color())
						}
						el.Children(func() { ui.Text(c, meta).NoWrap() })
					}
				})
			}
			rowSpacer(c, max(e.LineCount()-rows.End, 0))
		})
}

// SetVocabulary swaps in the names of the connected database, so the
// tokenizer can tell a real relation from a typo.
func (e *SqlEditor) SetVocabulary(vocabulary *Vocabulary) {
	if vocabulary == nil {
		vocabulary = &Vocabulary{}
	}
	e.vocabulary = vocabulary
}

func (e *SqlEditor) SetText(text string) {
	e.pushUndo(editNone)
	e.edited()
	e.content = normalize(text)
	e.reindex()
	end := len(e.content)
	e.selectedRange = Range{end, end}
	e.selectionReversed = false
	e.markedRange = nil
	e.touched()
}

// --- selection -------------------------------------------------------

// edited: the text has moved. Everything worked out from the old text goes
// with it — the run's gutter marks and the last check's squiggles both
// name byte ranges the edit has just shifted — and whoever owns the editor
// is told, so the check can run again.
func (e *SqlEditor) edited() {
	e.found = nil
	e.statements = e.statements[:0]
	e.statementsRev++
	e.diagnostics = nil
	e.pendingChanged = true
}

// touched redraws, and puts the caret back on show. Every edit and every
// motion goes through here rather than calling `cx.notify()` itself. (The
// redraw comes by itself in mygo: a frame follows the input that caused
// the edit. The `Changed` an edit raised is told here, once the text is
// the new one, as GPUI told it after the update.)
func (e *SqlEditor) touched() {
	e.found = nil
	uikit.RestartBlink(e)
	if e.pendingChanged {
		e.pendingChanged = false
		e.emit(Changed)
	}
}

func (e *SqlEditor) cursorOffset() int {
	if e.selectionReversed {
		return e.selectedRange.Start
	}
	return e.selectedRange.End
}

func (e *SqlEditor) moveTo(offset int) {
	offset = clampOffset(e.content, offset)
	e.closeCompletions()
	e.goalColumn = nil
	e.selectedRange = Range{offset, offset}
	e.selectionReversed = false
	// Moving the caret ends the current typing run: undo should stop
	// where the user stopped, not swallow the previous sentence too.
	e.lastEdit = editNone
	e.pendingAutoscroll = true
	e.touched()
}

func (e *SqlEditor) selectTo(offset int) {
	offset = clampOffset(e.content, offset)
	e.closeCompletions()
	e.goalColumn = nil
	if e.selectionReversed {
		e.selectedRange.Start = offset
	} else {
		e.selectedRange.End = offset
	}
	if e.selectedRange.End < e.selectedRange.Start {
		e.selectionReversed = !e.selectionReversed
		e.selectedRange = Range{e.selectedRange.End, e.selectedRange.Start}
	}
	e.lastEdit = editNone
	e.pendingAutoscroll = true
	e.touched()
}

// deleteTo deletes from the caret to `offset`, in one undo step.
func (e *SqlEditor) deleteTo(offset int) {
	if e.selectedRange.IsEmpty() {
		if offset == e.cursorOffset() {
			return
		}
		e.selectTo(offset)
	}
	e.replaceTextInRange(nil, "")
}

// --- undo ------------------------------------------------------------

// pushUndo records the buffer before an edit, unless this edit continues
// the run the last one started.
func (e *SqlEditor) pushUndo(kind editKind) {
	if kind != editNone && kind == e.lastEdit {
		return
	}
	e.undo = append(e.undo, snapshot{
		content:           e.content,
		selectedRange:     e.selectedRange,
		selectionReversed: e.selectionReversed,
	})
	if len(e.undo) > undoDepth {
		e.undo = slices.Delete(e.undo, 0, 1)
	}
	e.redo = e.redo[:0]
}

func (e *SqlEditor) restore(s snapshot) snapshot {
	current := snapshot{
		content:           e.content,
		selectedRange:     e.selectedRange,
		selectionReversed: e.selectionReversed,
	}
	e.content = s.content
	e.selectedRange = s.selectedRange
	e.selectionReversed = s.selectionReversed
	e.reindex()
	e.found = nil
	e.goalColumn = nil
	e.markedRange = nil
	e.lastEdit = editNone
	e.pendingAutoscroll = true
	return current
}

func (e *SqlEditor) undoAction() {
	if len(e.undo) == 0 {
		return
	}
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	current := e.restore(s)
	e.redo = append(e.redo, current)
	// Undo and redo are edits like any other: they move the text the
	// marks were worked out from.
	e.edited()
	e.touched()
}

func (e *SqlEditor) redoAction() {
	if len(e.redo) == 0 {
		return
	}
	s := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	current := e.restore(s)
	e.undo = append(e.undo, current)
	e.edited()
	e.touched()
}

// --- offset helpers --------------------------------------------------

func (e *SqlEditor) lineStart(offset int) int {
	return lineStart(e.content, offset)
}

func (e *SqlEditor) lineEnd(offset int) int {
	return lineEnd(e.content, offset)
}

func (e *SqlEditor) previousBoundary(offset int) int {
	return previousBoundary(e.content, offset)
}

func (e *SqlEditor) nextBoundary(offset int) int {
	return nextBoundary(e.content, offset)
}

// moveVertically moves `rows` lines up or down, holding the column the walk
// started in. Past either end of the buffer it goes to that end, as ↑ on
// the first line goes to the start of it in every macOS field.
func (e *SqlEditor) moveVertically(rows int, sel bool) {
	offset := e.cursorOffset()
	row := e.rowOf(offset)
	var goal int
	if e.goalColumn != nil {
		goal = *e.goalColumn
	} else {
		goal = columnOf(e.line(row), offset-e.lineStarts[row])
	}
	targetRow := row + rows
	var target int
	switch {
	case targetRow < 0:
		target = 0
	case targetRow >= e.LineCount():
		target = len(e.content)
	default:
		target = e.lineStarts[targetRow] + indexAtColumn(e.line(targetRow), goal)
	}
	if sel {
		e.selectTo(target)
	} else {
		e.moveTo(target)
	}
	e.goalColumn = &goal
}

// pageRows is how many rows a page key moves: what the pane shows, less
// one, so the last line read is still on screen as the first.
func (e *SqlEditor) pageRows() int {
	rows := int(math.Floor(float64(e.scrollHandle.Bounds.H / lineHeight)))
	return max(rows-1, 1)
}

// setSelection sets the selection outright, for the gestures that name
// both ends.
func (e *SqlEditor) setSelection(r Range, reversed bool) {
	e.closeCompletions()
	e.goalColumn = nil
	e.selectedRange = clampRange(e.content, r)
	e.selectionReversed = reversed && !e.selectedRange.IsEmpty()
	e.lastEdit = editNone
	e.pendingAutoscroll = true
	e.touched()
}

// wholeLine is a line and the break that ends it: what a triple click, ⌘L
// and a copy with nothing selected all take.
func (e *SqlEditor) wholeLine(offset int) Range {
	end := e.lineEnd(offset)
	return Range{e.lineStart(offset), min(end+1, len(e.content))}
}

// apply makes one of `edit`'s line edits, as one undo step.
func (e *SqlEditor) apply(edit *Edit) {
	if edit == nil {
		return
	}
	e.lastEdit = editNone
	e.replace(edit.Range, edit.Text)
	e.lastEdit = editNone
	e.selectedRange = clampRange(e.content, edit.Selection)
	e.selectionReversed = false
	e.closeCompletions()
}

// offsetFromUTF16 is the byte offset of UTF-16 offset `target`. mygo asks
// for the selection and the IME's rectangle in UTF-16 on every frame, so
// this is not a walk over the buffer: an ASCII buffer counts the same in
// both, and any other is a binary search over the line index and a walk
// along one line. (A buffer that is not valid UTF-8 keeps the walk from
// the start, which is what its counts were always made of.)
func (e *SqlEditor) offsetFromUTF16(target int) int {
	if e.ascii {
		return min(max(target, 0), len(e.content))
	}
	from, utf16n := 0, 0
	if len(e.lineUTF16) > 0 {
		row := sort.Search(len(e.lineUTF16), func(i int) bool { return e.lineUTF16[i] > target }) - 1
		if row > 0 {
			from, utf16n = e.lineStarts[row], e.lineUTF16[row]
		}
	}
	utf8n := from
	for _, ch := range e.content[from:] {
		if utf16n >= target {
			break
		}
		utf16n += utf16Len(ch)
		utf8n += utf8.RuneLen(ch)
	}
	return utf8n
}

// offsetToUTF16 is the UTF-16 offset of byte offset `target`, by the same
// index as offsetFromUTF16.
func (e *SqlEditor) offsetToUTF16(target int) int {
	if e.ascii {
		return min(max(target, 0), len(e.content))
	}
	from, utf16n := 0, 0
	if len(e.lineUTF16) > 0 && target > 0 {
		row := e.rowOf(min(target, len(e.content)))
		from, utf16n = e.lineStarts[row], e.lineUTF16[row]
	}
	utf8n := from
	for _, ch := range e.content[from:] {
		if utf8n >= target {
			break
		}
		utf8n += utf8.RuneLen(ch)
		utf16n += utf16Len(ch)
	}
	return utf16n
}

func (e *SqlEditor) rangeToUTF16(r Range) ui.TextInputRange {
	return ui.TextInputRange{Start: e.offsetToUTF16(r.Start), End: e.offsetToUTF16(r.End)}
}

func (e *SqlEditor) rangeFromUTF16(r ui.TextInputRange) Range {
	return Range{e.offsetFromUTF16(r.Start), e.offsetFromUTF16(r.End)}
}

// offsetForPosition is the byte under `position`, in window DIPs.
func (e *SqlEditor) offsetForPosition(position ui.Point) int {
	bounds, layout := e.lastBounds, e.lastLayout
	if bounds == nil || layout == nil {
		return e.cursorOffset()
	}
	if len(layout.lines) == 0 {
		return e.cursorOffset()
	}
	// A point past the rows on screen reaches for the nearest of them:
	// that is the row a drag over the edge is pulling toward, and the
	// viewport then follows the selection there.
	rows := layout.rows()
	row := toUsize(float32(math.Floor(float64((position.Y - bounds.Y) / layout.lineHeight))))
	row = min(min(max(row, rows.Start), rows.End-1), e.LineCount()-1)
	line := layout.line(row)
	if line == nil {
		return e.cursorOffset()
	}
	r := e.lineRange(row)
	local := line.indexForX(position.X - bounds.X)
	return clampOffset(e.content, r.Start+min(local, r.End-r.Start))
}

// --- finding text ----------------------------------------------------

// Find is ⌘F: open the strip, or put the keys back in it. A selection on
// one line is what the user wants to find more of, so it becomes the line;
// otherwise the line opens on what was last searched for. Either way it is
// marked, so the next character typed replaces it.
func (e *SqlEditor) Find() {
	// A hit the last search left selected is not a new needle: ⎋ then
	// ⌘F goes back to that search rather than to one spelled the way
	// the hit happens to be.
	r := clampRange(e.content, e.chosenRange())
	selected := e.content[r.Start:r.End]
	var seed *string
	if selected != "" && !strings.Contains(selected, "\n") {
		seed = &selected
	}
	var field *uikit.TextField
	if e.find != nil {
		field = e.find.query
	} else {
		field = uikit.NewTextField("find").Bare(uikit.FontSize)
		field.Subscribe(e.onFindEvent)
		e.find = &findStrip{query: field}
		if seed == nil && e.lastNeedle != "" {
			field.SetText(e.lastNeedle)
		}
	}
	if seed != nil {
		field.SetText(*seed)
	}
	field.SelectEverything()
	field.FocusHandle().Focus()
}

// closeFind is ⎋ in the line: close the strip and give the keys back to the
// buffer, with the hit the walk stood on still selected.
func (e *SqlEditor) closeFind() {
	if e.find != nil {
		e.lastNeedle = e.find.query.Text()
		e.find = nil
	}
	e.hits = nil
	e.focusHandle.Focus()
}

// needle is what the strip is searching for: the open line's text, or what
// the closed one last held.
func (e *SqlEditor) needle() string {
	if e.find != nil {
		return e.find.query.Text()
	}
	return e.lastNeedle
}

func (e *SqlEditor) search(needle string) []Range {
	hits, _ := e.searchCapped(needle)
	return hits
}

// searchCapped is every hit of `needle`, up to `find_bar::MAX_HITS`. The
// strip asks on every frame it is open and each step of the walk asks
// again, so the answer is kept for the buffer and the needle it was found
// for: a search walks the buffer once per edit or keystroke in the line,
// not once per frame. The hits are shared and never written to.
func (e *SqlEditor) searchCapped(needle string) ([]Range, bool) {
	if m := &e.hitMemo; m.valid && m.needle == needle && m.content == e.content {
		return m.hits, m.capped
	}
	hits, capped := fuzzy.NewNeedle(needle).FindCapped(e.content, uikit.MaxHits)
	e.hitMemo = hitMemo{content: e.content, needle: needle, hits: fromFuzzy(hits), capped: capped, valid: true}
	return e.hitMemo.hits, capped
}

// hitMemo is what searchCapped last found, and in what.
type hitMemo struct {
	content, needle string
	hits            []Range
	capped, valid   bool
}

// compareInt is the order `find_bar`'s walk is given over byte offsets.
func compareInt(a, b int) int { return a - b }

// land: a keystroke in the line lands on the first hit from where the
// selection starts — so a hit already under the user stays put as the
// needle grows, and the search moves on from where they were rather than
// from the top of the buffer.
func (e *SqlEditor) land() {
	hits := e.search(e.needle())
	starts := make([]int, len(hits))
	for i, hit := range hits {
		starts[i] = hit.Start
	}
	if ix, ok := uikit.FirstFrom(starts, e.selectedRange.Start, compareInt); ok {
		e.selectHit(hits[ix])
	}
}

// stepFind is ⏎ and ⇧⏎ in the line, ⌘G and ⇧⌘G anywhere: the next hit or
// the one before, wrapping at both ends.
func (e *SqlEditor) stepFind(forward bool) {
	hits := e.search(e.needle())
	starts := make([]int, len(hits))
	for i, hit := range hits {
		starts[i] = hit.Start
	}
	at := e.selectedRange.Start
	// A bare caret sitting right in front of a hit stands before it, so
	// a step forward lands there rather than skipping it.
	var ix int
	var ok bool
	if forward && e.selectedRange.IsEmpty() {
		ix, ok = uikit.FirstFrom(starts, at, compareInt)
	} else {
		ix, ok = uikit.Step(starts, &at, forward, compareInt)
	}
	if ok {
		e.selectHit(hits[ix])
	}
}

// selectHit selects one hit and brings it into view. The hit is the
// selection, so ⎋ leaves it marked and ⌘C copies it.
func (e *SqlEditor) selectHit(hit Range) {
	e.closeCompletions()
	e.selectedRange = clampRange(e.content, hit)
	e.selectionReversed = false
	e.lastEdit = editNone
	e.pendingAutoscroll = true
	e.touched()
	found := e.selectedRange
	e.found = &found
}

func (e *SqlEditor) onFindEvent(_ *uikit.TextField, event uikit.TextFieldEvent) {
	switch event {
	case uikit.TextFieldChanged:
		e.land()
	case uikit.TextFieldSubmitted:
		e.stepFind(true)
	case uikit.TextFieldCancelled:
		e.closeFind()
	case uikit.TextFieldNextFieldEvent:
		// The strip has one field and nothing to finish.
	}
}

// findStripView is the strip, in a key context of its own.
func (e *SqlEditor) findStripView(c *ui.Context, find *findStrip) {
	var current *int
	for i, hit := range e.hits {
		if hit == e.selectedRange {
			current = &i
			break
		}
	}
	count := uikit.FindCount{
		Typed:   find.query.Text() != "",
		Total:   len(e.hits),
		Current: current,
		Capped:  e.hitsCapped,
	}
	strip := ui.Box(c.Key("sql-editor-find-context")).Shrink(0)
	// The `SqlEditorFind` key context.
	if action, ok := uikit.DispatchKeys(strip, findKeys); ok {
		switch action {
		case Find:
			e.Find()
		case FindNext:
			e.stepFind(true)
		case FindPrevious:
			e.stepFind(false)
		}
	}
	strip.Children(func() {
		uikit.FindBar(c, "sql-editor-find", find.query, count,
			func(forward bool) { e.stepFind(forward) },
			func() { e.closeFind() })
	})
}

// --- completions -----------------------------------------------------

func (e *SqlEditor) closeCompletions() {
	e.completions = e.completions[:0]
	e.completionIx = 0
}

// refreshCompletions recomputes the candidates for the caret's position.
// Called after every edit; `forced` is ⌃Space, which opens the panel even
// where typing alone would not.
func (e *SqlEditor) refreshCompletions(forced bool) {
	e.closeCompletions()
	if e.vocabulary.IsEmpty() && !forced {
		return
	}
	if !e.selectedRange.IsEmpty() || (e.completionsDismissed && !forced) {
		return
	}

	caret := e.cursorOffset()
	// Never inside a string or a comment: there is nothing there the
	// catalog can finish.
	start := e.lineStart(caret)
	line := e.content[start:e.lineEnd(caret)]
	column := max(caret-start-1, 0)
	for _, s := range spans(line, e.vocabulary) {
		if s.Range.Start <= column && column < s.Range.End && (s.Token == TokenLiteral || s.Token == TokenComment) {
			return
		}
	}

	r := prefixRange(e.content, caret)
	var qualifier *string
	if q, ok := qualifierRange(e.content, r.Start); ok {
		name := e.content[q.Start:q.End]
		qualifier = &name
	}
	// With nothing typed, only a qualifier justifies opening: `users.`
	// has an obvious answer, a blank line does not.
	if r.IsEmpty() && qualifier == nil && !forced {
		return
	}

	e.completions = e.vocabulary.Candidates(e.content[r.Start:r.End], qualifier)
	e.completionRange = r
}

func (e *SqlEditor) confirmCompletion() {
	e.applyCompletion(e.completionIx)
}

func (e *SqlEditor) applyCompletion(ix int) {
	if ix < 0 || ix >= len(e.completions) {
		return
	}
	r := e.completionRange
	label := completionText(e.completions[ix], e.content, r)
	// A name the user opened a quote for takes that quote in.
	if r.Start > 0 && e.content[r.Start-1] == '"' && e.completions[ix].Kind != KindKeyword {
		r.Start--
	}

	// Accepting is one undo step, never joined to the typing before it.
	e.lastEdit = editNone
	e.selectedRange = r
	e.replaceTextInRange(&r, label)
	e.lastEdit = editNone
	// The word is now complete; it is not a prefix waiting for more.
	e.closeCompletions()
}

func (e *SqlEditor) nextCompletion() {
	if len(e.completions) == 0 {
		return
	}
	e.completionIx = (e.completionIx + 1) % len(e.completions)
}

func (e *SqlEditor) previousCompletion() {
	if len(e.completions) == 0 {
		return
	}
	e.completionIx = (e.completionIx + len(e.completions) - 1) % len(e.completions)
}

func (e *SqlEditor) openCompletion() {
	e.completionsDismissed = false
	e.refreshCompletions(true)
}

func (e *SqlEditor) dismissCompletion() {
	e.completionsDismissed = true
	e.closeCompletions()
}

// --- motion actions --------------------------------------------------

func (e *SqlEditor) moveLeft() {
	target := e.selectedRange.Start
	if e.selectedRange.IsEmpty() {
		target = e.previousBoundary(e.cursorOffset())
	}
	e.moveTo(target)
}

func (e *SqlEditor) moveRight() {
	target := e.selectedRange.End
	if e.selectedRange.IsEmpty() {
		target = e.nextBoundary(e.cursorOffset())
	}
	e.moveTo(target)
}

func (e *SqlEditor) moveUp() { e.moveVertically(-1, false) }

func (e *SqlEditor) moveDown() { e.moveVertically(1, false) }

func (e *SqlEditor) moveToPreviousWordStart() {
	e.moveTo(previousWordStart(e.content, e.cursorOffset()))
}

func (e *SqlEditor) moveToNextWordEnd() {
	e.moveTo(nextWordEnd(e.content, e.cursorOffset()))
}

func (e *SqlEditor) moveToBeginningOfLine() {
	e.moveTo(smartLineStart(e.content, e.cursorOffset()))
}

func (e *SqlEditor) moveToEndOfLine() {
	e.moveTo(e.lineEnd(e.cursorOffset()))
}

func (e *SqlEditor) moveToBeginning() { e.moveTo(0) }

func (e *SqlEditor) moveToEnd() { e.moveTo(len(e.content)) }

// --- selection actions -----------------------------------------------

func (e *SqlEditor) selectLeft() { e.selectTo(e.previousBoundary(e.cursorOffset())) }

func (e *SqlEditor) selectRight() { e.selectTo(e.nextBoundary(e.cursorOffset())) }

func (e *SqlEditor) selectUp() { e.moveVertically(-1, true) }

func (e *SqlEditor) selectDown() { e.moveVertically(1, true) }

func (e *SqlEditor) selectToPreviousWordStart() {
	e.selectTo(previousWordStart(e.content, e.cursorOffset()))
}

func (e *SqlEditor) selectToNextWordEnd() {
	e.selectTo(nextWordEnd(e.content, e.cursorOffset()))
}

func (e *SqlEditor) selectToBeginningOfLine() {
	e.selectTo(smartLineStart(e.content, e.cursorOffset()))
}

func (e *SqlEditor) selectToEndOfLine() {
	e.selectTo(e.lineEnd(e.cursorOffset()))
}

func (e *SqlEditor) selectToBeginning() { e.selectTo(0) }

func (e *SqlEditor) selectToEnd() { e.selectTo(len(e.content)) }

func (e *SqlEditor) selectAll() {
	// No autoscroll: ⌘A is not a request to go to the end of the buffer.
	e.setSelection(Range{0, len(e.content)}, false)
	e.pendingAutoscroll = false
}

// selectLineAction is ⌘L: the caret's line, break and all — and the next
// one on each further press, as Zed's does.
func (e *SqlEditor) selectLineAction() {
	start := e.lineStart(e.selectedRange.Start)
	end := e.wholeLine(e.selectedRange.End).End
	e.setSelection(Range{start, end}, false)
}

// selectNext is ⌘D: the word under the caret, then the next place that
// text occurs, wrapping. One selection, so it is "find the next one of
// these" rather than Zed's second cursor.
func (e *SqlEditor) selectNext() {
	if e.selectedRange.IsEmpty() {
		word := wordAt(e.content, e.cursorOffset())
		e.setSelection(word, false)
		return
	}
	r := clampRange(e.content, e.selectedRange)
	needle := e.content[r.Start:r.End]
	from := e.selectedRange.End
	next := -1
	if ix := strings.Index(e.content[from:], needle); ix >= 0 {
		next = from + ix
	} else if ix := strings.Index(e.content, needle); ix >= 0 {
		next = ix
	}
	if next >= 0 {
		e.setSelection(Range{next, next + len(needle)}, false)
	}
}

// cancel is ⎋: drop the selection and keep the caret. With nothing
// selected the key is not the editor's, and goes on to whoever wants it
// (the key table leaves ⎋ out then; see `editorKeysNoCancel`).
func (e *SqlEditor) cancel() {
	if e.selectedRange.IsEmpty() {
		return
	}
	e.moveTo(e.cursorOffset())
}

// goTo moves or selects by one of the plain motions over the text. (Rust:
// `go`.)
func (e *SqlEditor) goTo(to func(string, int) int, sel bool) {
	target := to(e.content, e.cursorOffset())
	if sel {
		e.selectTo(target)
	} else {
		e.moveTo(target)
	}
}

// scrollCursorCenter is ⌃L: the caret's line to the middle of the pane.
func (e *SqlEditor) scrollCursorCenter() {
	e.autoscroll(true)
}

// --- editing actions -------------------------------------------------

func (e *SqlEditor) backspace() {
	e.deleteTo(e.previousBoundary(e.cursorOffset()))
}

func (e *SqlEditor) delete() {
	e.deleteTo(e.nextBoundary(e.cursorOffset()))
}

func (e *SqlEditor) deleteToPreviousWordStart() {
	e.deleteTo(previousWordStart(e.content, e.cursorOffset()))
}

func (e *SqlEditor) deleteToNextWordEnd() {
	e.deleteTo(nextWordEnd(e.content, e.cursorOffset()))
}

func (e *SqlEditor) deleteToBeginningOfLine() {
	e.deleteTo(e.lineStart(e.cursorOffset()))
}

func (e *SqlEditor) deleteToEndOfLine() {
	e.deleteTo(e.lineEnd(e.cursorOffset()))
}

func (e *SqlEditor) newline() {
	// Carry the current line's indent onto the new one, the way every
	// editor does; a query indented by hand stays indented.
	// Only the indent in front of the caret: a break made inside the
	// indent must not hand the new line more than the old one had.
	caret := e.selectedRange.Start
	start := e.lineStart(caret)
	head := e.content[start:caret]
	n := 0
	for n < len(head) && (head[n] == ' ' || head[n] == '\t') {
		n++
	}
	e.replaceTextInRange(nil, "\n"+head[:n])
}

// indentAction is ⇥: an indent at the caret, or — over a selection that
// reaches a second line — an indent in front of every line it touches.
func (e *SqlEditor) indentAction() {
	r := e.selectedRange
	clamped := clampRange(e.content, r)
	if strings.Contains(e.content[clamped.Start:clamped.End], "\n") {
		e.apply(indent(e.content, r, indentUnit))
	} else {
		e.replaceTextInRange(nil, indentUnit)
	}
}

func (e *SqlEditor) indentLines() {
	e.apply(indent(e.content, e.selectedRange, indentUnit))
}

func (e *SqlEditor) outdent() {
	e.apply(outdent(e.content, e.selectedRange, indentUnit))
}

func (e *SqlEditor) toggleComment() {
	e.apply(toggleComment(e.content, e.selectedRange))
}

func (e *SqlEditor) moveLineUp() {
	e.apply(moveLines(e.content, e.selectedRange, false))
}

func (e *SqlEditor) moveLineDown() {
	e.apply(moveLines(e.content, e.selectedRange, true))
}

func (e *SqlEditor) duplicateLineUp() {
	e.apply(duplicateLines(e.content, e.selectedRange, false))
}

func (e *SqlEditor) duplicateLineDown() {
	e.apply(duplicateLines(e.content, e.selectedRange, true))
}

func (e *SqlEditor) deleteLine() {
	e.apply(deleteLines(e.content, e.selectedRange))
}

func (e *SqlEditor) joinLines() {
	e.apply(joinLines(e.content, e.selectedRange))
}

func (e *SqlEditor) transpose() {
	if e.selectedRange.IsEmpty() {
		e.apply(transpose(e.content, e.cursorOffset()))
	}
}

// killToEndOfLine is ⌃K: cut from the caret to the end of the line — or
// the break itself, when the caret is already there, so repeated presses
// eat the lines below one at a time.
func (e *SqlEditor) killToEndOfLine(c *ui.Context) {
	caret := e.cursorOffset()
	end := e.lineEnd(caret)
	if end == caret {
		end = e.nextBoundary(caret)
	}
	if end > caret {
		c.WriteClipboard(e.content[caret:end])
		e.lineClip = nil
	}
	e.deleteTo(end)
}

func (e *SqlEditor) paste(c *ui.Context) {
	// GPUI told "no text" from an empty string; mygo answers "" for
	// both, and pasting nothing is nothing either way.
	text := c.ReadClipboard()
	if text == "" {
		return
	}
	text = normalize(text)
	// A paste is one undo step, never joined to the typing around it,
	// and it opens no completion panel: the word it ends in was not
	// typed, and ranking the catalog against it is work nobody asked
	// for on what may be a thousand lines.
	e.lastEdit = editNone
	if e.selectedRange.IsEmpty() && e.lineClip != nil && *e.lineClip == text {
		// A line copied whole goes back in as a line, above the
		// caret's, and the caret stays in the text it was in.
		caret := e.cursorOffset()
		at := e.lineStart(caret)
		e.replace(Range{at, at}, text)
		caret += len(text)
		e.selectedRange = Range{caret, caret}
	} else {
		e.replace(e.selectedRange, text)
	}
	e.lastEdit = editNone
	e.closeCompletions()
}

// copy is ⌘C: the selection — or, with nothing selected, the caret's whole
// line, which is what Zed copies and what makes ⌘C ⌘V duplicate it.
func (e *SqlEditor) copy(c *ui.Context) {
	r := clampRange(e.content, e.selectedRange)
	if r.IsEmpty() {
		e.copyLine(c)
	} else {
		e.lineClip = nil
		c.WriteClipboard(e.content[r.Start:r.End])
	}
}

func (e *SqlEditor) copyLine(c *ui.Context) {
	line := e.lineRange(e.rowOf(e.cursorOffset()))
	text := e.content[line.Start:line.End] + "\n"
	c.WriteClipboard(text)
	e.lineClip = &text
}

func (e *SqlEditor) cut(c *ui.Context) {
	r := clampRange(e.content, e.selectedRange)
	if r.IsEmpty() {
		e.copyLine(c)
		e.apply(deleteLines(e.content, e.selectedRange))
	} else {
		e.lineClip = nil
		c.WriteClipboard(e.content[r.Start:r.End])
		e.lastEdit = editNone
		e.replaceTextInRange(nil, "")
	}
}

// --- mouse -----------------------------------------------------------

// onInput takes the pointer, as the Rust's mouse listeners did; keys, text
// and edit commands are left to the key table and the TextInputClient
// (false). The press is the pane's, and mygo's pointer capture hands the
// moves and the release to it wherever they land — the window listeners
// the Rust hung while a drag was on.
func (e *SqlEditor) onInput(ev ui.InputEvent) bool {
	position := ui.Point{X: e.origin.X + ev.X, Y: e.origin.Y + ev.Y}
	switch ev.Kind {
	case ui.InputPointerDown:
		if ev.Button != 0 {
			return false
		}
		e.onMouseDown(position, ev.Clicks, ev.Mods)
		return true
	case ui.InputPointerMove:
		if !e.isSelecting {
			return false
		}
		// A move with no button held is a release nothing here heard
		// about; it must not leave the buffer selecting.
		if ev.Button == 0 {
			e.dragTo(position)
		} else {
			e.endDrag()
		}
		return true
	case ui.InputPointerUp:
		if ev.Button == 0 {
			e.endDrag()
			return true
		}
	}
	return false
}

func (e *SqlEditor) onMouseDown(position ui.Point, clickCount int, mods ui.Modifiers) {
	offset := e.offsetForPosition(position)
	e.isSelecting = true
	switch max(clickCount, 1) {
	case 1:
		e.selectMode = selectMode{kind: selectCharacter}
		if mods&ui.Shift != 0 {
			e.selectTo(offset)
		} else {
			e.moveTo(offset)
		}
	case 2:
		word := wordAt(e.content, offset)
		e.selectMode = selectMode{kind: selectWord, value: word}
		e.setSelection(word, false)
	case 3:
		line := e.wholeLine(offset)
		e.selectMode = selectMode{kind: selectLine, value: line}
		e.setSelection(line, false)
	default:
		e.isSelecting = false
		e.setSelection(Range{0, len(e.content)}, false)
	}
	// A click is not a request to scroll: the row under the pointer is
	// on screen already, and following the far end of a word or a line
	// would slide the text out from under the click.
	e.pendingAutoscroll = false
}

func (e *SqlEditor) endDrag() {
	if e.isSelecting {
		e.isSelecting = false
	}
}

// dragTo: the pointer moved with the button down: carry the selection to
// it, in the unit the click that started the drag took — a drag out of a
// double click grows by words, out of a triple click by lines.
func (e *SqlEditor) dragTo(position ui.Point) {
	offset := e.offsetForPosition(position)
	var origin, under Range
	switch e.selectMode.kind {
	case selectCharacter:
		e.selectTo(offset)
		return
	case selectWord:
		origin, under = e.selectMode.value, wordAt(e.content, offset)
	case selectLine:
		origin, under = e.selectMode.value, e.wholeLine(offset)
	}
	// The unit the drag started on is never given up; the caret is at
	// whichever end the pointer is pulling.
	if offset < origin.Start {
		e.setSelection(Range{min(under.Start, origin.Start), origin.End}, true)
	} else {
		e.setSelection(Range{origin.Start, max(under.End, origin.End)}, false)
	}
}

// replace puts `newText` where `r` was and leaves the caret after it.
// Every edit of the buffer comes through here: it is the one place the
// undo step is taken, the marks are dropped and the line index is redone.
func (e *SqlEditor) replace(r Range, newText string) {
	r = clampRange(e.content, r)

	// A newline breaks the undo run, so undo lands on line boundaries
	// rather than swallowing the whole query. So does replacing a
	// selection: the selection is part of what undo must bring back.
	var kind editKind
	switch {
	case !r.IsEmpty() || strings.Contains(newText, "\n"):
		kind = editNone
	case newText == "":
		kind = editDelete
	default:
		kind = editInsert
	}
	e.pushUndo(kind)
	e.lastEdit = kind
	// The marks name byte ranges in the text that was run, and this
	// moves that text. A tick beside a line the user has rewritten
	// would say the wrong thing about the wrong statement.
	e.edited()

	e.content = e.content[:r.Start] + newText + e.content[r.End:]
	e.reindex()
	cursor := r.Start + len(newText)
	e.selectedRange = Range{cursor, cursor}
	e.selectionReversed = false
	e.goalColumn = nil
	e.markedRange = nil
	e.pendingAutoscroll = true
	e.closeCompletions()
	e.touched()
}

// --- ui.TextInputClient (GPUI's EntityInputHandler) -------------------

// TextForRange is `text_for_range`.
func (e *SqlEditor) TextForRange(rangeUTF16 ui.TextInputRange) (string, ui.TextInputRange) {
	r := clampRange(e.content, e.rangeFromUTF16(rangeUTF16))
	return e.content[r.Start:r.End], e.rangeToUTF16(r)
}

// Selection is `selected_text_range`.
func (e *SqlEditor) Selection() ui.TextInputSelection {
	return ui.TextInputSelection{Range: e.rangeToUTF16(e.selectedRange), Reversed: e.selectionReversed}
}

// MarkedRange is `marked_text_range`.
func (e *SqlEditor) MarkedRange() (ui.TextInputRange, bool) {
	if e.markedRange == nil {
		return ui.TextInputRange{}, false
	}
	return e.rangeToUTF16(*e.markedRange), true
}

// UnmarkText is `unmark_text`.
func (e *SqlEditor) UnmarkText() {
	e.markedRange = nil
}

// ReplaceText is `replace_text_in_range`.
func (e *SqlEditor) ReplaceText(rangeUTF16 *ui.TextInputRange, newText string) {
	var r *Range
	if rangeUTF16 != nil {
		v := e.rangeFromUTF16(*rangeUTF16)
		r = &v
	}
	e.replaceTextInRange(r, newText)
}

// replaceTextInRange is `replace_text_in_range` with the UTF-16 range
// already brought into the buffer: nil is the marked range if there is
// one, else the selection.
func (e *SqlEditor) replaceTextInRange(byteRange *Range, newText string) {
	var r Range
	switch {
	case byteRange != nil:
		r = *byteRange
	case e.markedRange != nil:
		r = *e.markedRange
	default:
		r = e.selectedRange
	}
	e.replace(r, normalize(newText))
	// Any edit re-opens the panel that Escape closed: the user is
	// typing a different word now.
	e.completionsDismissed = false
	e.refreshCompletions(false)
}

// SetMarkedText is `replace_and_mark_text_in_range`. mygo always hands the
// new selection, which GPUI made optional; it is relative to `newText`.
func (e *SqlEditor) SetMarkedText(rangeUTF16 *ui.TextInputRange, newText string, newSelectedRangeUTF16 ui.TextInputRange) {
	var target Range
	switch {
	case rangeUTF16 != nil:
		target = e.rangeFromUTF16(*rangeUTF16)
	case e.markedRange != nil:
		target = *e.markedRange
	default:
		target = e.selectedRange
	}
	r := clampRange(e.content, target)

	// Composition rewrites itself as it goes: take one undo step when
	// it starts, none for the keystrokes inside it.
	if e.markedRange == nil {
		e.pushUndo(editNone)
	}
	e.lastEdit = editNone
	e.edited()

	e.content = e.content[:r.Start] + newText + e.content[r.End:]
	e.reindex()
	e.goalColumn = nil
	if newText != "" {
		e.markedRange = &Range{r.Start, r.Start + len(newText)}
	} else {
		e.markedRange = nil
	}
	// The platform reports this selection relative to `newText`, so
	// convert it inside that string and then shift it into the buffer.
	// Converting against the whole buffer would run the offset past
	// the end and slice mid-character on the next edit.
	start := r.Start + utf16ToByte(newText, newSelectedRangeUTF16.Start)
	end := r.Start + utf16ToByte(newText, newSelectedRangeUTF16.End)
	e.selectedRange = clampRange(e.content, Range{start, end})
	e.pendingAutoscroll = true
	e.touched()
}

// BoundsForRange is `bounds_for_range`, relative to the editor's element.
func (e *SqlEditor) BoundsForRange(rangeUTF16 ui.TextInputRange) (ui.Rect, ui.TextInputRange, bool) {
	layout, bounds := e.lastLayout, e.lastBounds
	if layout == nil || bounds == nil {
		return ui.Rect{}, rangeUTF16, false
	}
	r := clampRange(e.content, e.rangeFromUTF16(rangeUTF16))
	row := e.rowOf(r.Start)
	line := layout.line(row)
	if line == nil {
		return ui.Rect{}, rangeUTF16, false
	}
	onRow := e.lineRange(row)
	// The IME can ask about a range that runs past this line; keep the
	// rectangle on the line the range starts on.
	width := onRow.End - onRow.Start
	from := min(max(r.Start-onRow.Start, 0), width)
	to := min(max(r.End-onRow.Start, 0), width)
	top := bounds.Y + layout.lineHeight*float32(row)
	left := bounds.X + line.xFor(from)
	right := bounds.X + line.xFor(to)
	return ui.Rect{X: left - e.origin.X, Y: top - e.origin.Y, W: right - left, H: layout.lineHeight}, rangeUTF16, true
}

// IndexForPoint is `character_index_for_point`, for a point relative to
// the editor's element.
func (e *SqlEditor) IndexForPoint(point ui.Point) (int, bool) {
	position := ui.Point{X: e.origin.X + point.X, Y: e.origin.Y + point.Y}
	return e.offsetToUTF16(e.offsetForPosition(position)), true
}

// FocusHandle is the editor's focus identity (GPUI's `Focusable`).
func (e *SqlEditor) FocusHandle() *ui.Handle { return &e.focusHandle }

// keyTable is the bindings the editor's element dispatches this frame: the
// `completing` ones ahead of the rest while the panel is open, and ⎋ only
// while there is a selection for it to drop.
func (e *SqlEditor) keyTable() []uikit.KeyBinding[Action] {
	switch {
	case len(e.completions) > 0:
		return completingEditorKeys
	case !e.selectedRange.IsEmpty():
		return editorKeys
	default:
		return editorKeysNoCancel
	}
}

// perform runs an action, as the Rust's `on_action` listeners did.
func (e *SqlEditor) perform(c *ui.Context, action Action) {
	switch action {
	case Cancel:
		e.cancel()
	case Backspace:
		e.backspace()
	case Delete:
		e.delete()
	case DeleteToPreviousWordStart:
		e.deleteToPreviousWordStart()
	case DeleteToNextWordEnd:
		e.deleteToNextWordEnd()
	case DeleteToBeginningOfLine:
		e.deleteToBeginningOfLine()
	case DeleteToEndOfLine:
		e.deleteToEndOfLine()
	case DeleteToPreviousSubwordStart:
		e.deleteTo(previousSubwordStart(e.content, e.cursorOffset()))
	case DeleteToNextSubwordEnd:
		e.deleteTo(nextSubwordEnd(e.content, e.cursorOffset()))
	case KillToEndOfLine:
		e.killToEndOfLine(c)
	case Transpose:
		e.transpose()
	case MoveLeft:
		e.moveLeft()
	case MoveRight:
		e.moveRight()
	case MoveUp:
		e.moveUp()
	case MoveDown:
		e.moveDown()
	case MovePageUp:
		e.moveVertically(-e.pageRows(), false)
	case MovePageDown:
		e.moveVertically(e.pageRows(), false)
	case SelectPageUp:
		e.moveVertically(-e.pageRows(), true)
	case SelectPageDown:
		e.moveVertically(e.pageRows(), true)
	case MoveToPreviousWordStart:
		e.moveToPreviousWordStart()
	case MoveToNextWordEnd:
		e.moveToNextWordEnd()
	case MoveToPreviousSubwordStart:
		e.goTo(previousSubwordStart, false)
	case MoveToNextSubwordEnd:
		e.goTo(nextSubwordEnd, false)
	case SelectToPreviousSubwordStart:
		e.goTo(previousSubwordStart, true)
	case SelectToNextSubwordEnd:
		e.goTo(nextSubwordEnd, true)
	case MoveToStartOfParagraph:
		e.goTo(startOfParagraph, false)
	case MoveToEndOfParagraph:
		e.goTo(endOfParagraph, false)
	case SelectToStartOfParagraph:
		e.goTo(startOfParagraph, true)
	case SelectToEndOfParagraph:
		e.goTo(endOfParagraph, true)
	case MoveToBeginningOfLine:
		e.moveToBeginningOfLine()
	case MoveToEndOfLine:
		e.moveToEndOfLine()
	case MoveToBeginning:
		e.moveToBeginning()
	case MoveToEnd:
		e.moveToEnd()
	case ScrollCursorCenter:
		e.scrollCursorCenter()
	case SelectLeft:
		e.selectLeft()
	case SelectRight:
		e.selectRight()
	case SelectUp:
		e.selectUp()
	case SelectDown:
		e.selectDown()
	case SelectToPreviousWordStart:
		e.selectToPreviousWordStart()
	case SelectToNextWordEnd:
		e.selectToNextWordEnd()
	case SelectToBeginningOfLine:
		e.selectToBeginningOfLine()
	case SelectToEndOfLine:
		e.selectToEndOfLine()
	case SelectToBeginning:
		e.selectToBeginning()
	case SelectToEnd:
		e.selectToEnd()
	case SelectAll:
		e.selectAll()
	case SelectLine:
		e.selectLineAction()
	case SelectNext:
		e.selectNext()
	case Newline:
		e.newline()
	case Indent:
		e.indentAction()
	case IndentLines:
		e.indentLines()
	case Outdent:
		e.outdent()
	case ToggleComment:
		e.toggleComment()
	case MoveLineUp:
		e.moveLineUp()
	case MoveLineDown:
		e.moveLineDown()
	case DuplicateLineUp:
		e.duplicateLineUp()
	case DuplicateLineDown:
		e.duplicateLineDown()
	case DeleteLine:
		e.deleteLine()
	case JoinLines:
		e.joinLines()
	case Undo:
		e.undoAction()
	case Redo:
		e.redoAction()
	case Paste:
		e.paste(c)
	case Copy:
		e.copy(c)
	case Cut:
		e.cut(c)
	case OpenCompletion:
		e.openCompletion()
	case ConfirmCompletion:
		e.confirmCompletion()
	case NextCompletion:
		e.nextCompletion()
	case PreviousCompletion:
		e.previousCompletion()
	case DismissCompletion:
		e.dismissCompletion()
	case Find:
		e.Find()
	case FindNext:
		e.stepFind(true)
	case FindPrevious:
		e.stepFind(false)
	}
}

// View builds the editor (Rust: `Render::render`). Call it in every frame
// the editor is on screen; it fills the box it is given.
func (e *SqlEditor) View(c *ui.Context) ui.Element {
	colors := &theme.Current().Colors
	// Focus belongs to the window, and this is the one place that has
	// one, so the blink is started and stopped from the edge here
	// rather than from a focus listener the constructor cannot install.
	// The turns themselves are read in `paint`, from the painter's
	// clock, so a blink repaints the editor and builds nothing.
	focused := e.focusHandle.Focused(c)
	uikit.NoteBlinkFocus(e, focused)
	// The hits are read off the buffer and the line as they are now, so
	// an edit under an open strip is searched again on the next frame.
	if e.find != nil {
		e.hits, e.hitsCapped = e.searchCapped(e.find.query.Text())
	} else {
		e.hits, e.hitsCapped = nil, false
	}

	// The strip sits over the editor and beside its key context, never
	// inside it: see the module docs.
	return ui.Column(c.Key("sql-editor-root")).Fill().Children(func() {
		if find := e.find; find != nil {
			e.findStripView(c, find)
		}
		e.editorView(c, colors, focused)
	})
}

// editorView is the editor's own element, in its key context: the two
// scroll containers, the columns beside the text, and the bars.
func (e *SqlEditor) editorView(c *ui.Context, colors *theme.ThemeColors, focused bool) {
	editor := ui.Row(c.Key("sql-editor")).
		Bind(&e.focusHandle).
		HandleTextInput(e).
		HandleInput(e.onInput).
		Cursor(ui.CursorText).
		FillWidth().
		Grow(1).
		Basis(0).
		MinHeight(0).
		AlignItems(ui.Stretch).
		// The caret and the columns are placed by a fixed advance, so
		// the editor names its font rather than inheriting the chrome's.
		Font(theme.MonoFontFamily).
		FontSize(fontSize).
		FixedLineHeight(lineHeight)
	// Where the element is, for the pointer and the IME, which mygo
	// reports relative to it. It paints nothing.
	editor.Draw(func(_ *ui.Painter, r ui.Rect) { e.origin = ui.Point{X: r.X, Y: r.Y} })

	// The `SqlEditor` key context (and `completing` while the panel is
	// open). An action run here lands after the find strip above was
	// built; mygo builds the view again in the same frame after a key is
	// taken, so the strip shows it in the frame the key arrived in.
	if action, ok := uikit.DispatchKeys(editor, e.keyTable()); ok {
		e.perform(c, action)
	}

	// Before anything is laid out, so this frame is the one that shows
	// the caret where the edit left it. A pane nothing has measured
	// yet keeps the request, and `paint` asks for the frame after.
	if e.pendingAutoscroll && e.autoscroll(false) {
		e.pendingAutoscroll = false
	}
	lineCount := e.LineCount()
	// Only these rows get a gutter number, a rail and a statistic: a
	// row per line of a long script was most of what a frame cost.
	rows := e.visibleRows()
	marks := e.cachedLineMarks()
	shapeLines := e.shapeLines
	// The line the caret is on, while it is a bare caret: its number
	// is inked, and the element washes the line behind it.
	activeRow := -1
	if focused && e.selectedRange.IsEmpty() {
		activeRow = e.rowOf(e.cursorOffset())
	}
	// How wide the text is, and so how far there is to scroll. The
	// element inside fills this box; the box is what the scroll
	// container measures.
	textWidth := textPaddingX*2. + float32(e.widest+1)*charWidth
	viewportH := e.scrollHandle.Bounds.H

	editor.Children(func() {
		// Two scroll containers, nested, and the nesting is the
		// point: this one scrolls **down**, and the gutter is inside
		// it, so the line numbers travel with their lines. The one
		// within it scrolls **across** and holds the text alone, so
		// a long line slides under the numbers instead of pushing
		// them off the pane. One container over both would take the
		// gutter with it.
		vertical := ui.Scroll(c.Key("sql-editor-text")).
			TrackScroll(&e.scrollHandle.State).
			Scrollbars(false).
			Grow(1).
			Basis(0).
			MinWidth(0).
			FillHeight()
		vertical.Children(func() {
			// `items_start`, and it is load-bearing: a flex row
			// stretches its children to the line's height by
			// default, so a buffer of forty lines would still lay
			// out as boxes the height of the pane — content the same
			// size as the viewport is content with nothing to
			// scroll, and no bar.
			ui.Row(c).FillWidth().AlignItems(ui.Start).Children(func() {
				e.gutterView(c, colors, rows, marks, shapeLines, activeRow, lineCount, viewportH)
				e.railView(c, colors, rows, marks, shapeLines, lineCount, viewportH)
				horizontal := ui.ScrollHorizontal(c.Key("sql-editor-line")).
					TrackScroll(&e.hScrollHandle.State).
					Scrollbars(false).
					Grow(1).
					Basis(0).
					MinWidth(0)
				horizontal.Children(func() {
					// The width is **definite**, and that is the
					// whole trick: a scroll container measures the
					// box its children ask for. `min_w_full` keeps a
					// short buffer pane-wide, so a click to the right
					// of a line still lands in the editor; mygo reads
					// the pane's width off the last frame.
					text := ui.Box(c).
						Shrink(0).
						Width(textWidth).
						MinWidth(e.hScrollHandle.Bounds.W).
						MinHeight(viewportH).
						Padding(textPaddingY, textPaddingX)
					text.Children(func() {
						// The element fills the box sized for the
						// longest line, one row per line.
						ui.Box(c).
							FillWidth().
							Height(lineHeight * float32(lineCount)).
							Draw(func(p *ui.Painter, r ui.Rect) { e.paint(p, r, focused, colors) })
						if len(e.completions) > 0 {
							e.completionsMenu(c, colors)
						}
					})
				})
				e.hScrollHandle.Bounds = horizontal.Bounds()
				e.statisticsColumn(c, marks, shapeLines, rows, colors)
			})
		})
		e.scrollHandle.Bounds = vertical.Bounds()

		// The bars are painted outside the containers they drive, or
		// they would scroll away with the text. Both appear only when
		// the content does not fit, which `NewScrollbar` answers.
		if bar, ok := uikit.NewScrollbar(true, &e.scrollHandle, &e.scrollDrag, colors.TextFaint, colors.TextMuted); ok {
			ui.Box(c.Key("sql-editor-vbar")).
				Absolute().Top(0).Right(0).Bottom(0).Width(uikit.Thickness).
				Children(func() { bar.View(c) })
		}
		if bar, ok := uikit.NewScrollbar(false, &e.hScrollHandle, &e.scrollDrag, colors.TextFaint, colors.TextMuted); ok {
			// Start past the gutter: the bar drives the text, and the
			// gutter does not move sideways.
			ui.Box(c.Key("sql-editor-hbar")).
				Absolute().Left(gutterWidth).Right(0).Bottom(0).Height(uikit.Thickness).
				Children(func() { bar.View(c) })
		}
	})
}

// gutterView is the line-number gutter: a statement's mark on the left of
// it, the number right-aligned against the rule.
func (e *SqlEditor) gutterView(c *ui.Context, colors *theme.ThemeColors, rows Range, marks []*lineMark, shapeLines []shapeMark, activeRow, lineCount int, viewportH float32) {
	ui.Column(c.Key("sql-editor-gutter")).
		Width(gutterWidth).
		Shrink(0).
		// ...and with the stretch gone, both columns keep the pane's
		// height as a floor, so their backgrounds and the rule between
		// them still reach the bottom of a short buffer.
		MinHeight(viewportH).
		Padding(textPaddingY, 8., textPaddingY, 0).
		Background(colors.Window.Color()).
		BorderWidth(0, 1, 0, 0).
		BorderColor(colors.Hairline.Color()).
		FontSize(11.5).
		TextColor(colors.LineNumber.Color()).
		Children(func() {
			rowSpacer(c, rows.Start)
			for row := rows.Start; row < rows.End; row++ {
				n := row + 1
				var onLine *lineMark
				if n-1 < len(marks) {
					onLine = marks[n-1]
				}
				// The gutter beside a shape-changing statement takes the
				// family's own tint, as the comp's does, so the teal
				// reads from the numbers to the far edge.
				ddl := kindAt(shapeLines, n-1) == StatementDdl
				el := ui.Row(c.Key(row)).
					Shrink(0).
					Height(lineHeight).
					AlignItems(ui.Center)
				// The gutter's own padding is on the column, so the tint
				// has to reach past this row's box to meet the rule:
				// `-mr` pulls it there.
				if ddl {
					el.Background(colors.DdlGutter.Color()).
						TextColor(colors.DdlNumber.Color()).
						Margin(0, -8., 0, 0).
						Padding(0, 8., 0, 0)
				}
				el.Children(func() {
					ui.Row(c).Width(markWidth).Shrink(0).Justify(ui.Center).Children(func() {
						if onLine != nil && onLine.first {
							statementMark(c, onLine.status, colors)
						}
					})
					ui.Box(c).Grow(1).Basis(0)
					number := ui.Box(c).Shrink(0)
					if activeRow == row && !ddl {
						number.TextColor(colors.TextSecondary.Color())
					}
					number.Children(func() { ui.Text(c, strconv.Itoa(n)) })
				})
			}
			rowSpacer(c, max(lineCount-rows.End, 0))
		})
}

// railView is the rail. It is a column of its own rather than a border on
// the gutter, because it says something per line: which lines one
// statement covers, and how that statement ended. It carries the whole of
// a multi-line statement, which the mark on its first line cannot.
func (e *SqlEditor) railView(c *ui.Context, colors *theme.ThemeColors, rows Range, marks []*lineMark, shapeLines []shapeMark, lineCount int, viewportH float32) {
	ui.Column(c.Key("sql-editor-rail")).
		Width(railWidth).
		Shrink(0).
		MinHeight(viewportH).
		PaddingY(textPaddingY).
		Children(func() {
			rowSpacer(c, rows.Start)
			for row := rows.Start; row < rows.End; row++ {
				kind := kindAt(shapeLines, row)
				// A shape-changing statement nobody has run yet already
				// wears its rail, in the queued tone: the teal is what
				// says "this one changes things" before ⌘⏎.
				var status *StatementStatus
				if row < len(marks) && marks[row] != nil {
					status = &marks[row].status
				} else if kind == StatementDdl {
					queued := StatementQueued
					status = &queued
				}
				line := ui.Box(c.Key(row)).Shrink(0).Height(lineHeight)
				if status != nil {
					line.Background(railColor(kind, *status, colors).Color())
				}
			}
			rowSpacer(c, max(lineCount-rows.End, 0))
		})
}

// completionsMenu is the completion popup, anchored under the caret. It
// floats over whatever is below the editor, so it never resizes the query
// pane.
//
// (Rust drew it from the element's prepaint with `defer_draw`, because
// only the painted layout knew where the caret landed. mygo builds it in
// the view: the anchor is placed by the line's last layout, or by the
// mono font's fixed advance before there is one, and the popup is an
// overlay attached to it, which mygo flips above the line and keeps in
// the window as the Rust's own arithmetic did.)
func (e *SqlEditor) completionsMenu(c *ui.Context, colors *theme.ThemeColors) {
	// Anchor to the start of the word, so the popup lines up with what
	// it is completing rather than drifting right as the user types.
	offset := e.cursorOffset()
	row := e.rowOf(offset)
	onRow := e.lineRange(row)
	anchor := min(max(min(e.completionRange.Start, offset)-onRow.Start, 0), onRow.End-onRow.Start)
	x := e.xFor(row, anchor)
	target := ui.Box(c.Key("sql-editor-completion-anchor")).
		Absolute().
		Left(textPaddingX + x).
		Top(textPaddingY + lineHeight*float32(row)).
		Width(1).
		Height(lineHeight).
		PassThrough()

	ui.Overlay(c, func() {
		menu := ui.Column(c.Key("sql-editor-completions")).
			AttachTo(target, ui.AnchorBottomLeft, ui.AnchorTopLeft).
			Margin(menuGap, 0, menuGap, 0).
			KeepFocus().
			MinWidth(menuMinWidth).
			MaxWidth(menuMaxWidth).
			PaddingY(4.).
			Background(colors.Elevated.Color()).
			Border(1, colors.BorderStrong.Color()).
			// The rows are 6px round and sit 4px in, so the panel is 10:
			// the two curves stay one shape at the corner.
			Radius(uikit.SurfaceRadius).
			Font(theme.MonoFontFamily).
			FontSize(12.).
			TextColor(colors.Text.Color())
		uikit.WithShadows(menu, uikit.SurfaceShadow())
		menu.Children(func() {
			for ix, completion := range e.completions {
				selected := ix == e.completionIx
				pieces := matchedSpans(completion.Label, completion.Matched)
				last := max(len(pieces)-1, 0)
				row := ui.Row(c.Key(fmt.Sprintf("completion-%d", ix))).
					Margin(0, 4.).
					AlignItems(ui.Center).
					Justify(ui.SpaceBetween).
					Gap(12.).
					Padding(4., 8.).
					Radius(6.).
					Cursor(ui.CursorPointer)
				// After the build: accepting closes the panel being built.
				at := ix
				row.OnClick(func() { e.applyCompletion(at) })
				if selected {
					row.Background(colors.Selection.Color())
				} else if row.Hovered() {
					row.Background(colors.Hairline.Color())
				}
				row.Children(func() {
					ui.Row(c).MinWidth(0).Clip().Children(func() {
						for at, piece := range pieces {
							text := ui.Text(c, piece.text)
							if at == last {
								// Only the last span may give way: a
								// long name has to end in an ellipsis
								// rather than run out of the panel.
								text.SingleLine()
							} else {
								text.Shrink(0)
							}
							if piece.hit {
								text.FontWeight(500).TextColor(colors.AccentDeep.Color())
							} else {
								text.TextColor(colors.Text.Color())
							}
						}
					})
					ui.Box(c).Shrink(0).FontSize(11.).TextColor(colors.TextFaint.Color()).Children(func() {
						ui.Text(c, completion.Detail)
					})
				})
			}
			ui.Box(c).
				Margin(4., 0, 0, 0).
				Padding(5., 12., 2., 12.).
				BorderWidth(1, 0, 0, 0).
				BorderColor(colors.Hairline.Color()).
				FontSize(10.).
				TextColor(colors.TextFaint.Color()).
				Children(func() { ui.Text(c, "↩ or ⇥ to insert · esc to dismiss") })
		})
	})
}

// xFor is where byte `ix` of buffer row `row` sits from the line's left
// edge: off the last paint's layout when it holds the row, else by the
// mono font's fixed advance over the expanded columns.
func (e *SqlEditor) xFor(row, ix int) float32 {
	if e.lastLayout != nil {
		if line := e.lastLayout.line(row); line != nil {
			return line.xFor(ix)
		}
	}
	line := e.line(row)
	return float32(columnOf(line, min(ix, len(line)))) * monoAdvance()
}

// rowSpacer is the room `rows` lines take, with nothing in it: what stands
// in for the rows above and below the ones on screen, so the columns
// beside the text stay as tall as the buffer and scroll with it.
func rowSpacer(c *ui.Context, rows int) {
	ui.Box(c).Shrink(0).Height(lineHeight * float32(rows))
}

// lineMark is what one line of the buffer wears: which statement of the
// last run covers it, and whether it is the line that statement starts on
// — the mark and the statistic are drawn once, and the rail and the wash
// carry the rest of it.
type lineMark struct {
	// Index of the statement in the run, so the statistic can be read
	// back off the mark.
	ix     int
	status StatementStatus
	first  bool
}

// shapeMark is Rust's `Option<bool>` per line of `line_shape`: `shapeNone`
// is `None`, `shapeFirst` is `Some(true)`, `shapeRest` is `Some(false)`.
type shapeMark int8

const (
	shapeNone shapeMark = iota
	shapeFirst
	shapeRest
)

func allNone(shapeLines []shapeMark) bool {
	for _, m := range shapeLines {
		if m != shapeNone {
			return false
		}
	}
	return true
}

// lineCountOf is `content.split('\n').count()`.
func lineCountOf(content string) int {
	return strings.Count(content, "\n") + 1
}

// lineStartsOf is every line's start offset in `content`.
func lineStartsOf(content string) []int {
	starts := []int{0}
	for ix := 0; ix < len(content); ix++ {
		if content[ix] == '\n' {
			starts = append(starts, ix+1)
		}
	}
	return starts
}

// lineShape is which lines the buffer's shape-changing statements cover,
// and on which of them each starts — `shapeFirst` on the first line,
// `shapeRest` on the rest, `shapeNone` on a line no such statement covers.
// Read off the text alone, so it is right before a run and after an edit
// alike.
func lineShape(content string, shape []Range) []shapeMark {
	return lineShapeAt(lineStartsOf(content), shape)
}

// lineShapeAt is lineShape over a buffer whose lines start at `starts`.
func lineShapeAt(starts []int, shape []Range) []shapeMark {
	lines := make([]shapeMark, len(starts))
	if len(shape) == 0 {
		return lines
	}
	for _, r := range shape {
		first := rowForOffset(starts, r.Start)
		last := max(rowForOffset(starts, max(r.End-1, 0)), first)
		for row := first; row <= min(last, max(len(lines)-1, 0)); row++ {
			if row == first {
				lines[row] = shapeFirst
			} else {
				lines[row] = shapeRest
			}
		}
	}
	return lines
}

func kindAt(shapeLines []shapeMark, row int) StatementKind {
	if row >= 0 && row < len(shapeLines) && shapeLines[row] != shapeNone {
		return StatementDdl
	}
	return StatementPlain
}

// lineMarks is what each line of `content` wears in the gutter (nil for
// `None`).
//
// A plain function over the text and the ranges, so the mapping can be
// argued with in a test rather than in a running window.
func lineMarks(content string, statements []StatementMark) []*lineMark {
	marks := make([]*lineMark, lineCountOf(content))
	if len(statements) == 0 {
		return marks
	}
	starts := lineStartsOf(content)
	for ix, statement := range statements {
		first := rowForOffset(starts, statement.Range.Start)
		// The end offset is one past the statement's last character, so a
		// statement that ends at a line break belongs to the line before
		// it rather than opening the next one.
		last := max(rowForOffset(starts, max(statement.Range.End-1, 0)), first)
		for row := first; row <= min(last, max(len(marks)-1, 0)); row++ {
			marks[row] = &lineMark{
				ix:     ix,
				status: statement.Status,
				first:  row == first,
			}
		}
	}
	return marks
}

// cachedLineMarks is lineMarks for the buffer as it is, kept until the
// text or the statements change. The gutter, the rail and the wash all
// ask for it on every frame, and working it out walks the whole buffer
// and allocates a mark per marked line; the Rust paid that per frame too.
// Comparing the kept text with the buffer costs nothing while neither has
// changed: Go compares two strings that share their bytes by pointer.
func (e *SqlEditor) cachedLineMarks() []*lineMark {
	if e.marksCache == nil || e.marksRev != e.statementsRev || e.marksContent != e.content {
		e.marksCache = lineMarks(e.content, e.statements)
		e.marksContent = e.content
		e.marksRev = e.statementsRev
	}
	return e.marksCache
}

// washColor is the wash behind a statement's lines, if it wears one.
//
// A plain statement is washed only while it is news — running, or
// failed — and a shape-changing one is washed in its own family from the
// moment it is queued, because it is news whatever state it is in. A
// failure takes the error's surface in either kind: a `DROP` the server
// refused changed nothing, and teal would say it did.
func washColor(kind StatementKind, status StatementStatus, colors *theme.ThemeColors) (theme.Hsla, bool) {
	switch {
	case status == StatementFailed:
		return colors.ErrorSurface, true
	case kind == StatementDdl:
		return colors.DdlSurface, true
	case status == StatementRunning:
		return colors.RunningSurface, true
	default:
		return theme.Hsla{}, false
	}
}

// metaColor is the colour of the statistic beside a statement, by how it
// ended. The shape-changing family keeps its own ink for a statement that
// landed; the rest is shared, because "failed" is failed whatever the
// statement.
func metaColor(kind StatementKind, status StatementStatus, colors *theme.ThemeColors) theme.Hsla {
	switch {
	case status == StatementQueued || status == StatementSkipped:
		return colors.TextMuted
	case status == StatementFailed:
		return colors.Error
	case kind == StatementDdl:
		return colors.DdlText
	case status == StatementRunning:
		return colors.AccentDeep
	default: // Plain, Done
		return colors.EnvDevText
	}
}

// statementMark is the mark a statement wears in the gutter, on the line
// its text starts on. Five states, and they have to be told apart at 8
// pixels: **the shape says which**, not the colour alone — a ring, a dot,
// a tick, a filled square and a dash, in the same warm family as
// everything else on this screen.
func statementMark(c *ui.Context, status StatementStatus, colors *theme.ThemeColors) {
	switch status {
	// A ring: the outline of the dot it is about to become.
	case StatementQueued:
		ui.Box(c).
			Size(markSize, markSize).
			Radius(markSize/2.).
			Border(2, colors.RunningBorder.Color())
	case StatementRunning:
		// The dot is painted by its own `Draw`, which reads the breath
		// off the painter's clock and asks for repaints: a tone that
		// moves while nothing else does must not rebuild the whole view
		// at the display's rate for as long as a statement runs.
		color := colors.RunningMark.Color()
		ui.Box(c).
			Size(markSize, markSize).
			Draw(func(p *ui.Painter, r ui.Rect) {
				p.Fill(r, color.Alpha(1.-markBreathDepth*runningBreath(p)), markSize/2.)
			})
	case StatementDone:
		ui.Row(c).
			Size(markSize+1., markSize+1.).
			Radius((markSize + 1.) / 2.).
			Background(colors.Ok.Color()).
			AlignItems(ui.Center).
			Justify(ui.Center).
			Children(func() { uikit.DrawIcon(c, uikit.IconCheck, 10., colors.OnAccent) })
	// The one mark that is filled and clay: a failure is the only
	// state here that stops the run, and it is read at a glance from
	// the other side of the pane.
	case StatementFailed:
		ui.Row(c).
			Size(markSize+2., markSize+2.).
			Radius(3.).
			Background(colors.EnvProd.Color()).
			AlignItems(ui.Center).
			Justify(ui.Center).
			FontSize(10.).
			FontWeight(700).
			TextColor(colors.OnAccent.Color()).
			Children(func() { ui.Text(c, "!") })
	// A dash: nothing happened here, and nothing is what it draws.
	case StatementSkipped:
		ui.Box(c).
			Width(markSize).
			Height(2.).
			Background(colors.Idle.Color())
	}
}

// markBreathFPS is how often the breathing dot is repainted. A tone that
// takes 1.4 s to go down and back up looks no different at thirty frames
// a second than at a display's hundred and twenty.
const markBreathFPS = 30

// runningBreath is how far into its breath the running dot is, 0..1, in
// the frame being painted, and it asks the painter for the next repaint.
// The breath is phase-locked to the clock — its phase is the time since
// the Unix epoch modulo the period — as the run button's is, so the dot
// never starts over; under reduce motion it holds still at the start and
// asks for nothing. (`uikit.AnimationDelta`'s arithmetic for a synced
// repeating animation, read off the painter rather than the view.)
func runningBreath(p *ui.Painter) float32 {
	ease := uikit.PulsatingBetween(0., 1.)
	if p.Preferences().ReduceMotion {
		return ease(0.)
	}
	n := p.Now().UnixNano() % int64(markBreath)
	if n < 0 {
		n += int64(markBreath)
	}
	p.AnimationFrameRate(markBreathFPS)
	return ease(float32(time.Duration(n).Seconds()) / float32(markBreath.Seconds()))
}

// railColor is the rail's colour beside a statement's lines. Queued and
// skipped take the plain rule the buffer wears everywhere else: neither is
// news. A shape-changing statement's rail is teal in every state but
// failure, deepening as the statement goes from queued to running to
// landed.
func railColor(kind StatementKind, status StatementStatus, colors *theme.ThemeColors) theme.Hsla {
	switch {
	case status == StatementFailed:
		return colors.ErrorMark
	case kind == StatementDdl && (status == StatementQueued || status == StatementSkipped):
		return colors.DdlInner
	case kind == StatementDdl && status == StatementRunning:
		return colors.Ddl
	case kind == StatementDdl: // Done
		return colors.DdlDone
	case status == StatementQueued || status == StatementSkipped:
		return colors.Border
	case status == StatementRunning:
		return colors.RunningMark
	default:
		// The dev family's green, which the read-only mark and a commit
		// already wear: this app says "that worked" in one colour.
		return colors.EnvDevInner
	}
}

// ddlChip is the `DDL` chip on a shape-changing statement's first line:
// the comp's 8px cap in the family's ring colour, so the kind is named and
// not only coloured.
func ddlChip(c *ui.Context, colors *theme.ThemeColors) {
	ui.Box(c).
		Shrink(0).
		Padding(3., 6.).
		Radius(4.).
		Background(colors.Ddl.Color()).
		FontSize(8.).
		FontWeight(600).
		TextColor(colors.DdlSurface.Color()).
		Children(func() { ui.Text(c, "DDL") })
}

// matchedPiece is one `(String, bool)` of `matched_spans`.
type matchedPiece struct {
	text string
	hit  bool
}

// matchedSpans cuts a label into the pieces the panel paints: each piece
// with whether the typed word matched it. The ranges come from the
// matcher, in order and never overlapping, so this is one walk with no
// sorting.
func matchedSpans(label string, matched []Range) []matchedPiece {
	pieces := make([]matchedPiece, 0, len(matched)*2+1)
	at := 0
	for _, hit := range matched {
		if hit.Start < at || hit.End > len(label) {
			continue
		}
		if hit.Start > at {
			pieces = append(pieces, matchedPiece{label[at:hit.Start], false})
		}
		pieces = append(pieces, matchedPiece{label[hit.Start:hit.End], true})
		at = hit.End
	}
	if at < len(label) {
		pieces = append(pieces, matchedPiece{label[at:], false})
	}
	return pieces
}

// utf16ToByte is the byte offset for a UTF-16 offset *within* `text`,
// clamped to its end. The platform reports a composition's selection
// relative to the text it just handed us, not to the whole buffer.
func utf16ToByte(text string, utf16Offset int) int {
	utf8n, utf16n := 0, 0
	for _, ch := range text {
		if utf16n >= utf16Offset {
			return utf8n
		}
		utf16n += utf16Len(ch)
		utf8n += utf8.RuneLen(ch)
	}
	return len(text)
}

// utf16Len is Rust's `char::len_utf16`.
func utf16Len(ch rune) int {
	if ch >= 0x10000 {
		return 2
	}
	return 1
}

// isCharBoundary is Rust's `str::is_char_boundary`.
func isCharBoundary(text string, ix int) bool {
	switch {
	case ix == 0:
		return true
	case ix < len(text):
		return utf8.RuneStart(text[ix])
	default:
		return ix == len(text)
	}
}

// trim is Rust's `str::trim`: White_Space off both ends.
func trim(s string) string {
	return strings.TrimFunc(s, isWhitespace)
}

// clampOffset forces an offset inside `text` and onto a character
// boundary.
func clampOffset(text string, offset int) int {
	offset = min(max(offset, 0), len(text))
	for offset > 0 && !isCharBoundary(text, offset) {
		offset--
	}
	return offset
}

// clampRange forces a range inside `text`, onto character boundaries, and
// in order.
//
// The platform hands us ranges it worked out from its own copy of the
// buffer, which can lag ours by an edit. Slicing on a stale range panics,
// and a panic inside an input-handler callback crosses an `extern "C"`
// boundary, where Rust aborts the process instead of unwinding. So every
// range that reaches a slice goes through here first.
func clampRange(text string, r Range) Range {
	start := clampOffset(text, r.Start)
	end := max(clampOffset(text, r.End), start)
	return Range{start, end}
}

func rowForOffset(lineStarts []int, offset int) int {
	row, found := slices.BinarySearch(lineStarts, offset)
	if found {
		return row
	}
	return max(row-1, 0)
}

// --- the text element (Rust: `EditorElement`) ---------------------------

// quad is GPUI's `PaintQuad` as `fill(bounds, color)` makes it.
type quad struct {
	r     ui.Rect
	color theme.Hsla
}

// underlineStyle is GPUI's `UnderlineStyle`: a nil colour is the run's.
type underlineStyle struct {
	color     *theme.Hsla
	thickness float32
	wavy      bool
}

// textRun is GPUI's `TextRun`, as a range of the shaped text rather than a
// length.
type textRun struct {
	r         Range
	color     theme.Hsla
	underline *underlineStyle
}

// editorFont is the font every line is shaped in.
func editorFont() ui.Font {
	return ui.Font{Family: theme.MonoFontFamily, Size: fontSize}
}

// monoAdvance is the mono font's fixed advance (docs/tasks/gpui-to-mygo.md
// §5: every JetBrains Mono glyph has it), asked of the text system once.
func monoAdvance() float32 {
	advance, _ := fontGrid(editorFont())
	return advance
}

// paint is the Rust `EditorElement`: its prepaint — the rows the clip
// leaves in view, shaped, and the quads behind them — and its paint, in
// one `Draw`. `bounds` is the text element's box in window DIPs.
func (e *SqlEditor) paint(p *ui.Painter, bounds ui.Rect, focused bool, colors *theme.ThemeColors) {
	placeholder := e.content == ""
	// The text colour the query pane hands down (`colors.text_body`).
	textColor := colors.TextBody
	if placeholder {
		textColor = colors.TextFaint
	}

	// The rows the scroll containers leave in view, read off the clip
	// they set: these bounds are the whole buffer's, and shaping the
	// whole buffer is what made a long one slow.
	clip := p.Visible()
	rows := visibleRows(clip.Y-bounds.Y, clip.H, 0., lineHeight, e.LineCount())

	squiggle := underlineStyle{color: &colors.Error, thickness: 1., wavy: true}
	// Across, the clip leaves a stretch of each line in view as well:
	// a long line is shaped and coloured only there (see layout.go).
	visLeft, visRight := clip.X-bounds.X-glyphPad, clip.X+clip.W-bounds.X+glyphPad
	kept := e.lines
	e.lines = make(map[string]*lineCache, rows.End-rows.Start)
	lines := make([]lineLayout, 0, rows.End-rows.Start)
	for row := rows.Start; row < rows.End; row++ {
		onRow := e.lineRange(row)
		var line string
		if placeholder {
			line, _, _ = strings.Cut(e.placeholder, "\n")
		} else {
			line = e.content[onRow.Start:onRow.End]
		}
		text, tabs := NewExpanded(line)
		lc := kept[text]
		if lc == nil || lc.font != editorFont() {
			lc = newLineCache(text, editorFont())
		}
		e.lines[text] = lc
		ll := lineLayout{text: text, grid: lc.grid, tabs: tabs}
		// The stretch of the expanded text worth colouring: the glyphs
		// this paint walks, and no more.
		lo, hi := 0, len(text)
		if g := lc.grid; g != nil {
			from := toUsize(float32(math.Floor(float64(visLeft / g.advance))))
			to := toUsize(float32(math.Ceil(float64(visRight / g.advance))))
			window, column := lc.windowOf(from, to)
			ll.shaped, ll.paintX = window, float32(column)*g.advance
			lo, hi = lc.windowFrom, lc.windowTo
		} else {
			ll.shaped = lc.whole()
			if g0, g1 := ll.shaped.visible(visLeft, visRight); g0 > 0 || g1 < len(ll.shaped.glyphs) {
				lo, hi = len(text), 0
				for i := g0; i < g1; i++ {
					lo, hi = min(lo, ll.shaped.index[i]), max(hi, ll.shaped.index[i]+1)
				}
			}
		}
		underline := e.underlineFor(onRow.Start, onRow.End-onRow.Start)
		if placeholder {
			ll.runs = singleRun(text, textColor, underline)
		} else {
			// The marks are the buffer's bytes; the runs are the
			// shaped text's.
			var marks []Range
			for _, mark := range marksOnLine(e.diagnostics, onRow.Start, onRow.End-onRow.Start) {
				marks = append(marks, Range{tabs.ToDisplay(mark.Start), tabs.ToDisplay(mark.End)})
			}
			for _, piece := range splitSpans(spansIn(lc.spansFor(e.vocabulary), lo, hi), marks) {
				run := textRun{r: piece.Range, color: tokenColor(piece.Token, textColor, colors)}
				// A squiggle under an IME composition would be two
				// underlines in one place; the composition is what
				// the user is doing now, so it wins.
				if underline != nil {
					run.underline = underline
				} else if piece.Marked {
					run.underline = &squiggle
				}
				ll.runs = append(ll.runs, run)
			}
		}
		lines = append(lines, ll)
	}

	layout := &editorLayout{
		firstRow:   rows.Start,
		lines:      lines,
		lineHeight: lineHeight,
	}

	// Where the caret is in its blink, by this frame's clock; the
	// painter is asked for the repaint of the next turn.
	caretOn := e.blink.On()
	if focused {
		caretOn = e.blink.PaintOn(p)
	}
	var quads []quad
	var cursor *quad
	if placeholder {
		// An empty buffer still has a caret: it is where the typing
		// will go, and a pane with none reads as one that is not
		// listening.
		cursor = e.cursorQuad(layout, bounds, colors.Accent, caretOn)
	} else {
		if focused && e.selectedRange.IsEmpty() {
			if q := e.activeLineQuad(layout, bounds, clip, colors.ActiveLine); q != nil {
				quads = append(quads, *q)
			}
		}
		quads = append(quads, e.washQuads(layout, bounds, colors)...)
		quads = append(quads, e.rangeQuads(e.selectedRange, layout, bounds, colors.Selection)...)
		// Over the selection, because the hit the walk stands on *is*
		// the selection, and it has to read as the current hit rather
		// than as text the user happened to mark. Only the hits that
		// reach the rows on screen: a buffer can hold ten thousand.
		onScreen := Range{e.lineRange(layout.firstRow).Start, e.lineRange(max(layout.rows().End-1, 0)).End}
		for _, hit := range e.hits {
			if !(hit.Start <= onScreen.End && hit.End >= onScreen.Start) {
				continue
			}
			color := colors.FindHit
			if hit == e.selectedRange {
				color = colors.FindCurrent
			}
			quads = append(quads, e.rangeQuads(hit, layout, bounds, color)...)
		}
		cursor = e.cursorQuad(layout, bounds, colors.Accent, caretOn)
	}

	// --- paint ---

	for _, q := range quads {
		p.Fill(q.r, q.color.Color(), 0)
	}

	metrics := editorFont().Metrics()
	for ix, line := range layout.lines {
		top := bounds.Y + layout.lineHeight*float32(layout.firstRow+ix)
		paintLine(p, &line, bounds.X, top, layout.lineHeight, metrics, visLeft, visRight)
	}

	if focused && cursor != nil {
		p.Fill(cursor.r, cursor.color.Color(), 0)
	}

	e.lastLayout = layout
	box := bounds
	e.lastBounds = &box

	// `render` could not follow the caret because nothing had measured
	// the pane. This paint has, so ask once more.
	if e.pendingAutoscroll && e.scrollHandle.Bounds.H > 0. {
		p.AnimationFrame()
	}
}

// paintLine paints one shaped line at `top`, its glyphs centred in the
// line height as GPUI's `ShapedLine::paint` placed them, each run in its
// colour and with its underline.
//
// Only the glyphs whose left edge falls in `visLeft..=visRight` (from the
// line's left edge) are walked, and they are walked once: consecutive
// glyphs of one run go to the painter as one slice of the shaped line,
// never copied. A long line used to be walked whole once per run.
func paintLine(p *ui.Painter, line *lineLayout, left, top, height float32, metrics ui.FontMetrics, visLeft, visRight float32) {
	shaped := line.shaped
	if len(shaped.glyphs) == 0 {
		return
	}
	// GPUI centres the glyphs in the line height (text_system/line.rs,
	// `padding_top`).
	baseline := top + (height-metrics.Ascent-metrics.Descent)/2 + metrics.Ascent
	origin := left + line.paintX
	g0, g1 := shaped.visible(visLeft-line.paintX, visRight-line.paintX)
	run, from := -1, g0
	flush := func(to int) {
		if run >= 0 && to > from {
			p.Glyphs(shaped.glyphs[from:to], origin, baseline, line.runs[run].color.Color())
		}
	}
	for i := g0; i < g1; i++ {
		at := runAt(line.runs, shaped.index[i], run)
		if at != run {
			flush(i)
			run, from = at, i
		}
	}
	flush(g1)
	for _, run := range line.runs {
		u := run.underline
		if u == nil {
			continue
		}
		color := run.color
		if u.color != nil {
			color = *u.color
		}
		x0 := left + line.displayX(run.r.Start)
		x1 := left + line.displayX(run.r.End)
		// GPUI's underline origin (text_system/line.rs).
		y := baseline + metrics.Descent*0.618
		// Drawn only as far as the clip reaches: a squiggle the width of
		// a two-hundred-kilobyte line is a point per pixel of it.
		lo, hi := left+visLeft, left+visRight
		if u.wavy {
			paintWave(p, x0, x1, max(x0, lo), min(x1, hi), y, u.thickness, color)
		} else if a, b := max(x0, lo), min(x1, hi); b >= a {
			p.Fill(ui.Rect{X: a, Y: y, W: b - a, H: u.thickness}, color.Color(), 0)
		}
	}
}

// runAt is the run that holds offset `ix` of the shaped text, or -1. The
// runs are in order and never overlap, and the glyphs of a line mostly
// come in order too, so the run the last glyph was in is asked first.
func runAt(runs []textRun, ix, hint int) int {
	if hint >= 0 && hint < len(runs) {
		if r := runs[hint].r; r.Start <= ix && ix < r.End {
			return hint
		}
		if next := hint + 1; next < len(runs) {
			if r := runs[next].r; r.Start <= ix && ix < r.End {
				return next
			}
		}
	}
	i := sort.Search(len(runs), func(i int) bool { return runs[i].r.End > ix })
	if i < len(runs) && runs[i].r.Start <= ix {
		return i
	}
	return -1
}

// spansIn is the spans that reach into `lo..hi` of the line, whole: the
// colour of the stretch a paint walks, and nothing past it.
func spansIn(list []span, lo, hi int) []span {
	i := sort.Search(len(list), func(i int) bool { return list[i].Range.End > lo })
	j := sort.Search(len(list), func(i int) bool { return list[i].Range.Start >= hi })
	return list[i:max(j, i)]
}

// paintWave is GPUI's wavy underline: a box three thicknesses tall with a
// sine through its middle, a period of sixteen pixels at a thickness of
// one (the underline shader's `frequency` and `amplitude`). The wave runs
// `x0..x1` and is drawn over `from..to` of it, at the very points the
// whole wave would have there, so a clipped squiggle keeps its phase.
func paintWave(p *ui.Painter, x0, x1, from, to, y, thickness float32, color theme.Hsla) {
	if x1 <= x0 || to <= from {
		return
	}
	height := thickness * 3.
	middle := y + height/2.
	// In the shader's units of the box's height: `amplitude` is
	// 1/(2·thickness) and `frequency` π·3·thickness/8.
	amplitude := height / (2. * thickness)
	frequency := float32(math.Pi*3.*float64(thickness)/8.) / height
	var points [][2]float32
	start := x0 + float32(math.Max(math.Floor(float64(from-x0)), 0))
	for x := start; ; x += 1. {
		if x > to {
			x = to
		}
		dy := float32(math.Sin(float64((x - x0) * frequency)))
		points = append(points, [2]float32{x, middle + dy*amplitude})
		if x >= to {
			break
		}
	}
	p.StrokePolyline(points, thickness, color.Color())
}

func singleRun(line string, color theme.Hsla, underline *underlineStyle) []textRun {
	if line == "" {
		return nil
	}
	return []textRun{{r: Range{0, len(line)}, color: color, underline: underline}}
}

func tokenColor(token Token, plain theme.Hsla, colors *theme.ThemeColors) theme.Hsla {
	switch token {
	case TokenKeyword:
		return colors.AccentDeep
	case TokenLiteral:
		return colors.SyntaxLiteral
	case TokenComment:
		return colors.TextMuted
	// A name the connected database actually has. A typo stays plain,
	// which is the point: the colour doubles as a spell check.
	case TokenIdentifier:
		return colors.SyntaxIdentifier
	default:
		return plain
	}
}

// marksOnLine is the error marks that fall on one line, as byte ranges
// into the line.
//
// A statement is often several lines long, so one diagnostic can reach
// two of them; the shaped line is what carries the squiggle, and it knows
// only its own bytes.
func marksOnLine(diagnostics []Diagnostic, lineStart, lineLen int) []Range {
	lineEnd := lineStart + lineLen
	out := []Range{}
	for _, d := range diagnostics {
		if !(d.Range.Start < lineEnd && d.Range.End > lineStart) {
			continue
		}
		out = append(out, Range{
			min(max(d.Range.Start, lineStart), lineEnd) - lineStart,
			min(max(d.Range.End, lineStart), lineEnd) - lineStart,
		})
	}
	return out
}

// piece is one `(Range<usize>, Token, bool)` of `split_spans`.
type piece struct {
	Range  Range
	Token  Token
	Marked bool
}

// splitSpans cuts the coloured spans wherever a mark starts or ends, and
// says of each piece whether it is inside one.
//
// A `TextRun` carries one underline for its whole length, and a mark
// rarely lines up with a colour: `frm` is one plain span, and the mark on
// `select 1 frm` covers the last third of it. So the spans are split at
// the mark's own edges and every piece keeps the colour it had.
func splitSpans(list []span, marks []Range) []piece {
	if len(marks) == 0 {
		out := make([]piece, 0, len(list))
		for _, s := range list {
			out = append(out, piece{s.Range, s.Token, false})
		}
		return out
	}
	var pieces []piece
	for _, s := range list {
		var cuts []int
		for _, mark := range marks {
			for _, cut := range [2]int{mark.Start, mark.End} {
				if s.Range.Start < cut && cut < s.Range.End {
					cuts = append(cuts, cut)
				}
			}
		}
		slices.Sort(cuts)
		cuts = slices.Compact(cuts)
		start := s.Range.Start
		for _, cut := range append(cuts, s.Range.End) {
			marked := false
			for _, mark := range marks {
				if mark.Start <= start && start < mark.End {
					marked = true
					break
				}
			}
			pieces = append(pieces, piece{Range{start, cut}, s.Token, marked})
			start = cut
		}
	}
	return pieces
}

// underlineFor underlines the IME's marked text, if it falls on this line.
func (e *SqlEditor) underlineFor(lineStart, lineLen int) *underlineStyle {
	marked := e.markedRange
	if marked == nil {
		return nil
	}
	if !(marked.Start < lineStart+lineLen && marked.End > lineStart) {
		return nil
	}
	return &underlineStyle{color: nil, thickness: 1., wavy: false}
}

// washQuads is the wash behind every line of a statement that wears one,
// the full width of the text — so a `DROP` three lines long is one teal
// band and not three teal words. Painted under the selection, which stays
// legible over it because the two are far apart in tone.
func (e *SqlEditor) washQuads(layout *editorLayout, bounds ui.Rect, colors *theme.ThemeColors) []quad {
	shapeLines := e.shapeLines
	if len(e.statements) == 0 && !e.anyShape {
		return nil
	}
	marks := e.cachedLineMarks()
	var out []quad
	rows := layout.rows()
	for row := rows.Start; row < rows.End; row++ {
		var mark *lineMark
		if row < len(marks) {
			mark = marks[row]
		}
		kind := kindAt(shapeLines, row)
		// A shape-changing statement is washed before any run, in the
		// queued state; a plain one only once a run has marked it.
		var status StatementStatus
		switch {
		case mark != nil:
			status = mark.status
		case kind == StatementDdl:
			status = StatementQueued
		default:
			continue
		}
		color, ok := washColor(kind, status, colors)
		if !ok {
			continue
		}
		top := bounds.Y + layout.lineHeight*float32(row)
		// Past the text padding on either side, so the band meets the
		// rail on the left and the statistic on the right.
		out = append(out, quad{fromCorners(
			bounds.X-textPaddingX, top,
			bounds.X+bounds.W+textPaddingX, top+layout.lineHeight,
		), color})
	}
	return out
}

// activeLineQuad is the wash behind the caret's line, the width of the
// pane rather than of the text: it is the row that is marked, however far
// the text is scrolled across.
func (e *SqlEditor) activeLineQuad(layout *editorLayout, bounds, clip ui.Rect, color theme.Hsla) *quad {
	row := e.rowOf(e.cursorOffset())
	if layout.line(row) == nil {
		return nil
	}
	top := bounds.Y + layout.lineHeight*float32(row)
	return &quad{fromCorners(
		min(clip.X, bounds.X-textPaddingX), top,
		max(clip.X+clip.W, bounds.X+bounds.W+textPaddingX), top+layout.lineHeight,
	), color}
}

// rangeQuads is the blocks behind one range of the buffer, a block per
// line it covers of the lines on screen.
func (e *SqlEditor) rangeQuads(r Range, layout *editorLayout, bounds ui.Rect, color theme.Hsla) []quad {
	if r.IsEmpty() || len(layout.lines) == 0 {
		return nil
	}
	rows := layout.rows()
	first := max(e.rowOf(r.Start), rows.Start)
	last := min(e.rowOf(r.End), rows.End-1)
	var out []quad
	for row := first; row <= last; row++ {
		line := layout.line(row)
		if line == nil {
			continue
		}
		onRow := e.lineRange(row)
		start, end := onRow.Start, onRow.End
		from := min(max(r.Start, start), end)
		to := min(max(r.End, start), end)
		// A line whose break is inside the selection gets a sliver of
		// trailing highlight, so a multi-line selection reads as one block
		// instead of ragged stripes.
		var trailing float32
		if r.Start <= end && r.End > end {
			trailing = charWidth
		}
		if from == to && trailing == 0. {
			continue
		}
		top := bounds.Y + layout.lineHeight*float32(row)
		out = append(out, quad{fromCorners(
			bounds.X+line.xFor(from-start), top,
			bounds.X+line.xFor(to-start)+trailing, top+layout.lineHeight,
		), color})
	}
	return out
}

// cursorQuad is the caret, at the end of the selection that moves. It is
// painted with a selection as well as without one: the selection says
// what is marked, and only the caret says which end the next ⇧→ will
// carry.
func (e *SqlEditor) cursorQuad(layout *editorLayout, bounds ui.Rect, color theme.Hsla, on bool) *quad {
	// The off half of the blink simply has no caret to paint.
	if !on {
		return nil
	}
	offset := e.cursorOffset()
	row := e.rowOf(offset)
	line := layout.line(row)
	if line == nil {
		return nil
	}
	onRow := e.lineRange(row)
	x := line.xFor(min(max(offset-onRow.Start, 0), onRow.End-onRow.Start))
	return &quad{ui.Rect{
		X: bounds.X + x,
		Y: bounds.Y + layout.lineHeight*float32(row),
		W: caretWidth,
		H: layout.lineHeight,
	}, color}
}

// fromCorners is GPUI's `Bounds::from_corners`.
func fromCorners(x0, y0, x1, y1 float32) ui.Rect {
	return ui.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}
