//! Color tokens for the whole app. Components take colors from here,
//! never from literals, so themes stay swappable (Zed `theme` crate pattern).

use gpui::{Hsla, rgb};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Appearance {
    Light,
    Dark,
}

#[derive(Debug, Clone)]
pub struct Theme {
    pub appearance: Appearance,
    pub colors: ThemeColors,
}

#[derive(Debug, Clone)]
pub struct ThemeColors {
    pub background: Hsla,
    pub surface: Hsla,
    pub border: Hsla,
    pub text: Hsla,
    pub text_muted: Hsla,
    pub accent: Hsla,
    pub selection: Hsla,
    pub status_bar: Hsla,
}

impl Theme {
    pub fn dark() -> Self {
        Self {
            appearance: Appearance::Dark,
            colors: ThemeColors {
                background: rgb(0x12161c).into(),
                surface: rgb(0x1a2028).into(),
                border: rgb(0x2c3542).into(),
                text: rgb(0xe6eaf0).into(),
                text_muted: rgb(0x94a0ae).into(),
                accent: rgb(0x3fbfaf).into(),
                selection: rgb(0x24405c).into(),
                status_bar: rgb(0x161b22).into(),
            },
        }
    }

    pub fn light() -> Self {
        Self {
            appearance: Appearance::Light,
            colors: ThemeColors {
                background: rgb(0xf6f7f8).into(),
                surface: rgb(0xffffff).into(),
                border: rgb(0xd9dee4).into(),
                text: rgb(0x1b2430).into(),
                text_muted: rgb(0x5a6675).into(),
                accent: rgb(0x0e6e64).into(),
                selection: rgb(0xcfe3f7).into(),
                status_bar: rgb(0xeef1f4).into(),
            },
        }
    }
}

impl gpui::Global for Theme {}

/// Read the current theme from the app context.
pub fn theme(cx: &gpui::App) -> &Theme {
    cx.global::<Theme>()
}
