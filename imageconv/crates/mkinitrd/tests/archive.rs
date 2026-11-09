//! The initrd archive, read back with a reader written from the newc format's description and
//! not from the writer.

use mkinitrd::{MKFS_EROFS_PATH, initrd};

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

/// One entry as the kernel reads it: name, mode, owner, group, modification time and content.
type Entry = (String, u32, u32, u32, u32, Vec<u8>);

fn field(archive: &[u8], at: usize, index: usize) -> Result<u32> {
    let start = at + 6 + index * 8;
    let text = std::str::from_utf8(&archive[start..start + 8])?;
    Ok(u32::from_str_radix(text, 16)?)
}

fn align(offset: usize) -> usize {
    (offset + 3) & !3
}

/// Every entry before the trailer, and the offset where the archive ends.
fn read(archive: &[u8]) -> Result<(Vec<Entry>, usize)> {
    let mut entries = Vec::new();
    let mut at = 0;
    loop {
        assert_eq!(&archive[at..at + 6], b"070701", "header at {at}");
        let (mode, owner, group, mtime) = (
            field(archive, at, 1)?,
            field(archive, at, 2)?,
            field(archive, at, 3)?,
            field(archive, at, 5)?,
        );
        let size = field(archive, at, 6)? as usize;
        let name_size = field(archive, at, 11)? as usize;
        let name_at = at + 110;
        let name = std::str::from_utf8(&archive[name_at..name_at + name_size - 1])?;
        assert_eq!(archive[name_at + name_size - 1], 0, "the name ends in NUL");
        let data_at = align(name_at + name_size);
        at = align(data_at + size);
        if name == "TRAILER!!!" {
            return Ok((entries, at));
        }
        entries.push((
            name.to_owned(),
            mode,
            owner,
            group,
            mtime,
            archive[data_at..data_at + size].to_vec(),
        ));
    }
}

#[test]
fn the_archive_holds_the_converter_as_init_and_mkfs_erofs_beside_it() -> Result {
    let converter = b"\x7fELF and some more bytes";
    let mkfs_erofs = b"\x7fELF of the tool";
    let archive = initrd(converter, mkfs_erofs);

    let (entries, end) = read(&archive)?;

    assert_eq!(
        entries,
        [
            ("init".to_owned(), 0o100_755, 0, 0, 0, converter.to_vec()),
            (
                "mkfs.erofs".to_owned(),
                0o100_755,
                0,
                0,
                0,
                mkfs_erofs.to_vec()
            ),
        ]
    );
    assert_eq!(format!("/{}", entries[1].0), MKFS_EROFS_PATH);
    assert_eq!(end, archive.len());
    assert_eq!(archive.len() % 4, 0);
    Ok(())
}

#[test]
fn files_of_any_length_keep_the_alignment_and_their_content() -> Result {
    for length in 0..9 {
        let converter: Vec<u8> = (0..length).map(|byte| byte + 1).collect();
        let mkfs_erofs: Vec<u8> = (0..8 - length).map(|byte| byte + 100).collect();
        let archive = initrd(&converter, &mkfs_erofs);
        let (entries, end) = read(&archive)?;
        assert_eq!(entries[0].5, converter, "length {length}");
        assert_eq!(entries[1].5, mkfs_erofs, "length {length}");
        assert_eq!(end, archive.len(), "length {length}");
    }
    Ok(())
}

/// The archive for the converter `abc` and the tool `de`, written out field by field from the
/// format's description: the magic and thirteen eight-digit fields (inode, mode, owner, group,
/// links, time, size, device major and minor, rdev major and minor, name size, checksum), the name
/// and the content, each padded to four bytes, then the trailer.
const GOLDEN: &str = concat!(
    "070701",
    "00000001",
    "000081ED",
    "00000000",
    "00000000",
    "00000001",
    "00000000",
    "00000003",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "00000005",
    "00000000",
    "init\0\0",
    "abc\0",
    "070701",
    "00000002",
    "000081ED",
    "00000000",
    "00000000",
    "00000001",
    "00000000",
    "00000002",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "0000000B",
    "00000000",
    "mkfs.erofs\0\0\0\0",
    "de\0\0",
    "070701",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "00000001",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "00000000",
    "0000000B",
    "00000000",
    "TRAILER!!!\0\0\0\0",
);

#[test]
fn the_archive_is_byte_for_byte_the_golden_one() {
    // Owner, group and time are zero, the inodes are 1 and 2, and the padding is zeros: nothing
    // of the build host is in the bytes.
    assert_eq!(initrd(b"abc", b"de"), GOLDEN.as_bytes());
}
