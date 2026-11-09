//! Generates the converter channel's Rust code from its `.proto` at build time.
//! `protox` is a pure-Rust protobuf compiler, so the build needs no `protoc`.

use std::path::PathBuf;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_root = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("../../proto");
    let descriptors = protox::compile(["mvmd/convert/v1alpha1/convert.proto"], [&proto_root])?;
    tonic_prost_build::compile_fds(descriptors)?;
    println!("cargo:rerun-if-changed={}", proto_root.display());
    Ok(())
}
