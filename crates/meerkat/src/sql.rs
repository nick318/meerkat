//! SQL the app writes for itself.
//!
//! Only identifiers that came back from introspection are ever spliced in,
//! and they are always quoted. Nothing the user types reaches these
//! builders — a typed query goes to the driver verbatim instead.

use introspect::Table;

/// Rows fetched per page of a table view.
pub const PAGE_SIZE: usize = 500;

/// Quote an identifier for Postgres, doubling any embedded quote.
pub fn quote_ident(name: &str) -> String {
    format!("\"{}\"", name.replace('"', "\"\""))
}

/// `SELECT * FROM "schema"."table" ORDER BY <key> LIMIT n OFFSET n`.
///
/// Ordering keeps paging stable: without it Postgres may hand back the
/// same row on two pages. The primary key is the natural order; a view or
/// a keyless table falls back to its first column, and a table with no
/// columns at all gets no ORDER BY.
pub fn page_query(schema: &str, table: &Table, page: usize) -> String {
    let mut sql = format!(
        "SELECT * FROM {}.{}",
        quote_ident(schema),
        quote_ident(&table.name)
    );
    if let Some(order) = order_by(table) {
        sql.push_str(&format!(" ORDER BY {order}"));
    }
    sql.push_str(&format!(
        " LIMIT {PAGE_SIZE} OFFSET {}",
        page * PAGE_SIZE
    ));
    sql
}

fn order_by(table: &Table) -> Option<String> {
    let columns: Vec<&String> = if table.has_primary_key() {
        table.primary_key.iter().collect()
    } else {
        table.columns.first().map(|c| vec![&c.name]).unwrap_or_default()
    };
    if columns.is_empty() {
        return None;
    }
    Some(
        columns
            .into_iter()
            .map(|name| quote_ident(name))
            .collect::<Vec<_>>()
            .join(", "),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use introspect::{Column, TableKind};

    fn table(name: &str, columns: &[&str], primary_key: &[&str]) -> Table {
        Table {
            name: name.to_string(),
            kind: TableKind::Table,
            columns: columns
                .iter()
                .map(|c| Column {
                    name: c.to_string(),
                    data_type: "text".to_string(),
                    nullable: true,
                    default: None,
                })
                .collect(),
            primary_key: primary_key.iter().map(|c| c.to_string()).collect(),
            approx_rows: None,
        }
    }

    #[test]
    fn embedded_quotes_are_doubled() {
        assert_eq!(quote_ident("users"), "\"users\"");
        assert_eq!(quote_ident("we\"ird"), "\"we\"\"ird\"");
        // The classic injection attempt closes the quote; doubling it
        // leaves one harmless identifier.
        assert_eq!(
            quote_ident("x\"; DROP TABLE users; --"),
            "\"x\"\"; DROP TABLE users; --\""
        );
    }

    #[test]
    fn pages_order_by_the_primary_key() {
        let users = table("users", &["id", "email"], &["id"]);
        assert_eq!(
            page_query("public", &users, 0),
            "SELECT * FROM \"public\".\"users\" ORDER BY \"id\" LIMIT 500 OFFSET 0"
        );
        assert_eq!(
            page_query("public", &users, 3),
            "SELECT * FROM \"public\".\"users\" ORDER BY \"id\" LIMIT 500 OFFSET 1500"
        );
    }

    #[test]
    fn composite_keys_keep_their_order() {
        let items = table("order_items", &["order_id", "line"], &["order_id", "line"]);
        assert!(page_query("shop", &items, 0).contains("ORDER BY \"order_id\", \"line\""));
    }

    #[test]
    fn keyless_relations_fall_back_to_the_first_column() {
        let view = table("mrr_by_month", &["month", "mrr"], &[]);
        assert!(page_query("public", &view, 0).contains("ORDER BY \"month\""));

        let empty = table("nothing", &[], &[]);
        assert_eq!(
            page_query("public", &empty, 0),
            "SELECT * FROM \"public\".\"nothing\" LIMIT 500 OFFSET 0"
        );
    }
}
