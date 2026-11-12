//! Builds the converter's initrd: the files the `initrd` library writes into an archive are the
//! converter as `init` and the pinned `mkfs.erofs`.

/// Where the archive puts `mkfs.erofs`: the converter looks for it there.
pub const MKFS_EROFS_PATH: &str = "/mkfs.erofs";

/// A file the initrd cannot hold: the `initrd` library refused it.
#[derive(Debug, thiserror::Error)]
#[error(transparent)]
pub struct Error(initrd::Error);

/// The archive holding `converter` as `init` and `mkfs_erofs` at [`MKFS_EROFS_PATH`].
pub fn initrd(converter: &[u8], mkfs_erofs: &[u8]) -> Result<Vec<u8>, Error> {
    ::initrd::initrd(&[
        ("init", converter),
        (MKFS_EROFS_PATH.trim_start_matches('/'), mkfs_erofs),
    ])
    .map_err(Error)
}
