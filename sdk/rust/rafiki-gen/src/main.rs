// SPDX-License-Identifier: Apache-2.0
//! Regenerates the Rust SDK's protobuf types.
//!
//! `make proto` runs this after the Go and Python generators, so the Rust SDK
//! cannot lag `proto/`: one run regenerates every SDK's types together. The
//! output lives in `sdk/rust/rafiki/src/gen/` and is COMMITTED — generated
//! files are never hand-edited, and `git diff --exit-code --
//! sdk/rust/rafiki/src/gen` after a `make proto` is what proves it.
//!
//! Usage: `rafiki-gen <repo-root>`. `protoc` must be on PATH (the `proto`
//! target already requires it).

use std::error::Error;
use std::path::Path;
use std::process::ExitCode;

/// The protos whose messages the SDK needs. The two `rafiki.v1` files land in
/// one generated module (same package); daraja is separate but referenced by
/// Control's `Daraja*` RPCs.
const PROTOS: [&str; 3] = [
    "rafiki/v1/control.proto",
    "rafiki/v1/event.proto",
    "rafiki/daraja/v1/daraja.proto",
];

/// Where the crate's `lib.rs` includes the generated files from.
const OUT_DIR: &str = "sdk/rust/rafiki/src/gen";

fn main() -> ExitCode {
    let Some(root) = std::env::args_os().nth(1) else {
        eprintln!("rafiki-gen: usage: rafiki-gen <repo-root>");
        return ExitCode::from(2);
    };
    match generate(Path::new(&root)) {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("rafiki-gen: {err}");
            ExitCode::FAILURE
        }
    }
}

fn generate(root: &Path) -> Result<(), Box<dyn Error>> {
    let proto_root = root.join("proto");
    let out_dir = root.join(OUT_DIR);
    std::fs::create_dir_all(&out_dir)?;
    let protos: Vec<_> = PROTOS.iter().map(|p| proto_root.join(p)).collect();
    prost_build::Config::new()
        .out_dir(&out_dir)
        .compile_protos(&protos, &[proto_root])?;
    Ok(())
}
