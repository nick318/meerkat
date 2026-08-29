//! Matching a name against what the user has typed so far.
//!
//! Three places in the app ask the same question — the ⌘J column find, the
//! ⌘K palette with the sidebar filter behind it, and the SQL editor's
//! completion panel — and before this crate each answered it differently: a
//! substring, a substring per path part, a prefix. So `mast_cl` found
//! `master_client_reference` in none of them, and the same name was found
//! three ways depending on which line the user was typing into.
//!
//! ## The rule
//!
//! A database name is a path of words with the separators left in —
//! `master_client_reference`, `LAST_SEEN`, `masterClientReference` — and a
//! user who types part of one is nearly always typing the **starts of its
//! words**. That is the whole idea IntelliJ's `MinusculeMatcher` is built
//! on, and it is what this crate copies:
//!
//! 1. The query is cut into **fragments** at everything that is not a
//!    letter or a digit, and the separators themselves are dropped. So
//!    `mast_cl`, `mast cl` and `mast-cl` all ask the same question, and
//!    `mast_cl` finds `masterClientReference` as well — a case change is a
//!    word boundary too, so the typed `_` has something to line up with.
//! 2. The **first** fragment may sit anywhere inside the name, because
//!    `client` is a fair way to ask for `master_client_reference`.
//! 3. Every fragment after it must **begin a word** of the name.
//! 4. Inside a fragment a character either follows the one before it or
//!    begins a word. That is what lets `mcr` find
//!    `master_client_reference` by its initials.
//!
//! Rule 4 is the one that keeps this from turning into a fuzzy finder. An
//! unrestricted subsequence — fzf's rule — answers a three-letter query
//! with half of a hundred-column result, and a jump list that long is not
//! a jump. Every character either continues a word or starts one, so
//! nothing matches by accident in the middle of a name.
//!
//! ## Ranking
//!
//! A gate says which names match; it does not say which one the user meant.
//! The score is [fzf's], because the shape of it is well tested: a match is
//! worth [`SCORE_MATCH`], a word start is worth more, a character
//! immediately after the last one is worth more again, and a skip costs —
//! [`GAP_START`] for opening the gap and [`GAP_EXTEND`] per character after
//! it, so one long jump is cheaper than three short ones and a tight match
//! always beats a scattered one. The first character's word-start bonus is
//! doubled, because where a query begins says most about what it meant, and
//! the name's own first character is worth a shade more than an inner word
//! start — which is what puts `master_state_type_code` above
//! `invoice_master_name` for `mast`.
//!
//! The alignment is worked out with a dynamic program rather than greedily,
//! because the greedy answer is wrong often enough to notice: `cl` in
//! `client_cluster` should land on whichever of the two scores better, not
//! on the first one found.
//!
//! Callers break ties themselves, and should break them on the length of
//! the name: two names matched the same way, and the shorter one is more of
//! what the user typed.
//!
//! ## Case
//!
//! Case is ignored until the user shows they mean it. A query with both an
//! upper-case and a lower-case letter is read as **strict**: `Cl` then asks
//! for a capital `C` or a word starting with one, which is how a camel-case
//! name is picked out of a set that also holds a lower-case one. A query
//! typed all in one case asks nothing about case at all, so `EMAIL` still
//! finds `email` — a name the database spells in capitals is found by a
//! name typed in lower case, and the other way round.
//!
//! Every range this crate hands back is a byte range on character
//! boundaries of the string that was passed in, because the match is
//! walked over `char_indices` rather than over bytes. The palette paints
//! those ranges into the label it builds, so a range that fell inside a
//! character would panic on the slice.
//!
//! [fzf's]: https://github.com/junegunn/fzf/blob/master/src/algo/algo.go

use std::ops::Range;

/// What one character of the query is worth when it lands.
pub const SCORE_MATCH: i32 = 16;
/// Opening a gap: the query skipped over part of the name.
///
/// It costs more than the word start it buys ([`BONUS_BOUNDARY`]) plus the
/// tight step it gave up ([`BONUS_CONSECUTIVE`]), and that is a rule rather
/// than a number picked by feel: **a name matched in one piece always beats
/// a name matched in two**. Otherwise `users` would rank
/// `user_settings` — four characters and then a jump onto a word start —
/// above `users` itself, which is the name the user typed.
pub const GAP_START: i32 = -8;
/// Each further character of that gap.
pub const GAP_EXTEND: i32 = -1;
/// A character that starts a word: one after a separator.
pub const BONUS_BOUNDARY: i32 = 8;
/// The name's own first character. Worth a shade more than an inner word
/// start, which is what sorts `master_state_type_code` above
/// `invoice_master_name` for `mast`: both matched a whole word from its
/// start, and only one of them is that word.
const BONUS_NAME_START: i32 = 10;
/// A word start marked by a case change or a letter/digit change rather
/// than by a separator. Worth a shade less, so `_c` outranks `C`.
const BONUS_CAMEL: i32 = 7;
/// A character that follows the one the query matched before it.
pub const BONUS_CONSECUTIVE: i32 = 4;
/// The first character carries its word-start bonus twice: a name the query
/// starts is a better answer than one it hits in the middle.
const FIRST_CHAR_MULTIPLIER: i32 = 2;
/// The query and the name agree on the case of this character.
const BONUS_CASE: i32 = 2;

/// Well below any real score, and far enough from `i32::MIN` that the gap
/// penalties can be subtracted from it without wrapping.
const NONE: i32 = i32::MIN / 2;

/// Where a query landed in a name, and how well.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Match {
    pub score: i32,
    /// The matched runs, in order and never touching: two ranges next to
    /// each other are one range. Empty when the query is empty.
    pub ranges: Vec<Range<usize>>,
}

/// A query, cut into fragments once so a long list of names is not charged
/// for it per name.
#[derive(Debug, Clone)]
pub struct Pattern {
    chars: Vec<Typed>,
    /// Whether an upper-case character in the query is a demand. See the
    /// module docs: only a query written in both cases means it.
    strict_case: bool,
}

#[derive(Debug, Clone)]
struct Typed {
    folded: char,
    typed: char,
    /// This character opens a fragment, so it may only land on the start of
    /// a word.
    opens: bool,
}

impl Pattern {
    pub fn new(query: &str) -> Self {
        let strict_case =
            query.chars().any(char::is_uppercase) && query.chars().any(char::is_lowercase);
        let mut chars = Vec::new();
        let mut opens = true;
        for typed in query.chars() {
            // Anything that is not part of a word ends one. The separator
            // itself is dropped: what it says is "a word starts after me",
            // and the name may say that with a `_` or with a capital.
            if !typed.is_alphanumeric() {
                opens = true;
                continue;
            }
            chars.push(Typed {
                folded: fold(typed),
                typed,
                opens,
            });
            opens = false;
        }
        Self { chars, strict_case }
    }

    /// An empty query matches everything, so a search line that has just
    /// opened lists what it is searching rather than nothing.
    pub fn is_empty(&self) -> bool {
        self.chars.is_empty()
    }

    /// How well `name` answers this query, or `None` when it does not.
    pub fn score(&self, name: &str) -> Option<Match> {
        if self.is_empty() {
            return Some(Match {
                score: 0,
                ranges: Vec::new(),
            });
        }
        // A cheap ordered-subsequence scan first. It allocates nothing and
        // throws out most of a large vocabulary, which the table below
        // cannot do without building itself first.
        if !self.could_match(name) {
            return None;
        }
        self.align(&Name::new(name))
    }

    pub fn matches(&self, name: &str) -> bool {
        self.score(name).is_some()
    }

    /// Are the query's characters even present, in order? A necessary
    /// condition for the real match and a much cheaper one.
    fn could_match(&self, name: &str) -> bool {
        let mut wanted = self.chars.iter();
        let mut next = wanted.next();
        for ch in name.chars() {
            let Some(typed) = next else { return true };
            if typed.folded == fold(ch) {
                next = wanted.next();
            }
        }
        next.is_none()
    }

    /// The best legal alignment of the query on the name.
    ///
    /// `table[j * n + i]` is the best alignment of the query's first `j + 1`
    /// characters that puts character `j` on the name's character `i`:
    /// [`Cell::score`] is what it is worth, `NONE` where no such alignment
    /// is legal, and [`Cell::from`] is where the character before it sat, so
    /// the ranges can be walked back out.
    fn align(&self, name: &Name) -> Option<Match> {
        let (n, m) = (name.chars.len(), self.chars.len());
        if m > n {
            return None;
        }
        let mut table = vec![
            Cell {
                score: NONE,
                from: 0
            };
            n * m
        ];

        // The first character of the query may land anywhere.
        for i in 0..n {
            if let Some(points) = self.lands(0, name, i) {
                table[i].score = points;
            }
        }

        for j in 1..m {
            let opens = self.chars[j].opens;
            let (row, above) = (j * n, (j - 1) * n);
            // The best score of a jump into `i`, with the gap already paid
            // for: every place the character before could have sat, bar the
            // one right behind, which is the tight alignment below.
            let mut jump = NONE;
            let mut jump_from = 0;
            for i in 1..n {
                if let Some(points) = self.lands(j, name, i) {
                    // Tight: the character before it took the character
                    // before it. A fragment's first character may only do
                    // this where the name itself starts a word — a typed
                    // `_` has to line up with something.
                    let tight = table[above + i - 1].score;
                    if tight > NONE && (!opens || name.chars[i].bonus > 0) {
                        table[row + i] = Cell {
                            score: tight + points + BONUS_CONSECUTIVE,
                            from: i - 1,
                        };
                    }
                    // Or the query skipped ahead, which it may only do onto
                    // the start of a word. This is the whole of the gate.
                    if jump > NONE
                        && name.chars[i].bonus > 0
                        && jump + points > table[row + i].score
                    {
                        table[row + i] = Cell {
                            score: jump + points,
                            from: jump_from,
                        };
                    }
                }
                // Roll the running jump forward one place, for the next
                // `i`: either the gap it already holds grows by one, or the
                // cell that has just fallen two behind opens a new one.
                let opened = match table[above + i - 1].score {
                    reachable if reachable > NONE => reachable + GAP_START,
                    _ => NONE,
                };
                let widened = if jump > NONE { jump + GAP_EXTEND } else { NONE };
                if opened >= widened {
                    jump = opened;
                    jump_from = i - 1;
                } else {
                    jump = widened;
                }
            }
        }

        // Where the last character of the query ended up. On a tie the
        // earliest place wins, so an equally good match nearer the front of
        // the name is the one reported.
        let last = (m - 1) * n;
        let (end, score) = (0..n)
            .map(|i| (i, table[last + i].score))
            .max_by_key(|&(i, score)| (score, std::cmp::Reverse(i)))?;
        if score <= NONE {
            return None;
        }

        let mut at = end;
        let mut places = Vec::with_capacity(m);
        for j in (0..m).rev() {
            places.push(at);
            if j > 0 {
                at = table[j * n + at].from;
            }
        }
        places.reverse();
        Some(Match {
            score,
            ranges: name.runs(&places, &self.chars),
        })
    }

    /// What the query's character `j` is worth on the name's character `i`,
    /// or `None` when it cannot sit there at all.
    fn lands(&self, j: usize, name: &Name, i: usize) -> Option<i32> {
        let typed = &self.chars[j];
        let glyph = &name.chars[i];
        if glyph.folded != typed.folded {
            return None;
        }
        // A capital the user meant asks for a capital, or for a word start:
        // `Cl` finds `master_client_reference` and `masterClient`, and
        // leaves `include` alone.
        if self.strict_case
            && typed.typed.is_uppercase()
            && !(glyph.ch.is_uppercase() || glyph.bonus > 0)
        {
            return None;
        }
        let weight = if j == 0 { FIRST_CHAR_MULTIPLIER } else { 1 };
        let mut points = SCORE_MATCH + glyph.bonus * weight;
        if glyph.ch == typed.typed {
            points += BONUS_CASE;
        }
        Some(points)
    }
}

/// One square of the table [`Pattern::align`] fills in.
#[derive(Clone, Copy)]
struct Cell {
    score: i32,
    /// Where the character before this one sat. Only meaningful on a cell
    /// whose score is not `NONE`, which is the only kind the walk back
    /// visits.
    from: usize,
}

/// How well `name` answers `query`. Use [`Pattern`] instead when the same
/// query is asked of many names.
pub fn score(name: &str, query: &str) -> Option<Match> {
    Pattern::new(query).score(name)
}

/// Does `name` answer `query`?
pub fn matches(name: &str, query: &str) -> bool {
    Pattern::new(query).matches(name)
}

/// A name laid out for matching.
///
/// One vector rather than one per field, and the table in [`Pattern::align`]
/// is one vector too. That is not a micro-optimisation to shrug at: a
/// completion vocabulary is every column of every table, so a database of
/// 3,000 relations is tens of thousands of names, matched again on every
/// keystroke. What the matching costs there is mostly what it allocates.
struct Name {
    chars: Vec<Glyph>,
}

/// One character of a name: as it is written, folded for comparison, where
/// it sits in the original string, and what a match on it is worth.
struct Glyph {
    ch: char,
    folded: char,
    at: Range<usize>,
    /// The word-start bonus, and `0` for a character that starts no word.
    /// A positive value is what the gate reads as "a word starts here".
    bonus: i32,
}

impl Name {
    fn new(text: &str) -> Self {
        let mut chars = Vec::with_capacity(text.len());
        let mut before = None;
        for (at, ch) in text.char_indices() {
            chars.push(Glyph {
                ch,
                folded: fold(ch),
                at: at..at + ch.len_utf8(),
                bonus: bonus(before, ch),
            });
            before = Some(ch);
        }
        Name { chars }
    }

    /// The matched places as byte ranges, with anything adjacent joined:
    /// the palette underlines these, and two ranges that touch would draw
    /// as one anyway.
    ///
    /// A separator the query typed and the name has is joined over as well.
    /// The query said "a word ends here" and the name's `_` is where it
    /// ends, so it is part of what matched: `sample_dev_sample` typed whole
    /// underlines whole, rather than in three pieces with the bars between
    /// them left plain.
    fn runs(&self, places: &[usize], typed: &[Typed]) -> Vec<Range<usize>> {
        let mut runs: Vec<Range<usize>> = Vec::new();
        for (which, &place) in places.iter().enumerate() {
            let at = self.chars[place].at.clone();
            let joins = match runs.last() {
                Some(run) if run.end == at.start => true,
                Some(_) if typed[which].opens => (places[which - 1] + 1..place)
                    .all(|between| !self.chars[between].ch.is_alphanumeric()),
                _ => false,
            };
            match runs.last_mut() {
                Some(run) if joins => run.end = at.end,
                _ => runs.push(at),
            }
        }
        runs
    }
}

/// Where a word starts. A separator starts nothing itself — it is the
/// character *after* it that begins the word — and a case change or a step
/// between letters and digits is a word start with no separator to show it,
/// which is what makes `masterClient` and `col2` behave like `master_client`
/// and `col_2`.
fn bonus(before: Option<char>, ch: char) -> i32 {
    if !ch.is_alphanumeric() {
        return 0;
    }
    let Some(before) = before else {
        return BONUS_NAME_START;
    };
    if !before.is_alphanumeric() {
        return BONUS_BOUNDARY;
    }
    if before.is_lowercase() && ch.is_uppercase() {
        return BONUS_CAMEL;
    }
    if before.is_numeric() != ch.is_numeric() {
        return BONUS_CAMEL;
    }
    0
}

/// One character folded to one character, so a fold never changes how many
/// characters a name has and the places found in the folded copy are the
/// places in the original. Postgres folds unquoted identifiers to lower
/// case, so this is the same question the server asks.
fn fold(ch: char) -> char {
    ch.to_lowercase().next().unwrap_or(ch)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ranges(name: &str, query: &str) -> Vec<Range<usize>> {
        score(name, query).expect("the query matches").ranges
    }

    /// The case this crate was written for: a query that names two words of
    /// a name by their starts.
    #[test]
    fn a_query_names_the_starts_of_the_words() {
        assert!(matches("master_client_reference", "mast_cl"));
        assert_eq!(
            ranges("master_client_reference", "mast_cl"),
            vec![0..4, 7..9]
        );
        assert!(matches("master_client_reference", "master_client"));
        assert!(matches("master_client_reference", "mas_cli_ref"));
    }

    /// A case change is a word boundary, so the same query walks a name
    /// written in camel case. The typed separator has something to line up
    /// with even though the name holds none.
    #[test]
    fn a_case_change_is_a_word_boundary() {
        assert!(matches("masterClientReference", "mast_cl"));
        assert_eq!(ranges("masterClientReference", "mast_cl"), vec![0..4, 6..8]);
        assert!(matches("col2_name", "col_2"));
    }

    /// Initials, which rule 4 in the module docs is what allows.
    #[test]
    fn a_run_of_word_starts_is_a_match() {
        assert!(matches("master_client_reference", "mcr"));
        assert_eq!(
            ranges("master_client_reference", "mcr"),
            vec![0..1, 7..8, 14..15]
        );
        assert!(matches("LAST_SEEN", "ls"));
    }

    /// The first fragment sits anywhere: asking for a name by the word in
    /// the middle of it is a fair way to ask.
    #[test]
    fn the_first_fragment_may_sit_anywhere() {
        assert!(matches("master_client_reference", "client"));
        assert_eq!(ranges("master_client_reference", "client"), vec![7..13]);
        assert!(matches("master_client_reference", "ent_ref"));
    }

    /// The gate. Nothing lands in the middle of a word unless it follows
    /// the character before it, so a query cannot pick letters out of a
    /// name it does not name.
    #[test]
    fn nothing_matches_in_the_middle_of_a_word() {
        // `a`, `s` — then `e` is loose inside `master`.
        assert!(score("master_client_reference", "ase").is_none());
        assert!(score("master_client_reference", "mtr").is_none());
        // A typed separator asks for a word boundary the name has not got.
        assert!(score("mastcl", "mast_cl").is_none());
        assert!(score("email", "zzz").is_none());
        assert!(score("id", "identifier").is_none());
    }

    /// A name the query starts beats one it hits in the middle, and a
    /// tighter match beats a scattered one.
    #[test]
    fn a_closer_match_scores_higher() {
        let front = score("master_state_type_code", "mast")
            .expect("matches")
            .score;
        let middle = score("invoice_master_name", "mast").expect("matches").score;
        assert!(front > middle, "{front} should beat {middle}");

        let tight = score("master_client_reference", "mast_cl")
            .expect("matches")
            .score;
        let loose = score("master_client_reference", "mcr")
            .expect("matches")
            .score;
        assert!(tight > loose, "{tight} should beat {loose}");
    }

    /// A name matched in one piece beats one matched in two, whatever word
    /// starts the second piece happened to land on. The name the user typed
    /// has to come first.
    #[test]
    fn one_piece_beats_two() {
        let whole = score("users", "users").expect("matches").score;
        let split = score("user_settings", "users").expect("matches").score;
        assert!(whole > split, "{whole} should beat {split}");

        let tight = score("order_items", "order_it").expect("matches").score;
        let jumped = score("order_invoice_total", "order_it")
            .expect("matches")
            .score;
        assert!(tight > jumped, "{tight} should beat {jumped}");
    }

    /// A separator the query typed and the name has is part of what
    /// matched, so the underline does not come back in pieces with the
    /// bars between them left out.
    #[test]
    fn a_matched_separator_is_part_of_the_hit() {
        assert_eq!(
            ranges("sample_dev_sample", "sample_dev_sample"),
            vec![0..17]
        );
        assert_eq!(ranges("sample_dev_sample", "dev_sample"), vec![7..17]);
        // Only where the name really does end the word there. `mast` stops
        // inside `master`, so the two pieces stay apart.
        assert_eq!(
            ranges("master_client_reference", "mast_cl"),
            vec![0..4, 7..9]
        );
    }

    /// The alignment is the best one, not the first one found: `cl` in
    /// `client_cluster` lands where it scores highest.
    #[test]
    fn the_best_alignment_wins_not_the_first() {
        // Both `cl`s start a word, so the earlier one wins on the tie.
        assert_eq!(ranges("client_cluster", "cl"), vec![0..2]);
        // Here only the second one does.
        assert_eq!(ranges("include_client", "cl"), vec![8..10]);
    }

    /// Case is ignored while the query says nothing about it.
    #[test]
    fn case_is_ignored_until_the_query_means_it() {
        assert!(matches("email", "EMAIL"));
        assert!(matches("LAST_SEEN", "last"));
        assert!(matches("masterClientReference", "masterclientreference"));
    }

    /// A query written in both cases means the capital: it asks for a
    /// capital or for a word start.
    #[test]
    fn a_capital_in_a_mixed_query_asks_for_one() {
        assert!(matches("master_client_reference", "mast_Cl"));
        assert!(matches("masterClientReference", "mastCl"));
        assert!(score("include", "inCl").is_none());
    }

    /// An open search line lists what it is searching.
    #[test]
    fn an_empty_query_matches_everything() {
        let hit = score("email", "").expect("an empty query matches");
        assert_eq!(hit.score, 0);
        assert!(hit.ranges.is_empty());
        assert!(matches("email", "  "));
        assert!(matches("", ""));
        assert!(score("", "id").is_none());
    }

    /// The ranges are byte ranges into the name as it was handed in, on
    /// character boundaries, so a caller may slice with them.
    #[test]
    fn the_ranges_are_safe_to_slice() {
        let name = "größe_wert";
        let hit = score(name, "grö_we").expect("matches");
        for range in &hit.ranges {
            assert!(
                name.get(range.clone()).is_some(),
                "{range:?} splits a character"
            );
        }
        assert_eq!(hit.ranges, vec![0..4, 8..10]);
    }
}
