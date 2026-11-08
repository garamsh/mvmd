//! The guest's files: read in chunks, replace whole, patch by digest, list and search.

use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use globset::GlobBuilder;
use tokio::fs::File;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

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

/// A file open for reading in chunks.
pub(crate) struct Reader {
    file: File,
    path: PathBuf,
}

impl Reader {
    /// Open a regular file.
    pub(crate) async fn open(path: &str) -> Result<Self, Error> {
        let path = absolute(path)?.to_path_buf();
        let file = File::open(&path)
            .await
            .map_err(|e| Error::from_io("open", &path, e))?;
        let metadata = file
            .metadata()
            .await
            .map_err(|e| Error::from_io("stat", &path, e))?;
        if !metadata.is_file() {
            return Err(Error::Invalid(format!(
                "{} is not a regular file",
                path.display()
            )));
        }
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

/// New content for a path, written beside it and renamed over it on commit. Dropped uncommitted,
/// it removes what it wrote, so the path holds the old content or the whole new one.
struct Replacement {
    file: File,
    temp: PathBuf,
    target: PathBuf,
    committed: bool,
}

impl Replacement {
    async fn create(target: &str) -> Result<Self, Error> {
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let target = absolute(target)?.to_path_buf();
        let (Some(dir), Some(name)) = (target.parent(), target.file_name()) else {
            return Err(Error::Invalid(format!(
                "{} names no file",
                target.display()
            )));
        };
        let temp = dir.join(format!(
            ".{}.mvmd-{}-{}",
            name.to_string_lossy(),
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        let file = tokio::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temp)
            .await
            .map_err(|e| Error::from_io("create", &temp, e))?;
        Ok(Self {
            file,
            temp,
            target,
            committed: false,
        })
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
        // A replaced file keeps its mode.
        if let Ok(existing) = tokio::fs::metadata(&self.target).await {
            tokio::fs::set_permissions(&self.temp, existing.permissions())
                .await
                .map_err(|e| Error::from_io("set the mode of", &self.temp, e))?;
        }
        tokio::fs::rename(&self.temp, &self.target)
            .await
            .map_err(|e| Error::from_io("replace", &self.target, e))?;
        self.committed = true;
        if let Some(dir) = self.target.parent() {
            // The rename is in place; making it durable is the directory's sync.
            File::open(dir)
                .await
                .map_err(|e| Error::from_io("open", dir, e))?
                .sync_all()
                .await
                .map_err(|e| Error::from_io("sync", dir, e))?;
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
    /// Begin a write to `path`; the path holds its old content until `finish`.
    pub(crate) async fn begin(path: &str) -> Result<Self, Error> {
        Ok(Self {
            replacement: Replacement::create(path).await?,
            hasher: Hasher::new(),
        })
    }

    /// Add the next chunk of content.
    pub(crate) async fn chunk(&mut self, piece: &[u8]) -> Result<(), Error> {
        self.hasher.update(piece);
        self.replacement.write(piece).await
    }

    /// Replace the path with the content received, if it matches the digest the client sent.
    pub(crate) async fn finish(self, declared: &str) -> Result<(), Error> {
        let declared = Digest::parse(declared)?;
        let received = self.hasher.finish();
        if received != declared {
            return Err(Error::DigestMismatch(format!(
                "the client sent {}, the agent received {}",
                declared.as_str(),
                received.as_str()
            )));
        }
        self.replacement.commit().await
    }
}

/// Apply a unified diff to the file if it is at `base`; succeed without a change if it is at
/// `expected` already.
pub(crate) async fn patch(
    path: &str,
    base: &str,
    expected: &str,
    diff: &[u8],
) -> Result<PatchOutcome, Error> {
    let (base, expected) = (Digest::parse(base)?, Digest::parse(expected)?);
    let target = absolute(path)?;
    let current = tokio::fs::read(target)
        .await
        .map_err(|e| Error::from_io("read", target, e))?;
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
    let mut replacement = Replacement::create(path).await?;
    replacement.write(&patched).await?;
    replacement.commit().await?;
    Ok(PatchOutcome::Applied)
}

/// The entries of a directory, sorted by path.
pub(crate) async fn list(path: &str) -> Result<Vec<FileEntry>, Error> {
    let dir = absolute(path)?.to_path_buf();
    blocking(move || {
        let mut entries = Vec::new();
        for entry in fs::read_dir(&dir).map_err(|e| Error::from_io("list", &dir, e))? {
            let entry = entry.map_err(|e| Error::from_io("list", &dir, e))?;
            entries.push(entry_of(&entry.path()).map_err(|e| Error::from_io("stat", &dir, e))?);
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
