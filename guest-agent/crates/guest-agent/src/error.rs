//! The failures the Guest Agent reports, one variant per kind a caller branches on.

use std::io;
use std::path::Path;

use thiserror::Error;

/// A failure of the Guest Agent's service or of its server.
#[derive(Debug, Error)]
pub enum Error {
    /// The request is malformed or names something of the wrong kind.
    #[error("invalid request: {0}")]
    Invalid(String),

    /// An offset names output the agent no longer holds; the message names the earliest it holds.
    #[error("out of range: {0}")]
    OutOfRange(String),

    /// The path or the exec ID names nothing.
    #[error("not found: {0}")]
    NotFound(String),

    /// The guest refused access to the path.
    #[error("permission denied: {0}")]
    PermissionDenied(String),

    /// The state the operation requires does not hold, and nothing changed.
    #[error("precondition failed: {0}")]
    PreconditionFailed(String),

    /// Content received does not match the digest that came with it.
    #[error("digest mismatch: {0}")]
    DigestMismatch(String),

    /// An I/O call failed for a reason the caller cannot act on.
    #[error("{context}: {source}")]
    Io {
        /// What the agent was doing.
        context: String,
        /// The failed call's error.
        source: io::Error,
    },

    /// The server stopped serving.
    #[error("serving failed: {0}")]
    Serve(Box<dyn std::error::Error + Send + Sync>),
}

impl Error {
    /// Classify an I/O error on `path`, so the kinds a caller branches on stay variants.
    pub(crate) fn from_io(context: &str, path: &Path, source: io::Error) -> Self {
        match source.kind() {
            io::ErrorKind::NotFound => Self::NotFound(path.display().to_string()),
            io::ErrorKind::PermissionDenied => Self::PermissionDenied(path.display().to_string()),
            io::ErrorKind::NotADirectory | io::ErrorKind::IsADirectory => {
                Self::Invalid(format!("{} is the wrong kind of file", path.display()))
            }
            _ => Self::Io {
                context: format!("{context} {}", path.display()),
                source,
            },
        }
    }
}
