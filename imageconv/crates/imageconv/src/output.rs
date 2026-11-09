//! The EROFS on the output: its length, read from its superblock.

use std::fs::File;
use std::os::unix::fs::FileExt;
use std::path::Path;

use crate::error::Error;
use crate::uuid::Uuid;

/// Where the superblock starts.
const SUPERBLOCK_OFFSET: u64 = 1024;
/// The bytes of the superblock this reads: up to and including its feature flags.
const SUPERBLOCK_READ: usize = 84;
const MAGIC: u32 = 0xE0F5_E1E2;
/// The feature flag that adds the high bits of the block count.
const INCOMPAT_48BIT: u32 = 0x80;

fn le32(bytes: &[u8], at: usize) -> u32 {
    u32::from_le_bytes([bytes[at], bytes[at + 1], bytes[at + 2], bytes[at + 3]])
}

/// The length in bytes of the EROFS at `output`, which must carry `uuid`.
pub fn erofs_length(output: &Path, uuid: &Uuid) -> Result<u64, Error> {
    let mut head = [0u8; SUPERBLOCK_READ];
    File::open(output)
        .and_then(|file| file.read_exact_at(&mut head, SUPERBLOCK_OFFSET))
        .map_err(|error| Error::Output(format!("the superblock cannot be read: {error}")))?;
    length_from(&head, uuid)
}

/// The length the superblock bytes in `head` state, checked against `uuid`.
fn length_from(head: &[u8; SUPERBLOCK_READ], uuid: &Uuid) -> Result<u64, Error> {
    if le32(head, 0) != MAGIC {
        return Err(Error::Output("there is no EROFS superblock".into()));
    }
    if head[48..64] != uuid.bytes() {
        return Err(Error::Output(
            "the image carries another filesystem UUID than the one asked for".into(),
        ));
    }
    let block_bits = u32::from(head[12]);
    let mut blocks = u64::from(le32(head, 36));
    if le32(head, 80) & INCOMPAT_48BIT != 0 {
        blocks |= u64::from(u16::from_le_bytes([head[14], head[15]])) << 32;
    }
    if !(9..=16).contains(&block_bits) {
        return Err(Error::Output(format!(
            "the block size of 2^{block_bits} bytes is out of range"
        )));
    }
    Ok(blocks << block_bits)
}

#[cfg(test)]
mod tests {
    use super::*;

    const UUID: &str = "11111111-2222-3333-4444-555555555555";

    /// A superblock head written from the layout in `include/erofs_fs.h`.
    fn head(
        block_bits: u8,
        blocks_lo: u32,
        blocks_hi: u16,
        incompat: u32,
    ) -> [u8; SUPERBLOCK_READ] {
        let mut head = [0u8; SUPERBLOCK_READ];
        head[0..4].copy_from_slice(&MAGIC.to_le_bytes());
        head[12] = block_bits;
        head[14..16].copy_from_slice(&blocks_hi.to_le_bytes());
        head[36..40].copy_from_slice(&blocks_lo.to_le_bytes());
        head[48..64].copy_from_slice(&Uuid::parse(UUID).unwrap().bytes());
        head[80..84].copy_from_slice(&incompat.to_le_bytes());
        head
    }

    #[test]
    fn the_length_is_the_block_count_times_the_block_size() {
        let uuid = Uuid::parse(UUID).unwrap();
        assert_eq!(length_from(&head(12, 3, 0, 0), &uuid).unwrap(), 3 * 4096);
        assert_eq!(length_from(&head(9, 5, 0, 0), &uuid).unwrap(), 5 * 512);
    }

    #[test]
    fn the_high_block_bits_count_only_with_the_48_bit_feature() {
        let uuid = Uuid::parse(UUID).unwrap();
        assert_eq!(length_from(&head(12, 1, 2, 0), &uuid).unwrap(), 4096);
        assert_eq!(
            length_from(&head(12, 1, 2, INCOMPAT_48BIT), &uuid).unwrap(),
            ((2u64 << 32) | 1) * 4096
        );
    }

    #[test]
    fn a_missing_superblock_another_uuid_and_a_wild_block_size_are_refused() {
        let uuid = Uuid::parse(UUID).unwrap();
        let other = Uuid::parse("11111111-2222-3333-4444-555555555556").unwrap();
        let mut no_magic = head(12, 1, 0, 0);
        no_magic[0] ^= 1;
        assert!(length_from(&no_magic, &uuid).is_err());
        assert!(length_from(&head(12, 1, 0, 0), &other).is_err());
        assert!(length_from(&head(40, 1, 0, 0), &uuid).is_err());
        // The control: the same bytes with the right UUID and a sane block size are accepted.
        assert!(length_from(&head(12, 1, 0, 0), &uuid).is_ok());
    }
}
