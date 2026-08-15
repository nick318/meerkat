//! Database passwords live in the OS keychain (macOS Keychain, Windows
//! Credential Manager, Secret Service on Linux), keyed by profile id.
//! Passwords never touch the profiles database or settings files.

use anyhow::Result;
use keyring::Entry;

const SERVICE: &str = "dev.meerkat";

pub fn set_password(profile_id: &str, password: &str) -> Result<()> {
    Entry::new(SERVICE, profile_id)?.set_password(password)?;
    Ok(())
}

pub fn get_password(profile_id: &str) -> Result<Option<String>> {
    match Entry::new(SERVICE, profile_id)?.get_password() {
        Ok(password) => Ok(Some(password)),
        Err(keyring::Error::NoEntry) => Ok(None),
        Err(e) => Err(e.into()),
    }
}

pub fn delete_password(profile_id: &str) -> Result<()> {
    match Entry::new(SERVICE, profile_id)?.delete_credential() {
        Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
        Err(e) => Err(e.into()),
    }
}
