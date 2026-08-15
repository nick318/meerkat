//! Root view: breadcrumb bar | sidebar + table pane | status strip.
//! Implements the "table" screen of the Meerkat design comp with sample
//! data; panes get wired to real drivers as the Phase 1 crates land.

use gpui::{Context, Div, FontWeight, Hsla, Window, div, prelude::*, px};
use theme::{FONT_FAMILY, theme};
use ui::{card, section_label, status_dot, table_glyph, toolbar_button};

const SIDEBAR_WIDTH: f32 = 246.;
// ID, EMAIL, NAME, PLAN, MRR, CREATED_AT (LAST_SEEN takes the rest).
const COLUMN_WIDTHS: [f32; 6] = [64., 188., 150., 74., 96., 112.];

const TABLES: &[(&str, &str)] = &[
    ("users", "18.4k"),
    ("accounts", "2.1k"),
    ("orders", "96.7k"),
    ("order_items", "311k"),
    ("payments", "88.2k"),
    ("subscriptions", "4.9k"),
    ("sessions", "1.2m"),
    ("webhooks", "640"),
    ("feature_flags", "37"),
    ("audit_log", "2.4m"),
];

const VIEWS: &[&str] = &["mrr_by_month", "active_users_7d", "churn_risk"];

struct SampleRow {
    id: &'static str,
    email: &'static str,
    name: &'static str,
    plan: &'static str,
    mrr: &'static str,
    created: &'static str,
    last_seen: &'static str,
}

const ROWS: &[SampleRow] = &[
    SampleRow { id: "1041", email: "ida.vos@northwind.io", name: "Ida Vos", plan: "scale", mrr: "1,280.00", created: "2026-08-14", last_seen: "2026-08-15 09:12" },
    SampleRow { id: "1040", email: "m.okafor@lumen.dev", name: "Michael Okafor", plan: "pro", mrr: "420.00", created: "2026-08-13", last_seen: "2026-08-15 08:44" },
    SampleRow { id: "1039", email: "sara.lindqvist@atlas.co", name: "Sara Lindqvist", plan: "pro", mrr: "420.00", created: "2026-08-12", last_seen: "2026-08-14 21:03" },
    SampleRow { id: "1038", email: "petra@brightloom.com", name: "Petra Nowak", plan: "free", mrr: "NULL", created: "2026-08-12", last_seen: "2026-08-12 11:47" },
    SampleRow { id: "1037", email: "t.haddad@quaystreet.org", name: "Tariq Haddad", plan: "scale", mrr: "1,280.00", created: "2026-08-11", last_seen: "2026-08-15 07:20" },
    SampleRow { id: "1036", email: "gwen.oyelaran@fern.app", name: "Gwen Oyelaran", plan: "pro", mrr: "420.00", created: "2026-08-10", last_seen: "2026-08-14 16:58" },
    SampleRow { id: "1035", email: "hello@studiobark.se", name: "NULL", plan: "free", mrr: "NULL", created: "2026-08-09", last_seen: "2026-08-09 09:02" },
    SampleRow { id: "1034", email: "j.mbeki@rivergate.io", name: "Joseph Mbeki", plan: "scale", mrr: "2,140.00", created: "2026-08-08", last_seen: "2026-08-15 10:31" },
    SampleRow { id: "1033", email: "ana.ferreira@nube.mx", name: "Ana Ferreira", plan: "pro", mrr: "420.00", created: "2026-08-07", last_seen: "2026-08-13 13:19" },
    SampleRow { id: "1032", email: "liam.chen@parcelworks.com", name: "Liam Chen", plan: "free", mrr: "NULL", created: "2026-08-06", last_seen: "2026-08-11 18:40" },
    SampleRow { id: "1031", email: "office@haldencraft.no", name: "Nora Halden", plan: "pro", mrr: "380.00", created: "2026-08-05", last_seen: "2026-08-14 08:07" },
    SampleRow { id: "1030", email: "dev@tinyforge.dev", name: "Rui Tavares", plan: "free", mrr: "NULL", created: "2026-08-04", last_seen: "2026-08-04 12:22" },
    SampleRow { id: "1029", email: "k.svensson@bergen.io", name: "Karin Svensson", plan: "scale", mrr: "1,780.00", created: "2026-08-03", last_seen: "2026-08-15 06:55" },
    SampleRow { id: "1028", email: "team@vergedata.ai", name: "Priya Raman", plan: "pro", mrr: "420.00", created: "2026-08-02", last_seen: "2026-08-12 22:14" },
    SampleRow { id: "1027", email: "luca.moretti@ortica.it", name: "Luca Moretti", plan: "free", mrr: "NULL", created: "2026-08-01", last_seen: "2026-08-10 15:36" },
];

pub struct Shell {
    selected_row: usize,
}

impl Shell {
    pub fn new() -> Self {
        Self { selected_row: 0 }
    }
}

impl Render for Shell {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let colors = theme(cx).colors.clone();

        div()
            .flex()
            .flex_col()
            .size_full()
            .bg(colors.window)
            .font_family(FONT_FAMILY)
            .text_color(colors.text_body)
            .child(breadcrumb_bar(cx))
            .child(
                div()
                    .flex()
                    .flex_1()
                    .min_h(px(0.))
                    .child(sidebar(cx))
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .flex_1()
                            .min_w(px(0.))
                            .child(tab_strip(cx))
                            .child(table_toolbar(cx))
                            .child(grid_header(cx))
                            .child(grid_rows(self.selected_row, cx))
                            .child(status_strip(cx)),
                    ),
            )
    }
}

fn breadcrumb_bar(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    let sep = |cx: &gpui::App| {
        div()
            .text_color(theme(cx).colors.text_faint)
            .child("/")
    };
    div()
        .h(px(38.))
        .flex_none()
        .flex()
        .items_center()
        .px(px(12.))
        .gap(px(14.))
        .border_b_1()
        .border_color(colors.border)
        .bg(colors.panel)
        .text_size(px(11.))
        .child(
            div()
                .flex_1()
                .flex()
                .justify_center()
                .items_center()
                .gap(px(7.))
                .text_color(colors.text_muted)
                .child(div().text_color(colors.text_secondary).child("meerkat_prod"))
                .child(sep(cx))
                .child("public")
                .child(sep(cx))
                .child(
                    div()
                        .text_color(colors.text)
                        .font_weight(FontWeight::MEDIUM)
                        .child("users"),
                ),
        )
        .child(key_hint("⌘K", cx))
        .child(key_hint("⌘⏎", cx))
}

fn key_hint(text: &'static str, cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .px(px(6.))
        .py(px(4.))
        .border_1()
        .border_color(colors.border_strong)
        .rounded(px(4.))
        .bg(colors.window)
        .text_size(px(10.))
        .font_weight(FontWeight::MEDIUM)
        .text_color(colors.text_muted)
        .child(text)
}

fn sidebar(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .w(px(SIDEBAR_WIDTH))
        .flex_none()
        .flex()
        .flex_col()
        .border_r_1()
        .border_color(colors.border)
        .bg(colors.panel)
        .child(
            // Connection card
            div().p(px(12.)).border_b_1().border_color(colors.hairline).child(
                card(cx)
                    .flex()
                    .items_center()
                    .gap(px(9.))
                    .px(px(8.))
                    .py(px(7.))
                    .cursor_pointer()
                    .child(
                        div()
                            .flex_1()
                            .flex()
                            .flex_col()
                            .gap(px(2.))
                            .child(
                                div()
                                    .text_size(px(12.))
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_color(colors.text)
                                    .child("meerkat_prod"),
                            )
                            .child(
                                div()
                                    .text_size(px(10.))
                                    .text_color(colors.text_muted)
                                    .child("db.internal · 5432"),
                            ),
                    )
                    .child(status_dot(colors.ok)),
            ),
        )
        .child(
            // Search box
            div().px(px(12.)).py(px(10.)).child(
                card(cx)
                    .flex()
                    .items_center()
                    .gap(px(7.))
                    .px(px(8.))
                    .py(px(6.))
                    .text_size(px(11.))
                    .child(div().flex_1().text_color(colors.text_faint).child("Search tables"))
                    .child(div().text_size(px(10.)).text_color(colors.text_faint).child("⌘K")),
            ),
        )
        .child(
            // Table + view lists
            div()
                .flex_1()
                .min_h(px(0.))
                .overflow_hidden()
                .px(px(8.))
                .pb(px(12.))
                .flex()
                .flex_col()
                .child(list_header("SCHEMA · PUBLIC", "14", cx))
                .children(
                    TABLES
                        .iter()
                        .enumerate()
                        .map(|(i, (name, count))| table_item(name, count, i == 0, cx)),
                )
                .child(list_header("VIEWS", "3", cx))
                .children(VIEWS.iter().map(|name| view_item(name, cx))),
        )
        .child(
            // Sidebar footer
            div()
                .flex_none()
                .px(px(12.))
                .py(px(9.))
                .border_t_1()
                .border_color(colors.hairline)
                .flex()
                .justify_between()
                .text_size(px(10.))
                .text_color(colors.text_muted)
                .child(div().cursor_pointer().hover(|s| s.text_color(colors.accent)).child("query history"))
                .child(div().text_color(colors.text_faint).child("read-only")),
        )
}

fn list_header(label: &'static str, count: &'static str, cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .flex()
        .justify_between()
        .items_center()
        .px(px(6.))
        .pt(px(8.))
        .pb(px(6.))
        .child(section_label(label, cx))
        .child(div().text_size(px(10.)).text_color(colors.text_faint).child(count))
}

fn table_item(name: &'static str, count: &'static str, active: bool, cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    let base = div()
        .flex()
        .items_center()
        .gap(px(8.))
        .px(px(8.))
        .py(px(5.))
        .rounded(px(5.))
        .cursor_pointer()
        .child(table_glyph(active, cx))
        .child(
            div()
                .flex_1()
                .text_size(px(12.))
                .font_weight(if active { FontWeight::MEDIUM } else { FontWeight::NORMAL })
                .text_color(if active { colors.text } else { colors.text_secondary })
                .child(name),
        )
        .child(div().text_size(px(10.)).text_color(colors.text_faint).child(count));
    if active {
        base.bg(colors.selection)
    } else {
        base.hover(move |s| s.bg(colors.hairline))
    }
}

fn view_item(name: &'static str, cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .flex()
        .items_center()
        .gap(px(8.))
        .px(px(8.))
        .py(px(5.))
        .rounded(px(5.))
        .cursor_pointer()
        .hover(move |s| s.bg(colors.hairline))
        .child(
            div()
                .size(px(5.))
                .rounded_full()
                .border_1()
                .border_color(colors.text_faint),
        )
        .child(
            div()
                .flex_1()
                .text_size(px(12.))
                .text_color(colors.text_secondary)
                .child(name),
        )
}

fn tab_strip(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .h(px(34.))
        .flex_none()
        .flex()
        .items_stretch()
        .border_b_1()
        .border_color(colors.border)
        .bg(colors.panel)
        .child(
            // Active tab
            div()
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(14.))
                .border_r_1()
                .border_color(colors.border)
                .bg(colors.window)
                .cursor_pointer()
                .child(table_glyph(true, cx))
                .child(
                    div()
                        .text_size(px(11.))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.text)
                        .child("public.users"),
                )
                .child(div().text_size(px(12.)).text_color(colors.text_faint).child("×")),
        )
        .child(
            // Inactive tab
            div()
                .flex()
                .items_center()
                .gap(px(8.))
                .px(px(14.))
                .border_r_1()
                .border_color(colors.border)
                .cursor_pointer()
                .hover(|s| s.bg(theme(cx).colors.hairline))
                .child(div().size(px(5.)).rounded_full().bg(colors.text_faint))
                .child(div().text_size(px(11.)).text_color(colors.text_muted).child("churn_query.sql"))
                .child(div().text_size(px(12.)).text_color(colors.text_faint).child("×")),
        )
        .child(div().flex_1())
        .child(
            div()
                .flex()
                .items_center()
                .px(px(12.))
                .text_size(px(11.))
                .text_color(colors.accent)
                .cursor_pointer()
                .child("+ new query"),
        )
}

fn table_toolbar(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    div()
        .flex_none()
        .flex()
        .items_center()
        .gap(px(10.))
        .px(px(14.))
        .py(px(9.))
        .border_b_1()
        .border_color(colors.hairline)
        .child(
            div()
                .text_size(px(12.))
                .font_weight(FontWeight::MEDIUM)
                .text_color(colors.text)
                .child("users"),
        )
        .child(
            div()
                .text_size(px(11.))
                .text_color(colors.text_muted)
                .child("18,412 rows · 11 columns"),
        )
        .child(div().flex_1())
        .child(toolbar_button("filter", cx))
        .child(toolbar_button("sort · created_at ↓", cx))
        .child(toolbar_button("export", cx))
}

fn grid_header(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    let header_cell = |i: usize, label: &'static str, sorted: bool| {
        let mut cell = div()
            .px(px(12.))
            .overflow_hidden()
            .flex()
            .items_center()
            .gap(px(4.))
            .child(label);
        cell = match COLUMN_WIDTHS.get(i) {
            Some(w) => cell.w(px(*w)).flex_none(),
            None => cell.flex_1(),
        };
        if i > 0 {
            cell = cell.border_l_1().border_color(colors.hairline).h_full();
        }
        if sorted {
            cell = cell.child(div().text_color(colors.accent).child("↓"));
        }
        cell
    };
    div()
        .h(px(30.))
        .flex_none()
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.border_strong)
        .bg(colors.panel)
        .text_size(px(10.))
        .font_weight(FontWeight::SEMIBOLD)
        .text_color(colors.text_muted)
        .child(header_cell(0, "ID", false))
        .child(header_cell(1, "EMAIL", false))
        .child(header_cell(2, "NAME", false))
        .child(header_cell(3, "PLAN", false))
        .child(header_cell(4, "MRR", false))
        .child(header_cell(5, "CREATED_AT", true))
        .child(header_cell(6, "LAST_SEEN", false))
}

fn grid_rows(selected: usize, cx: &gpui::App) -> Div {
    // TODO: replace with uniform_list virtualization once the results_grid
    // crate streams real rows.
    div()
        .flex_1()
        .min_h(px(0.))
        .overflow_hidden()
        .flex()
        .flex_col()
        .text_size(px(12.))
        .children(ROWS.iter().enumerate().map(|(i, row)| grid_row(row, i == selected, cx)))
}

fn grid_row(row: &SampleRow, selected: bool, cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    let value_color = |value: &str, muted: Hsla| {
        if value == "NULL" { colors.text_faint } else { muted }
    };
    let plan_color = match row.plan {
        "scale" => colors.accent_deep,
        "pro" => colors.text_secondary,
        _ => colors.text_muted,
    };
    let cell = |i: usize, text: &'static str, color: Hsla| {
        let mut cell = div()
            .px(px(12.))
            .overflow_hidden()
            .text_color(color)
            .child(text);
        cell = match COLUMN_WIDTHS.get(i) {
            Some(w) => cell.w(px(*w)).flex_none(),
            None => cell.flex_1(),
        };
        cell
    };

    let base = div()
        .h(px(28.))
        .flex_none()
        .flex()
        .items_center()
        .border_b_1()
        .border_color(colors.hairline)
        .cursor_pointer()
        .child(cell(0, row.id, colors.text_faint))
        .child(cell(1, row.email, value_color(row.email, colors.text_body)))
        .child(cell(2, row.name, value_color(row.name, colors.text_body)))
        .child(cell(3, row.plan, plan_color))
        .child(cell(4, row.mrr, value_color(row.mrr, colors.text_body)))
        .child(cell(5, row.created, colors.text_muted))
        .child(cell(6, row.last_seen, colors.text_muted));

    if selected {
        base.bg(colors.selection)
    } else {
        base.hover(move |s| s.bg(colors.panel))
    }
}

fn status_strip(cx: &gpui::App) -> Div {
    let colors = theme(cx).colors.clone();
    let divider = |cx: &gpui::App| div().text_color(theme(cx).colors.text_faint).child("|");
    let link = |text: &'static str, cx: &gpui::App| {
        let accent = theme(cx).colors.accent;
        div().cursor_pointer().hover(move |s| s.text_color(accent)).child(text)
    };
    div()
        .h(px(30.))
        .flex_none()
        .flex()
        .items_center()
        .gap(px(12.))
        .px(px(14.))
        .border_t_1()
        .border_color(colors.border_strong)
        .bg(colors.panel)
        .text_size(px(10.))
        .text_color(colors.text_muted)
        .child("rows 1–15 of 18,412")
        .child(divider(cx))
        .child(link("prev", cx))
        .child(link("next", cx))
        .child(div().flex_1())
        .child("queried in 34 ms")
        .child(divider(cx))
        .child("on lookout")
        .child(status_dot(colors.ok))
}
