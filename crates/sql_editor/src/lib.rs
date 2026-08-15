//! A small multi-line text editor for the query tab.
//!
//! Built on the same shape as `gpui/examples/input.rs`: the entity owns the
//! buffer and implements `EntityInputHandler` so the platform delivers
//! typed text and IME edits; a custom element shapes and paints the lines.
//! This one is multi-line, so it keeps a shaped line per row and maps byte
//! offsets to (row, column) both ways.
//!
//! It is deliberately small: colouring comes from a one-pass tokenizer,
//! and there is no undo and no wrapping. Offsets are byte offsets into the
//! buffer and always sit on a character boundary.

mod highlight;

use gpui::{
    App, Bounds, ClipboardItem, Context, CursorStyle, Element, ElementId, ElementInputHandler,
    Entity, EntityInputHandler, FocusHandle, Focusable, GlobalElementId, Hsla,
    IntoElement, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, PaintQuad,
    Pixels, Point, ShapedLine, SharedString, Style, TextRun, UTF16Selection, UnderlineStyle,
    Window, actions, div, fill, point, prelude::*, px, relative, size,
};
use highlight::Token;
use std::ops::Range;
use theme::theme;

actions!(
    sql_editor,
    [
        Backspace,
        Delete,
        Left,
        Right,
        Up,
        Down,
        SelectLeft,
        SelectRight,
        SelectAll,
        Home,
        End,
        Newline,
        Indent,
        Paste,
        Cut,
        Copy,
    ]
);

/// Key bindings for the editor. Bind these once at startup, scoped to the
/// editor's key context so they never shadow the app's own bindings.
pub fn key_bindings() -> Vec<gpui::KeyBinding> {
    let context = Some(KEY_CONTEXT);
    vec![
        gpui::KeyBinding::new("backspace", Backspace, context),
        gpui::KeyBinding::new("delete", Delete, context),
        gpui::KeyBinding::new("left", Left, context),
        gpui::KeyBinding::new("right", Right, context),
        gpui::KeyBinding::new("up", Up, context),
        gpui::KeyBinding::new("down", Down, context),
        gpui::KeyBinding::new("shift-left", SelectLeft, context),
        gpui::KeyBinding::new("shift-right", SelectRight, context),
        gpui::KeyBinding::new("cmd-a", SelectAll, context),
        gpui::KeyBinding::new("ctrl-a", SelectAll, context),
        gpui::KeyBinding::new("home", Home, context),
        gpui::KeyBinding::new("end", End, context),
        gpui::KeyBinding::new("enter", Newline, context),
        gpui::KeyBinding::new("tab", Indent, context),
        gpui::KeyBinding::new("cmd-v", Paste, context),
        gpui::KeyBinding::new("cmd-c", Copy, context),
        gpui::KeyBinding::new("cmd-x", Cut, context),
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

pub struct SqlEditor {
    focus_handle: FocusHandle,
    content: String,
    placeholder: SharedString,
    selected_range: Range<usize>,
    selection_reversed: bool,
    marked_range: Option<Range<usize>>,
    last_layout: Option<EditorLayout>,
    last_bounds: Option<Bounds<Pixels>>,
    is_selecting: bool,
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
    pub fn new(content: impl Into<String>, cx: &mut Context<Self>) -> Self {
        let content = content.into();
        let end = content.len();
        Self {
            focus_handle: cx.focus_handle(),
            content,
            placeholder: "select 1".into(),
            selected_range: end..end,
            selection_reversed: false,
            marked_range: None,
            last_layout: None,
            last_bounds: None,
            is_selecting: false,
        }
    }

    pub fn text(&self) -> &str {
        &self.content
    }

    pub fn line_count(&self) -> usize {
        self.content.split('\n').count()
    }

    pub fn set_text(&mut self, text: impl Into<String>, cx: &mut Context<Self>) {
        self.content = text.into();
        let end = self.content.len();
        self.selected_range = end..end;
        self.marked_range = None;
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
        self.selected_range = offset..offset;
        self.selection_reversed = false;
        cx.notify();
    }

    fn select_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        if self.selection_reversed {
            self.selected_range.start = offset;
        } else {
            self.selected_range.end = offset;
        }
        if self.selected_range.end < self.selected_range.start {
            self.selection_reversed = !self.selection_reversed;
            self.selected_range = self.selected_range.end..self.selected_range.start;
        }
        cx.notify();
    }

    /// Byte offset of the start of the line holding `offset`.
    fn line_start(&self, offset: usize) -> usize {
        self.content[..offset].rfind('\n').map(|ix| ix + 1).unwrap_or(0)
    }

    /// Byte offset of the end of the line holding `offset`, before the newline.
    fn line_end(&self, offset: usize) -> usize {
        self.content[offset..]
            .find('\n')
            .map(|ix| offset + ix)
            .unwrap_or(self.content.len())
    }

    /// Move the cursor one line up or down, keeping the column as close to
    /// the current one as the target line allows.
    fn move_vertically(&mut self, down: bool, cx: &mut Context<Self>) {
        let offset = self.cursor_offset();
        let start = self.line_start(offset);
        let column = self.content[start..offset].chars().count();

        let target_start = if down {
            let end = self.line_end(offset);
            if end == self.content.len() {
                return;
            }
            end + 1
        } else {
            if start == 0 {
                return;
            }
            self.line_start(start - 1)
        };

        let target_end = self.line_end(target_start);
        let target = self.content[target_start..target_end]
            .char_indices()
            .nth(column)
            .map(|(ix, _)| target_start + ix)
            .unwrap_or(target_end);
        self.move_to(target, cx);
    }

    fn previous_boundary(&self, offset: usize) -> usize {
        self.content[..offset]
            .char_indices()
            .next_back()
            .map(|(ix, _)| ix)
            .unwrap_or(0)
    }

    fn next_boundary(&self, offset: usize) -> usize {
        self.content[offset..]
            .char_indices()
            .nth(1)
            .map(|(ix, _)| offset + ix)
            .unwrap_or(self.content.len())
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

    // --- actions ---------------------------------------------------------

    fn backspace(&mut self, _: &Backspace, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            let previous = self.previous_boundary(self.cursor_offset());
            if previous == self.cursor_offset() {
                return;
            }
            self.select_to(previous, cx);
        }
        self.replace_text_in_range(None, "", window, cx);
    }

    fn delete(&mut self, _: &Delete, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            let next = self.next_boundary(self.cursor_offset());
            if next == self.cursor_offset() {
                return;
            }
            self.select_to(next, cx);
        }
        self.replace_text_in_range(None, "", window, cx);
    }

    fn left(&mut self, _: &Left, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            self.move_to(self.previous_boundary(self.cursor_offset()), cx);
        } else {
            self.move_to(self.selected_range.start, cx);
        }
    }

    fn right(&mut self, _: &Right, _: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            self.move_to(self.next_boundary(self.cursor_offset()), cx);
        } else {
            self.move_to(self.selected_range.end, cx);
        }
    }

    fn up(&mut self, _: &Up, _: &mut Window, cx: &mut Context<Self>) {
        self.move_vertically(false, cx);
    }

    fn down(&mut self, _: &Down, _: &mut Window, cx: &mut Context<Self>) {
        self.move_vertically(true, cx);
    }

    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.previous_boundary(self.cursor_offset()), cx);
    }

    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.next_boundary(self.cursor_offset()), cx);
    }

    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx);
        self.select_to(self.content.len(), cx);
    }

    fn home(&mut self, _: &Home, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.line_start(self.cursor_offset()), cx);
    }

    fn end(&mut self, _: &End, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(self.line_end(self.cursor_offset()), cx);
    }

    fn newline(&mut self, _: &Newline, window: &mut Window, cx: &mut Context<Self>) {
        self.replace_text_in_range(None, "\n", window, cx);
    }

    fn indent(&mut self, _: &Indent, window: &mut Window, cx: &mut Context<Self>) {
        self.replace_text_in_range(None, INDENT, window, cx);
    }

    fn paste(&mut self, _: &Paste, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
            self.replace_text_in_range(None, &text, window, cx);
        }
    }

    fn copy(&mut self, _: &Copy, _: &mut Window, cx: &mut Context<Self>) {
        if !self.selected_range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(
                self.content[self.selected_range.clone()].to_string(),
            ));
        }
    }

    fn cut(&mut self, _: &Cut, window: &mut Window, cx: &mut Context<Self>) {
        if !self.selected_range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(
                self.content[self.selected_range.clone()].to_string(),
            ));
            self.replace_text_in_range(None, "", window, cx);
        }
    }

    fn on_mouse_down(&mut self, event: &MouseDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        self.is_selecting = true;
        let offset = self.offset_for_position(event.position);
        if event.modifiers.shift {
            self.select_to(offset, cx);
        } else {
            self.move_to(offset, cx);
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
        let range = self.range_from_utf16(&range_utf16);
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
        let range = range_utf16
            .as_ref()
            .map(|range| self.range_from_utf16(range))
            .or(self.marked_range.clone())
            .unwrap_or(self.selected_range.clone());

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        let cursor = range.start + new_text.len();
        self.selected_range = cursor..cursor;
        self.selection_reversed = false;
        self.marked_range.take();
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
        let range = range_utf16
            .as_ref()
            .map(|range| self.range_from_utf16(range))
            .or(self.marked_range.clone())
            .unwrap_or(self.selected_range.clone());

        self.content =
            self.content[..range.start].to_owned() + new_text + &self.content[range.end..];
        self.marked_range = (!new_text.is_empty())
            .then(|| range.start..range.start + new_text.len());
        self.selected_range = new_selected_range_utf16
            .as_ref()
            .map(|utf16| self.range_from_utf16(utf16))
            .map(|selected| selected.start + range.start..selected.end + range.start)
            .unwrap_or_else(|| {
                let cursor = range.start + new_text.len();
                cursor..cursor
            });
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
        let range = self.range_from_utf16(&range_utf16);
        let row = row_for_offset(&layout.line_starts, range.start);
        let line = layout.lines.get(row)?;
        let top = bounds.top() + layout.line_height * row as f32;
        Some(Bounds::from_corners(
            point(
                bounds.left() + line.x_for_index(range.start - layout.line_starts[row]),
                top,
            ),
            point(
                bounds.left()
                    + line.x_for_index(range.end.saturating_sub(layout.line_starts[row])),
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
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();
        let line_count = self.line_count();

        div()
            .id("sql-editor")
            .key_context(KEY_CONTEXT)
            .track_focus(&self.focus_handle(cx))
            .cursor(CursorStyle::IBeam)
            .on_action(cx.listener(Self::backspace))
            .on_action(cx.listener(Self::delete))
            .on_action(cx.listener(Self::left))
            .on_action(cx.listener(Self::right))
            .on_action(cx.listener(Self::up))
            .on_action(cx.listener(Self::down))
            .on_action(cx.listener(Self::select_left))
            .on_action(cx.listener(Self::select_right))
            .on_action(cx.listener(Self::select_all))
            .on_action(cx.listener(Self::home))
            .on_action(cx.listener(Self::end))
            .on_action(cx.listener(Self::newline))
            .on_action(cx.listener(Self::indent))
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
                highlight::spans(line)
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

        let (quads, cursor) = if placeholder {
            (Vec::new(), None)
        } else {
            (
                selection_quads(editor, &layout, bounds, colors.selection),
                cursor_quad(editor, &layout, bounds, colors.accent),
            )
        };

        PrepaintState { layout: Some(layout), quads, cursor }
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

        self.editor.update(cx, |editor, _| {
            editor.last_layout = Some(layout);
            editor.last_bounds = Some(bounds);
        });
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
        if from == to {
            continue;
        }
        let top = bounds.top() + layout.line_height * row as f32;
        quads.push(fill(
            Bounds::from_corners(
                point(bounds.left() + line.x_for_index(from - start), top),
                point(bounds.left() + line.x_for_index(to - start), top + layout.line_height),
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
