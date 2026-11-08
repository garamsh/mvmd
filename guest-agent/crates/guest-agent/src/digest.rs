//! Content digests in the form the contract carries them: `sha256:` and 64 lowercase hex digits.

use sha2::{Digest as _, Sha256};

use crate::error::Error;

const PREFIX: &str = "sha256:";

/// The SHA-256 of some content.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Digest(String);

impl Digest {
    /// Parse a digest from the contract's text form.
    pub(crate) fn parse(text: &str) -> Result<Self, Error> {
        let hex = text.strip_prefix(PREFIX).unwrap_or_default();
        let well_formed =
            hex.len() == 64 && hex.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'));
        if well_formed {
            Ok(Self(text.to_owned()))
        } else {
            Err(Error::Invalid(format!("malformed digest {text:?}")))
        }
    }

    /// The digest of `content`.
    pub(crate) fn of(content: &[u8]) -> Self {
        let mut hasher = Hasher::new();
        hasher.update(content);
        hasher.finish()
    }

    pub(crate) fn as_str(&self) -> &str {
        &self.0
    }
}

/// A digest computed over content that arrives in pieces.
pub(crate) struct Hasher(Sha256);

impl Hasher {
    pub(crate) fn new() -> Self {
        Self(Sha256::new())
    }

    pub(crate) fn update(&mut self, piece: &[u8]) {
        self.0.update(piece);
    }

    pub(crate) fn finish(self) -> Digest {
        let hex: String = self
            .0
            .finalize()
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect();
        Digest(format!("{PREFIX}{hex}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn digest_of_empty_content_is_the_known_sha256() {
        assert_eq!(
            Digest::of(b"").as_str(),
            "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }

    #[test]
    fn parse_accepts_what_of_produces_and_rejects_the_rest() {
        let ok = Digest::of(b"abc");
        assert_eq!(Digest::parse(ok.as_str()).unwrap(), ok);
        assert!(Digest::parse("sha256:ABC").is_err());
        assert!(Digest::parse(&ok.as_str().to_uppercase()).is_err());
        assert!(Digest::parse(&ok.as_str()[7..]).is_err());
    }
}
