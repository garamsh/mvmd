//! The EROFS's filesystem UUID.

use std::fmt;

/// A filesystem UUID, parsed from the 8-4-4-4-12 form.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Uuid([u8; 16]);

impl Uuid {
    /// Parse the canonical form: 32 hexadecimal digits in groups of 8, 4, 4, 4 and 12 digits.
    /// `None` for any other text, so only a UUID reaches `mkfs.erofs`'s command line.
    pub fn parse(text: &str) -> Option<Self> {
        let digits = text.as_bytes();
        if digits.len() != 36 {
            return None;
        }
        let mut bytes = [0u8; 16];
        let mut nibbles = 0;
        for (at, &digit) in digits.iter().enumerate() {
            if matches!(at, 8 | 13 | 18 | 23) {
                if digit != b'-' {
                    return None;
                }
                continue;
            }
            let value = u8::try_from(char::from(digit).to_digit(16)?).ok()?;
            let byte = &mut bytes[nibbles / 2];
            *byte = (*byte << 4) | value;
            nibbles += 1;
        }
        Some(Self(bytes))
    }

    /// The 16 bytes, as a filesystem superblock stores them.
    pub fn bytes(&self) -> [u8; 16] {
        self.0
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_canonical_form_round_trips_in_lower_case() {
        let uuid = Uuid::parse("0123ABCD-4567-89ab-cdef-0123456789AB").unwrap();
        assert_eq!(uuid.to_string(), "0123abcd-4567-89ab-cdef-0123456789ab");
        assert_eq!(uuid.bytes()[..4], [0x01, 0x23, 0xab, 0xcd]);
    }

    #[test]
    fn anything_else_is_refused() {
        for text in [
            "",
            "0123abcd-4567-89ab-cdef-0123456789a",
            "0123abcd456789abcdef0123456789abcdef",
            "0123abcd-4567-89ab-cdef-0123456789ag",
            "-0123abcd-4567-89ab-cdef-01234567890",
            "--tar=f",
        ] {
            assert_eq!(Uuid::parse(text), None, "{text:?}");
        }
    }
}
