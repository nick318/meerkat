//! What the editor can offer to finish the word under the caret: the
//! names in the connected database, and SQL's own keywords.
//!
//! Everything here is pure text work over the buffer, so it is all
//! testable without a window.

use crate::highlight::KEYWORDS;
use std::collections::HashSet;
use std::ops::Range;

/// How many candidates the panel shows. The design's panel holds a
/// handful; a longer list is a menu, not a hint.
pub const MAX_CANDIDATES: usize = 8;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Kind {
    Relation,
    Column,
    Schema,
    Keyword,
}

/// One name the editor knows about.
#[derive(Debug, Clone)]
pub struct Name {
    pub name: String,
    /// Shown greyed to the right of the name: `table`, `int8`, `schema`.
    pub detail: String,
    pub kind: Kind,
    /// The relation a column belongs to, or the schema a relation belongs
    /// to. This is what lets `users.` offer only that table's columns.
    pub owner: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Completion {
    pub label: String,
    pub detail: String,
    pub kind: Kind,
}

/// The names in the connected database, folded to lower case for lookup
/// because Postgres folds unquoted identifiers the same way.
#[derive(Debug, Default)]
pub struct Vocabulary {
    entries: Vec<Entry>,
    lowered: HashSet<String>,
}

#[derive(Debug)]
struct Entry {
    name: String,
    lowered: String,
    detail: String,
    kind: Kind,
    owner: Option<String>,
}

impl Vocabulary {
    pub fn new(names: impl IntoIterator<Item = Name>) -> Self {
        let entries: Vec<Entry> = names
            .into_iter()
            .map(|name| Entry {
                lowered: name.name.to_ascii_lowercase(),
                owner: name.owner.map(|owner| owner.to_ascii_lowercase()),
                name: name.name,
                detail: name.detail,
                kind: name.kind,
            })
            .collect();
        let lowered = entries.iter().map(|entry| entry.lowered.clone()).collect();
        Self { entries, lowered }
    }

    /// Does the database have this name? Used by the tokenizer to colour
    /// real names differently from typos.
    pub fn contains(&self, word: &str) -> bool {
        // A one-character name would light up half the buffer for no
        // information; `id` and longer is where this starts to help.
        word.len() > 1 && self.lowered.contains(&word.to_ascii_lowercase())
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Candidates for `prefix`, restricted to `qualifier`'s children when
    /// the caret sits after `something.`.
    ///
    /// An empty prefix is allowed: that is what `users.` should offer.
    pub fn candidates(&self, prefix: &str, qualifier: Option<&str>) -> Vec<Completion> {
        let prefix = prefix.to_ascii_lowercase();
        let owner = qualifier.map(|name| name.to_ascii_lowercase());

        // A qualifier the database does not know is a typo, not a filter:
        // fall back to everything rather than showing an empty panel.
        let qualified = owner
            .as_ref()
            .is_some_and(|owner| self.entries.iter().any(|e| e.owner.as_ref() == Some(owner)));

        let mut candidates: Vec<Completion> = self
            .entries
            .iter()
            .filter(|entry| !qualified || entry.owner == owner)
            .filter(|entry| entry.lowered.starts_with(&prefix))
            .map(|entry| Completion {
                label: entry.name.clone(),
                detail: entry.detail.clone(),
                kind: entry.kind,
            })
            .collect();

        // Keywords are not owned by anything, so a qualified caret never
        // wants them, and neither does a caret with nothing typed yet.
        if !qualified && !prefix.is_empty() {
            candidates.extend(
                KEYWORDS
                    .iter()
                    .filter(|keyword| keyword.starts_with(&prefix))
                    .map(|keyword| Completion {
                        label: keyword.to_string(),
                        detail: "keyword".to_string(),
                        kind: Kind::Keyword,
                    }),
            );
        }

        // Shortest first: the closest match to what was typed. Ties go to
        // database names before keywords, then alphabetically, so the
        // order never depends on how the catalog happened to be walked.
        candidates.sort_by(|a, b| {
            a.label
                .len()
                .cmp(&b.label.len())
                .then(a.kind.cmp(&b.kind))
                .then(a.label.cmp(&b.label))
        });
        candidates.dedup_by(|a, b| a.label == b.label && a.kind == b.kind);
        candidates.truncate(MAX_CANDIDATES);
        candidates
    }
}

/// The word being typed at `offset`: the run of identifier characters
/// ending there. Empty when the caret is not on a word.
pub fn prefix_range(text: &str, offset: usize) -> Range<usize> {
    let mut start = offset;
    while let Some((ix, ch)) = text[..start].char_indices().next_back() {
        if !crate::motion::is_word_char(ch) {
            break;
        }
        start = ix;
    }
    start..offset
}

/// The name qualifying the word that starts at `prefix_start`, when the
/// caret sits after `schema.` or `table.`.
pub fn qualifier_range(text: &str, prefix_start: usize) -> Option<Range<usize>> {
    let before = text[..prefix_start].strip_suffix('.')?;
    let range = prefix_range(text, before.len());
    (!range.is_empty()).then_some(range)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn vocabulary() -> Vocabulary {
        Vocabulary::new([
            Name {
                name: "public".into(),
                detail: "schema".into(),
                kind: Kind::Schema,
                owner: None,
            },
            Name {
                name: "users".into(),
                detail: "table".into(),
                kind: Kind::Relation,
                owner: Some("public".into()),
            },
            Name {
                name: "sessions".into(),
                detail: "table".into(),
                kind: Kind::Relation,
                owner: Some("public".into()),
            },
            Name {
                name: "email".into(),
                detail: "text".into(),
                kind: Kind::Column,
                owner: Some("users".into()),
            },
            Name {
                name: "seen_on".into(),
                detail: "date".into(),
                kind: Kind::Column,
                owner: Some("users".into()),
            },
        ])
    }

    fn labels(completions: &[Completion]) -> Vec<&str> {
        completions.iter().map(|c| c.label.as_str()).collect()
    }

    #[test]
    fn the_prefix_is_the_word_ending_at_the_caret() {
        let text = "select ema";
        assert_eq!(prefix_range(text, text.len()), 7..10);
        // Caret on a space completes nothing.
        assert_eq!(prefix_range("select ", 7), 7..7);
        // Mid-word, only what is behind the caret counts.
        assert_eq!(prefix_range("select email", 10), 7..10);
    }

    #[test]
    fn a_dot_qualifies_the_word_after_it() {
        let text = "from public.us";
        let prefix = prefix_range(text, text.len());
        assert_eq!(&text[prefix.clone()], "us");
        let qualifier = qualifier_range(text, prefix.start).unwrap();
        assert_eq!(&text[qualifier], "public");

        // Nothing typed yet after the dot still qualifies.
        let text = "from public.";
        let prefix = prefix_range(text, text.len());
        assert!(prefix.is_empty());
        assert_eq!(&text[qualifier_range(text, prefix.start).unwrap()], "public");

        // A bare word is not qualified.
        assert!(qualifier_range("from users", 5).is_none());
    }

    #[test]
    fn a_qualifier_narrows_to_its_children() {
        let vocabulary = vocabulary();
        // A table offers its columns, and nothing else.
        assert_eq!(labels(&vocabulary.candidates("", Some("users"))), vec!["email", "seen_on"]);
        // A schema offers its relations.
        assert_eq!(labels(&vocabulary.candidates("", Some("public"))), vec!["users", "sessions"]);
        // A keyword never appears behind a dot.
        assert!(
            vocabulary
                .candidates("se", Some("users"))
                .iter()
                .all(|c| c.kind != Kind::Keyword)
        );
    }

    #[test]
    fn an_unknown_qualifier_falls_back_to_everything() {
        // `userz.` is a typo; an empty panel would just look broken.
        let candidates = vocabulary().candidates("em", Some("userz"));
        assert_eq!(labels(&candidates), vec!["email"]);
    }

    #[test]
    fn keywords_and_names_share_the_unqualified_list() {
        let candidates = vocabulary().candidates("se", None);
        let labels = labels(&candidates);
        assert!(labels.contains(&"select"), "{labels:?}");
        assert!(labels.contains(&"sessions"), "{labels:?}");
        assert!(labels.contains(&"seen_on"), "{labels:?}");
        // Shortest first.
        assert_eq!(labels[0], "set");
    }

    #[test]
    fn matching_ignores_case() {
        assert_eq!(labels(&vocabulary().candidates("EMA", Some("USERS"))), vec!["email"]);
    }

    #[test]
    fn the_list_stays_short() {
        let candidates = vocabulary().candidates("", None);
        assert!(candidates.len() <= MAX_CANDIDATES);
        // Nothing typed and nothing qualifying: names only, no keywords.
        assert!(candidates.iter().all(|c| c.kind != Kind::Keyword));
    }

    #[test]
    fn nothing_matches_a_word_the_database_does_not_have() {
        assert!(vocabulary().candidates("zzz", None).is_empty());
    }
}
