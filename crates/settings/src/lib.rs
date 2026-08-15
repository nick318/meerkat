//! User settings: a JSON file the user can edit by hand (Zed pattern).
//! Unknown keys are ignored; missing keys fall back to defaults.

use anyhow::{Context as _, Result};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Settings {
    /// "dark" or "light".
    pub theme: String,
    /// Default row cap for query results.
    pub fetch_limit: u32,
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            theme: "dark".to_string(),
            fetch_limit: 10_000,
        }
    }
}

impl Settings {
    pub fn load() -> Result<Self> {
        let path = settings_path()?;
        if !path.exists() {
            return Ok(Self::default());
        }
        let text = std::fs::read_to_string(&path)
            .with_context(|| format!("failed to read {}", path.display()))?;
        Ok(serde_json::from_str(&text)?)
    }

    pub fn save(&self) -> Result<()> {
        let path = settings_path()?;
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        std::fs::write(&path, serde_json::to_string_pretty(self)?)?;
        Ok(())
    }
}

pub fn settings_path() -> Result<PathBuf> {
    Ok(dirs::config_dir()
        .context("no user config directory")?
        .join("meerkat")
        .join("settings.json"))
}
