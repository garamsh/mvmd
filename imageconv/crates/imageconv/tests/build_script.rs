//! `imageconv/mkfs-erofs/build.sh` at a download that fails, with a `curl` of the test's own first
//! on the `PATH`. The script stops at the download and says which one failed, and reports a
//! digest mismatch only for a file that was downloaded or cached and does not match.
//!
//! One test, one thread: each shim is written and then executed, and a second test forking while
//! a shim is open for writing would make that exec fail with ETXTBSY.

use std::ffi::OsString;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};

use tempfile::TempDir;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

/// The zig file `build.sh` pins for this machine.
fn zig_file() -> &'static str {
    match std::env::consts::ARCH {
        "aarch64" => "zig-aarch64-linux-0.17.1.tar.xz",
        _ => "zig-x86_64-linux-0.17.1.tar.xz",
    }
}

/// A `curl` that logs its arguments to `log` and then runs `body`.
fn shim(dir: &Path, log: &Path, body: &str) -> Result<()> {
    let path = dir.join("curl");
    fs::write(
        &path,
        format!("#!/bin/sh\necho \"$@\" >> {}\n{body}\n", log.display()),
    )?;
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755))?;
    Ok(())
}

/// Run `build.sh` with the shims first on the `PATH`, over `cache` and a fresh output directory.
fn build(shims: &Path, cache: &Path, out: &Path) -> Result<Output> {
    let script = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?)
        .join("../../mkfs-erofs/build.sh")
        .canonicalize()?;
    let mut path = OsString::from(shims);
    path.push(":");
    path.push(std::env::var_os("PATH").unwrap_or_default());
    Ok(Command::new("bash")
        .arg(script)
        .arg(cache)
        .arg(out)
        .env("PATH", path)
        .output()?)
}

fn calls(log: &Path) -> Result<Vec<String>> {
    Ok(fs::read_to_string(log)
        .unwrap_or_default()
        .lines()
        .map(str::to_owned)
        .collect())
}

#[test]
fn a_failed_download_stops_the_build_there_and_names_it_and_a_mismatch_is_reported_only_for_a_file()
-> Result {
    let dir = TempDir::new()?;
    let (shims, out) = (dir.path().join("shims"), dir.path().join("out"));
    fs::create_dir(&shims)?;

    // A server error: curl fails with exit status 22 and says why.
    let log = dir.path().join("failing.log");
    shim(
        &shims,
        &log,
        "echo 'curl: (22) The requested URL returned error: 504' >&2\nexit 22",
    )?;
    let output = build(&shims, &dir.path().join("cache-failing"), &out)?;
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(1), "{stderr}");
    assert!(
        stderr.contains("build.sh: downloading https://ziglang.org/download/0.17.1/zig-"),
        "{stderr}"
    );
    assert!(stderr.contains("curl exit status 22"), "{stderr}");
    assert!(stderr.contains("curl: (22)"), "{stderr}");
    assert!(!stderr.contains("does not match"), "{stderr}");
    assert!(!stderr.contains("No such file"), "{stderr}");
    // It stopped at the first download, which was asked to retry a bounded number of times.
    let asked = calls(&log)?;
    assert_eq!(asked.len(), 1, "{asked:?}");
    assert!(
        asked[0].contains("--retry 4 --retry-all-errors"),
        "{asked:?}"
    );

    // A download that succeeds with the wrong content is a mismatch, and the file is dropped.
    let log = dir.path().join("wrong.log");
    shim(
        &shims,
        &log,
        "while [ $# -gt 0 ]; do [ \"$1\" = --output ] && out=$2; shift; done\necho wrong > \"$out\"",
    )?;
    let cache = dir.path().join("cache-wrong");
    let output = build(&shims, &cache, &out)?;
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(1), "{stderr}");
    assert!(
        stderr.contains("does not match its pinned SHA-256"),
        "{stderr}"
    );
    assert!(!stderr.contains("downloading"), "{stderr}");
    assert_eq!(calls(&log)?.len(), 1);
    assert!(!cache.join("dl").join(zig_file()).exists());

    // A cached file is checked too: wrong content is a mismatch and nothing is downloaded.
    let log = dir.path().join("cached.log");
    shim(&shims, &log, "exit 22")?;
    let cache = dir.path().join("cache-cached");
    fs::create_dir_all(cache.join("dl"))?;
    fs::write(cache.join("dl").join(zig_file()), "stale")?;
    let output = build(&shims, &cache, &out)?;
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(1), "{stderr}");
    assert!(
        stderr.contains("does not match its pinned SHA-256"),
        "{stderr}"
    );
    assert!(
        calls(&log)?.is_empty(),
        "a cached file was downloaded again"
    );

    fetch_tries_each_url_until_one_gives_the_pinned_digest(dir.path(), &shims)
}

/// `sha256sum` of `good\n`.
const GOOD: &str = "106675dc1490d5cdd6d1f0410731316ce93fc964c6cf6726e2b0d53e19688feb";

/// The text of `fetch` in `build.sh`, from its `fetched=` line to the closing brace.
fn fetch_definition() -> Result<String> {
    let script = fs::read_to_string(
        PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("../../mkfs-erofs/build.sh"),
    )?;
    let start = script.find("\nfetched=\n").ok_or("no fetch in build.sh")? + 1;
    let end = script[start..].find("\n}\n").ok_or("fetch is not closed")? + start + 3;
    Ok(script[start..end].to_owned())
}

/// Run `fetch <GOOD> <urls>` from `build.sh` over `cache`. The shim answers by URL: one with
/// `bad` in it fails, one with `wrong` gives other content, one with `good` gives `good\n`.
fn run_fetch(shims: &Path, log: &Path, cache: &Path, urls: &[&str]) -> Result<Output> {
    shim(
        shims,
        log,
        "for last; do :; done\n\
         while [ $# -gt 0 ]; do [ \"$1\" = --output ] && out=$2; shift; done\n\
         case \"$last\" in\n\
         *bad*) echo 'curl: (22) 504' >&2; exit 22 ;;\n\
         *wrong*) echo other > \"$out\" ;;\n\
         *) echo good > \"$out\" ;;\nesac",
    )?;
    let mut path = OsString::from(shims);
    path.push(":");
    path.push(std::env::var_os("PATH").unwrap_or_default());
    let program = format!(
        "set -euo pipefail\ncache=$1\nshift\n{}\nfetch {GOOD} \"$@\"\necho \"fetched=$fetched\"",
        fetch_definition()?
    );
    Ok(Command::new("bash")
        .args(["-c", &program, "fetch", &cache.display().to_string()])
        .args(urls)
        .env("PATH", path)
        .output()?)
}

fn fetch_tries_each_url_until_one_gives_the_pinned_digest(dir: &Path, shims: &Path) -> Result {
    let bad = "https://x.invalid/bad/f.tar";
    let wrong = "https://x.invalid/wrong/f.tar";
    let good = "https://y.invalid/good/f.tar";

    // A URL that fails, then one that answers: the second is used.
    let (log, cache) = (dir.join("a.log"), dir.join("fetch-a"));
    let output = run_fetch(shims, &log, &cache, &[bad, good])?;
    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(
        stdout.contains(&format!("fetched={}/dl/f.tar", cache.display())),
        "{stdout}"
    );
    assert_eq!(fs::read_to_string(cache.join("dl/f.tar"))?, "good\n");
    assert!(
        stderr.contains(&format!("downloading {bad} failed")),
        "{stderr}"
    );
    assert_eq!(calls(&log)?.len(), 2);

    // A URL that answers with other content is not taken: the digest decides.
    let (log, cache) = (dir.join("b.log"), dir.join("fetch-b"));
    let output = run_fetch(shims, &log, &cache, &[wrong, good])?;
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(
        stderr.contains(&format!("{wrong} does not match")),
        "{stderr}"
    );
    assert_eq!(fs::read_to_string(cache.join("dl/f.tar"))?, "good\n");

    // Every URL failing, in either way, stops with the failure and leaves no file.
    let (log, cache) = (dir.join("c.log"), dir.join("fetch-c"));
    let output = run_fetch(shims, &log, &cache, &[bad, wrong])?;
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(1), "{stderr}");
    assert!(
        stderr.contains("no URL for f.tar gave a file with the pinned SHA-256"),
        "{stderr}"
    );
    assert_eq!(calls(&log)?.len(), 2);
    assert!(!cache.join("dl/f.tar").exists());

    // A cached file with the pinned digest is used without a download.
    let (log, cache) = (dir.join("d.log"), dir.join("fetch-d"));
    fs::create_dir_all(cache.join("dl"))?;
    fs::write(cache.join("dl/f.tar"), "good\n")?;
    let output = run_fetch(shims, &log, &cache, &[bad])?;
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(calls(&log)?.is_empty());
    Ok(())
}
