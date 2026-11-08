//! The guest's files: read in chunks, replace whole, patch by digest, list and search.
//!
//! Every operation acts on the file its path names after following symbolic links. A write or a
//! patch replaces the regular file the path resolves to, in that file's own directory, and refuses
//! a target that is anything else.

use std::collections::HashMap;
use std::fs;
use std::io;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Weak};

use globset::GlobBuilder;
use tokio::fs::File;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{Mutex, OwnedMutexGuard};

use crate::digest::{Digest, Hasher};
use crate::error::Error;
use crate::v1alpha1::{EntryKind, FileEntry, PatchOutcome};

const READ_SIZE: usize = 64 * 1024;

/// A path from a request must be absolute, so the agent's own working directory never decides.
fn absolute(path: &str) -> Result<&Path, Error> {
    let path = Path::new(path);
    if path.is_absolute() {
        Ok(path)
    } else {
        Err(Error::Invalid(format!(
            "path {} is not absolute",
            path.display()
        )))
    }
}

fn not_regular(path: &Path) -> Error {
    Error::Invalid(format!("{} is not a regular file", path.display()))
}

/// Open a regular file for reading. The open does not wait for a writer, so a FIFO is refused
/// where it would otherwise hold a thread for good.
async fn open_regular(path: &Path) -> Result<File, Error> {
    let file = tokio::fs::OpenOptions::new()
        .read(true)
        .custom_flags(nix::libc::O_NONBLOCK)
        .open(path)
        .await
        .map_err(|e| Error::from_io("open", path, e))?;
    let metadata = file
        .metadata()
        .await
        .map_err(|e| Error::from_io("stat", path, e))?;
    if metadata.is_file() {
        Ok(file)
    } else {
        Err(not_regular(path))
    }
}

/// The regular file `path` names after following links, or `None` when nothing is there.
async fn existing(path: &Path) -> Result<Option<PathBuf>, Error> {
    match tokio::fs::metadata(path).await {
        Ok(metadata) if metadata.is_file() => tokio::fs::canonicalize(path)
            .await
            .map(Some)
            .map_err(|e| Error::from_io("resolve", path, e)),
        Ok(_) => Err(not_regular(path)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            // A link to nothing names no file to replace and must not be replaced itself.
            if tokio::fs::symlink_metadata(path).await.is_ok() {
                return Err(Error::Invalid(format!(
                    "{} is a link to nothing",
                    path.display()
                )));
            }
            Ok(None)
        }
        Err(e) => Err(Error::from_io("stat", path, e)),
    }
}

/// Where a write to `path` lands: the file it resolves to, or a new file in its directory.
async fn write_target(path: &str) -> Result<PathBuf, Error> {
    let path = absolute(path)?;
    if let Some(found) = existing(path).await? {
        return Ok(found);
    }
    let (Some(dir), Some(name)) = (path.parent(), path.file_name()) else {
        return Err(Error::Invalid(format!("{} names no file", path.display())));
    };
    let dir = tokio::fs::canonicalize(dir)
        .await
        .map_err(|e| Error::from_io("resolve", dir, e))?;
    Ok(dir.join(name))
}

/// One lock per path being replaced, so a patch's read, check and commit are not interleaved
/// with another replacement of the same file.
#[derive(Default)]
pub(crate) struct Locks {
    held: Mutex<HashMap<PathBuf, Weak<Mutex<()>>>>,
}

impl Locks {
    async fn acquire(&self, path: &Path) -> OwnedMutexGuard<()> {
        let lock = {
            let mut held = self.held.lock().await;
            // A lock nobody holds any more is dropped from the table.
            held.retain(|_, lock| lock.strong_count() > 0);
            if let Some(lock) = held.get(path).and_then(Weak::upgrade) {
                lock
            } else {
                let lock = Arc::new(Mutex::new(()));
                held.insert(path.to_path_buf(), Arc::downgrade(&lock));
                lock
            }
        };
        lock.lock_owned().await
    }
}

/// A file open for reading in chunks.
pub(crate) struct Reader {
    file: File,
    path: PathBuf,
}

impl Reader {
    /// Open a regular file.
    pub(crate) async fn open(path: &str) -> Result<Self, Error> {
        let path = absolute(path)?.to_path_buf();
        let file = open_regular(&path).await?;
        Ok(Self { file, path })
    }

    /// The next chunk, or `None` at the end of the file.
    pub(crate) async fn next(&mut self) -> Result<Option<Vec<u8>>, Error> {
        let mut buf = vec![0; READ_SIZE];
        let n = self
            .file
            .read(&mut buf)
            .await
            .map_err(|e| Error::from_io("read", &self.path, e))?;
        buf.truncate(n);
        Ok((n > 0).then_some(buf))
    }
}

/// New content for a file, written beside it and renamed over it on commit. Dropped uncommitted,
/// it removes what it wrote, so the path holds the old content or the whole new one.
struct Replacement {
    file: File,
    temp: PathBuf,
    target: PathBuf,
    committed: bool,
}

impl Replacement {
    /// Start a replacement of `target`, which `write_target` resolved. A file that exists keeps
    /// its owner, group and mode, set before any content is written; a new file gets the mode a
    /// plain create gives.
    async fn create(target: PathBuf) -> Result<Self, Error> {
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let (Some(dir), true) = (target.parent(), target.file_name().is_some()) else {
            return Err(Error::Invalid(format!(
                "{} names no file",
                target.display()
            )));
        };
        let old = match tokio::fs::metadata(&target).await {
            Ok(metadata) => Some(metadata),
            Err(e) if e.kind() == io::ErrorKind::NotFound => None,
            Err(e) => return Err(Error::from_io("stat", &target, e)),
        };
        let temp = dir.join(format!(
            ".mvmd-{:x}-{:x}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        let file = tokio::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(if old.is_some() { 0o600 } else { 0o666 })
            .open(&temp)
            .await
            .map_err(|e| Error::from_io("create", &temp, e))?;
        let replacement = Self {
            file,
            temp,
            target,
            committed: false,
        };
        if let Some(old) = old {
            // The owner first: changing it clears set-id bits, which the mode then restores.
            std::os::unix::fs::fchown(&replacement.file, Some(old.uid()), Some(old.gid()))
                .map_err(|e| Error::from_io("set the owner of", &replacement.temp, e))?;
            replacement
                .file
                .set_permissions(old.permissions())
                .await
                .map_err(|e| Error::from_io("set the mode of", &replacement.temp, e))?;
        }
        Ok(replacement)
    }

    async fn write(&mut self, piece: &[u8]) -> Result<(), Error> {
        self.file
            .write_all(piece)
            .await
            .map_err(|e| Error::from_io("write", &self.temp, e))
    }

    async fn commit(mut self) -> Result<(), Error> {
        self.file
            .sync_all()
            .await
            .map_err(|e| Error::from_io("sync", &self.temp, e))?;
        tokio::fs::rename(&self.temp, &self.target)
            .await
            .map_err(|e| Error::from_io("replace", &self.target, e))?;
        self.committed = true;
        if let Some(dir) = self.target.parent() {
            // The new content is in place. Syncing the directory only makes the rename durable, so
            // a failure there is not reported as a failed write.
            if let Ok(dir) = File::open(dir).await {
                let _ = dir.sync_all().await;
            }
        }
        Ok(())
    }
}

impl Drop for Replacement {
    fn drop(&mut self) {
        if !self.committed {
            // Nothing to report to: the temporary file is the only thing left behind.
            let _ = fs::remove_file(&self.temp);
        }
    }
}

/// A write of a whole file that arrives in chunks.
pub(crate) struct Upload {
    replacement: Replacement,
    hasher: Hasher,
}

impl Upload {
    /// Begin a write to `path`; the file holds its old content until `finish`.
    pub(crate) async fn begin(path: &str) -> Result<Self, Error> {
        Ok(Self {
            replacement: Replacement::create(write_target(path).await?).await?,
            hasher: Hasher::new(),
        })
    }

    /// Add the next chunk of content.
    pub(crate) async fn chunk(&mut self, piece: &[u8]) -> Result<(), Error> {
        self.hasher.update(piece);
        self.replacement.write(piece).await
    }

    /// Replace the file with the content received, if it matches the digest the client sent.
    pub(crate) async fn finish(self, locks: &Locks, declared: &str) -> Result<(), Error> {
        let declared = Digest::parse(declared)?;
        let received = self.hasher.finish();
        if received != declared {
            return Err(Error::DigestMismatch(format!(
                "the client sent {}, the agent received {}",
                declared.as_str(),
                received.as_str()
            )));
        }
        let _held = locks.acquire(&self.replacement.target).await;
        self.replacement.commit().await
    }
}

/// Apply a unified diff to the file if it is at `base`; succeed without a change if it is at
/// `expected` already.
pub(crate) async fn patch(
    locks: &Locks,
    path: &str,
    base: &str,
    expected: &str,
    diff: &[u8],
) -> Result<PatchOutcome, Error> {
    let (base, expected) = (Digest::parse(base)?, Digest::parse(expected)?);
    let path = absolute(path)?;
    let target = existing(path)
        .await?
        .ok_or_else(|| Error::NotFound(path.display().to_string()))?;
    // Held from the read to the commit: the file cannot change between the check and the rename.
    let _held = locks.acquire(&target).await;
    let mut current = Vec::new();
    open_regular(&target)
        .await?
        .read_to_end(&mut current)
        .await
        .map_err(|e| Error::from_io("read", &target, e))?;
    let found = Digest::of(&current);
    if found == expected {
        return Ok(PatchOutcome::AlreadyApplied);
    }
    if found != base {
        return Err(Error::PreconditionFailed(format!(
            "{} is at {}, neither the base {} nor the expected {}",
            target.display(),
            found.as_str(),
            base.as_str(),
            expected.as_str()
        )));
    }
    let diff = diffy::Patch::from_bytes(diff)
        .map_err(|e| Error::Invalid(format!("the patch does not parse: {e}")))?;
    let patched = diffy::apply_bytes(&current, &diff)
        .map_err(|e| Error::PreconditionFailed(format!("the patch does not apply: {e}")))?;
    let result = Digest::of(&patched);
    if result != expected {
        return Err(Error::PreconditionFailed(format!(
            "the patch produces {}, not the expected {}",
            result.as_str(),
            expected.as_str()
        )));
    }
    let mut replacement = Replacement::create(target).await?;
    replacement.write(&patched).await?;
    replacement.commit().await?;
    Ok(PatchOutcome::Applied)
}

/// The entries of a directory, sorted by path. An entry that vanishes while the directory is read
/// is left out.
pub(crate) async fn list(path: &str) -> Result<Vec<FileEntry>, Error> {
    let dir = absolute(path)?.to_path_buf();
    blocking(move || {
        let mut entries = Vec::new();
        for entry in fs::read_dir(&dir).map_err(|e| Error::from_io("list", &dir, e))? {
            let entry = entry.map_err(|e| Error::from_io("list", &dir, e))?;
            let path = entry.path();
            match entry_of(&path) {
                Ok(found) => entries.push(found),
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(e) => return Err(Error::from_io("stat", &path, e)),
            }
        }
        entries.sort_by(|a, b| a.path.cmp(&b.path));
        Ok(entries)
    })
    .await
}

/// The entries under a directory whose path relative to it matches the glob, sorted by path.
pub(crate) async fn search(path: &str, pattern: &str) -> Result<Vec<FileEntry>, Error> {
    let root = absolute(path)?.to_path_buf();
    let glob = GlobBuilder::new(pattern)
        .literal_separator(true)
        .build()
        .map_err(|e| Error::Invalid(format!("pattern {pattern:?}: {e}")))?
        .compile_matcher();
    blocking(move || {
        let mut matches = Vec::new();
        let mut pending = vec![root.clone()];
        while let Some(dir) = pending.pop() {
            let read = match fs::read_dir(&dir) {
                Ok(read) => read,
                // Only the directory searched must be readable; one deeper down is skipped.
                Err(_) if dir != root => continue,
                Err(e) => return Err(Error::from_io("search", &dir, e)),
            };
            for entry in read.flatten() {
                let path = entry.path();
                let is_dir = entry.file_type().is_ok_and(|kind| kind.is_dir());
                if path.strip_prefix(&root).is_ok_and(|rel| glob.is_match(rel))
                    && let Ok(found) = entry_of(&path)
                {
                    matches.push(found);
                }
                if is_dir {
                    pending.push(path);
                }
            }
        }
        matches.sort_by(|a, b| a.path.cmp(&b.path));
        Ok(matches)
    })
    .await
}

fn entry_of(path: &Path) -> io::Result<FileEntry> {
    let metadata = fs::symlink_metadata(path)?;
    let kind = if metadata.is_file() {
        EntryKind::File
    } else if metadata.is_dir() {
        EntryKind::Directory
    } else if metadata.is_symlink() {
        EntryKind::Symlink
    } else {
        EntryKind::Other
    };
    Ok(FileEntry {
        path: path.to_string_lossy().into_owned(),
        kind: kind.into(),
        size: metadata.len(),
    })
}

async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> Result<T, Error> + Send + 'static,
) -> Result<T, Error> {
    tokio::task::spawn_blocking(work)
        .await
        .unwrap_or_else(|_| unreachable!("a blocking file task panicked"))
}
