//! Generates the guest channel's Rust code from its `.proto` at build time.
//! `protox` is a pure-Rust protobuf compiler, so the build needs no `protoc`.

use std::path::PathBuf;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_root = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("../../proto");
    let descriptors = protox::compile(["mvmd/guest/v1alpha1/guest.proto"], [&proto_root])?;
    // Type names give each message its full name, which a status detail's type URL is made from.
    let mut config = tonic_prost_build::Config::new();
    config.enable_type_names();
    tonic_prost_build::configure().compile_fds_with_config(descriptors, config)?;
    println!("cargo:rerun-if-changed={}", proto_root.display());
    Ok(())
}
