//! Filesystem UUIDs, and where each filesystem the guest mounts keeps its UUID in its superblock.

use std::fmt;

use crate::error::Error;

/// A filesystem UUID.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Uuid([u8; 16]);

impl Uuid {
    /// Parse the canonical form: 32 hexadecimal digits in groups of 8, 4, 4, 4 and 12 digits.
    pub fn parse(text: &str) -> Result<Self, Error> {
        let invalid = || Error::Invalid(format!("{text:?} is not a UUID in the 8-4-4-4-12 form"));
        let digits = text.as_bytes();
        if digits.len() != 36 {
            return Err(invalid());
        }
        let mut bytes = [0u8; 16];
        let mut nibbles = 0;
        for (at, &digit) in digits.iter().enumerate() {
            if matches!(at, 8 | 13 | 18 | 23) {
                if digit != b'-' {
                    return Err(invalid());
                }
                continue;
            }
            let value = char::from(digit).to_digit(16).ok_or_else(invalid)?;
            let byte = &mut bytes[nibbles / 2];
            *byte = (*byte << 4) | u8::try_from(value).map_err(|_| invalid())?;
            nibbles += 1;
        }
        Ok(Self(bytes))
    }
}

impl fmt::Display for Uuid {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        for (at, byte) in self.0.iter().enumerate() {
            if matches!(at, 4 | 6 | 8 | 10) {
                write!(f, "-")?;
            }
            write!(f, "{byte:02x}")?;
        }
        Ok(())
    }
}

/// A filesystem the guest mounts, and so one it can recognise on a block device.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Filesystem {
    /// The MachineImage, mounted read-only as the overlay's lower layer.
    Erofs,
    /// The root disk, the overlay's upper layer.
    Ext4,
}

/// How many leading bytes of a device `Filesystem::uuid_in` reads.
pub const SUPERBLOCK_PROBE_BYTES: usize = 2048;

const EROFS_MAGIC: [u8; 4] = 0xE0F5_E1E2u32.to_le_bytes();
const EXT4_MAGIC: [u8; 2] = 0xEF53u16.to_le_bytes();

impl Filesystem {
    /// The UUID of the filesystem whose superblock is in `head`, the first
    /// `SUPERBLOCK_PROBE_BYTES` of a device, or `None` when the device holds another filesystem.
    pub fn uuid_in(self, head: &[u8]) -> Option<Uuid> {
        // The superblock of both starts at byte 1024: EROFS keeps its magic first and its UUID
        // at offset 48; ext4 keeps its magic at offset 56 and its UUID at offset 104.
        let (magic_at, magic, uuid_at): (usize, &[u8], usize) = match self {
            Self::Erofs => (1024, &EROFS_MAGIC, 1024 + 48),
            Self::Ext4 => (1024 + 56, &EXT4_MAGIC, 1024 + 104),
        };
        if head.get(magic_at..magic_at + magic.len())? != magic {
            return None;
        }
        let uuid: [u8; 16] = head.get(uuid_at..uuid_at + 16)?.try_into().ok()?;
        Some(Uuid(uuid))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TEXT: &str = "0123abcd-4567-89ab-cdef-0123456789ab";

    #[test]
    fn parse_reads_the_canonical_form_and_displays_it_back() {
        let uuid = Uuid::parse(TEXT).unwrap();
        assert_eq!(uuid.to_string(), TEXT);
        assert_eq!(Uuid::parse(&TEXT.to_uppercase()).unwrap(), uuid);
    }

    #[test]
    fn parse_refuses_what_is_not_the_canonical_form() {
        for text in [
            "",
            "0123abcd45678 9abcdef0123456789ab",
            "0123abcd456789abcdef0123456789ab",
            "0123abcd-4567-89ab-cdef-0123456789a",
            "0123abcd-4567-89ab-cdef-0123456789abc",
            "0123abcd-4567-89ab-cdef-0123456789ag",
            "0123abcd_4567-89ab-cdef-0123456789ab",
        ] {
            assert!(Uuid::parse(text).is_err(), "{text:?} was accepted");
        }
    }

    fn head(magic_at: usize, magic: &[u8], uuid_at: usize, uuid: &Uuid) -> Vec<u8> {
        let mut head = vec![0; SUPERBLOCK_PROBE_BYTES];
        head[magic_at..magic_at + magic.len()].copy_from_slice(magic);
        head[uuid_at..uuid_at + 16].copy_from_slice(&uuid.0);
        head
    }

    #[test]
    fn each_filesystem_reads_its_uuid_from_its_own_superblock() {
        let uuid = Uuid::parse(TEXT).unwrap();
        let erofs = head(1024, &EROFS_MAGIC, 1072, &uuid);
        let ext4 = head(1080, &EXT4_MAGIC, 1128, &uuid);
        assert_eq!(Filesystem::Erofs.uuid_in(&erofs), Some(uuid));
        assert_eq!(Filesystem::Ext4.uuid_in(&ext4), Some(uuid));
        // The control: the other filesystem's reading of the same bytes finds no UUID.
        assert_eq!(Filesystem::Ext4.uuid_in(&erofs), None);
        assert_eq!(Filesystem::Erofs.uuid_in(&ext4), None);
    }

    #[test]
    fn a_device_shorter_than_the_probe_has_no_uuid() {
        assert_eq!(Filesystem::Erofs.uuid_in(&[0; 100]), None);
        assert_eq!(Filesystem::Ext4.uuid_in(&[]), None);
    }
}
