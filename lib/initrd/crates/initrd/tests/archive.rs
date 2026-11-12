//! The initrd archive, read back with a reader written from the newc format's description and
//! not from the writer.

use initrd::{Error, initrd};

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
fn a_single_file_is_an_executable_entry_and_the_archive_ends_at_its_trailer() -> Result {
    let init = b"\x7fELF and some more bytes";
    let archive = initrd(&[("init", init)])?;

    let (entries, end) = read(&archive)?;

    assert_eq!(
        entries,
        [("init".to_owned(), 0o100_755, 0, 0, 0, init.to_vec())]
    );
    assert_eq!(end, archive.len());
    assert_eq!(archive.len() % 4, 0);
    Ok(())
}

#[test]
fn files_come_out_in_the_order_given_each_with_its_own_name() -> Result {
    let archive = initrd(&[("init", b"one"), ("mkfs.erofs", b"two"), ("etc", b"")])?;

    let (entries, end) = read(&archive)?;

    assert_eq!(
        entries,
        [
            ("init".to_owned(), 0o100_755, 0, 0, 0, b"one".to_vec()),
            ("mkfs.erofs".to_owned(), 0o100_755, 0, 0, 0, b"two".to_vec()),
            ("etc".to_owned(), 0o100_755, 0, 0, 0, Vec::new()),
        ]
    );
    assert_eq!(end, archive.len());
    Ok(())
}

#[test]
fn files_of_any_length_keep_the_alignment_and_their_content() -> Result {
    for length in 0..9 {
        let first: Vec<u8> = (0..length).map(|byte| byte + 1).collect();
        let second: Vec<u8> = (0..8 - length).map(|byte| byte + 100).collect();
        let archive = initrd(&[("init", &first), ("tool", &second)])?;
        let (entries, end) = read(&archive)?;
        assert_eq!(entries[0].5, first, "length {length}");
        assert_eq!(entries[1].5, second, "length {length}");
        assert_eq!(end, archive.len(), "length {length}");
    }
    Ok(())
}

/// The entry for the file `abc` named `init`, written out field by field from the format's
/// description: the magic and thirteen eight-digit fields (inode, mode, owner, group, links, time,
/// size, device major and minor, rdev major and minor, name size, checksum), the name and the
/// content, each padded to four bytes.
const INIT: &str = concat!(
    "070701", "00000001", "000081ED", "00000000", "00000000", "00000001", "00000000", "00000003",
    "00000000", "00000000", "00000000", "00000000", "00000005", "00000000", "init\0\0", "abc\0",
);

/// The entry for the file `de` named `mkfs.erofs`, the second of an archive.
const TOOL: &str = concat!(
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
);

/// The trailer that ends every archive.
const TRAILER: &str = concat!(
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
fn one_file_is_byte_for_byte_the_golden_archive() -> Result {
    // Owner, group and time are zero, the inode is 1, and the padding is zeros: nothing of the
    // build host is in the bytes.
    assert_eq!(
        initrd(&[("init", b"abc")])?,
        [INIT, TRAILER].concat().as_bytes()
    );
    Ok(())
}

#[test]
fn two_files_are_byte_for_byte_the_golden_archive() -> Result {
    // The inodes count from 1 in the order given.
    assert_eq!(
        initrd(&[("init", b"abc"), ("mkfs.erofs", b"de")])?,
        [INIT, TOOL, TRAILER].concat().as_bytes()
    );
    Ok(())
}

/// The error a refused file gives, with the archive of the files before it dropped.
fn refusal(files: &[(&str, &[u8])]) -> Result<Error> {
    match initrd(files) {
        Ok(archive) => Err(format!("accepted, {} bytes", archive.len()).into()),
        Err(error) => Ok(error),
    }
}

#[test]
fn a_name_the_header_cannot_record_is_refused_with_the_name_and_a_plain_one_is_not() -> Result {
    assert!(matches!(refusal(&[("", b"x")])?, Error::EmptyName));
    assert!(
        matches!(refusal(&[("/init", b"x")])?, Error::LeadingSlash { name } if name == "/init")
    );
    assert!(matches!(refusal(&[("in\0it", b"x")])?, Error::NulInName { name } if name == "in\0it"));
    // A later file is refused as much as the first, and the archive is not returned.
    assert!(matches!(
        refusal(&[("init", b"x"), ("/tool", b"y")])?,
        Error::LeadingSlash { .. }
    ));
    // Controls: a slash inside a name, a dot and a leading dot are names the kernel unpacks.
    let (entries, end) = read(&initrd(&[("etc/init", b"x"), (".hidden", b"y")])?)?;
    assert_eq!(entries[0].0, "etc/init");
    assert_eq!(entries[1].0, ".hidden");
    assert!(end > 0);
    Ok(())
}
