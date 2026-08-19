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
    App, Bounds, ClipboardItem, Context, CursorStyle, Div, Element, ElementId,
    ElementInputHandler, Entity, EntityInputHandler, FocusHandle, Focusable, FontWeight,
    GlobalElementId, Hsla, IntoElement, KeyContext, LayoutId, MouseButton, MouseDownEvent,
    MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point, ScrollHandle, ShapedLine, SharedString,
    Style, TextRun, UTF16Selection, UnderlineStyle, Window, actions, div, fill, point, prelude::*,
    px, relative, size,
};
use highlight::Token;
use std::ops::Range;
use std::sync::Arc;
use theme::theme;
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
const GUTTER_WIDTH: f32 = 44.;
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

    pub fn line_count(&self) -> usize {
        self.content.split('\n').count()
    }

    /// The longest line, in characters. The element sizes itself by it, so
    /// the container around it knows there is something to scroll across to.
    ///
    /// Characters, not shaped pixels: the font is monospaced, and measuring
    /// it exactly would mean shaping every line a second time on every
    /// frame, before there is a layout to shape into. The placeholder
    /// counts when the buffer is empty, because that is what is on screen.
    fn widest_line(&self) -> usize {
        let text = if self.content.is_empty() { self.placeholder.as_ref() } else { &self.content };
        text.split('\n').map(|line| line.chars().count()).max().unwrap_or(0)
    }

    /// Swap in the names of the connected database, so the tokenizer can
    /// tell a real relation from a typo.
    pub fn set_vocabulary(&mut self, vocabulary: Arc<Vocabulary>, cx: &mut Context<Self>) {
        self.vocabulary = vocabulary;
        cx.notify();
    }

    pub fn set_text(&mut self, text: impl Into<String>, cx: &mut Context<Self>) {
        self.push_undo(EditKind::None);
        self.content = text.into();
        let end = self.content.len();
        self.selected_range = end..end;
        self.selection_reversed = false;
        self.marked_range = None;
        cx.notify();
    }

    // --- selection -------------------------------------------------------

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
        cx.notify();
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
        cx.notify();
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
        let Some(snapshot) = self.undo.pop() else { return };
        let current = self.restore(snapshot);
        self.redo.push(current);
        cx.notify();
    }

    fn redo(&mut self, _: &Redo, _: &mut Window, cx: &mut Context<Self>) {
        let Some(snapshot) = self.redo.pop() else { return };
        let current = self.restore(snapshot);
        self.undo.push(current);
        cx.notify();
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
        if highlight::spans(line, &self.vocabulary).iter().any(|(range, token)| {
            range.contains(&column.saturating_sub(1))
                && matches!(token, highlight::Token::Literal | highlight::Token::Comment)
        }) {
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
        let Some(completion) = self.completions.get(ix) else { return };
        let label = completion.label.clone();
        let range = self.completion_range.clone();

        // Accepting is one undo step, never joined to the typing before it.
        self.last_edit = EditKind::None;
        self.selected_range = range.clone();
        self.replace_text_in_range(
            Some(self.range_to_utf16(&range)),
            &label,
            window,
            cx,
        );
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
        self.marked_range.as_ref().map(|range| self.range_to_utf16(range))
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
        cx.notify();
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
        cx.notify();
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
            point(bounds.left() + line.x_for_index(to), top + layout.line_height),
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
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();
        let line_count = self.line_count();
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
                        // Line-number gutter, right-aligned against the rule.
                        div()
                            .w(px(GUTTER_WIDTH))
                            .flex_none()
                            // ...and with the stretch gone, both columns
                            // keep the pane's height as a floor, so their
                            // backgrounds and the rule between them still
                            // reach the bottom of a short buffer.
                            .min_h(relative(1.))
                            .py(px(TEXT_PADDING_Y))
                            .pr(px(10.))
                            .bg(colors.panel)
                            .border_r_1()
                            .border_color(colors.border)
                            .text_color(colors.line_number)
                            .flex()
                            .flex_col()
                            .items_end()
                            .children((1..=line_count).map(|n| div().child(n.to_string()))),
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
                                    .child(EditorElement { editor: cx.entity() }),
                            ),
                    ),
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
        // How much of each label the user has already typed, so the
        // matched head can be marked and the rest left plain.
        let typed = self.content[clamp_range(&self.content, self.completion_range.clone())]
            .chars()
            .count();

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
                let (head, tail) = split_at_chars(&completion.label, typed);
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
                    .child(
                        div()
                            .min_w(px(0.))
                            .flex()
                            .overflow_hidden()
                            .child(
                                div()
                                    .flex_none()
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_color(colors.accent_deep)
                                    .child(head),
                            )
                            .child(div().truncate().text_color(colors.text).child(tail)),
                    )
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

/// Split a label after `count` characters, never mid-character.
fn split_at_chars(label: &str, count: usize) -> (String, String) {
    let split = label
        .char_indices()
        .nth(count)
        .map(|(ix, _)| ix)
        .unwrap_or(label.len());
    (label[..split].to_string(), label[split..].to_string())
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
        let text_color = if placeholder { colors.text_faint } else { style.color };

        let mut lines = Vec::new();
        let mut line_starts = Vec::new();
        let mut offset = 0;
        for line in text.split('\n') {
            line_starts.push(offset);
            let underline = underline_for(editor, offset, line.len());
            let runs = if placeholder {
                single_run(line, style.font(), text_color, underline)
            } else {
                highlight::spans(line, &editor.vocabulary)
                    .into_iter()
                    .map(|(range, token)| TextRun {
                        len: range.len(),
                        font: style.font(),
                        color: token_color(token, text_color, &colors),
                        background_color: None,
                        underline,
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

        let layout = EditorLayout { lines, line_starts, line_height };

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
            (
                selection_quads(editor, &layout, bounds, colors.selection),
                cursor_quad(editor, &layout, bounds, colors.accent),
                Some(bounds.top() + line_height * row as f32),
                Some(bounds.left() + column),
            )
        };

        if !editor.completions.is_empty() {
            self.draw_completions_menu(&layout, bounds, window, cx);
        }

        PrepaintState { layout: Some(layout), quads, cursor, cursor_top, cursor_left }
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

        let layout = prepaint.layout.take().expect("prepaint always builds a layout");
        for (row, line) in layout.lines.iter().enumerate() {
            let origin = point(bounds.left(), bounds.top() + layout.line_height * row as f32);
            line.paint(origin, layout.line_height, gpui::TextAlign::Left, None, window, cx)
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
        let Some(line) = layout.lines.get(row) else { return };
        // Anchor to the start of the word, so the popup lines up with what
        // it is completing rather than drifting right as the user types.
        let anchor = {
            let editor = self.editor.read(cx);
            editor.completion_range.start.min(offset)
        };
        let x = line.x_for_index(anchor.saturating_sub(layout.line_starts[row]).min(line.len()));

        let mut menu = self
            .editor
            .update(cx, |editor, cx| editor.completions_menu(cx).into_any_element());
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
        let x = (bounds.left() + x).min(viewport.width - size.width).max(px(0.));

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

        self.editor.update(cx, |editor, _| editor.pending_autoscroll = false);
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
fn underline_for(editor: &SqlEditor, line_start: usize, line_len: usize) -> Option<UnderlineStyle> {
    let marked = editor.marked_range.as_ref()?;
    (marked.start < line_start + line_len && marked.end > line_start).then(|| UnderlineStyle {
        color: None,
        thickness: px(1.),
        wavy: false,
    })
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
    if !editor.selected_range.is_empty() {
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

    #[test]
    fn a_stale_range_is_pulled_back_into_the_buffer() {
        let text = "select 1";
        // The platform's copy of the buffer can be an edit ahead of ours.
        assert_eq!(clamp_range(text, 40..50), 8..8);
        assert_eq!(clamp_range(text, 2..50), 2..8);
        // Out of order comes back ordered rather than panicking on slice.
        assert_eq!(clamp_range(text, 6..2), 6..6);
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
