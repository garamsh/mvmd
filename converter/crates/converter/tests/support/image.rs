//! What the test binaries share: layers built with the `tar` crate, the pinned tools, and the tree
//! an EROFS holds, read back with the pinned `fsck.erofs`.

use std::collections::BTreeMap;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Command;

pub type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

/// The filesystem UUID the tests ask for.
pub const UUID: &str = "11111111-2222-3333-4444-555555555555";

pub const TAR: &str = "application/vnd.oci.image.layer.v1.tar";
pub const GZIP: &str = "application/vnd.oci.image.layer.v1.tar+gzip";
pub const ZSTD: &str = "application/vnd.oci.image.layer.v1.tar+zstd";

/// One entry of a layer.
pub enum Entry<'a> {
    Dir(&'a str),
    File(&'a str, &'a str),
    /// A whiteout of the named path: the entry `.wh.<name>` beside it.
    Whiteout(&'a str),
    /// The opaque marker of the named directory.
    Opaque(&'a str),
}

/// The tar archive of `entries`, in that order.
pub fn layer(entries: &[Entry<'_>]) -> Vec<u8> {
    let mut builder = tar::Builder::new(Vec::new());
    for entry in entries {
        let (path, kind, data) = match entry {
            Entry::Dir(path) => ((*path).to_owned(), tar::EntryType::Directory, ""),
            Entry::File(path, data) => ((*path).to_owned(), tar::EntryType::Regular, *data),
            Entry::Whiteout(path) => (whiteout_of(path), tar::EntryType::Regular, ""),
            Entry::Opaque(dir) => (format!("{dir}/.wh..wh..opq"), tar::EntryType::Regular, ""),
        };
        let mut header = tar::Header::new_ustar();
        header.set_entry_type(kind);
        header.set_size(data.len() as u64);
        header.set_mode(if data.is_empty() && kind.is_dir() {
            0o755
        } else {
            0o644
        });
        header.set_mtime(1_700_000_000);
        // `mkfs.erofs` refuses a header whose owner fields are empty, as the `tar` crate leaves them.
        header.set_uid(0);
        header.set_gid(0);
        header.set_cksum();
        if let Err(error) = builder.append_data(&mut header, &path, data.as_bytes()) {
            unreachable!("a Vec takes the entry {path}: {error}");
        }
    }
    match builder.into_inner() {
        Ok(bytes) => bytes,
        Err(error) => unreachable!("a Vec takes the end of the archive: {error}"),
    }
}

/// `d/.wh.a` for `d/a`.
fn whiteout_of(path: &str) -> String {
    match path.rsplit_once('/') {
        Some((dir, name)) => format!("{dir}/.wh.{name}"),
        None => format!(".wh.{path}"),
    }
}

pub fn gzip(bytes: &[u8]) -> Vec<u8> {
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    if let Err(error) = encoder.write_all(bytes) {
        unreachable!("a Vec takes the bytes: {error}");
    }
    match encoder.finish() {
        Ok(compressed) => compressed,
        Err(error) => unreachable!("a Vec takes the end of the stream: {error}"),
    }
}

pub fn zstd(bytes: &[u8]) -> Vec<u8> {
    ruzstd::encoding::compress_to_vec(bytes, ruzstd::encoding::CompressionLevel::Fastest)
}

/// The path of a pinned tool the build made. A missing variable fails the test, so a run that
/// cannot reach the real tool is not a pass.
pub fn tool(variable: &str) -> PathBuf {
    let path = std::env::var_os(variable).unwrap_or_else(|| {
        panic!("{variable} names the pinned erofs-utils tool; run the tests through `make test`")
    });
    PathBuf::from(path)
}

/// A sparse file of 64 MiB for the converter to write to, in `dir`.
pub fn output_file(dir: &Path) -> Result<PathBuf> {
    let path = dir.join("output.img");
    fs::File::create(&path)?.set_len(64 << 20)?;
    Ok(path)
}

/// What a file of the tree holds: `None` for a directory, the content for a regular file.
pub type Tree = BTreeMap<String, Option<String>>;

/// The tree the EROFS at `image` holds, read by extracting it with the pinned `fsck.erofs`. A
/// whiteout left in the image would be a character device the extraction cannot make, and fails
/// it.
pub fn tree(image: &Path, scratch: &Path) -> Result<Tree> {
    let target = scratch.join("extracted");
    fs::create_dir_all(&target)?;
    let output = Command::new(tool("FSCK_EROFS"))
        .arg(format!("--extract={}", target.display()))
        .arg(image)
        .output()?;
    if !output.status.success() {
        return Err(format!(
            "fsck.erofs {}: {}",
            output.status,
            String::from_utf8_lossy(&output.stderr)
        )
        .into());
    }
    let mut tree = Tree::new();
    walk(&target, &target, &mut tree)?;
    Ok(tree)
}

fn walk(root: &Path, dir: &Path, tree: &mut Tree) -> Result {
    for entry in fs::read_dir(dir)? {
        let path = entry?.path();
        let name = path.strip_prefix(root)?.to_string_lossy().into_owned();
        if fs::symlink_metadata(&path)?.is_dir() {
            tree.insert(name, None);
            walk(root, &path, tree)?;
        } else {
            tree.insert(name, Some(fs::read_to_string(&path)?));
        }
    }
    Ok(())
}

/// A tree written out: `d/` for a directory and `f=text` for a file.
pub fn expect(entries: &[&str]) -> Tree {
    entries
        .iter()
        .map(|entry| match entry.split_once('=') {
            Some((path, text)) => ((*path).to_owned(), Some(text.to_owned())),
            None => (entry.trim_end_matches('/').to_owned(), None),
        })
        .collect()
}
