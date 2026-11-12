//! Writes an initrd: a cpio archive in the kernel's "newc" format. The archive is a function of
//! the files' names and bytes alone, so two builds of the same files give one digest.

use thiserror::Error;

/// The magic that opens every newc header.
const MAGIC: &str = "070701";

/// The regular file type bits of a mode.
const REGULAR_FILE: u32 = 0o100_000;

/// Why a file cannot be written into the archive: its header would disagree with the bytes that
/// follow it, and the kernel would read a different archive.
#[derive(Debug, Error)]
#[error(transparent)]
pub struct Error(Refusal);

/// The refusals an [`Error`] holds.
#[derive(Debug, Error)]
enum Refusal {
    /// The name is empty.
    #[error("a file has an empty name")]
    EmptyName,
    /// The name starts with a slash: the kernel unpacks the archive under `/` itself.
    #[error("the name {name:?} starts with a slash")]
    LeadingSlash { name: String },
    /// The name holds a NUL, which would end the name the header records.
    #[error("the name {name:?} contains a NUL")]
    NulInName { name: String },
    /// The name and its NUL do not fit the header's 32-bit name size.
    #[error(
        "the name {name:?} is {size} bytes with its NUL, over the 32-bit name size of a header"
    )]
    NameTooLong { name: String, size: usize },
    /// The content does not fit the header's 32-bit file size.
    #[error("the file {name:?} is {size} bytes, over the 32-bit file size of a header")]
    FileTooLarge { name: String, size: usize },
}

/// The archive holding each `(name, content)` of `files`, in order, as an executable regular file.
/// A name is not empty, has no leading slash (the kernel unpacks `init` as `/init`) and no NUL, and
/// its length with a NUL and the content's length each fit 32 bits; any other file is refused.
pub fn initrd(files: &[(&str, &[u8])]) -> Result<Vec<u8>, Error> {
    build(files).map_err(Error)
}

/// The archive of [`initrd`], with the refusal unwrapped.
fn build(files: &[(&str, &[u8])]) -> Result<Vec<u8>, Refusal> {
    let mut archive = Vec::with_capacity(files.iter().map(|(_, data)| data.len() + 512).sum());
    for (inode, (name, data)) in (1..).zip(files) {
        check_name(name)?;
        entry(&mut archive, inode, name, REGULAR_FILE | 0o755, data)?;
    }
    // The trailer names no file: the kernel stops reading at it.
    entry(&mut archive, 0, "TRAILER!!!", 0, &[])?;
    Ok(archive)
}

/// Refuse a name the header cannot record as written.
fn check_name(name: &str) -> Result<(), Refusal> {
    if name.is_empty() {
        Err(Refusal::EmptyName)
    } else if name.starts_with('/') {
        Err(Refusal::LeadingSlash {
            name: name.to_owned(),
        })
    } else if name.contains('\0') {
        Err(Refusal::NulInName {
            name: name.to_owned(),
        })
    } else {
        Ok(())
    }
}

/// The header's name size and file size for a name of `name_size` bytes with its NUL and a content
/// of `data_len` bytes, or the refusal. The lengths are arguments so a test reaches the limit
/// without a file of 4 GiB.
fn sizes(name: &str, name_size: usize, data_len: usize) -> Result<(u32, u32), Refusal> {
    let name_field = u32::try_from(name_size).map_err(|_| Refusal::NameTooLong {
        name: name.to_owned(),
        size: name_size,
    })?;
    let size_field = u32::try_from(data_len).map_err(|_| Refusal::FileTooLarge {
        name: name.to_owned(),
        size: data_len,
    })?;
    Ok((name_field, size_field))
}

/// Append one entry. Owner, group and times are zero and the inode is the caller's counter, so
/// nothing of the machine that builds the archive reaches it. A refusal appends nothing.
fn entry(
    archive: &mut Vec<u8>,
    inode: u32,
    name: &str,
    mode: u32,
    data: &[u8],
) -> Result<(), Refusal> {
    // The name is stored with its terminating NUL.
    let (name_size, size) = sizes(name, name.len() + 1, data.len())?;
    let fields = [
        inode, mode, 0, // owner
        0, // group
        1, // links
        0, // modification time
        size, 0, // device major
        0, // device minor
        0, // rdev major
        0, // rdev minor
        name_size, 0, // checksum, unused by the "070701" format
    ];
    archive.extend(MAGIC.bytes());
    for field in fields {
        archive.extend(format!("{field:08X}").bytes());
    }
    archive.extend(name.bytes());
    archive.push(0);
    pad(archive);
    archive.extend(data);
    pad(archive);
    Ok(())
}

/// Pad to a multiple of four bytes, as the format aligns the name and the data.
fn pad(archive: &mut Vec<u8>) {
    archive.resize(archive.len().next_multiple_of(4), 0);
}

#[cfg(test)]
mod tests {
    use super::*;

    type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

    /// The largest length a header's 32-bit field holds, as a length.
    fn limit() -> Result<usize> {
        Ok(usize::try_from(u32::MAX)?)
    }

    /// The refusal `build` gives for `files`, or a description of the archive it accepted.
    fn refusal(files: &[(&str, &[u8])]) -> Result<Refusal> {
        match build(files) {
            Ok(archive) => Err(format!("accepted, {} bytes", archive.len()).into()),
            Err(refusal) => Ok(refusal),
        }
    }

    #[test]
    fn a_name_the_header_cannot_record_is_refused_with_the_name_and_a_plain_one_is_not() -> Result {
        assert!(matches!(refusal(&[("", b"x")])?, Refusal::EmptyName));
        assert!(
            matches!(refusal(&[("/init", b"x")])?, Refusal::LeadingSlash { name } if name == "/init")
        );
        assert!(
            matches!(refusal(&[("in\0it", b"x")])?, Refusal::NulInName { name } if name == "in\0it")
        );
        // A later file is refused as much as the first.
        assert!(matches!(
            refusal(&[("init", b"x"), ("/tool", b"y")])?,
            Refusal::LeadingSlash { .. }
        ));
        // Controls: a slash inside a name and a leading dot are names the kernel unpacks.
        assert!(build(&[("etc/init", b"x"), (".hidden", b"y")]).is_ok());
        Ok(())
    }

    #[test]
    fn a_name_size_over_32_bits_is_refused_and_the_largest_one_is_not() -> Result {
        let over = limit()? + 1;

        assert_eq!(sizes("n", limit()?, 0)?, (u32::MAX, 0));
        let refusal = sizes("n", over, 0).err();
        assert!(
            matches!(&refusal, Some(Refusal::NameTooLong { name, size }) if name == "n" && *size == over),
            "{refusal:?}"
        );
        Ok(())
    }

    #[test]
    fn a_file_size_over_32_bits_is_refused_and_the_largest_one_is_not() -> Result {
        let over = limit()? + 1;

        assert_eq!(sizes("n", 2, limit()?)?, (2, u32::MAX));
        let refusal = sizes("n", 2, over).err();
        assert!(
            matches!(&refusal, Some(Refusal::FileTooLarge { name, size }) if name == "n" && *size == over),
            "{refusal:?}"
        );
        Ok(())
    }
}
