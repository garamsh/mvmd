//! The failures of a conversion, one variant per step the report names.

use thiserror::Error;

use crate::{mkfs, plan};

/// A step of a conversion, as the report names it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Step {
    /// The start was unusable.
    Start,
    /// A layer's media type is not accepted.
    LayerType,
    /// The messages broke the call's order.
    Stream,
    /// A names pass could not read the layer.
    Names,
    /// A layer's entry names exceed the limit.
    NameLimit,
    /// A content pass could not read the layer, or `mkfs.erofs` failed on it.
    Convert,
    /// The output device or the image on it is unusable.
    Output,
}

/// Why a conversion failed.
#[derive(Debug, Error)]
pub enum Error {
    /// The start has no layers, or its filesystem UUID is not in the 8-4-4-4-12 form.
    #[error("the start is unusable: {0}")]
    Start(String),

    /// A layer's media type is not tar, tar+gzip or tar+zstd.
    #[error("layer {layer} has media type {media_type:?}, which is not tar, tar+gzip or tar+zstd")]
    LayerType {
        /// The layer's index.
        layer: u32,
        /// The media type as the client gave it.
        media_type: String,
    },

    /// The passes or their data broke the order the call requires.
    #[error("the stream broke the order of the call: {0}")]
    Stream(String),

    /// A names pass could not read the layer.
    #[error("could not read the names of layer {layer}: {reason}")]
    Names {
        /// The layer's index.
        layer: u32,
        /// What went wrong.
        reason: String,
    },

    /// A layer's entry names exceed the limit the start gave.
    #[error("the entry names of layer {layer} exceed {limit} bytes")]
    NameLimit {
        /// The layer's index.
        layer: u32,
        /// The limit the start gave.
        limit: u64,
    },

    /// A content pass could not read the layer, or `mkfs.erofs` failed on it.
    #[error("could not convert layer {layer}: {source}")]
    Convert {
        /// The layer's index.
        layer: u32,
        /// What went wrong.
        source: mkfs::Error,
    },

    /// The output device or the image on it is unusable.
    #[error("the output is unusable: {0}")]
    Output(String),
}

impl Error {
    /// The step the report names for the failure.
    pub fn step(&self) -> Step {
        match self {
            Self::Start(_) => Step::Start,
            Self::LayerType { .. } => Step::LayerType,
            Self::Stream(_) => Step::Stream,
            Self::Names { .. } => Step::Names,
            Self::NameLimit { .. } => Step::NameLimit,
            Self::Convert { .. } => Step::Convert,
            Self::Output(_) => Step::Output,
        }
    }

    /// The failure of a names pass over `layer`.
    pub(crate) fn names(layer: u32, error: plan::Error, limit: u64) -> Self {
        match error {
            plan::Error::NameLimit { .. } => Self::NameLimit { layer, limit },
            plan::Error::Tar(error) => Self::Names {
                layer,
                reason: error.to_string(),
            },
        }
    }
}
