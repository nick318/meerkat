//! The ⌘K palette: one search line over everything this session can open
//! — the tables and views in the catalog, their columns, and the queries
//! this connection has already run.
//!
//! The palette holds no data of its own. The shell hands it the catalog it
//! introspected and the runs it read back from the local file, and this
//! module turns them into a flat list of rows. That keeps the matching
//! pure, so it can be tested without a window, and it keeps the palette
//! honest: it can only offer what the session already has.
//!
//! Rows are flattened the way the sidebar and the history list are —
//! headings and results share one height — because `uniform_list` measures
//! one row and lays out only what is on screen. Arrow keys then move the
//! selection by index, and the list scrolls to it.
//!
//! Matching is a case-insensitive substring, not a fuzzy score. A database
//! viewer's names are typed, not guessed: someone looking for `user_id`
//! types part of `user_id`, and a contiguous hit is the one the palette can
//! underline honestly.
//!
//! A **dot in the query names a path**. Every result carries its name in
//! parts — schema, relation, and a column's own name — and the typed parts
//! are lined up with the *end* of that path. So `task` finds every `task`
//! in the database, `sample_dev_sample.task` finds the one, `dev.ta` finds
//! it without typing it out, and `task.name` finds a column without naming
//! a schema. It is one rule; nothing about it is special-cased per section.

use chrono::{Local, NaiveDate, TimeZone};
use gpui::{
    AnyElement, App, ElementId, FontWeight, KeyBinding, SharedString, Window, actions, div,
    prelude::*, px,
};
use introspect::{Catalog, TableKind};
use std::ops::Range;
use std::rc::Rc;
use storage::QueryRun;
use theme::ThemeColors;
use ui::{format_millis, section_label};

actions!(palette, [Toggle, SelectPrev, SelectNext, OpenInNewTab]);

/// The context the palette's own keys are scoped to. It sits below the
/// shell's context, so a key bound in both — ⌘⏎ — goes to the palette
/// while the palette is open and to the shell when it is not.
pub const KEY_CONTEXT: &str = "Palette";

/// Key bindings for the palette. ⌘K is scoped to the shell rather than to
/// the palette, because it has to work when the palette is closed as well.
pub fn key_bindings() -> Vec<KeyBinding> {
    vec![
        KeyBinding::new("cmd-k", Toggle, Some("Shell")),
        KeyBinding::new("up", SelectPrev, Some(KEY_CONTEXT)),
        KeyBinding::new("down", SelectNext, Some(KEY_CONTEXT)),
        KeyBinding::new("ctrl-p", SelectPrev, Some(KEY_CONTEXT)),
        KeyBinding::new("ctrl-n", SelectNext, Some(KEY_CONTEXT)),
        KeyBinding::new("cmd-enter", OpenInNewTab, Some(KEY_CONTEXT)),
    ]
}

/// The dialog's geometry, from the comp.
pub const WIDTH: f32 = 660.;
pub const LIST_HEIGHT: f32 = 404.;
pub const TOP_MARGIN: f32 = 96.;
/// The search line reads a size larger than the rest of the app: it is the
/// one thing on the screen the user is looking at.
pub const INPUT_FONT_SIZE: f32 = 14.;
/// One height for headings and results alike, so the list can virtualize.
pub const ROW_HEIGHT: f32 = 28.;

/// Column widths from the comp: glyph, name, meta, trailing.
const GLYPH_WIDTH: f32 = 16.;
const META_WIDTH: f32 = 84.;
const TRAILING_WIDTH: f32 = 96.;

/// How many results each section shows when the palette is searching
/// everything, and how many one section shows on its own. The list is a
/// way in, not a report: past a screenful the user should narrow instead.
const SECTION_CAP: usize = 6;
const SCOPE_CAP: usize = 40;

/// How long a name may read before it is cut. A statement runs far past
/// the name column, and the column that holds it is about this wide.
const LABEL_CHARS: usize = 68;

/// Which of the four lists the palette is searching.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    All,
    Tables,
    Columns,
    History,
}

impl Scope {
    /// The chips, in the order the comp draws them.
    pub const ALL: [Scope; 4] = [Scope::All, Scope::Tables, Scope::Columns, Scope::History];

    pub fn label(self) -> &'static str {
        match self {
            Scope::All => "all",
            Scope::Tables => "tables",
            Scope::Columns => "columns",
            Scope::History => "history",
        }
    }

    /// The prefix that picks this scope from the search line itself.
    pub fn prefix(self) -> Option<&'static str> {
        match self {
            Scope::All => None,
            Scope::Tables => Some("t:"),
            Scope::Columns => Some("c:"),
            Scope::History => Some("h:"),
        }
    }

    fn shows_tables(self) -> bool {
        matches!(self, Scope::All | Scope::Tables)
    }

    fn shows_columns(self) -> bool {
        matches!(self, Scope::All | Scope::Columns)
    }

    fn shows_history(self) -> bool {
        matches!(self, Scope::All | Scope::History)
    }
}

/// Read the scope out of the query itself. A typed `t:` wins over the chip
/// that is lit, because the user just said what they meant.
pub fn parse(query: &str, chip: Scope) -> (Scope, &str) {
    for scope in Scope::ALL {
        if let Some(prefix) = scope.prefix()
            && let Some(rest) = query.strip_prefix(prefix)
        {
            return (scope, rest.trim_start());
        }
    }
    (chip, query.trim_start())
}

/// What opening a row does. Everything the palette offers ends in one of
/// these two, so the shell needs no knowledge of the row that produced it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Pick {
    /// Open (or focus) a table or view.
    Table { schema: String, table: String },
    /// Open a statement in a query tab. It is never re-run behind the
    /// user, for the same reason the history screen does not re-run one.
    Query(String),
}

/// Called with the row the user picked, and whether ⌘⏎ asked for a tab of
/// its own rather than the tab that is already open.
pub type OnPick = Rc<dyn Fn(Pick, bool, &mut Window, &mut App)>;

/// The list, flattened: a heading per section, then that section's rows.
pub enum Row {
    Header { label: SharedString, count: SharedString },
    Item(Item),
}

/// One result, ready to paint.
pub struct Item {
    label: SharedString,
    /// The parts of `label` the search matched, as byte ranges in order
    /// and never overlapping. Empty when the query is empty, or when the
    /// hits fell outside the part of a long label that is on screen.
    hits: Vec<Range<usize>>,
    glyph: Glyph,
    meta: SharedString,
    /// What the last column says when the row is not selected: a clock for
    /// a run, nothing for a relation.
    trailing: SharedString,
    failed: bool,
    pick: Pick,
    /// The real path this result sits at — schema, relation, and a
    /// column's own name — and which part of it the query addressed. The
    /// search line completes from these; the row paints only `label`.
    /// Empty for a run, which is prose rather than a path.
    path: Vec<String>,
    named: usize,
}

/// A result before the palette knows how much of its name the row will
/// show. That is not decided per result: it depends on what the whole
/// section found, so it waits for [`dress`].
struct Candidate {
    /// The name in parts, outermost first: the schema, the relation, and
    /// a column's own name when there is one.
    parts: Vec<String>,
    /// Where the query hit inside each part, if it hit that part at all.
    hits: Vec<Option<Range<usize>>>,
    /// The outermost part the query itself named. Everything before it the
    /// user did not ask about.
    named: usize,
    /// The outermost part worth showing when nothing forces the whole
    /// path: a relation and a column both hide their schema.
    from: usize,
    /// Whether the search line may be completed from this path. A run
    /// carries a statement, not a path, so it may not.
    completes: bool,
    /// How far out the query aligned, where the hit sat, and how long the
    /// name is. A table the query named directly comes before one its
    /// schema matched, and `users` before `active_users_7d`.
    rank: (usize, usize, usize),
    glyph: Glyph,
    meta: SharedString,
    trailing: SharedString,
    failed: bool,
    pick: Pick,
}

enum Glyph {
    Table,
    View,
    Column,
    Run,
}

impl Row {
    pub fn pick(&self) -> Option<Pick> {
        match self {
            Row::Header { .. } => None,
            Row::Item(item) => Some(item.pick.clone()),
        }
    }
}

/// What one search found: the rows to paint, and how many results matched
/// before the caps above cut the list down.
pub struct Results {
    pub rows: Vec<Row>,
    pub matches: usize,
}

/// Search the session and flatten what it found.
///
/// `runs` is the history newest first, as the store returns it; the order
/// within each section is the order the caller handed it in, so the newest
/// query and the first table of the catalog stay where they were.
pub fn build(
    catalog: Option<&Catalog>,
    runs: &[QueryRun],
    scope: Scope,
    needle: &str,
    today: NaiveDate,
) -> Results {
    let cap = if scope == Scope::All { SECTION_CAP } else { SCOPE_CAP };
    let mut rows = Vec::new();
    let mut matches = 0;

    if scope.shows_tables() {
        let found = tables(catalog, needle);
        matches += found.len();
        let schemas = schemas_of(&found);
        let (items, qualified) = dress(found);
        // The schema is named once: on the rows if they carry it, in the
        // heading if they do not.
        let heading = match (qualified, schemas.as_slice()) {
            (false, [only]) => format!("TABLES · {}", only.to_ascii_uppercase()).into(),
            _ => SharedString::from("TABLES"),
        };
        push_section(&mut rows, heading, items, cap);
    }
    if scope.shows_columns() {
        let found = columns(catalog, needle);
        matches += found.len();
        push_section(&mut rows, "COLUMNS".into(), dress(found).0, cap);
    }
    if scope.shows_history() {
        let found = history(runs, needle, today);
        matches += found.len();
        push_section(&mut rows, "RECENT QUERIES".into(), dress(found).0, cap);
    }

    Results { rows, matches }
}

/// A heading, then that section's rows, cut to the cap. An empty section
/// leaves no heading behind: a palette showing `COLUMNS 0` teaches nothing.
fn push_section(rows: &mut Vec<Row>, label: SharedString, found: Vec<Item>, cap: usize) {
    if found.is_empty() {
        return;
    }
    let total = found.len();
    let shown = total.min(cap);
    rows.push(Row::Header {
        label,
        count: if shown < total {
            format!("{shown} of {total}").into()
        } else {
            total.to_string().into()
        },
    });
    rows.extend(found.into_iter().take(cap).map(Row::Item));
}

/// The distinct schemas a section's results came from, in catalog order.
/// A run belongs to the connection rather than to a schema, and carries
/// only one part, so it contributes none.
fn schemas_of(found: &[Candidate]) -> Vec<String> {
    let mut schemas: Vec<String> = Vec::new();
    for candidate in found.iter().filter(|candidate| candidate.parts.len() > 1) {
        if !schemas.contains(&candidate.parts[0]) {
            schemas.push(candidate.parts[0].clone());
        }
    }
    schemas
}

/// The last pass over a section, once its results are known: settle how
/// much of each name the row shows, join it, and cut the long ones down to
/// the name column.
///
/// The order matters. Clipping first would put the ellipsis in the wrong
/// place and could drop a hit that joining then shifts.
///
/// Also reports whether the rows ended up carrying their schema, so the
/// heading can stop naming it.
fn dress(found: Vec<Candidate>) -> (Vec<Item>, bool) {
    // A section whose results run over more than one schema shows every
    // schema. With a single schema the heading names it already, and
    // repeating it on every row says nothing.
    let spread = schemas_of(&found).len() > 1;
    // Unless the query named the schema: someone who typed one wants to
    // read it back.
    let named = found
        .iter()
        .any(|candidate| candidate.parts.len() > 1 && candidate.named == 0);

    let items = found
        .into_iter()
        .map(|candidate| {
            let from = if spread { 0 } else { candidate.from.min(candidate.named) };
            let path = if candidate.completes { candidate.parts.clone() } else { Vec::new() };

            let mut label = String::new();
            let mut hits = Vec::new();
            for (part, hit) in candidate.parts[from..].iter().zip(&candidate.hits[from..]) {
                if !label.is_empty() {
                    label.push('.');
                }
                if let Some(hit) = hit.clone().filter(|hit| !hit.is_empty()) {
                    hits.push((label.len() + hit.start)..(label.len() + hit.end));
                }
                label.push_str(part);
            }

            let (label, hits) = clip(&label, hits, LABEL_CHARS);
            Item {
                label: label.into(),
                hits,
                glyph: candidate.glyph,
                meta: candidate.meta,
                trailing: candidate.trailing,
                failed: candidate.failed,
                pick: candidate.pick,
                path,
                named: candidate.named,
            }
        })
        .collect();
    (items, spread || named)
}

/// The relations the query names, ranked by where the hit sits and then by
/// how short the name is: `users` before `active_users_7d`.
fn tables(catalog: Option<&Catalog>, needle: &str) -> Vec<Candidate> {
    let Some(catalog) = catalog else { return Vec::new() };
    let mut found = Vec::new();
    for schema in &catalog.schemas {
        for table in &schema.tables {
            let parts = vec![schema.name.clone(), table.name.clone()];
            // A bare schema name answers with the tables it holds.
            let Some((hits, named)) = find_path(&parts, needle, true) else { continue };
            found.push(Candidate {
                rank: rank(&hits, &table.name),
                parts,
                hits,
                named,
                // A relation hides its schema: the heading has it.
                from: 1,
                completes: true,
                glyph: match table.kind {
                    TableKind::Table => Glyph::Table,
                    TableKind::View => Glyph::View,
                },
                // A relation says what it is, not how big it is. The
                // estimate belongs to the sidebar; here it only makes one
                // row in a list of near-identical names louder than the
                // rest.
                meta: match table.kind {
                    TableKind::Table => SharedString::default(),
                    TableKind::View => "view".into(),
                },
                trailing: SharedString::default(),
                failed: false,
                pick: Pick::Table {
                    schema: schema.name.clone(),
                    table: table.name.clone(),
                },
            });
        }
    }
    found.sort_by_key(|candidate| candidate.rank);
    found
}

/// The columns the query names. A bare query matches the column's own
/// name only: `users` already matched as a table, and matching it again
/// for each of its columns would bury the list. `users.id` names both.
fn columns(catalog: Option<&Catalog>, needle: &str) -> Vec<Candidate> {
    // With no query every column in the database would match, which is
    // thousands of rows saying nothing. Columns are what you narrow to.
    if needle.is_empty() {
        return Vec::new();
    }
    let Some(catalog) = catalog else { return Vec::new() };
    let mut found = Vec::new();
    for schema in &catalog.schemas {
        for table in &schema.tables {
            for column in &table.columns {
                let parts =
                    vec![schema.name.clone(), table.name.clone(), column.name.clone()];
                let Some((hits, named)) = find_path(&parts, needle, false) else { continue };
                found.push(Candidate {
                    rank: rank(&hits, &column.name),
                    parts,
                    hits,
                    named,
                    // A column keeps its table — a bare `id` says nothing
                    // — but hides its schema.
                    from: 1,
                    completes: true,
                    glyph: Glyph::Column,
                    meta: column.data_type.clone().into(),
                    trailing: SharedString::default(),
                    failed: false,
                    pick: Pick::Table {
                        schema: schema.name.clone(),
                        table: table.name.clone(),
                    },
                });
            }
        }
    }
    found.sort_by_key(|candidate| candidate.rank);
    found
}

/// The runs whose statement holds the query, newest first — the order the
/// store already returned them in, which is the order that matters here.
///
/// A statement is one part, not a path: a dot in the query is matched
/// literally, because `public.users` is how the statement itself reads.
fn history(runs: &[QueryRun], needle: &str, today: NaiveDate) -> Vec<Candidate> {
    runs.iter()
        .filter_map(|run| {
            let statement = one_line(&run.statement);
            let hit = find(&statement, needle)?;
            Some(Candidate {
                rank: (0, hit.start, statement.len()),
                parts: vec![statement],
                hits: vec![Some(hit)],
                named: 0,
                from: 0,
                completes: false,
                glyph: Glyph::Run,
                meta: match (&run.error, run.elapsed_ms) {
                    (Some(_), _) => "error".into(),
                    (None, Some(millis)) => format_millis(millis as u128).into(),
                    (None, None) => SharedString::default(),
                },
                trailing: clock(run.ran_at, today),
                failed: run.error.is_some(),
                pick: Pick::Query(run.statement.clone()),
            })
        })
        .collect()
}

/// What the search line would become if the selected row's name were
/// accepted: its real path, as far as the query has reached, with a
/// trailing dot when there is another part to go. `None` when there is
/// nothing to add.
///
/// It **replaces** what was typed rather than appending to it, because a
/// hit sits anywhere inside a name: `dev` completes to
/// `sample_dev_sample.`, which no amount of appending would reach. That
/// is also why the caller only paints a hint when the completion happens
/// to start with what the user typed — the rest of the time accepting it
/// rewrites the line, and a hint would have lied about that.
///
/// An empty query completes nothing. There is a selected row, but the
/// user has said nothing for it to finish.
pub fn completion(rows: &[Row], selected: usize, needle: &str) -> Option<String> {
    if needle.is_empty() {
        return None;
    }
    let Some(Row::Item(item)) = rows.get(selected) else { return None };
    if item.path.is_empty() {
        return None;
    }
    // One past the part being typed. A query cannot reach deeper than the
    // path it matched, but a stale selection could still say so.
    let reached = item.named + needle.split('.').count();
    if reached > item.path.len() {
        return None;
    }

    let mut completed = item.path[item.named..reached].join(".");
    if reached < item.path.len() {
        completed.push('.');
    }
    (completed != needle).then_some(completed)
}

/// Where a dotted query hits a name, part by part, and which part of the
/// path it started at.
///
/// The typed parts line up with the *end* of the path first, so a query
/// says as much of the name as the user cares to type and no more. Every
/// typed part must hit its own part; a query with more parts than the name
/// has cannot match at all.
///
/// `slide` lets a query that came up empty at the end try again further
/// out, which is how a bare schema name finds the tables it holds. It is
/// off for columns: a bare `task` there would answer with every column of
/// every `task` table, which is not what anyone typing it wants.
///
/// ```text
/// parts:  [sample_dev_sample, task]        [public, orders, user_id]
/// "task"                     ^hit          "user"                ^hit
/// "dev.ta"      ^hit         ^hit          "orders.user"  ^hit    ^hit
/// "dev_sample"  ^hit  (slid out one part, so the whole schema answers)
/// ```
fn find_path(
    parts: &[String],
    needle: &str,
    slide: bool,
) -> Option<(Vec<Option<Range<usize>>>, usize)> {
    let typed: Vec<&str> = needle.split('.').collect();
    if typed.len() > parts.len() {
        return None;
    }
    let flush = parts.len() - typed.len();
    let outermost = if slide { 0 } else { flush };

    // Innermost alignment first: `task` is a table before it is a schema.
    (outermost..=flush).rev().find_map(|named| {
        let mut hits = vec![None; parts.len()];
        for (ix, part) in typed.iter().enumerate() {
            hits[named + ix] = Some(find(&parts[named + ix], part)?);
        }
        Some((hits, named))
    })
}

/// How a result sorts: an alignment further out first, because a name the
/// query hit directly beats a name its container matched; then where the
/// hit sat, then how long the name is.
fn rank(hits: &[Option<Range<usize>>], name: &str) -> (usize, usize, usize) {
    let innermost = hits.iter().rposition(|hit| hit.is_some()).unwrap_or(0);
    let start = hits[innermost].as_ref().map_or(0, |hit| hit.start);
    (hits.len() - 1 - innermost, start, name.len())
}

/// A multi-line statement is one row here, so the newlines become spaces
/// and the runs of whitespace collapse.
fn one_line(statement: &str) -> String {
    statement.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// `today 09:12`, `yest. 18:04`, then the date. The near days are named
/// for the same reason the history screen names them.
fn clock(ran_at: i64, today: NaiveDate) -> SharedString {
    let Some(at) = Local.timestamp_opt(ran_at, 0).single() else {
        return SharedString::default();
    };
    let time = at.format("%H:%M");
    match (today - at.date_naive()).num_days() {
        0 => format!("today {time}").into(),
        1 => format!("yest. {time}").into(),
        _ => at.format("%-d %b %H:%M").to_string().into(),
    }
}

/// Where the query sits inside a name, as a byte range, or `None` when it
/// is not there at all. An empty query matches everything at offset zero
/// but underlines nothing.
///
/// The comparison lowercases ASCII only, which is what keeps the range
/// usable: `to_ascii_lowercase` never changes a character's byte length,
/// so an offset into the lowered copy is an offset into the original.
fn find(haystack: &str, needle: &str) -> Option<Range<usize>> {
    if needle.is_empty() {
        return Some(0..0);
    }
    let start = haystack.to_ascii_lowercase().find(&needle.to_ascii_lowercase())?;
    Some(start..start + needle.len())
}

/// Cut a long label down to the name column, keeping the matched part in
/// view. A cut end is marked with an ellipsis, and the hits move with the
/// text; a hit that fell outside the window is dropped rather than
/// pointed at the wrong characters.
fn clip(label: &str, hits: Vec<Range<usize>>, max_chars: usize) -> (String, Vec<Range<usize>>) {
    let total = label.chars().count();
    if total <= max_chars {
        return (label.to_string(), hits);
    }

    // The window is placed around the first hit, with a little text before
    // it so it does not sit flush against the leading ellipsis.
    const LEAD: usize = 8;
    let hit_char = hits.first().map(|hit| label[..hit.start].chars().count()).unwrap_or(0);
    let start_char = if hit_char > max_chars.saturating_sub(LEAD) { hit_char - LEAD } else { 0 };
    let end_char = (start_char + max_chars).min(total);
    let (start, end) = (char_offset(label, start_char), char_offset(label, end_char));

    let mut text = String::new();
    if start_char > 0 {
        text.push('…');
    }
    text.push_str(&label[start..end]);
    if end_char < total {
        text.push('…');
    }

    let shift = if start_char > 0 { '…'.len_utf8() } else { 0 };
    let moved = hits
        .into_iter()
        .filter(|hit| hit.start >= start && hit.end <= end)
        .map(|hit| (hit.start - start + shift)..(hit.end - start + shift))
        .collect();
    (text, moved)
}

/// The byte offset of the nth character, or the end of the string.
fn char_offset(text: &str, chars: usize) -> usize {
    text.char_indices().nth(chars).map_or(text.len(), |(offset, _)| offset)
}

// --- painting ------------------------------------------------------------

/// One row of the flattened list. Free-standing for the same reason the
/// catalog's and the history's rows are: the list's render closure
/// outlives the borrow of the view it came from.
pub fn palette_row(
    ix: usize,
    row: &Row,
    selected: bool,
    on_pick: &OnPick,
    colors: &ThemeColors,
    cx: &App,
) -> AnyElement {
    match row {
        Row::Header { label, count } => div()
            .h(px(ROW_HEIGHT))
            .w_full()
            .flex()
            .items_end()
            .justify_between()
            .px(px(9.))
            .pb(px(5.))
            .child(section_label(label.clone(), cx))
            .child(
                div()
                    .text_size(px(9.))
                    .text_color(colors.text_faint)
                    .child(count.clone()),
            )
            .into_any_element(),
        Row::Item(item) => {
            let on_pick = on_pick.clone();
            let pick = item.pick.clone();
            let text = if item.failed { colors.error } else { colors.text_body };
            let meta = if item.failed { colors.error_secondary } else { colors.text_muted };

            let mut card = div()
                .id(ElementId::NamedInteger("palette-row".into(), ix as u64))
                .h(px(ROW_HEIGHT))
                .w_full()
                .flex()
                .items_center()
                .gap(px(10.))
                .px(px(9.))
                .rounded(px(6.))
                .cursor_pointer()
                .on_click(move |event, window, cx| {
                    on_pick(pick.clone(), event.modifiers().platform, window, cx)
                })
                .child(
                    div()
                        .w(px(GLYPH_WIDTH))
                        .flex_none()
                        .child(glyph(&item.glyph, selected, colors, cx)),
                )
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .flex()
                        .overflow_hidden()
                        .text_size(px(12.))
                        .font_weight(if selected { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                        .text_color(if selected && !item.failed { colors.text } else { text })
                        .children(spans(item, selected, colors)),
                )
                .child(
                    div()
                        .w(px(META_WIDTH))
                        .flex_none()
                        .text_size(px(10.))
                        .text_color(meta)
                        .truncate()
                        .child(item.meta.clone()),
                )
                .child(
                    div()
                        .w(px(TRAILING_WIDTH))
                        .flex_none()
                        .flex()
                        .justify_end()
                        .text_size(px(10.))
                        .text_color(if selected { colors.accent } else { colors.text_faint })
                        .truncate()
                        // The selected row is the one that says what ⏎
                        // would do; the rest keep their own last column.
                        .child(if selected {
                            SharedString::from("open ⏎")
                        } else {
                            item.trailing.clone()
                        }),
                );

            card = if selected {
                card.bg(if item.failed { colors.error_surface } else { colors.selection })
            } else {
                let hover = colors.hairline;
                card.hover(move |s| s.bg(hover))
            };
            card.into_any_element()
        }
    }
}

/// The name, cut into the matched parts and the text between them. A hit
/// keeps a warm wash behind it, which is the only place in the app where
/// text carries a background.
fn spans(item: &Item, selected: bool, colors: &ThemeColors) -> Vec<gpui::Div> {
    let label = item.label.as_ref();
    if item.hits.is_empty() {
        return vec![div().truncate().child(label.to_string())];
    }
    let wash = match (item.failed, selected) {
        (true, _) => colors.match_error,
        (false, true) => colors.match_strong,
        (false, false) => colors.match_wash,
    };

    let mut spans = Vec::with_capacity(item.hits.len() * 2 + 1);
    let mut at = 0;
    for hit in &item.hits {
        if hit.start > at {
            spans.push(div().flex_none().child(label[at..hit.start].to_string()));
        }
        spans.push(
            div()
                .flex_none()
                .rounded(px(2.))
                .bg(wash)
                .child(label[hit.clone()].to_string()),
        );
        at = hit.end;
    }
    // The tail is the one span allowed to truncate: everything the search
    // matched has already been painted by the time it is reached.
    spans.push(div().truncate().child(label[at..].to_string()));
    spans
}

fn glyph(glyph: &Glyph, selected: bool, colors: &ThemeColors, cx: &App) -> gpui::Div {
    match glyph {
        Glyph::Table => ui::table_glyph(selected, cx),
        Glyph::View => div()
            .size(px(5.))
            .rounded_full()
            .border_1()
            .border_color(if selected { colors.accent } else { colors.text_faint }),
        // A column is a slice of a table, so it is drawn as one.
        Glyph::Column => div().w(px(6.)).h(px(2.)).bg(colors.idle),
        // The letter the history screen is reached by.
        Glyph::Run => div()
            .text_size(px(10.))
            .text_color(if selected { colors.accent } else { colors.text_faint })
            .child("h"),
    }
}

/// One of the scope chips over the list. The lit chip wears the accent
/// wash; the rest are bare until hovered.
pub fn scope_chip(scope: Scope, active: bool, colors: &ThemeColors) -> gpui::Stateful<gpui::Div> {
    let mut chip = div()
        .id(ElementId::Name(format!("scope-{}", scope.label()).into()))
        .flex()
        .items_center()
        .gap(px(5.))
        .px(px(9.))
        .py(px(5.))
        .rounded(px(5.))
        .text_size(px(10.))
        .font_weight(if active { FontWeight::MEDIUM } else { FontWeight::NORMAL })
        .text_color(if active { colors.accent_deep } else { colors.text_secondary })
        .cursor_pointer()
        .child(scope.label());
    if let Some(prefix) = scope.prefix() {
        chip = chip.child(div().text_color(colors.text_faint).child(prefix));
    }
    if active {
        chip.bg(colors.selection)
    } else {
        let hover = colors.hairline;
        chip.hover(move |s| s.bg(hover))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use introspect::{Column, Schema, Table};
    use storage::RunSource;

    fn column(name: &str, data_type: &str) -> Column {
        Column {
            name: name.to_string(),
            data_type: data_type.to_string(),
            nullable: true,
            default: None,
        }
    }

    fn table(name: &str, kind: TableKind, columns: Vec<Column>) -> Table {
        Table {
            name: name.to_string(),
            kind,
            columns,
            primary_key: Vec::new(),
            approx_rows: Some(18_412),
        }
    }

    fn catalog() -> Catalog {
        Catalog {
            schemas: vec![Schema {
                name: "public".to_string(),
                tables: vec![
                    table("users", TableKind::Table, vec![column("id", "int8")]),
                    table("user_settings", TableKind::Table, Vec::new()),
                    table("accoustics_test", TableKind::Table, Vec::new()),
                    table("active_users_7d", TableKind::View, Vec::new()),
                    table("orders", TableKind::Table, vec![column("user_id", "int8")]),
                ],
            }],
        }
    }

    /// The shape that made the dotted query necessary: one table name
    /// repeated across many schemas.
    fn schemas() -> Catalog {
        let of = |schema: &str| Schema {
            name: schema.to_string(),
            tables: vec![
                table("task", TableKind::Table, vec![column("name", "text")]),
                table("task_run", TableKind::Table, Vec::new()),
            ],
        };
        Catalog {
            schemas: vec![of("sample_dev_sample"), of("sample_big_dummy"), of("public")],
        }
    }

    fn run(statement: &str) -> QueryRun {
        QueryRun {
            id: 1,
            statement: statement.to_string(),
            ran_at: Local::now().timestamp(),
            source: RunSource::User,
            elapsed_ms: Some(34),
            row_count: Some(6),
            error: None,
        }
    }

    fn labels(results: &Results) -> Vec<String> {
        results
            .rows
            .iter()
            .map(|row| match row {
                Row::Header { label, count } => format!("[{label} {count}]"),
                Row::Item(item) => item.label.to_string(),
            })
            .collect()
    }

    /// What each hit underlines, so a test can read the highlight the way
    /// the row paints it.
    fn underlined(item: &Item) -> Vec<&str> {
        item.hits.iter().map(|hit| &item.label[hit.clone()]).collect()
    }

    fn only_item(results: &Results) -> &Item {
        let Row::Item(item) = &results.rows[1] else { panic!("expected a result") };
        item
    }

    fn today() -> NaiveDate {
        Local::now().date_naive()
    }

    #[test]
    fn a_hit_at_the_start_of_a_short_name_ranks_first() {
        let results = build(Some(&catalog()), &[], Scope::Tables, "us", today());
        assert_eq!(
            labels(&results),
            [
                "[TABLES · PUBLIC 4]",
                // `users` and `user_settings` hit at 0, shortest first;
                // then the two whose hit sits further in, the nearer one
                // first.
                "users",
                "user_settings",
                "accoustics_test",
                "active_users_7d",
            ]
        );
        assert_eq!(results.matches, 4);
    }

    #[test]
    fn the_matched_part_is_reported_as_a_range_of_the_label() {
        let results = build(Some(&catalog()), &[], Scope::Tables, "US", today());
        let Row::Item(item) = &results.rows[4] else { panic!("expected a result") };
        assert_eq!(item.label, "active_users_7d");
        // Case-insensitive, and the range points at the original casing.
        assert_eq!(underlined(item), ["us"]);
        assert_eq!(item.hits, vec![7..9]);
    }

    /// The case the dotted query exists for: pick one `task` out of a
    /// schema among many, without typing the schema out.
    #[test]
    fn a_dot_names_the_schema_and_the_table() {
        let results = build(Some(&schemas()), &[], Scope::Tables, "sample_dev_sample.task", today());
        assert_eq!(labels(&results), ["[TABLES 2]", "sample_dev_sample.task", "sample_dev_sample.task_run"]);
        // Both halves are underlined, in order and without overlapping.
        assert_eq!(underlined(only_item(&results)), ["sample_dev_sample", "task"]);
    }

    #[test]
    fn each_half_of_a_dotted_query_matches_on_its_own() {
        // Neither half has to be the whole name.
        let results = build(Some(&schemas()), &[], Scope::Tables, "dev.ta", today());
        assert_eq!(labels(&results), ["[TABLES 2]", "sample_dev_sample.task", "sample_dev_sample.task_run"]);
        assert_eq!(underlined(only_item(&results)), ["dev", "ta"]);

        // A trailing dot is a schema on its own: everything it holds.
        let results = build(Some(&schemas()), &[], Scope::Tables, "big_dummy.", today());
        assert_eq!(labels(&results), ["[TABLES 2]", "sample_big_dummy.task", "sample_big_dummy.task_run"]);

        // A leading dot names no schema, so it matches every schema.
        let results = build(Some(&schemas()), &[], Scope::Tables, ".task_run", today());
        assert_eq!(results.matches, 3);
    }

    /// A schema name on its own is a question about that schema, so it
    /// answers with the tables it holds rather than with nothing.
    #[test]
    fn a_bare_schema_name_answers_with_its_tables() {
        let results = build(Some(&schemas()), &[], Scope::Tables, "sample_dev_sample", today());
        assert_eq!(
            labels(&results),
            ["[TABLES 2]", "sample_dev_sample.task", "sample_dev_sample.task_run"]
        );
        // The schema is what matched, so the schema is what is underlined.
        assert_eq!(underlined(only_item(&results)), ["sample_dev_sample"]);
    }

    /// A name the query hit directly beats one its schema matched.
    #[test]
    fn a_table_of_that_name_comes_before_a_schema_of_that_name() {
        let catalog = Catalog {
            schemas: vec![
                Schema {
                    name: "task_archive".to_string(),
                    tables: vec![table("orders", TableKind::Table, Vec::new())],
                },
                Schema {
                    name: "public".to_string(),
                    tables: vec![table("task", TableKind::Table, Vec::new())],
                },
            ],
        };
        let results = build(Some(&catalog), &[], Scope::Tables, "task", today());
        assert_eq!(labels(&results), ["[TABLES 2]", "public.task", "task_archive.orders"]);
    }

    /// Sliding is for relations only. A bare word over columns must stay
    /// on the column, or `task` answers with every column of every task
    /// table.
    #[test]
    fn a_bare_word_over_columns_stays_on_the_column() {
        let results = build(Some(&schemas()), &[], Scope::Columns, "task", today());
        assert!(results.rows.is_empty());
    }

    #[test]
    fn a_query_with_more_parts_than_the_name_matches_nothing() {
        // A relation is two parts deep, so three cannot fit it.
        let results = build(Some(&schemas()), &[], Scope::Tables, "a.b.c", today());
        assert!(results.rows.is_empty());
    }

    #[test]
    fn a_dotted_query_shows_the_schema_it_named() {
        // One schema matched, so nothing forces the full path — but the
        // user typed the schema, so the rows read it back and the heading
        // stops saying it.
        let results = build(Some(&catalog()), &[], Scope::Tables, "public.users", today());
        assert_eq!(
            labels(&results),
            ["[TABLES 2]", "public.users", "public.active_users_7d"]
        );

        // Without the dot, the heading carries the schema instead.
        let results = build(Some(&catalog()), &[], Scope::Tables, "orders", today());
        assert_eq!(labels(&results), ["[TABLES · PUBLIC 1]", "orders"]);
    }

    #[test]
    fn a_name_shared_by_two_schemas_is_qualified() {
        let results = build(Some(&schemas()), &[], Scope::Tables, "task", today());
        assert_eq!(labels(&results)[0], "[TABLES 6]");
        assert_eq!(labels(&results)[1], "sample_dev_sample.task");
        // The underline follows the name, not the schema in front of it.
        assert_eq!(underlined(only_item(&results)), ["task"]);
    }

    #[test]
    fn a_column_is_shown_under_its_table_and_opens_it() {
        let results = build(Some(&catalog()), &[], Scope::Columns, "user_", today());
        assert_eq!(labels(&results), ["[COLUMNS 1]", "orders.user_id"]);
        let item = only_item(&results);
        // The hit is in the column, so it sits past the table's name.
        assert_eq!(item.hits, vec![7..12]);
        assert_eq!(
            item.pick,
            Pick::Table { schema: "public".to_string(), table: "orders".to_string() }
        );
    }

    #[test]
    fn a_dotted_query_over_columns_names_the_table_then_the_column() {
        let results = build(Some(&schemas()), &[], Scope::Columns, "task.name", today());
        // Three schemas hold a `task`, so every row shows its own.
        assert_eq!(labels(&results)[1], "sample_dev_sample.task.name");
        assert_eq!(underlined(only_item(&results)), ["task", "name"]);

        // Three parts reach the schema, which a column has room for.
        let results = build(Some(&schemas()), &[], Scope::Columns, "dev.task.na", today());
        assert_eq!(labels(&results), ["[COLUMNS 1]", "sample_dev_sample.task.name"]);
        assert_eq!(underlined(only_item(&results)), ["dev", "task", "na"]);
    }

    /// A statement is prose, not a path: `public.users` is how the SQL
    /// itself reads, so the dot is matched literally.
    #[test]
    fn a_dot_in_a_history_query_is_matched_literally() {
        let results = build(
            None,
            &[run("select * from public.users"), run("select * from public_users")],
            Scope::History,
            "public.users",
            today(),
        );
        assert_eq!(labels(&results), ["[RECENT QUERIES 1]", "select * from public.users"]);
        assert_eq!(underlined(only_item(&results)), ["public.users"]);
    }

    #[test]
    fn columns_stay_out_of_the_way_until_there_is_a_query() {
        let results = build(Some(&catalog()), &[], Scope::All, "", today());
        // Every table, no columns: an empty query is a starting point, not
        // a dump of the schema.
        assert!(labels(&results).iter().all(|label| !label.contains('.')));
        assert!(labels(&results).contains(&"[TABLES · PUBLIC 5]".to_string()));
    }

    #[test]
    fn a_section_that_found_nothing_leaves_no_heading() {
        let results = build(Some(&catalog()), &[], Scope::All, "zzz", today());
        assert!(results.rows.is_empty());
        assert_eq!(results.matches, 0);
    }

    #[test]
    fn a_capped_section_says_how_many_it_left_out() {
        let mut schema = Schema { name: "public".to_string(), tables: Vec::new() };
        for ix in 0..9 {
            schema.tables.push(table(&format!("log_{ix}"), TableKind::Table, Vec::new()));
        }
        let results =
            build(Some(&Catalog { schemas: vec![schema] }), &[], Scope::All, "log", today());
        assert_eq!(labels(&results)[0], "[TABLES · PUBLIC 6 of 9]");
        assert_eq!(results.rows.len(), 1 + SECTION_CAP);
        // The count in the footer is what matched, not what fitted.
        assert_eq!(results.matches, 9);
    }

    /// The sidebar carries the row estimate. In a list of near-identical
    /// names it only makes one row louder than the rest.
    #[test]
    fn a_table_does_not_carry_its_row_count() {
        let results = build(Some(&catalog()), &[], Scope::Tables, "users", today());
        let item = only_item(&results);
        assert_eq!(item.label, "users");
        assert_eq!(item.meta, "");

        // A view still says that it is one: that is what it is, not how
        // big it is.
        let results = build(Some(&catalog()), &[], Scope::Tables, "7d", today());
        assert_eq!(only_item(&results).meta, "view");
    }

    #[test]
    fn a_run_reads_as_one_line() {
        let results = build(
            None,
            &[run("select *\n  from public.users\n  order by id")],
            Scope::History,
            "order by",
            today(),
        );
        let item = only_item(&results);
        assert_eq!(item.label, "select * from public.users order by id");
        assert!(item.trailing.starts_with("today "));
        // Opening a run gives back the statement as it was written, not
        // the single line the palette painted.
        assert_eq!(item.pick, Pick::Query("select *\n  from public.users\n  order by id".into()));
    }

    /// ⇥ walks in one part at a time: schema, then relation, then done.
    #[test]
    fn completing_drills_one_part_at_a_time() {
        let step = |needle: &str| {
            let results = build(Some(&schemas()), &[], Scope::Tables, needle, today());
            let selected =
                results.rows.iter().position(|row| row.pick().is_some()).expect("a result");
            completion(&results.rows, selected, needle)
        };

        // A hit in the middle of a schema still completes to the whole of
        // it, with the dot that leads into the relation.
        assert_eq!(step("dev").as_deref(), Some("sample_dev_sample."));
        assert_eq!(step("sample_dev_sample.").as_deref(), Some("sample_dev_sample.task"));
        // A path that is already whole has nothing left to add, which is
        // what frees ⇥ to go back to walking the chips.
        assert_eq!(step("sample_dev_sample.task"), None);
    }

    #[test]
    fn completing_a_column_reaches_its_own_name() {
        let needle = "task.na";
        let results = build(Some(&schemas()), &[], Scope::Columns, needle, today());
        assert_eq!(completion(&results.rows, 1, needle).as_deref(), Some("task.name"));
    }

    #[test]
    fn there_is_nothing_to_complete_from_a_run_or_an_empty_line() {
        let results = build(None, &[run("select * from users")], Scope::History, "users", today());
        // A statement is prose, not a path.
        assert_eq!(completion(&results.rows, 1, "users"), None);

        // An empty line has a selected row but nothing to finish.
        let results = build(Some(&schemas()), &[], Scope::Tables, "", today());
        assert_eq!(completion(&results.rows, 1, ""), None);

        // A heading is not a result.
        assert_eq!(completion(&results.rows, 0, "task"), None);
    }

    #[test]
    fn a_typed_prefix_picks_the_scope_over_the_chip() {
        assert_eq!(parse("c: user", Scope::Tables), (Scope::Columns, "user"));
        assert_eq!(parse("h:select", Scope::All), (Scope::History, "select"));
        assert_eq!(parse("users", Scope::Tables), (Scope::Tables, "users"));
        assert_eq!(parse("  users", Scope::All), (Scope::All, "users"));
    }

    #[test]
    fn a_long_label_keeps_its_hit_in_view() {
        let long = format!("select {} from users where name = 'meerkat'", "a, ".repeat(30));
        let hits = find(&long, "meerkat").into_iter().collect();
        let (text, hits) = clip(&long, hits, LABEL_CHARS);
        assert!(text.starts_with('…'));
        assert!(text.chars().count() <= LABEL_CHARS + 2);
        let hit = hits.first().expect("the hit is what the window was placed around");
        assert_eq!(&text[hit.clone()], "meerkat");
    }

    #[test]
    fn a_short_label_is_left_alone() {
        let (text, hits) = clip("users", vec![1..4], LABEL_CHARS);
        assert_eq!(text, "users");
        assert_eq!(hits, vec![1..4]);
    }
}
