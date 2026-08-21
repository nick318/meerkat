fn main() {
    // The channel and the commit are baked in from the environment, so a
    // build after the environment changed must not reuse the old values.
    println!("cargo:rerun-if-env-changed=MEERKAT_CHANNEL");
    println!("cargo:rerun-if-env-changed=MEERKAT_COMMIT_SHA");
}
