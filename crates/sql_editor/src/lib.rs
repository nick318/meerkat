//! A small multi-line text editor for the query tab.
//!
//! Built on the same shape as `gpui/examples/input.rs`: the entity owns the
//! buffer and implements `EntityInputHandler` so the platform delivers
//! typed text and IME edits; a custom element shapes and paints the lines.
//! This one is multi-line, so it keeps a shaped line per row and maps byte
//! offsets to (row, column) both ways.
//!
//! It carries what a macOS editor is expected to have: word and line
//! motion, selection for every motion, word and line deletion, undo and
//! redo, double and triple click selection, and a viewport that follows
//! the caret. Colouring comes from a one-pass tokenizer that also knows
//! the names in the connected database. There is no soft wrap.
//!
//! Offsets are byte offsets into the buffer and always sit on a character
//! boundary.

mod completion;
mod highlight;
mod motion;

pub use completion::{Kind, Name, Vocabulary};

use completion::Completion;
use gpui::{
    Animation, AnimationExt, AnyElement, App, Bounds, ClipboardItem, Context, CursorStyle, Div,
    Element, ElementId, ElementInputHandler, Entity, EntityInputHandler, FocusHandle, Focusable,
    FontWeight, GlobalElementId, Hsla, IntoElement, KeyContext, LayoutId, MouseButton,
    MouseDownEvent, MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point, ScrollHandle,
    ShapedLine, SharedString, Style, TextRun, UTF16Selection, UnderlineStyle, Window, actions, div,
    fill, point, prelude::*, px, relative, size,
};
use highlight::Token;
use std::ops::Range;
use std::sync::Arc;
use theme::{ThemeColors, theme};
use ui::blink::{Blink, Blinking};
use ui::scrollbar::{self, DragState, Scrollbar};

actions!(
    sql_editor,
    [
        Backspace,
        Copy,
        ConfirmCompletion,
        Cut,
        DismissCompletion,
        Delete,
        DeleteToBeginningOfLine,
        DeleteToEndOfLine,
        DeleteToNextWordEnd,
        DeleteToPreviousWordStart,
        Indent,
        MoveDown,
        MoveLeft,
        MoveRight,
        MoveToBeginning,
        MoveToBeginningOfLine,
        MoveToEnd,
        MoveToEndOfLine,
        MoveToNextWordEnd,
        MoveToPreviousWordStart,
        MoveUp,
        Newline,
        NextCompletion,
        OpenCompletion,
        Paste,
        PreviousCompletion,
        Redo,
        SelectAll,
        SelectDown,
        SelectLeft,
        SelectRight,
        SelectToBeginning,
        SelectToBeginningOfLine,
        SelectToEnd,
        SelectToEndOfLine,
        SelectToNextWordEnd,
        SelectToPreviousWordStart,
        SelectUp,
        Undo,
    ]
);

/// Key bindings for the editor, following Zed's macOS keymap. Bind these
/// once at startup; they are scoped to the editor's key context so they
/// never shadow the app's own bindings.
pub fn key_bindings() -> Vec<gpui::KeyBinding> {
    // A closure cannot take the action: `KeyBinding::new` is generic over
    // the concrete action type, and `Box<dyn Action>` is not one.
    macro_rules! bind {
        ($keystroke:expr, $action:expr) => {
            gpui::KeyBinding::new($keystroke, $action, Some(KEY_CONTEXT))
        };
    }
    macro_rules! completing {
        ($keystroke:expr, $action:expr) => {
            gpui::KeyBinding::new($keystroke, $action, Some(COMPLETING_CONTEXT))
        };
    }
    vec![
        bind!("backspace", Backspace),
        bind!("delete", Delete),
        bind!("alt-backspace", DeleteToPreviousWordStart),
        bind!("alt-delete", DeleteToNextWordEnd),
        bind!("cmd-backspace", DeleteToBeginningOfLine),
        bind!("cmd-delete", DeleteToEndOfLine),
        bind!("left", MoveLeft),
        bind!("right", MoveRight),
        bind!("up", MoveUp),
        bind!("down", MoveDown),
        bind!("alt-left", MoveToPreviousWordStart),
        bind!("alt-right", MoveToNextWordEnd),
        bind!("cmd-left", MoveToBeginningOfLine),
        bind!("cmd-right", MoveToEndOfLine),
        bind!("home", MoveToBeginningOfLine),
        bind!("end", MoveToEndOfLine),
        // The emacs pair macOS honours in every text field.
        bind!("ctrl-a", MoveToBeginningOfLine),
        bind!("ctrl-e", MoveToEndOfLine),
        bind!("cmd-up", MoveToBeginning),
        bind!("cmd-down", MoveToEnd),
        bind!("shift-left", SelectLeft),
        bind!("shift-right", SelectRight),
        bind!("shift-up", SelectUp),
        bind!("shift-down", SelectDown),
        bind!("alt-shift-left", SelectToPreviousWordStart),
        bind!("alt-shift-right", SelectToNextWordEnd),
        bind!("cmd-shift-left", SelectToBeginningOfLine),
        bind!("cmd-shift-right", SelectToEndOfLine),
        bind!("shift-home", SelectToBeginningOfLine),
        bind!("shift-end", SelectToEndOfLine),
        bind!("cmd-shift-up", SelectToBeginning),
        bind!("cmd-shift-down", SelectToEnd),
        bind!("cmd-a", SelectAll),
        bind!("enter", Newline),
        bind!("tab", Indent),
        bind!("cmd-z", Undo),
        bind!("cmd-shift-z", Redo),
        bind!("cmd-v", Paste),
        bind!("cmd-c", Copy),
        bind!("cmd-x", Cut),
        bind!("ctrl-space", OpenCompletion),
        // Registered last, and scoped to the completing context, so they
        // outrank the caret bindings above only while the panel is open.
        completing!("down", NextCompletion),
        completing!("ctrl-n", NextCompletion),
        completing!("up", PreviousCompletion),
        completing!("ctrl-p", PreviousCompletion),
        completing!("enter", ConfirmCompletion),
        completing!("tab", ConfirmCompletion),
        completing!("escape", DismissCompletion),
    ]
}

const KEY_CONTEXT: &str = "SqlEditor";
/// Only true while the completion panel is open, so ↑↓/enter/tab keep
/// their ordinary meaning the rest of the time.
const COMPLETING_CONTEXT: &str = "SqlEditor && completing";
/// The completion popup keeps a readable width without stretching to fit
/// a long column name.
const MENU_MIN_WIDTH: f32 = 220.;
const MENU_MAX_WIDTH: f32 = 380.;
/// Gap between the caret's line and the popup.
const MENU_GAP: f32 = 4.;
/// One tab inserts this much, matching the design comp's indented SQL.
const INDENT: &str = "  ";
/// 12px text on the comp's 1.75 line height.
const FONT_SIZE: f32 = 12.;
const LINE_HEIGHT: f32 = 21.;
const GUTTER_WIDTH: f32 = 64.;
/// The lane of the gutter that carries a statement's mark, left of the
/// line numbers. Wide enough for the widest of them with air either side.
const MARK_WIDTH: f32 = 20.;
/// How big a mark is drawn. It is read beside 12px text at a glance, from
/// the other side of the pane, so it is a shade *larger* than the type
/// rather than a hint tucked under it — the first version was 8px and
/// disappeared into the gutter.
const MARK_SIZE: f32 = 12.;
/// The rail between the gutter and the text: one column of colour saying
/// which lines belong to which statement, and how that statement ended.
/// The mark alone sits on the statement's first line, and a statement is
/// often several lines long — the rail is what says where it reaches to.
const RAIL_WIDTH: f32 = 2.;
/// The running mark breathes rather than spins: the run button already
/// says "working" by mixing a tone in and back out, and a shape turning in
/// the gutter of a text editor is a lot of movement for one 8px dot.
const MARK_BREATH: std::time::Duration = std::time::Duration::from_millis(1400);
/// How far down the breath takes the dot. It never leaves the screen: a
/// mark that blinks out is one the eye reads as gone rather than as busy.
const MARK_BREATH_DEPTH: f32 = 0.55;
/// One character's advance. The font is monospaced, so a line's width is
/// its character count times this — JetBrains Mono advances 0.6em, which is
/// exactly what the text system reports for it. It sizes the scrollable
/// width only, so a double-width script costs a little scroll travel and
/// nothing else.
const CHAR_WIDTH: f32 = FONT_SIZE * 0.6;
const TEXT_PADDING_X: f32 = 16.;
const TEXT_PADDING_Y: f32 = 12.;
/// Deep enough for a long editing session, bounded so a runaway paste
/// loop cannot grow the history without limit.
const UNDO_DEPTH: usize = 256;

/// How far one statement of the last run got.
///
/// A buffer is several statements and a run sends them in turn, so "did
/// that work" has one answer per statement rather than one for the run.
/// The gutter is where that answer belongs: the user is reading the
/// statements there, and a line of prose under the grid cannot point at
/// the third of five.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum StatementStatus {
    /// Sent nowhere yet: the statements ahead of it are still running.
    #[default]
    Queued,
    /// The server has this one.
    Running,
    /// It came back.
    Done,
    /// The server refused it. It is the last statement a run reaches.
    Failed,
    /// Never sent, because the statement before it failed or the user
    /// stopped the run.
    Skipped,
}

/// What kind of statement a mark is on, for the one kind that is painted
/// apart from the rest.
///
/// A `CREATE`, `ALTER`, `DROP` or `TRUNCATE` changes something the user is
/// looking at elsewhere — the sidebar, the next query's columns — and
/// answers with neither rows nor a count, so it is the one statement whose
/// effect is invisible from the result pane. The comp gives it a cool teal
/// family of its own against the warm paper, a `DDL` chip on its first
/// line, and a wash over every line it covers, so the statement that
/// changed the shape of the database reads as such from across the pane.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum StatementKind {
    #[default]
    Plain,
    Ddl,
}

/// One statement of the last run, and where its text sits in the buffer.
#[derive(Debug, Clone, Default)]
pub struct StatementMark {
    pub range: Range<usize>,
    pub status: StatementStatus,
    pub kind: StatementKind,
    /// The statistic beside the statement, on its first line and at the
    /// right of the pane: `6 rows · 34 ms`, `1,204 rows deleted · 61 ms`,
    /// `altered · 12 ms`, `failed · 8 ms`, `not run · statement 3 failed`.
    /// Whoever owns the editor decides the words; the editor paints them
    /// in the status's colour. Empty paints nothing.
    pub meta: SharedString,
}

/// One thing the server refused to parse, and where it sits in the buffer.
///
/// The editor holds these rather than the message alone, because a mark
/// under the token is the whole point: a line of prose under the pane
/// cannot point at the third statement of five, which is the same reason
/// [`StatementMark`] exists.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Diagnostic {
    pub range: Range<usize>,
    pub message: String,
}

/// What the editor tells whoever owns it.
///
/// One event, and it is the text: a syntax check has to run again when the
/// buffer changes and must not run again when the caret merely moves.
pub enum SqlEditorEvent {
    Changed,
}

pub struct SqlEditor {
    focus_handle: FocusHandle,
    /// Where the buffer is scrolled down to. The gutter is inside this one,
    /// so the line numbers travel with the text.
    scroll_handle: ScrollHandle,
    /// Where it is scrolled across to. A second container, holding the text
    /// alone: a long line must not push the line numbers off the left of
    /// the pane, which one container over both would do.
    h_scroll_handle: ScrollHandle,
    /// Which of the two scrollbars is being dragged, if either.
    scroll_drag: DragState,
    content: String,
    placeholder: SharedString,
    vocabulary: Arc<Vocabulary>,
    selected_range: Range<usize>,
    selection_reversed: bool,
    marked_range: Option<Range<usize>>,
    undo: Vec<Snapshot>,
    redo: Vec<Snapshot>,
    /// What the last edit was, so a run of typing collapses into one undo
    /// step instead of one step per character.
    last_edit: EditKind,
    last_layout: Option<EditorLayout>,
    last_bounds: Option<Bounds<Pixels>>,
    is_selecting: bool,
    /// Set whenever the caret moves, cleared once the viewport follows it.
    pending_autoscroll: bool,
    /// Candidates for the word under the caret. Empty means the panel is
    /// closed, which is also what drives the `completing` key context.
    completions: Vec<Completion>,
    completion_ix: usize,
    /// The word the candidates would replace.
    completion_range: Range<usize>,
    /// Escape closes the panel until the next edit, so it does not spring
    /// straight back open on the next keystroke.
    completions_dismissed: bool,
    /// The statements of the last run, in the order they were sent, and
    /// how far each got. Empty means nothing has been run over this
    /// buffer — or that the buffer has been edited since, which is the
    /// same thing to the gutter: the ranges are byte offsets into the
    /// text that was sent, and an edit moves the text out from under
    /// them. So **every edit drops them**, rather than paint a tick
    /// beside a line the user has since rewritten.
    statements: Vec<StatementMark>,
    /// What the last syntax check found, if anything. Empty is both
    /// "nothing is wrong" and "nobody has checked since the last edit",
    /// and the editor need not tell them apart: it paints marks, and there
    /// are none either way. **Every edit drops these**, for the reason it
    /// drops `statements` — the ranges name text the edit has moved.
    diagnostics: Vec<Diagnostic>,
    /// Where the caret is in its blink.
    blink: Blink,
}

impl gpui::EventEmitter<SqlEditorEvent> for SqlEditor {}

impl Blinking for SqlEditor {
    fn blink(&self) -> &Blink {
        &self.blink
    }

    fn blink_mut(&mut self) -> &mut Blink {
        &mut self.blink
    }
}

#[derive(Clone)]
struct Snapshot {
    content: String,
    selected_range: Range<usize>,
    selection_reversed: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum EditKind {
    /// Nothing to join onto: the next edit always starts a new undo step.
    None,
    Insert,
    Delete,
}

/// What the last paint produced, kept so mouse hits and IME rectangles can
/// be answered without shaping the text again.
struct EditorLayout {
    lines: Vec<ShapedLine>,
    /// Byte offset where each line starts in the buffer.
    line_starts: Vec<usize>,
    line_height: Pixels,
}

impl SqlEditor {
    pub fn new(
        content: impl Into<String>,
        vocabulary: Arc<Vocabulary>,
        cx: &mut Context<Self>,
    ) -> Self {
        let content = content.into();
        let end = content.len();
        Self {
            focus_handle: cx.focus_handle(),
            scroll_handle: ScrollHandle::new(),
            h_scroll_handle: ScrollHandle::new(),
            scroll_drag: DragState::default(),
            content,
            placeholder: "select * from".into(),
            vocabulary,
            selected_range: end..end,
            selection_reversed: false,
            marked_range: None,
            undo: Vec::new(),
            redo: Vec::new(),
            last_edit: EditKind::None,
            last_layout: None,
            last_bounds: None,
            is_selecting: false,
            pending_autoscroll: false,
            completions: Vec::new(),
            completion_ix: 0,
            completion_range: 0..0,
            completions_dismissed: false,
            statements: Vec::new(),
            diagnostics: Vec::new(),
            blink: Blink::default(),
        }
    }

    pub fn text(&self) -> &str {
        &self.content
    }

    /// The selected text, when there is a selection. The query tab runs
    /// this in place of the whole buffer.
    pub fn selected_text(&self) -> Option<String> {
        let range = clamp_range(&self.content, self.selected_range.clone());
        let selected = self.content[range].trim().to_string();
        (!selected.is_empty()).then_some(selected)
    }

    /// The text a run sends and where it starts in the buffer: the
    /// selection when there is one, the whole buffer otherwise.
    ///
    /// The offset is what the gutter marks are worked out from. ⌘⏎ over a
    /// selection runs the selection alone, and its statements are still
    /// statements of *this* buffer — a mark on the first of them belongs
    /// on the line the selection starts on, not on line one.
    pub fn run_source(&self) -> (String, usize) {
        let range = clamp_range(&self.content, self.selected_range.clone());
        let slice = &self.content[range.clone()];
        let trimmed = slice.trim();
        if trimmed.is_empty() {
            return (self.content.clone(), 0);
        }
        let start = range.start + (slice.len() - slice.trim_start().len());
        (trimmed.to_string(), start)
    }

    /// Take the statements a run is about to send, all of them queued.
    /// `offset` is where the run's text starts in the buffer, so the
    /// ranges are the buffer's own. Each comes with its kind, which the
    /// caller reads off the text: the editor paints and does not parse.
    pub fn set_statements(
        &mut self,
        statements: Vec<(Range<usize>, StatementKind)>,
        offset: usize,
        cx: &mut Context<Self>,
    ) {
        self.statements = statements
            .into_iter()
            .map(|(range, kind)| StatementMark {
                range: offset + range.start..offset + range.end,
                status: StatementStatus::Queued,
                kind,
                meta: "queued".into(),
            })
            .collect();
        cx.notify();
    }

    /// Say how the statement at `ix` ended, and what to write beside it.
    /// A run whose buffer was edited under it has no marks left to write
    /// on, and this says nothing rather than guessing which line the
    /// statement moved to.
    pub fn set_statement_status(
        &mut self,
        ix: usize,
        status: StatementStatus,
        meta: impl Into<SharedString>,
        cx: &mut Context<Self>,
    ) {
        let Some(mark) = self.statements.get_mut(ix) else {
            return;
        };
        mark.status = status;
        mark.meta = meta.into();
        cx.notify();
    }

    /// Rewrite the statistic beside the statement at `ix` and nothing
    /// else. A shape-changing statement learns what it changed one round
    /// trip after it lands, and the line is worth updating for it.
    pub fn set_statement_meta(
        &mut self,
        ix: usize,
        meta: impl Into<SharedString>,
        cx: &mut Context<Self>,
    ) {
        let Some(mark) = self.statements.get_mut(ix) else {
            return;
        };
        mark.meta = meta.into();
        cx.notify();
    }

    /// Mark every statement from `ix` on as never sent, with `meta` saying
    /// why. A run stops at its first failure, and the statements behind it
    /// were not skipped by anyone's choice — they never left.
    pub fn skip_statements_from(
        &mut self,
        ix: usize,
        meta: impl Into<SharedString>,
        cx: &mut Context<Self>,
    ) {
        let meta = meta.into();
        for mark in self.statements.iter_mut().skip(ix) {
            mark.status = StatementStatus::Skipped;
            mark.meta = meta.clone();
        }
        cx.notify();
    }

    /// Take what a syntax check found. The ranges are byte offsets into
    /// **this** buffer; a check whose text has since been edited must be
    /// thrown away by the caller rather than moved, because there is no
    /// way to move it that is not a guess.
    pub fn set_diagnostics(&mut self, diagnostics: Vec<Diagnostic>, cx: &mut Context<Self>) {
        if self.diagnostics == diagnostics {
            return;
        }
        self.diagnostics = diagnostics;
        cx.notify();
    }

    pub fn diagnostics(&self) -> &[Diagnostic] {
        &self.diagnostics
    }

    /// Where the caret is, in bytes into the buffer.
    pub fn cursor(&self) -> usize {
        self.cursor_offset()
    }

    pub fn clear_statements(&mut self, cx: &mut Context<Self>) {
        if self.statements.is_empty() {
            return;
        }
        self.statements.clear();
        cx.notify();
    }

    pub fn line_count(&self) -> usize {
        self.content.split('\n').count()
    }

    /// The column at the right of the pane that carries each statement's
    /// statistic, and the `DDL` chip beside a shape-changing one.
    ///
    /// It is a column of its own, outside the horizontal scroll, for the
    /// reason the gutter is: a long line must slide under it rather than
    /// carry it off the pane, and the number has to sit where the eye
    /// finds it — at the right edge, on the statement's first line — however
    /// far across the text goes. It is inside the vertical scroll, so it
    /// travels with its lines. `None` with nothing run, so a buffer nobody
    /// has sent gives up no room to a column of nothing.
    fn statistics_column(
        &self,
        line_marks: &[Option<LineMark>],
        colors: &ThemeColors,
    ) -> Option<Div> {
        if self.statements.is_empty() {
            return None;
        }
        Some(
            div()
                .flex_none()
                .min_h(relative(1.))
                .py(px(TEXT_PADDING_Y))
                .flex()
                .flex_col()
                .items_end()
                .children(line_marks.iter().map(|on_line| {
                    let row = div()
                        .h(px(LINE_HEIGHT))
                        .w_full()
                        .flex()
                        .items_center()
                        .justify_end()
                        .gap(px(10.))
                        .pl(px(16.))
                        .pr(px(12.));
                    let Some(mark) = on_line else {
                        return row;
                    };
                    // The wash reaches under the statistic too, so the
                    // statement's lines read as one band from the rail to
                    // the pane's edge.
                    let row = match wash_color(mark.kind, mark.status, colors) {
                        Some(color) => row.bg(color),
                        None => row,
                    };
                    if !mark.first {
                        return row;
                    }
                    let meta = self
                        .statements
                        .get(mark.ix)
                        .map(|statement| statement.meta.clone())
                        .filter(|meta| !meta.is_empty());
                    row.when(mark.kind == StatementKind::Ddl, |row| {
                        row.child(ddl_chip(colors))
                    })
                    .children(meta.map(|meta| {
                        div()
                            .flex_none()
                            .whitespace_nowrap()
                            .text_size(px(10.))
                            .text_color(meta_color(mark.kind, mark.status, colors))
                            .child(meta)
                    }))
                })),
        )
    }

    /// The longest line, in characters. The element sizes itself by it, so
    /// the container around it knows there is something to scroll across to.
    ///
    /// Characters, not shaped pixels: the font is monospaced, and measuring
    /// it exactly would mean shaping every line a second time on every
    /// frame, before there is a layout to shape into. The placeholder
    /// counts when the buffer is empty, because that is what is on screen.
    fn widest_line(&self) -> usize {
        let text = if self.content.is_empty() {
            self.placeholder.as_ref()
        } else {
            &self.content
        };
        text.split('\n')
            .map(|line| line.chars().count())
            .max()
            .unwrap_or(0)
    }

    /// Swap in the names of the connected database, so the tokenizer can
    /// tell a real relation from a typo.
    pub fn set_vocabulary(&mut self, vocabulary: Arc<Vocabulary>, cx: &mut Context<Self>) {
        self.vocabulary = vocabulary;
        cx.notify();
    }

    pub fn set_text(&mut self, text: impl Into<String>, cx: &mut Context<Self>) {
        self.push_undo(EditKind::None);
        self.edited(cx);
        self.content = text.into();
        let end = self.content.len();
        self.selected_range = end..end;
        self.selection_reversed = false;
        self.marked_range = None;
        self.touched(cx);
    }

    // --- selection -------------------------------------------------------

    /// The text has moved. Everything worked out from the old text goes
    /// with it — the run's gutter marks and the last check's squiggles
    /// both name byte ranges the edit has just shifted — and whoever owns
    /// the editor is told, so the check can run again.
    fn edited(&mut self, cx: &mut Context<Self>) {
        self.statements.clear();
        self.diagnostics.clear();
        cx.emit(SqlEditorEvent::Changed);
    }

    /// Redraw, and put the caret back on show. Every edit and every
    /// motion goes through here rather than calling `cx.notify()` itself.
    fn touched(&mut self, cx: &mut Context<Self>) {
        self.restart_blink(cx);
        cx.notify();
    }

    fn cursor_offset(&self) -> usize {
        if self.selection_reversed {
            self.selected_range.start
        } else {
            self.selected_range.end
        }
    }

    fn move_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        let offset = clamp_offset(&self.content, offset);
        self.close_completions();
        self.selected_range = offset..offset;
        self.selection_reversed = false;
        // Moving the caret ends the current typing run: undo should stop
        // where the user stopped, not swallow the previous sentence too.
        self.last_edit = EditKind::None;
        self.pending_autoscroll = true;
        self.touched(cx);
    }

    fn select_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        let offset = clamp_offset(&self.content, offset);
        self.close_completions();
        if self.selection_reversed {
            self.selected_range.start = offset;
        } else {
            self.selected_range.end = offset;
        }
        if self.selected_range.end < self.selected_range.start {
            self.selection_reversed = !self.selection_reversed;
            self.selected_range = self.selected_range.end..self.selected_range.start;
        }
        self.last_edit = EditKind::None;
        self.pending_autoscroll = true;
        self.touched(cx);
    }

    /// Delete from the caret to `offset`, in one undo step.
    fn delete_to(&mut self, offset: usize, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            if offset == self.cursor_offset() {
                return;
            }
            self.select_to(offset, cx);
        }
        self.replace_text_in_range(None, "", window, cx);
    }

    // --- undo ------------------------------------------------------------

    /// Record the buffer before an edit, unless this edit continues the
    /// run the last one started.
    fn push_undo(&mut self, kind: EditKind) {
        if kind != EditKind::None && kind == self.last_edit {
            return;
        }
        self.undo.push(Snapshot {
            content: self.content.clone(),
            selected_range: self.selected_range.clone(),
            selection_reversed: self.selection_reversed,
        });
        if self.undo.len() > UNDO_DEPTH {
            self.undo.remove(0);
        }
        self.redo.clear();
    }

    fn restore(&mut self, snapshot: Snapshot) -> Snapshot {
        let current = Snapshot {
            content: std::mem::replace(&mut self.content, snapshot.content),
            selected_range: std::mem::replace(&mut self.selected_range, snapshot.selected_range),
            selection_reversed: std::mem::replace(
                &mut self.selection_reversed,
                snapshot.selection_reversed,
            ),
        };
        self.marked_range = None;
        self.last_edit = EditKind::None;
        self.pending_autoscroll = true;
        current
    }

    fn undo(&mut self, _: &Undo, _: &mut Window, cx: &mut Context<Self>) {
        let Some(snapshot) = self.undo.pop() else {
            return;
        };
        let current = self.restore(snapshot);
        self.redo.push(current);
        // Undo and redo are edits like any other: they move the text the
        // marks were worked out from.
        self.edited(cx);
        self.touched(cx);
    }

    fn redo(&mut self, _: &Redo, _: &mut Window, cx: &mut Context<Self>) {
        let Some(snapshot) = self.redo.pop() else {
            return;
        };
        let current = self.restore(snapshot);
        self.undo.push(current);
        self.edited(cx);
        self.touched(cx);
    }

    // --- offset helpers --------------------------------------------------

    fn line_start(&self, offset: usize) -> usize {
        motion::line_start(&self.content, offset)
    }

    fn line_end(&self, offset: usize) -> usize {
        motion::line_end(&self.content, offset)
    }

    fn previous_boundary(&self, offset: usize) -> usize {
        motion::previous_boundary(&self.content, offset)
    }

    fn next_boundary(&self, offset: usize) -> usize {
        motion::next_boundary(&self.content, offset)
    }

    /// One line up or down, keeping the column as close to the current one
    /// as the target line allows.
    fn vertical_target(&self, down: bool) -> Option<usize> {
        let offset = self.cursor_offset();
        let start = self.line_start(offset);
        let column = self.content[start..offset].chars().count();

        let target_start = if down {
            let end = self.line_end(offset);
            if end == self.content.len() {
                return None;
            }
            end + 1
        } else {
            if start == 0 {
                return None;
            }
            self.line_start(start - 1)
        };

        let target_end = self.line_end(target_start);
        Some(
            self.content[target_start..target_end]
                .char_indices()
                .nth(column)
                .map(|(ix, _)| target_start + ix)
                .unwrap_or(target_end),
        )
    }

    fn offset_from_utf16(&self, target: usize) -> usize {
        let mut utf8 = 0;
        let mut utf16 = 0;
        for ch in self.content.chars() {
            if utf16 >= target {
                break;
            }
            utf16 += ch.len_utf16();
            utf8 += ch.len_utf8();
        }
        utf8
    }

    fn offset_to_utf16(&self, target: usize) -> usize {
        let mut utf8 = 0;
        let mut utf16 = 0;
        for ch in self.content.chars() {
            if utf8 >= target {
                break;
            }
            utf8 += ch.len_utf8();
            utf16 += ch.len_utf16();
        }
        utf16
    }

    fn range_to_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_to_utf16(range.start)..self.offset_to_utf16(range.end)
    }

    fn range_from_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_from_utf16(range.start)..self.offset_from_utf16(range.end)
    }

    fn offset_for_position(&self, position: Point<Pixels>) -> usize {
        let (Some(bounds), Some(layout)) = (self.last_bounds.as_ref(), self.last_layout.as_ref())
        else {
            return self.cursor_offset();
        };
        if layout.lines.is_empty() {
            return 0;
        }
        let row = ((position.y - bounds.top()) / layout.line_height).floor();
        let row = (row.max(0.) as usize).min(layout.lines.len() - 1);
        let line = &layout.lines[row];
        let local = line.closest_index_for_x(position.x - bounds.left());
        layout.line_starts[row] + local
    }

    // --- completions -----------------------------------------------------

    fn close_completions(&mut self) {
        self.completions.clear();
        self.completion_ix = 0;
    }

    /// Recompute the candidates for the caret's position. Called after
    /// every edit; `forced` is ⌃Space, which opens the panel even where
    /// typing alone would not.
    fn refresh_completions(&mut self, forced: bool) {
        self.close_completions();
        if self.vocabulary.is_empty() && !forced {
            return;
        }
        if !self.selected_range.is_empty() || (self.completions_dismissed && !forced) {
            return;
        }

        let caret = self.cursor_offset();
        // Never inside a string or a comment: there is nothing there the
        // catalog can finish.
        let line_start = self.line_start(caret);
        let line = &self.content[line_start..self.line_end(caret)];
        let column = caret - line_start;
        if highlight::spans(line, &self.vocabulary)
            .iter()
            .any(|(range, token)| {
                range.contains(&column.saturating_sub(1))
                    && matches!(token, highlight::Token::Literal | highlight::Token::Comment)
            })
        {
            return;
        }

        let range = completion::prefix_range(&self.content, caret);
        let qualifier = completion::qualifier_range(&self.content, range.start)
            .map(|range| self.content[range].to_string());
        // With nothing typed, only a qualifier justifies opening: `users.`
        // has an obvious answer, a blank line does not.
        if range.is_empty() && qualifier.is_none() && !forced {
            return;
        }

        self.completions = self
            .vocabulary
            .candidates(&self.content[range.clone()], qualifier.as_deref());
        self.completion_range = range;
    }

    fn confirm_completion(
        &mut self,
        _: &ConfirmCompletion,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.apply_completion(self.completion_ix, window, cx);
    }

    fn apply_completion(&mut self, ix: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Some(completion) = self.completions.get(ix) else {
            return;
        };
        let label = completion.label.clone();
        let range = self.completion_range.clone();

        // Accepting is one undo step, never joined to the typing before it.
        self.last_edit = EditKind::None;
        self.selected_range = range.clone();
        self.replace_text_in_range(Some(self.range_to_utf16(&range)), &label, window, cx);
        self.last_edit = EditKind::None;
        // The word is now complete; it is not a prefix waiting for more.
        self.close_completions();
        cx.notify();
    }

    fn next_completion(&mut self, _: &NextCompletion, _: &mut Window, cx: &mut Context<Self>) {
        if self.completions.is_empty() {
            return;
        }
        self.completion_ix = (self.completion_ix + 1) % self.completions.len();
        cx.notify();
    }

    fn previous_completion(
        &mut self,
        _: &PreviousCompletion,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.completions.is_empty() {
            return;
        }
        self.completion_ix =
            (self.completion_ix + self.completions.len() - 1) % self.completions.len();
        cx.notify();
    }

    fn open_completion(&mut self, _: &OpenCompletion, _: &mut Window, cx: &mut Context<Self>) {
        self.completions_dismissed = false;
        self.refresh_completions(true);
        cx.notify();
    }

    fn dismiss_completion(
        &mut self,
        _: &DismissCompletion,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.completions_dismissed = true;
        self.close_completions();
        cx.notify();
    }

    // --- motion actions --------------------------------------------------

    fn move_left(&mut self, _: &MoveLeft, _: &mut Window, cx: &mut Context<Self>) {
        let target = if self.selected_range.is_empty() {
            self.previous_boundary(self.cursor_offset())
        } else {
            self.selected_range.start
        };
        self.move_to(target, cx);
    }

    fn move_right(&mut self, _: &MoveRight, _: &mut Window, cx: &mut Context<Self>) {
        let target = if self.selected_range.is_empty() {
            self.next_boundary(self.cursor_offset())
        } else {
            self.selected_range.end
        };
        self.move_to(target, cx);
    }

    fn move_up(&mut self, _: &MoveUp, _: &mut Window, cx: &mut Context<Self>) {
        let target = self.vertical_target(false).unwrap_or(0);
        self.move_to(target, cx);
    }

    fn move_down(&mut self, _: &MoveDown, _: &mut Window, cx: &mut Context<Self>) {
        let target = self.vertical_target(true).unwrap_or(self.content.len());
        self.move_to(target, cx);
    }

    fn move_to_previous_word_start(
        &mut self,
        _: &MoveToPreviousWordStart,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_next_word_end(
        &mut self,
        _: &MoveToNextWordEnd,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_beginning_of_line(
        &mut self,
        _: &MoveToBeginningOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = self.line_start(self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_end_of_line(&mut self, _: &MoveToEndOfLine, _: &mut Window, cx: &mut Context<Self>) {
        let target = self.line_end(self.cursor_offset());
        self.move_to(target, cx);
    }

    fn move_to_beginning(&mut self, _: &MoveToBeginning, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx);
    }

    fn move_to_end(&mut self, _: &MoveToEnd, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.content.len(), cx);
    }

    // --- selection actions -----------------------------------------------

    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.previous_boundary(self.cursor_offset()), cx);
    }

    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.next_boundary(self.cursor_offset()), cx);
    }

    fn select_up(&mut self, _: &SelectUp, _: &mut Window, cx: &mut Context<Self>) {
        let target = self.vertical_target(false).unwrap_or(0);
        self.select_to(target, cx);
    }

    fn select_down(&mut self, _: &SelectDown, _: &mut Window, cx: &mut Context<Self>) {
        let target = self.vertical_target(true).unwrap_or(self.content.len());
        self.select_to(target, cx);
    }

    fn select_to_previous_word_start(
        &mut self,
        _: &SelectToPreviousWordStart,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_next_word_end(
        &mut self,
        _: &SelectToNextWordEnd,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_beginning_of_line(
        &mut self,
        _: &SelectToBeginningOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = self.line_start(self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_end_of_line(
        &mut self,
        _: &SelectToEndOfLine,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = self.line_end(self.cursor_offset());
        self.select_to(target, cx);
    }

    fn select_to_beginning(
        &mut self,
        _: &SelectToBeginning,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.select_to(0, cx);
    }

    fn select_to_end(&mut self, _: &SelectToEnd, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.content.len(), cx);
    }

    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx);
        self.select_to(self.content.len(), cx);
    }

    // --- editing actions -------------------------------------------------

    fn backspace(&mut self, _: &Backspace, window: &mut Window, cx: &mut Context<Self>) {
        let target = self.previous_boundary(self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete(&mut self, _: &Delete, window: &mut Window, cx: &mut Context<Self>) {
        let target = self.next_boundary(self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_previous_word_start(
        &mut self,
        _: &DeleteToPreviousWordStart,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::previous_word_start(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_next_word_end(
        &mut self,
        _: &DeleteToNextWordEnd,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = motion::next_word_end(&self.content, self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_beginning_of_line(
        &mut self,
        _: &DeleteToBeginningOfLine,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = self.line_start(self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn delete_to_end_of_line(
        &mut self,
        _: &DeleteToEndOfLine,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let target = self.line_end(self.cursor_offset());
        self.delete_to(target, window, cx);
    }

    fn newline(&mut self, _: &Newline, window: &mut Window, cx: &mut Context<Self>) {
        // Carry the current line's indent onto the new one, the way every
        // editor does; a query indented by hand stays indented.
        let start = self.line_start(self.cursor_offset());
        let indent: String = self.content[start..]
            .chars()
            .take_while(|ch| *ch == ' ' || *ch == '\t')
            .collect();
        self.replace_text_in_range(None, &format!("\n{indent}"), window, cx);
    }

    fn indent(&mut self, _: &Indent, window: &mut Window, cx: &mut Context<Self>) {
        self.replace_text_in_range(None, INDENT, window, cx);
    }

    fn paste(&mut self, _: &Paste, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
            // A paste is one undo step, never joined to the typing around it.
            self.last_edit = EditKind::None;
            self.replace_text_in_range(None, &text, window, cx);
            self.last_edit = EditKind::None;
        }
    }

    fn copy(&mut self, _: &Copy, _: &mut Window, cx: &mut Context<Self>) {
        let range = clamp_range(&self.content, self.selected_range.clone());
        if !range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[range].to_string()));
        }
    }

    fn cut(&mut self, _: &Cut, window: &mut Window, cx: &mut Context<Self>) {
        let range = clamp_range(&self.content, self.selected_range.clone());
        if !range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[range].to_string()));
            self.last_edit = EditKind::None;
            self.replace_text_in_range(None, "", window, cx);
        }
    }

    // --- mouse -----------------------------------------------------------

    fn on_mouse_down(&mut self, event: &MouseDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        let offset = self.offset_for_position(event.position);
        match event.click_count {
            1 if event.modifiers.shift => self.select_to(offset, cx),
            1 => {
                self.is_selecting = true;
                self.move_to(offset, cx);
            }
            2 => {
                let word = motion::word_at(&self.content, offset);
                self.move_to(word.start, cx);
                self.select_to(word.end, cx);
            }
            _ => {
                self.move_to(self.line_start(offset), cx);
                self.select_to(self.line_end(offset), cx);
            }
        }
    }

    fn on_mouse_up(&mut self, _: &MouseUpEvent, _: &mut Window, _: &mut Context<Self>) {
        self.is_selecting = false;
    }

    fn on_mouse_move(&mut self, event: &MouseMoveEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.is_selecting {
            self.select_to(self.offset_for_position(event.position), cx);
        }
    }
}

impl EntityInputHandler for SqlEditor {
    fn text_for_range(
        &mut self,
        range_utf16: Range<usize>,
        actual_range: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        let range = clamp_range(&self.content, self.range_from_utf16(&range_utf16));
        actual_range.replace(self.range_to_utf16(&range));
        Some(self.content[range].to_string())
    }

    fn selected_text_range(
        &mut self,
        _ignore_disabled_input: bool,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<UTF16Selection> {
        Some(UTF16Selection {
            range: self.range_to_utf16(&self.selected_range),
            reversed: self.selection_reversed,
        })
    }

    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.marked_range
            .as_ref()
            .map(|range| self.range_to_utf16(range))
    }

    fn unmark_text(&mut self, _: &mut Window, _: &mut Context<Self>) {
        self.marked_range = None;
    }

    fn replace_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let range = clamp_range(
            &self.content,
            range_utf16
                .as_ref()
                .map(|range| self.range_from_utf16(range))
                .or(self.marked_range.clone())
                .unwrap_or(self.selected_range.clone()),
        );

        // A newline breaks the undo run, so undo lands on line boundaries
        // rather than swallowing the whole query. So does replacing a
        // selection: the selection is part of what undo must bring back.
        let kind = if !range.is_empty() || new_text.contains('\n') {
            EditKind::None
        } else if new_text.is_empty() {
            EditKind::Delete
        } else {
            EditKind::Insert
        };
        self.push_undo(kind);
        self.last_edit = kind;
        // The marks name byte ranges in the text that was run, and this
        // moves that text. A tick beside a line the user has rewritten
        // would say the wrong thing about the wrong statement.
        self.edited(cx);

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        let cursor = range.start + new_text.len();
        self.selected_range = cursor..cursor;
        self.selection_reversed = false;
        self.marked_range.take();
        self.pending_autoscroll = true;
        // Any edit re-opens the panel that Escape closed: the user is
        // typing a different word now.
        self.completions_dismissed = false;
        self.refresh_completions(false);
        self.touched(cx);
    }

    fn replace_and_mark_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        new_selected_range_utf16: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let range = clamp_range(
            &self.content,
            range_utf16
                .as_ref()
                .map(|range| self.range_from_utf16(range))
                .or(self.marked_range.clone())
                .unwrap_or(self.selected_range.clone()),
        );

        // Composition rewrites itself as it goes: take one undo step when
        // it starts, none for the keystrokes inside it.
        if self.marked_range.is_none() {
            self.push_undo(EditKind::None);
        }
        self.last_edit = EditKind::None;
        self.edited(cx);

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        self.marked_range =
            (!new_text.is_empty()).then(|| range.start..range.start + new_text.len());
        // The platform reports this selection relative to `new_text`, so
        // convert it inside that string and then shift it into the buffer.
        // Converting against the whole buffer would run the offset past
        // the end and slice mid-character on the next edit.
        self.selected_range = match new_selected_range_utf16.as_ref() {
            Some(selected) => {
                let start = range.start + utf16_to_byte(new_text, selected.start);
                let end = range.start + utf16_to_byte(new_text, selected.end);
                clamp_range(&self.content, start..end)
            }
            None => {
                let cursor = range.start + new_text.len();
                cursor..cursor
            }
        };
        self.pending_autoscroll = true;
        self.touched(cx);
    }

    fn bounds_for_range(
        &mut self,
        range_utf16: Range<usize>,
        bounds: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        let layout = self.last_layout.as_ref()?;
        let range = clamp_range(&self.content, self.range_from_utf16(&range_utf16));
        let row = row_for_offset(&layout.line_starts, range.start);
        let line = layout.lines.get(row)?;
        let line_start = layout.line_starts[row];
        // The IME can ask about a range that runs past this line; keep the
        // rectangle on the line the range starts on.
        let from = range.start.saturating_sub(line_start).min(line.len());
        let to = range.end.saturating_sub(line_start).min(line.len());
        let top = bounds.top() + layout.line_height * row as f32;
        Some(Bounds::from_corners(
            point(bounds.left() + line.x_for_index(from), top),
            point(
                bounds.left() + line.x_for_index(to),
                top + layout.line_height,
            ),
        ))
    }

    fn character_index_for_point(
        &mut self,
        point: Point<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<usize> {
        Some(self.offset_to_utf16(self.offset_for_position(point)))
    }
}

impl Focusable for SqlEditor {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for SqlEditor {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();
        // Focus belongs to the window, and this is the one place that has
        // one, so the blink is started and stopped from the edge here
        // rather than from a focus listener the constructor cannot install.
        self.track_blink_focus(self.focus_handle.is_focused(window), cx);
        let line_count = self.line_count();
        let line_marks = line_marks(&self.content, &self.statements);
        // How wide the text is, and so how far there is to scroll. The
        // element inside fills this box; the box is what the scroll
        // container measures.
        let text_width = TEXT_PADDING_X * 2. + self.widest_line() as f32 * CHAR_WIDTH;

        // The `completing` entry is what lets the completion bindings
        // outrank the caret bindings, and only while the panel is open.
        let mut key_context = KeyContext::new_with_defaults();
        key_context.add(KEY_CONTEXT);
        if !self.completions.is_empty() {
            key_context.add("completing");
        }

        div()
            .id("sql-editor")
            .key_context(key_context)
            .track_focus(&self.focus_handle(cx))
            .cursor(CursorStyle::IBeam)
            .on_action(cx.listener(Self::backspace))
            .on_action(cx.listener(Self::delete))
            .on_action(cx.listener(Self::delete_to_previous_word_start))
            .on_action(cx.listener(Self::delete_to_next_word_end))
            .on_action(cx.listener(Self::delete_to_beginning_of_line))
            .on_action(cx.listener(Self::delete_to_end_of_line))
            .on_action(cx.listener(Self::move_left))
            .on_action(cx.listener(Self::move_right))
            .on_action(cx.listener(Self::move_up))
            .on_action(cx.listener(Self::move_down))
            .on_action(cx.listener(Self::move_to_previous_word_start))
            .on_action(cx.listener(Self::move_to_next_word_end))
            .on_action(cx.listener(Self::move_to_beginning_of_line))
            .on_action(cx.listener(Self::move_to_end_of_line))
            .on_action(cx.listener(Self::move_to_beginning))
            .on_action(cx.listener(Self::move_to_end))
            .on_action(cx.listener(Self::select_left))
            .on_action(cx.listener(Self::select_right))
            .on_action(cx.listener(Self::select_up))
            .on_action(cx.listener(Self::select_down))
            .on_action(cx.listener(Self::select_to_previous_word_start))
            .on_action(cx.listener(Self::select_to_next_word_end))
            .on_action(cx.listener(Self::select_to_beginning_of_line))
            .on_action(cx.listener(Self::select_to_end_of_line))
            .on_action(cx.listener(Self::select_to_beginning))
            .on_action(cx.listener(Self::select_to_end))
            .on_action(cx.listener(Self::select_all))
            .on_action(cx.listener(Self::newline))
            .on_action(cx.listener(Self::indent))
            .on_action(cx.listener(Self::undo))
            .on_action(cx.listener(Self::redo))
            .on_action(cx.listener(Self::paste))
            .on_action(cx.listener(Self::copy))
            .on_action(cx.listener(Self::cut))
            .on_action(cx.listener(Self::open_completion))
            .on_action(cx.listener(Self::confirm_completion))
            .on_action(cx.listener(Self::next_completion))
            .on_action(cx.listener(Self::previous_completion))
            .on_action(cx.listener(Self::dismiss_completion))
            .on_mouse_down(MouseButton::Left, cx.listener(Self::on_mouse_down))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_move(cx.listener(Self::on_mouse_move))
            .size_full()
            .relative()
            .flex()
            .text_size(px(FONT_SIZE))
            .line_height(px(LINE_HEIGHT))
            .child(
                // Two scroll containers, nested, and the nesting is the
                // point: this one scrolls **down**, and the gutter is inside
                // it, so the line numbers travel with their lines. The one
                // within it scrolls **across** and holds the text alone, so
                // a long line slides under the numbers instead of pushing
                // them off the pane. One container over both would take the
                // gutter with it.
                //
                // GPUI applies a wheel gesture to every scroll container
                // under the pointer, each on the axis it has, so the pair
                // reads as one surface: vertical to the outer, horizontal to
                // the inner.
                div()
                    .id("sql-editor-text")
                    .track_scroll(&self.scroll_handle)
                    .flex_1()
                    .min_w(px(0.))
                    .h_full()
                    .overflow_y_scroll()
                    // Each container is locked to the axis it has, as the
                    // results grid's pair are. Without it GPUI hands a
                    // container that scrolls on one axis the delta from the
                    // *other* when its own is zero — so a vertical gesture
                    // would drag the text sideways as it went down. It also
                    // holds a gesture to the axis it started on, which is
                    // what keeps a trackpad swipe from wandering.
                    .restrict_scroll_to_axis()
                    .flex()
                    // `items_start`, and it is load-bearing: a flex row
                    // stretches its children to the line's height by
                    // default, so a buffer of forty lines would still lay
                    // out as boxes the height of the pane — content the same
                    // size as the viewport is content with nothing to
                    // scroll, and no bar. It is the vertical twin of the
                    // definite width below.
                    .items_start()
                    .child(
                        // Line-number gutter: a statement's mark on the
                        // left of it, the number right-aligned against the
                        // rule.
                        div()
                            .w(px(GUTTER_WIDTH))
                            .flex_none()
                            // ...and with the stretch gone, both columns
                            // keep the pane's height as a floor, so their
                            // backgrounds and the rule between them still
                            // reach the bottom of a short buffer.
                            .min_h(relative(1.))
                            .py(px(TEXT_PADDING_Y))
                            .pr(px(8.))
                            .bg(colors.panel)
                            .border_r_1()
                            .border_color(colors.border)
                            .text_color(colors.line_number)
                            .flex()
                            .flex_col()
                            .children((1..=line_count).map(|n| {
                                let on_line = line_marks.get(n - 1).copied().flatten();
                                let mark = on_line
                                    .filter(|mark| mark.first)
                                    .map(|mark| statement_mark(n, mark.status, &colors));
                                // The gutter beside a shape-changing
                                // statement takes the family's own tint,
                                // as the comp's does, so the teal reads
                                // from the numbers to the far edge.
                                let ddl =
                                    on_line.is_some_and(|mark| mark.kind == StatementKind::Ddl);
                                div()
                                    .h(px(LINE_HEIGHT))
                                    .flex()
                                    .items_center()
                                    // The gutter's own padding is on the
                                    // column, so the tint has to reach
                                    // past this row's box to meet the
                                    // rule: `-mr` pulls it there.
                                    .when(ddl, |row| {
                                        row.bg(colors.ddl_gutter)
                                            .text_color(colors.ddl_number)
                                            .mr(px(-8.))
                                            .pr(px(8.))
                                    })
                                    .child(
                                        div()
                                            .w(px(MARK_WIDTH))
                                            .flex_none()
                                            .flex()
                                            .justify_center()
                                            .children(mark),
                                    )
                                    .child(div().flex_1())
                                    .child(div().flex_none().child(n.to_string()))
                            })),
                    )
                    .child(
                        // The rail. It is a column of its own rather than a
                        // border on the gutter, because it says something
                        // per line: which lines one statement covers, and
                        // how that statement ended. It carries the whole of
                        // a multi-line statement, which the mark on its
                        // first line cannot.
                        div()
                            .w(px(RAIL_WIDTH))
                            .flex_none()
                            .min_h(relative(1.))
                            .py(px(TEXT_PADDING_Y))
                            .flex()
                            .flex_col()
                            .children((0..line_count).map(|row| {
                                let rail = line_marks
                                    .get(row)
                                    .copied()
                                    .flatten()
                                    .map(|mark| rail_color(mark.kind, mark.status, &colors));
                                let line = div().h(px(LINE_HEIGHT));
                                match rail {
                                    Some(color) => line.bg(color),
                                    None => line,
                                }
                            })),
                    )
                    .child(
                        div()
                            .id("sql-editor-line")
                            .track_scroll(&self.h_scroll_handle)
                            .flex_1()
                            .min_w(px(0.))
                            .overflow_x_scroll()
                            .restrict_scroll_to_axis()
                            .child(
                                // The width is **definite**, and that is the
                                // whole trick: a scroll container measures
                                // the box its children ask for, and an
                                // auto-width child inside one is measured
                                // as the container itself — content the same
                                // size as the viewport is content with
                                // nothing to scroll, and so no bar. The
                                // results grid sizes its own content the
                                // same way. `min_w_full` keeps a short
                                // buffer pane-wide, so a click to the right
                                // of a line still lands in the editor.
                                div()
                                    .flex_none()
                                    .w(px(text_width))
                                    .min_w_full()
                                    .min_h(relative(1.))
                                    .px(px(TEXT_PADDING_X))
                                    .py(px(TEXT_PADDING_Y))
                                    .child(EditorElement {
                                        editor: cx.entity(),
                                    }),
                            ),
                    )
                    .children(self.statistics_column(&line_marks, &colors)),
            )
            // The bars are painted outside the containers they drive, or
            // they would scroll away with the text. Both appear only when
            // the content does not fit, which `Scrollbar::new` answers.
            .children(
                Scrollbar::new(
                    true,
                    self.scroll_handle.clone(),
                    self.scroll_drag.clone(),
                    colors.text_faint,
                    colors.text_muted,
                )
                .map(|bar| {
                    div()
                        .absolute()
                        .top(px(0.))
                        .right(px(0.))
                        .bottom(px(0.))
                        .w(px(scrollbar::THICKNESS))
                        .child(bar)
                }),
            )
            .children(
                Scrollbar::new(
                    false,
                    self.h_scroll_handle.clone(),
                    self.scroll_drag.clone(),
                    colors.text_faint,
                    colors.text_muted,
                )
                .map(|bar| {
                    div()
                        .absolute()
                        // Start past the gutter: the bar drives the text,
                        // and the gutter does not move sideways.
                        .left(px(GUTTER_WIDTH))
                        .right(px(0.))
                        .bottom(px(0.))
                        .h(px(scrollbar::THICKNESS))
                        .child(bar)
                }),
            )
    }
}

impl SqlEditor {
    /// The completion popup, anchored under the caret by the element that
    /// paints the text. It floats over whatever is below the editor, so
    /// it never resizes the query pane.
    fn completions_menu(&self, cx: &mut Context<Self>) -> Div {
        let colors = theme(cx).colors.clone();

        div()
            .flex()
            .flex_col()
            .min_w(px(MENU_MIN_WIDTH))
            .max_w(px(MENU_MAX_WIDTH))
            .py(px(4.))
            .bg(colors.elevated)
            .border_1()
            .border_color(colors.border_strong)
            .rounded(px(8.))
            .shadow_md()
            .text_size(px(12.))
            .children(self.completions.iter().enumerate().map(|(ix, completion)| {
                let selected = ix == self.completion_ix;
                let spans = matched_spans(&completion.label, &completion.matched);
                let last = spans.len().saturating_sub(1);
                let row = div()
                    .id(ElementId::Name(format!("completion-{ix}").into()))
                    .mx(px(4.))
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(px(12.))
                    .px(px(8.))
                    .py(px(4.))
                    .rounded(px(5.))
                    .cursor_pointer()
                    .on_click(cx.listener(move |this, _event, window, cx| {
                        this.apply_completion(ix, window, cx)
                    }))
                    .child(div().min_w(px(0.)).flex().overflow_hidden().children(
                        spans.into_iter().enumerate().map(|(at, (text, hit))| {
                            let span = if at == last {
                                // Only the last span may give way: a
                                // long name has to end in an ellipsis
                                // rather than run out of the panel.
                                div().truncate()
                            } else {
                                div().flex_none()
                            };
                            if hit {
                                span.font_weight(FontWeight::MEDIUM)
                                    .text_color(colors.accent_deep)
                                    .child(text)
                            } else {
                                span.text_color(colors.text).child(text)
                            }
                        }),
                    ))
                    .child(
                        div()
                            .flex_none()
                            .text_size(px(11.))
                            .text_color(colors.text_faint)
                            .child(completion.detail.clone()),
                    );
                if selected {
                    row.bg(colors.selection)
                } else {
                    let hover = colors.hairline;
                    row.hover(move |s| s.bg(hover))
                }
            }))
            .child(
                div()
                    .mt(px(4.))
                    .pt(px(5.))
                    .px(px(12.))
                    .pb(px(2.))
                    .border_t_1()
                    .border_color(colors.hairline)
                    .text_size(px(10.))
                    .text_color(colors.text_faint)
                    .child("↩ or ⇥ to insert · esc to dismiss"),
            )
    }
}

/// What one line of the buffer wears: which statement of the last run
/// covers it, and whether it is the line that statement starts on — the
/// mark and the statistic are drawn once, and the rail and the wash carry
/// the rest of it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct LineMark {
    /// Index of the statement in the run, so the statistic can be read
    /// back off the mark.
    ix: usize,
    status: StatementStatus,
    kind: StatementKind,
    first: bool,
}

/// What each line of `content` wears in the gutter.
///
/// A plain function over the text and the ranges, so the mapping can be
/// argued with in a test rather than in a running window.
fn line_marks(content: &str, statements: &[StatementMark]) -> Vec<Option<LineMark>> {
    let mut marks = vec![None; content.split('\n').count()];
    if statements.is_empty() {
        return marks;
    }
    let mut starts = vec![0usize];
    starts.extend(content.match_indices('\n').map(|(ix, _)| ix + 1));
    for (ix, statement) in statements.iter().enumerate() {
        let first = row_for_offset(&starts, statement.range.start);
        // The end offset is one past the statement's last character, so a
        // statement that ends at a line break belongs to the line before
        // it rather than opening the next one.
        let last = row_for_offset(&starts, statement.range.end.saturating_sub(1)).max(first);
        for row in first..=last.min(marks.len().saturating_sub(1)) {
            marks[row] = Some(LineMark {
                ix,
                status: statement.status,
                kind: statement.kind,
                first: row == first,
            });
        }
    }
    marks
}

/// The wash behind a statement's lines, if it wears one.
///
/// A plain statement is washed only while it is news — running, or
/// failed — and a shape-changing one is washed in its own family from the
/// moment it is queued, because it is news whatever state it is in. A
/// failure takes the error's surface in either kind: a `DROP` the server
/// refused changed nothing, and teal would say it did.
fn wash_color(kind: StatementKind, status: StatementStatus, colors: &ThemeColors) -> Option<Hsla> {
    match (kind, status) {
        (_, StatementStatus::Failed) => Some(colors.error_surface),
        (StatementKind::Ddl, _) => Some(colors.ddl_surface),
        (StatementKind::Plain, StatementStatus::Running) => Some(colors.running_surface),
        (StatementKind::Plain, _) => None,
    }
}

/// The colour of the statistic beside a statement, by how it ended. The
/// shape-changing family keeps its own ink for a statement that landed;
/// the rest is shared, because "failed" is failed whatever the statement.
fn meta_color(kind: StatementKind, status: StatementStatus, colors: &ThemeColors) -> Hsla {
    match (kind, status) {
        (_, StatementStatus::Queued) | (_, StatementStatus::Skipped) => colors.text_faint,
        (_, StatementStatus::Failed) => colors.env_prod,
        (StatementKind::Ddl, _) => colors.ddl_text,
        (StatementKind::Plain, StatementStatus::Running) => colors.accent,
        (StatementKind::Plain, StatementStatus::Done) => colors.ok_muted,
    }
}

/// The mark a statement wears in the gutter, on the line its text starts
/// on. Five states, and they have to be told apart at 8 pixels: **the
/// shape says which**, not the colour alone — a ring, a dot, a tick, a
/// filled square and a dash, in the same warm family as everything else on
/// this screen.
fn statement_mark(line: usize, status: StatementStatus, colors: &ThemeColors) -> AnyElement {
    match status {
        // A ring: the outline of the dot it is about to become.
        StatementStatus::Queued => div()
            .size(px(MARK_SIZE))
            .rounded_full()
            .border_2()
            .border_color(colors.running_border)
            .into_any_element(),
        StatementStatus::Running => div()
            .size(px(MARK_SIZE))
            .rounded_full()
            .bg(colors.running_mark)
            // Phase-locked to the app's clock, as the run button's breath
            // is, so the dot does not start over every time a keystroke
            // rebuilds the editor. It holds still under `reduce_motion`,
            // which `with_animation` answers for us.
            .with_animation(
                ("statement-running", line),
                Animation::new(MARK_BREATH)
                    .repeat_synced()
                    .with_easing(gpui::pulsating_between(0., 1.)),
                |dot, delta| dot.opacity(1. - MARK_BREATH_DEPTH * delta),
            )
            .into_any_element(),
        StatementStatus::Done => div()
            .text_size(px(14.))
            .font_weight(FontWeight::BOLD)
            .text_color(colors.ok)
            .child("✓")
            .into_any_element(),
        // The one mark that is filled and clay: a failure is the only
        // state here that stops the run, and it is read at a glance from
        // the other side of the pane.
        StatementStatus::Failed => div()
            .size(px(MARK_SIZE + 2.))
            .rounded(px(3.))
            .bg(colors.env_prod)
            .flex()
            .items_center()
            .justify_center()
            .text_size(px(10.))
            .font_weight(FontWeight::BOLD)
            .text_color(colors.window)
            .child("!")
            .into_any_element(),
        // A dash: nothing happened here, and nothing is what it draws.
        StatementStatus::Skipped => div()
            .w(px(MARK_SIZE))
            .h(px(2.))
            .bg(colors.idle)
            .into_any_element(),
    }
}

/// The rail's colour beside a statement's lines. Queued and skipped take
/// the plain rule the buffer wears everywhere else: neither is news. A
/// shape-changing statement's rail is teal in every state but failure,
/// deepening as the statement goes from queued to running to landed.
fn rail_color(kind: StatementKind, status: StatementStatus, colors: &ThemeColors) -> Hsla {
    match (kind, status) {
        (_, StatementStatus::Failed) => colors.error_mark,
        (StatementKind::Ddl, StatementStatus::Queued | StatementStatus::Skipped) => {
            colors.ddl_inner
        }
        (StatementKind::Ddl, StatementStatus::Running) => colors.ddl,
        (StatementKind::Ddl, StatementStatus::Done) => colors.ddl_done,
        (StatementKind::Plain, StatementStatus::Queued | StatementStatus::Skipped) => colors.border,
        (StatementKind::Plain, StatementStatus::Running) => colors.running_mark,
        // The dev family's green, which the read-only mark and a commit
        // already wear: this app says "that worked" in one colour.
        (StatementKind::Plain, StatementStatus::Done) => colors.env_dev_inner,
    }
}

/// The `DDL` chip on a shape-changing statement's first line: the comp's
/// 8px cap in the family's ring colour, so the kind is named and not
/// only coloured.
fn ddl_chip(colors: &ThemeColors) -> Div {
    div()
        .flex_none()
        .px(px(6.))
        .py(px(3.))
        .rounded(px(4.))
        .bg(colors.ddl)
        .text_size(px(8.))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(colors.ddl_surface)
        .child("DDL")
}

/// A label cut into the pieces the panel paints: each piece with whether
/// the typed word matched it. The ranges come from the matcher, in order and
/// never overlapping, so this is one walk with no sorting.
fn matched_spans(label: &str, matched: &[Range<usize>]) -> Vec<(String, bool)> {
    let mut spans = Vec::with_capacity(matched.len() * 2 + 1);
    let mut at = 0;
    for hit in matched {
        if hit.start < at || hit.end > label.len() {
            continue;
        }
        if hit.start > at {
            spans.push((label[at..hit.start].to_string(), false));
        }
        spans.push((label[hit.clone()].to_string(), true));
        at = hit.end;
    }
    if at < label.len() {
        spans.push((label[at..].to_string(), false));
    }
    spans
}

/// Byte offset for a UTF-16 offset *within* `text`, clamped to its end.
/// The platform reports a composition's selection relative to the text it
/// just handed us, not to the whole buffer.
fn utf16_to_byte(text: &str, utf16_offset: usize) -> usize {
    let mut utf8 = 0;
    let mut utf16 = 0;
    for ch in text.chars() {
        if utf16 >= utf16_offset {
            return utf8;
        }
        utf16 += ch.len_utf16();
        utf8 += ch.len_utf8();
    }
    text.len()
}

/// Force an offset inside `text` and onto a character boundary.
fn clamp_offset(text: &str, offset: usize) -> usize {
    let mut offset = offset.min(text.len());
    while offset > 0 && !text.is_char_boundary(offset) {
        offset -= 1;
    }
    offset
}

/// Force a range inside `text`, onto character boundaries, and in order.
///
/// The platform hands us ranges it worked out from its own copy of the
/// buffer, which can lag ours by an edit. Slicing on a stale range panics,
/// and a panic inside an input-handler callback crosses an `extern "C"`
/// boundary, where Rust aborts the process instead of unwinding. So every
/// range that reaches a slice goes through here first.
fn clamp_range(text: &str, range: Range<usize>) -> Range<usize> {
    let start = clamp_offset(text, range.start);
    let end = clamp_offset(text, range.end).max(start);
    start..end
}

fn row_for_offset(line_starts: &[usize], offset: usize) -> usize {
    match line_starts.binary_search(&offset) {
        Ok(row) => row,
        Err(row) => row.saturating_sub(1),
    }
}

/// Paints the buffer: one shaped line per row, plus the selection and the
/// caret.
struct EditorElement {
    editor: Entity<SqlEditor>,
}

struct PrepaintState {
    layout: Option<EditorLayout>,
    quads: Vec<PaintQuad>,
    cursor: Option<PaintQuad>,
    /// Top of the caret's line in window coordinates, so paint can pull
    /// the viewport back over it.
    cursor_top: Option<Pixels>,
    /// The caret's left edge, likewise: a line long enough to scroll is a
    /// line the caret can walk off the right of.
    cursor_left: Option<Pixels>,
}

impl IntoElement for EditorElement {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for EditorElement {
    type RequestLayoutState = ();
    type PrepaintState = PrepaintState;

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, Self::RequestLayoutState) {
        let line_count = self.editor.read(cx).line_count();
        let mut style = Style::default();
        // The element fills the box `render` sized for the longest line —
        // that box is what the scroll container measures, so the width does
        // not have to be worked out twice.
        style.size.width = relative(1.).into();
        style.size.height = (window.line_height() * line_count as f32).into();
        (window.request_layout(style, [], cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        window: &mut Window,
        cx: &mut App,
    ) -> Self::PrepaintState {
        let colors = theme(cx).colors.clone();
        let editor = self.editor.read(cx);
        let style = window.text_style();
        let line_height = window.line_height();
        let font_size = style.font_size.to_pixels(window.rem_size());

        let placeholder = editor.content.is_empty();
        let text = if placeholder {
            editor.placeholder.to_string()
        } else {
            editor.content.clone()
        };
        let text_color = if placeholder {
            colors.text_faint
        } else {
            style.color
        };

        let mut lines = Vec::new();
        let mut line_starts = Vec::new();
        let mut offset = 0;
        for line in text.split('\n') {
            line_starts.push(offset);
            let underline = underline_for(editor, offset, line.len());
            let runs = if placeholder {
                single_run(line, style.font(), text_color, underline)
            } else {
                let marks = marks_on_line(&editor.diagnostics, offset, line.len());
                let squiggle = UnderlineStyle {
                    color: Some(colors.error),
                    thickness: px(1.),
                    wavy: true,
                };
                split_spans(highlight::spans(line, &editor.vocabulary), &marks)
                    .into_iter()
                    .map(|(range, token, marked)| TextRun {
                        len: range.len(),
                        font: style.font(),
                        color: token_color(token, text_color, &colors),
                        background_color: None,
                        // A squiggle under an IME composition would be two
                        // underlines in one place; the composition is what
                        // the user is doing now, so it wins.
                        underline: underline.or(marked.then_some(squiggle)),
                        strikethrough: None,
                    })
                    .collect()
            };
            lines.push(window.text_system().shape_line(
                SharedString::from(line.to_string()),
                font_size,
                &runs,
                None,
            ));
            offset += line.len() + 1;
        }

        let layout = EditorLayout {
            lines,
            line_starts,
            line_height,
        };

        let (quads, cursor, cursor_top, cursor_left) = if placeholder {
            (Vec::new(), None, None, None)
        } else {
            let offset = editor.cursor_offset();
            let row = row_for_offset(&layout.line_starts, offset);
            let column = layout
                .lines
                .get(row)
                .map(|line| line.x_for_index(offset - layout.line_starts[row]))
                .unwrap_or_default();
            let mut quads = wash_quads(editor, &layout, bounds, &colors);
            quads.extend(selection_quads(editor, &layout, bounds, colors.selection));
            (
                quads,
                cursor_quad(editor, &layout, bounds, colors.accent),
                Some(bounds.top() + line_height * row as f32),
                Some(bounds.left() + column),
            )
        };

        if !editor.completions.is_empty() {
            self.draw_completions_menu(&layout, bounds, window, cx);
        }

        PrepaintState {
            layout: Some(layout),
            quads,
            cursor,
            cursor_top,
            cursor_left,
        }
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        prepaint: &mut Self::PrepaintState,
        window: &mut Window,
        cx: &mut App,
    ) {
        let focus_handle = self.editor.read(cx).focus_handle.clone();
        window.handle_input(
            &focus_handle,
            ElementInputHandler::new(bounds, self.editor.clone()),
            cx,
        );

        for quad in prepaint.quads.drain(..) {
            window.paint_quad(quad);
        }

        let layout = prepaint
            .layout
            .take()
            .expect("prepaint always builds a layout");
        for (row, line) in layout.lines.iter().enumerate() {
            let origin = point(
                bounds.left(),
                bounds.top() + layout.line_height * row as f32,
            );
            line.paint(
                origin,
                layout.line_height,
                gpui::TextAlign::Left,
                None,
                window,
                cx,
            )
            .ok();
        }

        if focus_handle.is_focused(window)
            && let Some(cursor) = prepaint.cursor.take()
        {
            window.paint_quad(cursor);
        }

        let line_height = layout.line_height;
        let following = self.editor.read(cx).pending_autoscroll;
        let follow = prepaint.cursor_top.filter(|_| following);
        let follow_x = prepaint.cursor_left.filter(|_| following);

        self.editor.update(cx, |editor, _| {
            editor.last_layout = Some(layout);
            editor.last_bounds = Some(bounds);
        });

        if follow.is_some() || follow_x.is_some() {
            self.follow_cursor(follow, follow_x, line_height, window, cx);
        }
    }
}

impl EditorElement {
    /// Draw the completion popup under the caret. It is deferred so it
    /// paints above everything else in the window instead of being
    /// clipped by the editor's own pane, and it is built here rather than
    /// in `render` because only the painted layout knows where the caret
    /// actually landed this frame.
    fn draw_completions_menu(
        &self,
        layout: &EditorLayout,
        bounds: Bounds<Pixels>,
        window: &mut Window,
        cx: &mut App,
    ) {
        let (offset, row) = {
            let editor = self.editor.read(cx);
            let offset = editor.cursor_offset();
            (offset, row_for_offset(&layout.line_starts, offset))
        };
        let Some(line) = layout.lines.get(row) else {
            return;
        };
        // Anchor to the start of the word, so the popup lines up with what
        // it is completing rather than drifting right as the user types.
        let anchor = {
            let editor = self.editor.read(cx);
            editor.completion_range.start.min(offset)
        };
        let x = line.x_for_index(
            anchor
                .saturating_sub(layout.line_starts[row])
                .min(line.len()),
        );

        let mut menu = self.editor.update(cx, |editor, cx| {
            editor.completions_menu(cx).into_any_element()
        });
        let size = menu.layout_as_root(gpui::AvailableSpace::min_size(), window, cx);

        let line_top = bounds.top() + layout.line_height * row as f32;
        let below = line_top + layout.line_height + px(MENU_GAP);
        let viewport = window.viewport_size();
        // Flip above the caret when there is no room below, the way every
        // editor does near the bottom of the screen.
        let y = if below + size.height > viewport.height && line_top - size.height > px(0.) {
            line_top - size.height - px(MENU_GAP)
        } else {
            below
        };
        let x = (bounds.left() + x)
            .min(viewport.width - size.width)
            .max(px(0.));

        window.defer_draw(menu, point(x, y), 1, None);
    }

    /// Pull the viewport back over the caret after it moved out of sight,
    /// **down and across**: the caret walks off the right of a long line as
    /// readily as off the bottom of a long buffer.
    ///
    /// A new offset only takes effect on the next frame, so ask for one —
    /// and only when something actually moved, or the editor would refresh
    /// itself for ever.
    fn follow_cursor(
        &self,
        cursor_top: Option<Pixels>,
        cursor_left: Option<Pixels>,
        line_height: Pixels,
        window: &mut Window,
        cx: &mut App,
    ) {
        let (rows, columns) = {
            let editor = self.editor.read(cx);
            (editor.scroll_handle.clone(), editor.h_scroll_handle.clone())
        };
        let mut moved = false;

        let viewport = rows.bounds();
        if let Some(cursor_top) = cursor_top
            && viewport.size.height > px(0.)
        {
            // Keep the editor's padding visible above and below the caret,
            // so it never sits flush against the edge of the pane.
            let above = cursor_top - px(TEXT_PADDING_Y);
            let below = cursor_top + line_height + px(TEXT_PADDING_Y);
            let mut offset = rows.offset();
            if above < viewport.top() {
                offset.y += viewport.top() - above;
            } else if below > viewport.bottom() {
                offset.y -= below - viewport.bottom();
            }
            offset.y = offset.y.min(px(0.));
            if offset.y != rows.offset().y {
                rows.set_offset(offset);
                moved = true;
            }
        }

        let viewport = columns.bounds();
        if let Some(cursor_left) = cursor_left
            && viewport.size.width > px(0.)
        {
            // A caret sitting exactly on the right edge is a caret the user
            // cannot see, so it keeps the text padding beside it as well —
            // and one column of slack, so typing at the end of a long line
            // slides the view along rather than following a character late.
            let left = cursor_left - px(TEXT_PADDING_X);
            let right = cursor_left + px(TEXT_PADDING_X);
            let mut offset = columns.offset();
            if left < viewport.left() {
                offset.x += viewport.left() - left;
            } else if right > viewport.right() {
                offset.x -= right - viewport.right();
            }
            offset.x = offset.x.min(px(0.));
            if offset.x != columns.offset().x {
                columns.set_offset(offset);
                moved = true;
            }
        }

        self.editor
            .update(cx, |editor, _| editor.pending_autoscroll = false);
        if moved {
            window.refresh();
        }
    }
}

fn single_run(
    line: &str,
    font: gpui::Font,
    color: Hsla,
    underline: Option<UnderlineStyle>,
) -> Vec<TextRun> {
    if line.is_empty() {
        return Vec::new();
    }
    vec![TextRun {
        len: line.len(),
        font,
        color,
        background_color: None,
        underline,
        strikethrough: None,
    }]
}

fn token_color(token: Token, plain: Hsla, colors: &theme::ThemeColors) -> Hsla {
    match token {
        Token::Keyword => colors.accent_deep,
        Token::Literal => colors.syntax_literal,
        Token::Comment => colors.text_muted,
        // A name the connected database actually has. A typo stays plain,
        // which is the point: the colour doubles as a spell check.
        Token::Identifier => colors.syntax_identifier,
        Token::Plain => plain,
    }
}

/// Underline the IME's marked text, if it falls on this line.
/// The error marks that fall on one line, as byte ranges into the line.
///
/// A statement is often several lines long, so one diagnostic can reach
/// two of them; the shaped line is what carries the squiggle, and it knows
/// only its own bytes.
fn marks_on_line(
    diagnostics: &[Diagnostic],
    line_start: usize,
    line_len: usize,
) -> Vec<Range<usize>> {
    let line_end = line_start + line_len;
    diagnostics
        .iter()
        .filter(|diagnostic| diagnostic.range.start < line_end && diagnostic.range.end > line_start)
        .map(|diagnostic| {
            diagnostic.range.start.clamp(line_start, line_end) - line_start
                ..diagnostic.range.end.clamp(line_start, line_end) - line_start
        })
        .collect()
}

/// Cut the coloured spans wherever a mark starts or ends, and say of each
/// piece whether it is inside one.
///
/// A `TextRun` carries one underline for its whole length, and a mark
/// rarely lines up with a colour: `frm` is one plain span, and the mark on
/// `select 1 frm` covers the last third of it. So the spans are split at
/// the mark's own edges and every piece keeps the colour it had.
fn split_spans(
    spans: Vec<(Range<usize>, Token)>,
    marks: &[Range<usize>],
) -> Vec<(Range<usize>, Token, bool)> {
    if marks.is_empty() {
        return spans
            .into_iter()
            .map(|(range, token)| (range, token, false))
            .collect();
    }
    let mut pieces = Vec::new();
    for (range, token) in spans {
        let mut cuts: Vec<usize> = marks
            .iter()
            .flat_map(|mark| [mark.start, mark.end])
            .filter(|cut| range.start < *cut && *cut < range.end)
            .collect();
        cuts.sort_unstable();
        cuts.dedup();
        let mut start = range.start;
        for cut in cuts.into_iter().chain([range.end]) {
            let marked = marks
                .iter()
                .any(|mark| mark.start <= start && start < mark.end);
            pieces.push((start..cut, token, marked));
            start = cut;
        }
    }
    pieces
}

fn underline_for(editor: &SqlEditor, line_start: usize, line_len: usize) -> Option<UnderlineStyle> {
    let marked = editor.marked_range.as_ref()?;
    (marked.start < line_start + line_len && marked.end > line_start).then(|| UnderlineStyle {
        color: None,
        thickness: px(1.),
        wavy: false,
    })
}

/// The wash behind every line of a statement that wears one, the full
/// width of the text — so a `DROP` three lines long is one teal band and
/// not three teal words. Painted under the selection, which stays legible
/// over it because the two are far apart in tone.
fn wash_quads(
    editor: &SqlEditor,
    layout: &EditorLayout,
    bounds: Bounds<Pixels>,
    colors: &ThemeColors,
) -> Vec<PaintQuad> {
    if editor.statements.is_empty() {
        return Vec::new();
    }
    line_marks(&editor.content, &editor.statements)
        .into_iter()
        .enumerate()
        .filter_map(|(row, mark)| {
            let mark = mark?;
            let color = wash_color(mark.kind, mark.status, colors)?;
            let top = bounds.top() + layout.line_height * row as f32;
            // Past the text padding on either side, so the band meets the
            // rail on the left and the statistic on the right.
            Some(fill(
                Bounds::from_corners(
                    point(bounds.left() - px(TEXT_PADDING_X), top),
                    point(
                        bounds.right() + px(TEXT_PADDING_X),
                        top + layout.line_height,
                    ),
                ),
                color,
            ))
        })
        .collect()
}

fn selection_quads(
    editor: &SqlEditor,
    layout: &EditorLayout,
    bounds: Bounds<Pixels>,
    color: Hsla,
) -> Vec<PaintQuad> {
    if editor.selected_range.is_empty() {
        return Vec::new();
    }
    let mut quads = Vec::new();
    for (row, line) in layout.lines.iter().enumerate() {
        let start = layout.line_starts[row];
        let end = start + line.len();
        let from = editor.selected_range.start.clamp(start, end);
        let to = editor.selected_range.end.clamp(start, end);
        // A line whose break is inside the selection gets a sliver of
        // trailing highlight, so a multi-line selection reads as one block
        // instead of ragged stripes.
        let trailing = if editor.selected_range.start <= end && editor.selected_range.end > end {
            px(4.)
        } else {
            px(0.)
        };
        if from == to && trailing == px(0.) {
            continue;
        }
        let top = bounds.top() + layout.line_height * row as f32;
        quads.push(fill(
            Bounds::from_corners(
                point(bounds.left() + line.x_for_index(from - start), top),
                point(
                    bounds.left() + line.x_for_index(to - start) + trailing,
                    top + layout.line_height,
                ),
            ),
            color,
        ));
    }
    quads
}

fn cursor_quad(
    editor: &SqlEditor,
    layout: &EditorLayout,
    bounds: Bounds<Pixels>,
    color: Hsla,
) -> Option<PaintQuad> {
    // A selection paints its own block; the off half of the blink simply
    // has no caret to paint.
    if !editor.selected_range.is_empty() || !editor.blink.on() {
        return None;
    }
    let offset = editor.cursor_offset();
    let row = row_for_offset(&layout.line_starts, offset);
    let line = layout.lines.get(row)?;
    let x = line.x_for_index(offset.saturating_sub(layout.line_starts[row]));
    Some(fill(
        Bounds::new(
            point(
                bounds.left() + x,
                bounds.top() + layout.line_height * row as f32,
            ),
            size(px(1.5), layout.line_height),
        ),
        color,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn diagnostic(range: Range<usize>) -> Diagnostic {
        Diagnostic {
            range,
            message: "syntax error".to_string(),
        }
    }

    /// The squiggle is cut out of the colour, not painted over it: the
    /// pieces still cover the line exactly once, in order.
    #[test]
    fn a_mark_splits_the_span_it_lands_in() {
        let line = "select 1 frm users";
        let spans = highlight::spans(line, &Vocabulary::default());
        let pieces = split_spans(spans, &[9..12]);
        let marked: Vec<&str> = pieces
            .iter()
            .filter(|(_, _, marked)| *marked)
            .map(|(range, _, _)| &line[range.clone()])
            .collect();
        assert_eq!(marked, vec!["frm"]);
        let total: usize = pieces.iter().map(|(range, _, _)| range.len()).sum();
        assert_eq!(total, line.len());
        assert!(
            pieces
                .windows(2)
                .all(|pair| pair[0].0.end == pair[1].0.start)
        );
    }

    /// The colour survives the cut. A keyword half inside a mark is still
    /// a keyword on both sides of it.
    #[test]
    fn a_split_piece_keeps_its_colour() {
        let pieces = split_spans(highlight::spans("select", &Vocabulary::default()), &[0..3]);
        assert_eq!(
            pieces,
            vec![(0..3, Token::Keyword, true), (3..6, Token::Keyword, false)]
        );
    }

    #[test]
    fn nothing_found_marks_nothing() {
        let spans = highlight::spans("select 1", &Vocabulary::default());
        assert!(split_spans(spans, &[]).iter().all(|(_, _, marked)| !marked));
    }

    /// A statement runs over several lines, and each shaped line knows
    /// only its own bytes.
    #[test]
    fn a_mark_is_cut_to_the_line_it_falls_on() {
        // "select 1\nfrm users": line two starts at byte 9.
        let diagnostics = [diagnostic(4..12)];
        assert_eq!(marks_on_line(&diagnostics, 0, 8), vec![4..8]);
        assert_eq!(marks_on_line(&diagnostics, 9, 9), vec![0..3]);
        // A line the mark does not reach carries nothing.
        assert_eq!(
            marks_on_line(&diagnostics, 19, 5),
            Vec::<Range<usize>>::new()
        );
    }

    fn mark(range: Range<usize>, status: StatementStatus) -> StatementMark {
        StatementMark {
            range,
            status,
            ..StatementMark::default()
        }
    }

    fn on(ix: usize, status: StatementStatus, first: bool) -> Option<LineMark> {
        Some(LineMark {
            ix,
            status,
            kind: StatementKind::Plain,
            first,
        })
    }

    #[test]
    fn a_statement_marks_every_line_it_covers() {
        let text = "select 1;\nupdate t\n   set a = 1;\n";
        let marks = [
            mark(0..8, StatementStatus::Done),
            mark(10..31, StatementStatus::Running),
        ];
        assert_eq!(
            line_marks(text, &marks),
            vec![
                // The mark itself sits on the first line of each.
                on(0, StatementStatus::Done, true),
                on(1, StatementStatus::Running, true),
                on(1, StatementStatus::Running, false),
                // The line past the last semicolon belongs to no statement.
                None,
            ]
        );
    }

    #[test]
    fn a_statement_ending_at_a_line_break_does_not_open_the_next_line() {
        // `select 1` runs to offset 8, and offset 9 opens line two. A range
        // whose end is one past its last character must not reach there.
        let marks = [mark(0..8, StatementStatus::Done)];
        assert_eq!(
            line_marks("select 1;\nselect 2;", &marks),
            vec![on(0, StatementStatus::Done, true), None]
        );
    }

    #[test]
    fn nothing_run_marks_nothing() {
        assert_eq!(line_marks("select 1;\nselect 2;", &[]), vec![None, None]);
    }

    #[test]
    fn a_stale_range_is_pulled_back_into_the_buffer() {
        let text = "select 1";
        // The platform's copy of the buffer can be an edit ahead of ours.
        assert_eq!(clamp_range(text, 40..50), 8..8);
        assert_eq!(clamp_range(text, 2..50), 2..8);
        // Out of order comes back ordered rather than panicking on slice.
        assert_eq!(clamp_range(text, 6..2), 6..6);
    }

    /// The panel marks what the word actually matched, wherever in the name
    /// that landed — not the label's first characters, which is what a
    /// prefix match used to make the same thing.
    #[test]
    fn a_label_is_cut_into_matched_and_plain() {
        let hit = fuzzy::score("master_client_reference", "mast_cl").expect("matches");
        assert_eq!(
            matched_spans("master_client_reference", &hit.ranges),
            [
                ("mast".to_string(), true),
                ("er_".to_string(), false),
                ("cl".to_string(), true),
                ("ient_reference".to_string(), false),
            ]
        );
        // Nothing typed marks nothing, and the whole label stays one piece.
        assert_eq!(matched_spans("users", &[]), [("users".to_string(), false)]);
    }

    #[test]
    fn clamping_never_splits_a_character() {
        let text = "héllo";
        // Byte 2 is inside the two-byte 'é'.
        assert_eq!(clamp_offset(text, 2), 1);
        assert!(text.is_char_boundary(clamp_offset(text, 2)));
        for offset in 0..=text.len() + 4 {
            assert!(text.is_char_boundary(clamp_offset(text, offset)));
        }
    }

    #[test]
    fn composition_selection_is_measured_inside_the_inserted_text() {
        // Two UTF-16 units into "héllo" is one code point plus one byte
        // of the two-byte one, so byte 3.
        assert_eq!(utf16_to_byte("héllo", 2), 3);
        assert_eq!(utf16_to_byte("abc", 0), 0);
        // Past the end clamps rather than running off the buffer.
        assert_eq!(utf16_to_byte("abc", 99), 3);
    }
}
