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

mod highlight;
mod motion;

pub use highlight::Vocabulary;

use gpui::{
    App, Bounds, ClipboardItem, Context, CursorStyle, Element, ElementId, ElementInputHandler,
    Entity, EntityInputHandler, FocusHandle, Focusable, GlobalElementId, Hsla, IntoElement,
    LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point,
    ScrollHandle, ShapedLine, SharedString, Style, TextRun, UTF16Selection, UnderlineStyle, Window,
    actions, div, fill, point, prelude::*, px, relative, size,
};
use highlight::Token;
use std::ops::Range;
use std::sync::Arc;
use theme::theme;

actions!(
    sql_editor,
    [
        Backspace,
        Copy,
        Cut,
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
        Paste,
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
    ]
}

const KEY_CONTEXT: &str = "SqlEditor";
/// One tab inserts this much, matching the design comp's indented SQL.
const INDENT: &str = "  ";
/// 12px text on the comp's 1.75 line height.
const FONT_SIZE: f32 = 12.;
const LINE_HEIGHT: f32 = 21.;
const GUTTER_WIDTH: f32 = 44.;
const TEXT_PADDING_X: f32 = 16.;
const TEXT_PADDING_Y: f32 = 12.;
/// Deep enough for a long editing session, bounded so a runaway paste
/// loop cannot grow the history without limit.
const UNDO_DEPTH: usize = 256;

pub struct SqlEditor {
    focus_handle: FocusHandle,
    scroll_handle: ScrollHandle,
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
            content,
            placeholder: "select 1".into(),
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
        }
    }

    pub fn text(&self) -> &str {
        &self.content
    }

    pub fn line_count(&self) -> usize {
        self.content.split('\n').count()
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

        div()
            .id("sql-editor")
            .key_context(KEY_CONTEXT)
            .track_focus(&self.focus_handle(cx))
            .track_scroll(&self.scroll_handle)
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
            .on_mouse_down(MouseButton::Left, cx.listener(Self::on_mouse_down))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_move(cx.listener(Self::on_mouse_move))
            .size_full()
            .overflow_y_scroll()
            .flex()
            .text_size(px(FONT_SIZE))
            .line_height(px(LINE_HEIGHT))
            .child(
                // Line-number gutter, right-aligned against the rule.
                div()
                    .w(px(GUTTER_WIDTH))
                    .flex_none()
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
                    .flex_1()
                    .min_w(px(0.))
                    .px(px(TEXT_PADDING_X))
                    .py(px(TEXT_PADDING_Y))
                    .child(EditorElement { editor: cx.entity() }),
            )
    }
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

        let (quads, cursor, cursor_top) = if placeholder {
            (Vec::new(), None, None)
        } else {
            let row = row_for_offset(&layout.line_starts, editor.cursor_offset());
            (
                selection_quads(editor, &layout, bounds, colors.selection),
                cursor_quad(editor, &layout, bounds, colors.accent),
                Some(bounds.top() + line_height * row as f32),
            )
        };

        PrepaintState { layout: Some(layout), quads, cursor, cursor_top }
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
        let follow = prepaint
            .cursor_top
            .filter(|_| self.editor.read(cx).pending_autoscroll);

        self.editor.update(cx, |editor, _| {
            editor.last_layout = Some(layout);
            editor.last_bounds = Some(bounds);
        });

        if let Some(cursor_top) = follow {
            self.follow_cursor(cursor_top, line_height, window, cx);
        }
    }
}

impl EditorElement {
    /// Pull the viewport back over the caret after it moved out of sight.
    /// A new offset only takes effect on the next frame, so ask for one.
    fn follow_cursor(
        &self,
        cursor_top: Pixels,
        line_height: Pixels,
        window: &mut Window,
        cx: &mut App,
    ) {
        let (scroll_handle, viewport) = {
            let editor = self.editor.read(cx);
            (editor.scroll_handle.clone(), editor.scroll_handle.bounds())
        };
        if viewport.size.height <= px(0.) {
            return;
        }

        // Keep the editor's padding visible above and below the caret, so
        // it never sits flush against the edge of the pane.
        let above = cursor_top - px(TEXT_PADDING_Y);
        let below = cursor_top + line_height + px(TEXT_PADDING_Y);
        let mut offset = scroll_handle.offset();
        if above < viewport.top() {
            offset.y += viewport.top() - above;
        } else if below > viewport.bottom() {
            offset.y -= below - viewport.bottom();
        } else {
            self.editor.update(cx, |editor, _| editor.pending_autoscroll = false);
            return;
        }
        offset.y = offset.y.min(px(0.));

        scroll_handle.set_offset(offset);
        self.editor.update(cx, |editor, _| editor.pending_autoscroll = false);
        window.refresh();
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
