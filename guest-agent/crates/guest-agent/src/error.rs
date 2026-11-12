//! The failures the Guest Agent reports, one variant per kind a caller branches on.

use std::fmt;
use std::io;
use std::path::Path;

use thiserror::Error;

/// A failure of the Guest Agent's service or of its server.
#[derive(Debug, Error)]
pub enum Error {
    /// The request is malformed or names something of the wrong kind.
    #[error("invalid request: {0}")]
    Invalid(String),

    /// A path names a file of the wrong kind for the call.
    #[error("invalid request: {path} is {}", .expected.wrong_kind())]
    WrongKind {
        /// The path the request named.
        path: String,
        /// The kind the call needs.
        expected: Expected,
    },

    /// An offset names output the agent no longer holds.
    #[error(
        "out of range: output before offset {earliest_offset} is no longer held; the earliest held offset is {earliest_offset}"
    )]
    OutOfRange {
        /// The offset of the earliest byte still held.
        earliest_offset: u64,
    },

    /// The path or the exec ID names nothing.
    #[error("not found: {0}")]
    NotFound(String),

    /// The guest refused access to the path.
    #[error("permission denied: {0}")]
    PermissionDenied(String),

    /// The state the operation requires does not hold, and nothing changed.
    #[error("precondition failed: {0}")]
    PreconditionFailed(String),

    /// The call needs a guest that has booted, and it has not.
    #[error("precondition failed: {reason}")]
    NotBooted {
        /// Whether the boot is yet to settle or has failed.
        state: Unbooted,
        /// Why the call is refused, as text for people.
        reason: String,
    },

    /// A disk's device, or a partition of it, is mounted or held, or the filesystem the agent
    /// mounted for it is busy, so it is not released.
    #[error(
        "precondition failed: disk {disk_id} is in use{}",
        mount.as_deref().map(|path| format!(": the filesystem mounted at {path} is busy")).unwrap_or_default()
    )]
    DiskInUse {
        /// The disk the call named.
        disk_id: String,
        /// The path of the busy filesystem the agent mounted for it, if that is why.
        mount: Option<String>,
    },

    /// Content received does not match the digest that came with it.
    #[error("digest mismatch: {0}")]
    DigestMismatch(String),

    /// A step of the boot failed. The guest stays as the step left it, and mvmd stops the Machine.
    #[error("boot step {step}: {detail}")]
    Boot {
        /// The step that failed.
        step: BootStep,
        /// Why it failed.
        detail: String,
    },

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

/// The kind of file a call needs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Expected {
    /// A regular file.
    File,
    /// A directory.
    Directory,
}

impl Expected {
    /// What the path is instead, for the message.
    fn wrong_kind(self) -> &'static str {
        match self {
            Self::File => "not a regular file",
            Self::Directory => "not a directory",
        }
    }
}

/// How a guest that has not booted stands.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Unbooted {
    /// `Boot` has not been called, or its steps are still running.
    Pending,
    /// A step failed.
    Failed,
}

/// A step of the boot, in the order the agent takes them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BootStep {
    /// Setting the wall clock from the host's time, before any other step.
    Clock,
    /// Finding the image and the root disk by filesystem UUID.
    Check,
    /// Mounting the image read-only.
    MountImage,
    /// Mounting the root disk.
    MountRootDisk,
    /// Overlaying the root disk on the image.
    Overlay,
    /// Making the overlay the root.
    SwitchRoot,
    /// Configuring the interface, the routes and the resolvers.
    Network,
    /// Placing the files over the booted root.
    PlaceFiles,
    /// Mounting the disks the Machine was created with, over the booted root.
    MountDisks,
    /// Resolving the entrypoint's user in the booted root.
    ApplyUser,
    /// Starting the entrypoint.
    Entrypoint,
}

impl fmt::Display for BootStep {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Clock => "set the wall clock",
            Self::Check => "check the devices",
            Self::MountImage => "mount the image",
            Self::MountRootDisk => "mount the root disk",
            Self::Overlay => "overlay the root disk",
            Self::SwitchRoot => "switch root",
            Self::Network => "configure the network",
            Self::PlaceFiles => "place the files",
            Self::MountDisks => "mount the disks",
            Self::ApplyUser => "apply the user",
            Self::Entrypoint => "start the entrypoint",
        })
    }
}

impl Error {
    /// Classify an I/O error on `path`, so the kinds a caller branches on stay variants.
    pub(crate) fn from_io(context: &str, path: &Path, source: io::Error) -> Self {
        match source.kind() {
            io::ErrorKind::NotFound => Self::NotFound(path.display().to_string()),
            io::ErrorKind::PermissionDenied => Self::PermissionDenied(path.display().to_string()),
            io::ErrorKind::NotADirectory => Self::WrongKind {
                path: path.display().to_string(),
                expected: Expected::Directory,
            },
            io::ErrorKind::IsADirectory => Self::WrongKind {
                path: path.display().to_string(),
                expected: Expected::File,
            },
            _ => Self::Io {
                context: format!("{context} {}", path.display()),
                source,
            },
        }
    }
}
