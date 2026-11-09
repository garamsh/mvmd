//! Tar archives as the converter reads them: entry by entry, with the bytes of each kept as they
//! came so that an entry passed on is identical to the one read. What counts as an entry, a path
//! and a size follows `lib/tar.c` of erofs-utils, so the converter and `mkfs.erofs` agree on the
//! stream they both read.

use std::io::{self, Read, Write};

/// The size of a tar block.
const BLOCK: usize = 512;

/// The largest extension header (pax records or a GNU long name) the reader holds in memory.
const MAX_EXTENSION: u64 = 1 << 20;

/// Why a tar stream could not be read.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The stream could not be read.
    #[error(transparent)]
    Io(#[from] io::Error),
    /// The bytes are not a tar archive the converter can follow.
    #[error("not a valid tar archive: {0}")]
    Invalid(String),
}

fn invalid(reason: &str) -> Error {
    Error::Invalid(reason.to_owned())
}

/// One entry of an archive, apart from its data.
#[derive(Debug)]
pub struct Entry {
    /// The extension headers that belong to the entry, then its header block, as read.
    pub raw: Vec<u8>,
    /// The entry's path as the archive spells it, with any pax or GNU long name applied.
    pub path: Vec<u8>,
    /// The type flag.
    pub kind: u8,
    /// How many bytes of data follow the header, before the padding to a whole block.
    pub size: u64,
}

impl Entry {
    /// Whether `mkfs.erofs` makes a file of the entry. Other types it skips with their data.
    pub fn is_member(&self) -> bool {
        (b'0'..=b'7').contains(&self.kind)
    }
}

/// Reads the entries of a tar stream, one at a time, and the data of each.
pub struct Reader<R> {
    inner: R,
}

impl<R: Read> Reader<R> {
    /// A reader over the uncompressed archive in `inner`.
    pub fn new(inner: R) -> Self {
        Self { inner }
    }

    /// The next entry, or `None` at the end of the archive: the first block of zeros, or the end
    /// of the stream where a header would begin. The data of the entry is next in the stream and
    /// must be consumed with `copy_data` or `skip_data` before the next call.
    pub fn next_entry(&mut self) -> Result<Option<Entry>, Error> {
        let mut raw = Vec::new();
        let mut long_path: Option<Vec<u8>> = None;
        let mut pax_size: Option<u64> = None;
        loop {
            let Some(block) = self.read_block()? else {
                return if raw.is_empty() {
                    Ok(None)
                } else {
                    Err(invalid("the archive ends inside an entry's headers"))
                };
            };
            if block.iter().all(|&byte| byte == 0) {
                return if raw.is_empty() {
                    Ok(None)
                } else {
                    Err(invalid("the archive ends inside an entry's headers"))
                };
            }
            check_header(&block)?;
            let kind = block[156];
            let size = parse_number(&block[124..136])?;
            match kind {
                b'x' | b'L' | b'K' => {
                    let data = self.read_extension(size)?;
                    raw.extend_from_slice(&block);
                    raw.extend_from_slice(&data);
                    let data = &data[..usize::try_from(size).unwrap_or(data.len())];
                    match kind {
                        b'x' => {
                            for (key, value) in pax_records(data)? {
                                match key.as_slice() {
                                    b"path" => long_path = Some(value),
                                    b"size" => pax_size = Some(parse_decimal(&value)?),
                                    _ => {}
                                }
                            }
                        }
                        b'L' => long_path = Some(until_nul(data).to_vec()),
                        _ => {}
                    }
                }
                _ => {
                    raw.extend_from_slice(&block);
                    // A global header applies to what follows, and a pax path or size only to
                    // the next member; neither is the path of this entry.
                    let path = match (kind, long_path) {
                        (b'g', _) | (_, None) => header_path(&block),
                        (_, Some(path)) => path,
                    };
                    let size = if kind == b'g' {
                        size
                    } else {
                        pax_size.unwrap_or(size)
                    };
                    return Ok(Some(Entry {
                        raw,
                        path,
                        kind,
                        size,
                    }));
                }
            }
        }
    }

    /// Copy the entry's `size` bytes of data and the padding that follows them to `out`.
    pub fn copy_data(&mut self, size: u64, out: &mut impl Write) -> Result<(), Error> {
        let wanted = padded(size)?;
        let copied = io::copy(&mut (&mut self.inner).take(wanted), out)?;
        if copied == wanted {
            Ok(())
        } else {
            Err(invalid("the archive ends inside an entry's data"))
        }
    }

    /// Read past the entry's data and padding.
    pub fn skip_data(&mut self, size: u64) -> Result<(), Error> {
        self.copy_data(size, &mut io::sink())
    }

    /// The next block, or `None` where the stream ends at a block boundary.
    fn read_block(&mut self) -> Result<Option<[u8; BLOCK]>, Error> {
        let mut block = [0u8; BLOCK];
        let mut filled = 0;
        while filled < BLOCK {
            let read = self.inner.read(&mut block[filled..])?;
            if read == 0 {
                return if filled == 0 {
                    Ok(None)
                } else {
                    Err(invalid("the archive ends inside a header block"))
                };
            }
            filled += read;
        }
        Ok(Some(block))
    }

    /// The data of an extension header and its padding, as read.
    fn read_extension(&mut self, size: u64) -> Result<Vec<u8>, Error> {
        if size > MAX_EXTENSION {
            return Err(invalid(
                "an extension header is larger than the converter reads",
            ));
        }
        let mut data = Vec::new();
        self.copy_data(size, &mut data)?;
        Ok(data)
    }
}

/// `size` rounded up to whole blocks.
fn padded(size: u64) -> Result<u64, Error> {
    size.checked_next_multiple_of(BLOCK as u64)
        .ok_or_else(|| invalid("an entry's size is too large"))
}

/// Check the magic and the checksum of a header block, which `mkfs.erofs` also requires.
fn check_header(block: &[u8; BLOCK]) -> Result<(), Error> {
    if &block[257..262] != b"ustar" {
        return Err(invalid("a header does not carry the ustar magic"));
    }
    let stored = parse_number(&block[148..156])?;
    // The checksum is the sum of the header with its own field taken as spaces, over the bytes as
    // unsigned and as signed characters; writers differ.
    let unsigned: u64 = block
        .iter()
        .enumerate()
        .map(|(at, &byte)| if (148..156).contains(&at) { b' ' } else { byte })
        .map(u64::from)
        .sum();
    let signed: i64 = block
        .iter()
        .enumerate()
        .map(|(at, &byte)| if (148..156).contains(&at) { b' ' } else { byte })
        .map(|byte| i64::from(byte as i8))
        .sum();
    if stored == unsigned || i64::try_from(stored).is_ok_and(|stored| stored == signed) {
        Ok(())
    } else {
        Err(invalid("a header's checksum does not match"))
    }
}

/// A numeric header field: octal digits, or base-256 where the first byte has its high bit set.
fn parse_number(field: &[u8]) -> Result<u64, Error> {
    if field[0] & 0x80 != 0 {
        if field[0] & 0x40 != 0 {
            return Err(invalid("a header field holds a negative number"));
        }
        let mut value = u64::from(field[0] & 0x3f);
        for &byte in &field[1..] {
            value = value
                .checked_mul(256)
                .and_then(|value| value.checked_add(u64::from(byte)))
                .ok_or_else(|| invalid("a header field holds a number that is too large"))?;
        }
        return Ok(value);
    }
    let digits: Vec<u8> = field
        .iter()
        .copied()
        .skip_while(|&byte| byte == b' ' || byte == 0)
        .take_while(|&byte| byte != b' ' && byte != 0)
        .collect();
    let mut value = 0u64;
    for digit in digits {
        if !(b'0'..=b'7').contains(&digit) {
            return Err(invalid("a header field is not an octal number"));
        }
        value = value
            .checked_mul(8)
            .and_then(|value| value.checked_add(u64::from(digit - b'0')))
            .ok_or_else(|| invalid("a header field holds a number that is too large"))?;
    }
    Ok(value)
}

fn parse_decimal(text: &[u8]) -> Result<u64, Error> {
    std::str::from_utf8(text)
        .ok()
        .and_then(|text| text.parse().ok())
        .ok_or_else(|| invalid("a pax size is not a number"))
}

fn until_nul(bytes: &[u8]) -> &[u8] {
    bytes.split(|&byte| byte == 0).next().unwrap_or_default()
}

/// The path a header block spells, prefix first, as `mkfs.erofs` reads it.
fn header_path(block: &[u8; BLOCK]) -> Vec<u8> {
    let prefix = until_nul(&block[345..500]);
    let name = until_nul(&block[..100]);
    let mut path = prefix.to_vec();
    if !path.is_empty() && !path.ends_with(b"/") {
        path.push(b'/');
    }
    path.extend_from_slice(name);
    path
}

/// One record of a pax extended header: its key and its value.
type Record = (Vec<u8>, Vec<u8>);

/// The records of a pax extended header.
fn pax_records(mut data: &[u8]) -> Result<Vec<Record>, Error> {
    let mut records = Vec::new();
    while !data.is_empty() {
        let space = data
            .iter()
            .position(|&byte| byte == b' ')
            .ok_or_else(|| invalid("a pax record has no length"))?;
        let length: usize = std::str::from_utf8(&data[..space])
            .ok()
            .and_then(|text| text.parse().ok())
            .ok_or_else(|| invalid("a pax record has no length"))?;
        if length <= space + 1 || length > data.len() {
            return Err(invalid("a pax record's length is out of range"));
        }
        let record = data[space + 1..length]
            .strip_suffix(b"\n")
            .unwrap_or(&data[space + 1..length]);
        let split = record
            .iter()
            .position(|&byte| byte == b'=')
            .ok_or_else(|| invalid("a pax record has no key"))?;
        records.push((record[..split].to_vec(), record[split + 1..].to_vec()));
        data = &data[length..];
    }
    Ok(records)
}

/// The path with empty and `.` components removed and no trailing slash: the form two spellings
/// of one name share. The root is the empty path.
pub fn normalize(path: &[u8]) -> Vec<u8> {
    let mut normalized = Vec::with_capacity(path.len());
    for component in path.split(|&byte| byte == b'/') {
        if component.is_empty() || component == b"." {
            continue;
        }
        if !normalized.is_empty() {
            normalized.push(b'/');
        }
        normalized.extend_from_slice(component);
    }
    normalized
}

/// The bytes of an empty regular file entry named `path`: a GNU long name entry first where the
/// header's name field is too short. Owner, group and time are zero.
pub fn empty_file_entry(path: &[u8]) -> Vec<u8> {
    let mut entry = Vec::new();
    if path.len() >= 100 {
        let mut long = path.to_vec();
        long.push(0);
        entry.extend_from_slice(&header(b"././@LongLink", long.len() as u64, b'L'));
        entry.extend_from_slice(&long);
        entry.resize(entry.len().next_multiple_of(BLOCK), 0);
    }
    entry.extend_from_slice(&header(&path[..path.len().min(99)], 0, b'0'));
    entry
}

/// A ustar header block.
fn header(name: &[u8], size: u64, kind: u8) -> [u8; BLOCK] {
    let mut block = [0u8; BLOCK];
    block[..name.len()].copy_from_slice(name);
    block[100..108].copy_from_slice(b"0000644\0");
    block[108..116].copy_from_slice(b"0000000\0");
    block[116..124].copy_from_slice(b"0000000\0");
    block[124..136].copy_from_slice(format!("{size:011o}\0").as_bytes());
    block[136..148].copy_from_slice(b"00000000000\0");
    block[148..156].fill(b' ');
    block[156] = kind;
    block[257..265].copy_from_slice(b"ustar\x0000");
    let sum: u32 = block.iter().map(|&byte| u32::from(byte)).sum();
    block[148..156].copy_from_slice(format!("{sum:06o}\0 ").as_bytes());
    block
}

#[cfg(test)]
mod tests {
    use super::*;

    fn archive(build: impl FnOnce(&mut tar::Builder<Vec<u8>>)) -> Vec<u8> {
        let mut builder = tar::Builder::new(Vec::new());
        build(&mut builder);
        builder.into_inner().unwrap()
    }

    fn file(builder: &mut tar::Builder<Vec<u8>>, path: &str, data: &[u8]) {
        let mut header = tar::Header::new_gnu();
        header.set_entry_type(tar::EntryType::Regular);
        header.set_size(data.len() as u64);
        header.set_mode(0o644);
        header.set_cksum();
        builder.append_data(&mut header, path, data).unwrap();
    }

    #[test]
    fn entries_come_with_their_path_kind_and_size_and_their_bytes_are_kept() {
        let bytes = archive(|builder| {
            file(builder, "a/b", b"hello");
            file(builder, "c", b"");
        });
        let mut reader = Reader::new(&bytes[..]);
        let mut copied = Vec::new();

        let first = reader.next_entry().unwrap().unwrap();
        assert_eq!(
            (first.path.as_slice(), first.kind, first.size),
            (&b"a/b"[..], b'0', 5)
        );
        copied.extend_from_slice(&first.raw);
        reader.copy_data(first.size, &mut copied).unwrap();
        let second = reader.next_entry().unwrap().unwrap();
        assert_eq!((second.path.as_slice(), second.size), (&b"c"[..], 0));
        copied.extend_from_slice(&second.raw);
        reader.copy_data(second.size, &mut copied).unwrap();

        assert!(reader.next_entry().unwrap().is_none());
        assert_eq!(copied, bytes[..copied.len()]);
    }

    #[test]
    fn a_long_path_is_read_from_the_gnu_and_pax_extensions() {
        let long = format!("{}/leaf", "d".repeat(150));
        let gnu = archive(|builder| file(builder, &long, b"x"));
        let pax = archive(|builder| {
            builder
                .append_pax_extensions([("path", long.as_bytes())])
                .unwrap();
            file(builder, "short", b"x");
        });

        for bytes in [gnu, pax] {
            let mut reader = Reader::new(&bytes[..]);
            let entry = reader.next_entry().unwrap().unwrap();
            assert_eq!(entry.path, long.as_bytes());
            assert_eq!(entry.raw.len() % BLOCK, 0);
            assert!(
                entry.raw.len() > BLOCK,
                "the extension travels with the entry"
            );
        }
    }

    #[test]
    fn a_pax_size_replaces_the_size_of_the_header() {
        // The header says the file is empty; the pax record says it holds one block.
        let bytes = archive(|builder| {
            builder
                .append_pax_extensions([("size", &b"512"[..])])
                .unwrap();
            let mut header = tar::Header::new_gnu();
            header.set_entry_type(tar::EntryType::Regular);
            header.set_path("big").unwrap();
            header.set_size(0);
            header.set_cksum();
            builder.append(&header, &[7u8; 512][..]).unwrap();
            file(builder, "after", b"x");
        });
        let mut reader = Reader::new(&bytes[..]);

        let entry = reader.next_entry().unwrap().unwrap();
        assert_eq!(entry.size, 512);
        reader.skip_data(entry.size).unwrap();
        let next = reader.next_entry().unwrap().unwrap();
        assert_eq!(next.path, b"after");
    }

    #[test]
    fn a_corrupt_header_and_a_truncated_entry_are_errors_and_a_clean_end_is_not() {
        let good = archive(|builder| file(builder, "f", &[1u8; 600]));
        let mut bad_sum = good.clone();
        bad_sum[0] ^= 1;
        let truncated = good[..BLOCK + 100].to_vec();

        let mut reader = Reader::new(&bad_sum[..]);
        assert!(matches!(reader.next_entry(), Err(Error::Invalid(_))));
        let mut reader = Reader::new(&truncated[..]);
        let entry = reader.next_entry().unwrap().unwrap();
        assert!(matches!(
            reader.skip_data(entry.size),
            Err(Error::Invalid(_))
        ));
        let mut reader = Reader::new(&good[..]);
        let entry = reader.next_entry().unwrap().unwrap();
        reader.skip_data(entry.size).unwrap();
        assert!(reader.next_entry().unwrap().is_none());
        // The stream may also end at a block boundary without the zero blocks.
        let mut reader = Reader::new(&good[..BLOCK + 1024]);
        let entry = reader.next_entry().unwrap().unwrap();
        reader.skip_data(entry.size).unwrap();
        assert!(reader.next_entry().unwrap().is_none());
    }

    #[test]
    fn numbers_are_octal_or_base_256_and_overflow_is_an_error() {
        assert_eq!(parse_number(b"0000017\0").unwrap(), 15);
        assert_eq!(parse_number(b"   17 \0\0").unwrap(), 15);
        assert_eq!(
            parse_number(&[0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0]).unwrap(),
            256
        );
        assert!(parse_number(&[0x80, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]).is_err());
        assert!(
            parse_number(&[
                0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff
            ])
            .is_err()
        );
        assert!(parse_number(b"0000089\0").is_err());
    }

    #[test]
    fn normalize_drops_dot_components_and_slashes() {
        assert_eq!(normalize(b"./a//b/./c/"), b"a/b/c");
        assert_eq!(normalize(b"."), b"");
        assert_eq!(normalize(b"/a"), b"a");
    }

    #[test]
    fn the_empty_file_entry_reads_back_with_its_path_whether_short_or_long() {
        for path in [
            "d/.wh..wh..opq",
            &format!("{}/.wh..wh..opq", "e".repeat(130)),
        ] {
            let mut bytes = empty_file_entry(path.as_bytes());
            bytes.extend_from_slice(&[0u8; 1024]);

            let mut archive = tar::Archive::new(&bytes[..]);
            let entries: Vec<_> = archive.entries().unwrap().map(|e| e.unwrap()).collect();
            assert_eq!(entries.len(), 1);
            assert_eq!(entries[0].path().unwrap().to_str().unwrap(), path);
            assert_eq!(entries[0].header().size().unwrap(), 0);
            assert!(entries[0].header().entry_type().is_file());
            let mut ours = Reader::new(&bytes[..]);
            assert_eq!(ours.next_entry().unwrap().unwrap().path, path.as_bytes());
        }
    }
}
