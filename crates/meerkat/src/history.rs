//! The query history list: every statement this connection has run,
//! newest first, under a day heading.
//!
//! The rows are flattened the way the sidebar's catalog is, headings and
//! runs sharing one height, because `uniform_list` measures once and only
//! lays out what is on screen. A history of 500 runs then costs the same
//! as a history of five.
//!
//! Grouping is by *local* day, not by elapsed seconds: a run at 00:05
//! belongs to today even though it happened minutes ago, and one at 23:55
//! yesterday belongs to yesterday even though it is nearer in time.

use chrono::{Datelike, Local, NaiveDate, TimeZone};
use gpui::{AnyElement, App, ElementId, FontWeight, SharedString, Window, div, prelude::*, px};
use std::rc::Rc;
use storage::QueryRun;
use theme::ThemeColors;
use ui::{format_count, format_millis, section_label};

/// One height for headings and runs alike, so the list can virtualize.
/// A run card is 38px; the remaining 8px is the gap under it.
pub const HISTORY_ROW_HEIGHT: f32 = 46.;
const CARD_HEIGHT: f32 = 38.;

/// Column widths from the comp: statement, rows, timing, clock.
const ROWS_WIDTH: f32 = 88.;
const TIMING_WIDTH: f32 = 78.;
const CLOCK_WIDTH: f32 = 92.;

/// Past this a run reads as slow and takes the deep accent, as the comp
/// paints its 1.9 s row.
const SLOW_MILLIS: u64 = 1_000;

/// Called with the statement of the run the user clicked.
pub type OnOpenRun = Rc<dyn Fn(String, &mut Window, &mut App)>;

/// The list, flattened: a heading per day, then that day's runs.
pub enum HistoryRow {
    Day { label: SharedString },
    Run(RunRow),
}

/// One run, already rendered down to the four strings the card shows.
pub struct RunRow {
    pub statement: String,
    /// What the second column says: `6 rows`, or the reason it failed.
    detail: SharedString,
    /// What the third column says: `128 ms`, or `error`.
    timing: SharedString,
    /// Local clock time, `09:41`.
    clock: SharedString,
    failed: bool,
    slow: bool,
}

/// Group the runs by the local day they ran on. The order the store
/// returned them in is kept: newest first, within the day as well.
pub fn flatten(runs: &[QueryRun], today: NaiveDate) -> Vec<HistoryRow> {
    let mut rows = Vec::with_capacity(runs.len() + 4);
    let mut current: Option<NaiveDate> = None;
    for run in runs {
        let Some(at) = Local.timestamp_opt(run.ran_at, 0).single() else {
            continue;
        };
        let day = at.date_naive();
        if current != Some(day) {
            rows.push(HistoryRow::Day {
                label: day_label(day, today).into(),
            });
            current = Some(day);
        }
        rows.push(HistoryRow::Run(RunRow {
            statement: run.statement.clone(),
            detail: match (&run.error, run.affected, run.row_count) {
                (Some(error), _, _) => first_line(error).into(),
                // Rows changed win the line over rows returned. A statement
                // that changed rows returned none, so the count is the only
                // thing about it worth a column — and `0 rows` said nothing
                // at all about what it did.
                (None, Some(count), _) => changed_label(count).into(),
                (None, None, Some(count)) => rows_label(count).into(),
                (None, None, None) => SharedString::default(),
            },
            timing: match (&run.error, run.elapsed_ms) {
                (Some(_), _) => "error".into(),
                (None, Some(millis)) => format_millis(millis as u128).into(),
                (None, None) => SharedString::default(),
            },
            clock: at.format("%H:%M").to_string().into(),
            failed: run.error.is_some(),
            slow: run.elapsed_ms.is_some_and(|millis| millis >= SLOW_MILLIS),
        }));
    }
    rows
}

/// `TODAY`, `YESTERDAY`, then the date itself. The near days are named
/// because that is how anyone looking for the query they just ran reads
/// the list.
fn day_label(day: NaiveDate, today: NaiveDate) -> String {
    match (today - day).num_days() {
        0 => "TODAY".to_string(),
        1 => "YESTERDAY".to_string(),
        _ => format!(
            "{} {} {}",
            day.format("%a").to_string().to_uppercase(),
            day.day(),
            day.format("%b").to_string().to_uppercase()
        ),
    }
}

/// The row count as the comp writes it: `1 row`, `6 rows`, `18.4k rows`.
fn rows_label(count: u64) -> String {
    match count {
        1 => "1 row".to_string(),
        n => format!("{} rows", format_count(n)),
    }
}

/// Rows changed, said so: `1 row changed`, `6 rows changed`. The word is
/// there because the column otherwise reads as rows returned, and this run
/// returned none.
fn changed_label(count: u64) -> String {
    match count {
        0 => "no rows changed".to_string(),
        1 => "1 row changed".to_string(),
        n => format!("{} rows changed", format_count(n)),
    }
}

/// A driver error runs to several lines; the card has one narrow column.
fn first_line(error: &str) -> String {
    error.lines().next().unwrap_or_default().trim().to_string()
}

/// Today, in the local zone. The screen reads it once per load, so the
/// headings do not shift while the list is on screen.
pub fn today() -> NaiveDate {
    Local::now().date_naive()
}

/// One row of the flattened list. Free-standing for the same reason the
/// catalog's row is: the list's render closure outlives the borrow of the
/// view it came from.
pub fn history_row(
    ix: usize,
    row: &HistoryRow,
    open: &OnOpenRun,
    colors: &ThemeColors,
    cx: &App,
) -> AnyElement {
    match row {
        HistoryRow::Day { label } => div()
            .h(px(HISTORY_ROW_HEIGHT))
            .flex()
            .items_end()
            .pb(px(9.))
            .child(section_label(label.clone(), cx))
            .into_any_element(),
        HistoryRow::Run(run) => {
            let open = open.clone();
            let statement = run.statement.clone();
            let cell = |text: SharedString, color| {
                div()
                    .text_size(px(10.))
                    .text_color(color)
                    .truncate()
                    .child(text)
            };

            div()
                .h(px(HISTORY_ROW_HEIGHT))
                .child(
                    div()
                        .id(ElementId::NamedInteger("history-run".into(), ix as u64))
                        .h(px(CARD_HEIGHT))
                        .flex()
                        .items_center()
                        .gap(px(14.))
                        .px(px(12.))
                        .border_1()
                        .border_color(if run.failed {
                            colors.error_border
                        } else {
                            colors.border
                        })
                        .rounded(px(7.))
                        .bg(if run.failed {
                            colors.error_surface
                        } else {
                            colors.elevated
                        })
                        .cursor_pointer()
                        .hover(move |s| {
                            s.border_color(if run.failed {
                                colors.error_faint
                            } else {
                                colors.text_faint
                            })
                        })
                        .on_click(move |_event, window, cx| open(statement.clone(), window, cx))
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(0.))
                                .text_size(px(11.))
                                .text_color(if run.failed {
                                    colors.error
                                } else {
                                    colors.text_body
                                })
                                .truncate()
                                .child(run.statement.clone()),
                        )
                        .child(div().w(px(ROWS_WIDTH)).flex_none().child(cell(
                            run.detail.clone(),
                            if run.failed {
                                colors.error_secondary
                            } else {
                                colors.text_muted
                            },
                        )))
                        .child(div().w(px(TIMING_WIDTH)).flex_none().child(cell(
                            run.timing.clone(),
                            match (run.failed, run.slow) {
                                (true, _) => colors.error_secondary,
                                (false, true) => colors.accent_deep,
                                (false, false) => colors.ok,
                            },
                        )))
                        .child(
                            div()
                                .w(px(CLOCK_WIDTH))
                                .flex_none()
                                .flex()
                                .justify_end()
                                .child(cell(
                                    run.clock.clone(),
                                    if run.failed {
                                        colors.error_faint
                                    } else {
                                        colors.text_faint
                                    },
                                )),
                        ),
                )
                .into_any_element()
        }
    }
}

/// The chips over the list. Active is the accent; inactive is the same
/// bordered pill the toolbars use.
pub fn filter_chip(
    id: &'static str,
    label: &'static str,
    active: bool,
    colors: &ThemeColors,
) -> gpui::Stateful<gpui::Div> {
    div()
        .id(ElementId::Name(id.into()))
        .px(px(9.))
        .py(px(5.))
        .border_1()
        .border_color(if active {
            colors.accent
        } else {
            colors.border_strong
        })
        .rounded(px(6.))
        .bg(if active {
            colors.selection
        } else {
            colors.elevated
        })
        .text_size(px(11.))
        .font_weight(if active {
            FontWeight::MEDIUM
        } else {
            FontWeight::NORMAL
        })
        .text_color(if active {
            colors.accent_deep
        } else {
            colors.text_secondary
        })
        .cursor_pointer()
        .hover(move |s| {
            s.border_color(if active {
                colors.accent_deep
            } else {
                colors.text_faint
            })
        })
        .child(label)
}

#[cfg(test)]
mod tests {
    use super::*;
    use storage::RunSource;

    fn run(statement: &str, ran_at: i64) -> QueryRun {
        QueryRun {
            id: 1,
            statement: statement.to_string(),
            ran_at,
            source: RunSource::User,
            elapsed_ms: Some(128),
            row_count: Some(6),
            affected: None,
            error: None,
        }
    }

    /// Local noon on a day, so a timezone shift cannot move the run into
    /// the day before or after it.
    fn noon(days_ago: i64) -> i64 {
        let day = Local::now().date_naive() - chrono::Duration::days(days_ago);
        Local
            .from_local_datetime(&day.and_hms_opt(12, 0, 0).unwrap())
            .single()
            .unwrap()
            .timestamp()
    }

    #[test]
    fn runs_sit_under_the_day_they_ran_on() {
        let today = Local::now().date_naive();
        let rows = flatten(
            &[
                run("select 1", noon(0)),
                run("select 2", noon(0)),
                run("select 3", noon(1)),
                run("select 4", noon(9)),
            ],
            today,
        );
        let read: Vec<String> = rows
            .iter()
            .map(|row| match row {
                HistoryRow::Day { label } => format!("[{label}]"),
                HistoryRow::Run(run) => run.statement.clone(),
            })
            .collect();
        // One heading per day, however many runs it holds.
        assert_eq!(
            read[..5],
            [
                "[TODAY]".to_string(),
                "select 1".to_string(),
                "select 2".to_string(),
                "[YESTERDAY]".to_string(),
                "select 3".to_string(),
            ]
        );
        assert!(read[5].starts_with('['));
        assert_eq!(read[6], "select 4");
    }

    #[test]
    fn a_failed_run_reads_as_one_line() {
        let mut failed = run("select * from user_setings", noon(0));
        failed.error = Some("relation \"user_setings\" does not exist\nLINE 1: ...".to_string());
        failed.elapsed_ms = None;
        failed.row_count = None;

        let rows = flatten(&[failed], Local::now().date_naive());
        let HistoryRow::Run(row) = &rows[1] else {
            panic!("expected a run")
        };
        assert_eq!(row.detail, "relation \"user_setings\" does not exist");
        assert_eq!(row.timing, "error");
        assert!(row.failed);
    }

    /// A run that changed rows says so, and says it in a word the column
    /// cannot be read the other way round: the column otherwise means rows
    /// returned, and this run returned none.
    #[test]
    fn a_run_that_changed_rows_says_so_rather_than_counting_none() {
        let mut updated = run("update t set a = 1", noon(0));
        updated.row_count = Some(0);
        updated.affected = Some(1);
        let rows = flatten(&[updated], Local::now().date_naive());
        let HistoryRow::Run(row) = &rows[1] else {
            panic!("expected a run")
        };
        assert_eq!(row.detail, "1 row changed");

        assert_eq!(changed_label(0), "no rows changed");
        assert_eq!(changed_label(18_412), "18.4k rows changed");

        // A run that returned rows keeps the count it always had.
        let rows = flatten(&[run("select 1", noon(0))], Local::now().date_naive());
        let HistoryRow::Run(row) = &rows[1] else {
            panic!("expected a run")
        };
        assert_eq!(row.detail, "6 rows");
    }

    #[test]
    fn counts_and_timings_read_as_the_comp_writes_them() {
        assert_eq!(rows_label(1), "1 row");
        assert_eq!(rows_label(200), "200 rows");
        assert_eq!(rows_label(18_412), "18.4k rows");

        let mut slow = run("select 1", noon(0));
        slow.elapsed_ms = Some(1_900);
        let rows = flatten(&[slow], Local::now().date_naive());
        let HistoryRow::Run(row) = &rows[1] else {
            panic!("expected a run")
        };
        assert_eq!(row.timing, "1.9 s");
        assert!(row.slow);
    }
}
