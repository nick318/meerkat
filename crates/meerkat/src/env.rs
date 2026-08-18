//! The closed set of environment tags a connection can wear: prod,
//! staging, dev. The set is closed on purpose — the tags exist to be
//! compared across connections, and free text would give every database
//! its own spelling of "prod".
//!
//! Two screens share it: the connections screen (the form's chips, the
//! group headings) and the shell, where the tag colors the window frame
//! so a prod session can never be mistaken for a dev one. Each tag maps
//! to four `theme` tones: ring, inner, surface, text.

use theme::ThemeColors;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Env {
    Prod,
    Staging,
    Dev,
}

impl Env {
    /// The form's order, and the groups' order.
    pub const ALL: [Env; 3] = [Env::Prod, Env::Staging, Env::Dev];

    pub fn as_str(self) -> &'static str {
        match self {
            Env::Prod => "prod",
            Env::Staging => "staging",
            Env::Dev => "dev",
        }
    }

    /// A stored tag this build does not know reads as untagged, never as
    /// an error: the file may have been written by a newer build.
    pub fn parse(tag: Option<&str>) -> Option<Env> {
        match tag?.trim().to_ascii_lowercase().as_str() {
            "prod" => Some(Env::Prod),
            "staging" => Some(Env::Staging),
            "dev" => Some(Env::Dev),
            _ => None,
        }
    }

    /// The saturated tone: dots, the badge, the window frame's ring.
    pub fn ring(self, colors: &ThemeColors) -> gpui::Hsla {
        match self {
            Env::Prod => colors.env_prod,
            Env::Staging => colors.env_staging,
            Env::Dev => colors.env_dev,
        }
    }

    /// The hairline just inside the frame's ring.
    pub fn inner(self, colors: &ThemeColors) -> gpui::Hsla {
        match self {
            Env::Prod => colors.env_prod_inner,
            Env::Staging => colors.env_staging_inner,
            Env::Dev => colors.env_dev_inner,
        }
    }

    /// The wash: the selected chip, and the framed shell's top bar.
    pub fn surface(self, colors: &ThemeColors) -> gpui::Hsla {
        match self {
            Env::Prod => colors.env_prod_surface,
            Env::Staging => colors.env_staging_surface,
            Env::Dev => colors.env_dev_surface,
        }
    }

    /// Text sitting on the wash.
    pub fn text(self, colors: &ThemeColors) -> gpui::Hsla {
        match self {
            Env::Prod => colors.env_prod_text,
            Env::Staging => colors.env_staging_text,
            Env::Dev => colors.env_dev_text,
        }
    }

    /// The line under the form's chips, from the comp. It names the
    /// frame, so the frame never has to explain itself.
    pub fn form_note(self) -> &'static str {
        match self {
            Env::Prod => "Prod-tagged connections warn before any write and wear the clay frame.",
            Env::Staging => "Staging wears the sand frame; writes go through without a prompt.",
            Env::Dev => "Dev wears the green frame — no guardrails.",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_tag_from_a_newer_build_reads_as_untagged() {
        assert_eq!(Env::parse(Some(" Prod ")), Some(Env::Prod));
        assert_eq!(Env::parse(Some("qa")), None);
        assert_eq!(Env::parse(None), None);
    }
}
