//! One conversion: each layer in manifest order, read twice, and built into the EROFS by one
//! `mkfs.erofs` run (ADR 0006).

use std::io::{self, Read, Write};
use std::path::Path;

use crate::error::Error;
use crate::media::MediaType;
use crate::mkfs::{Mkfs, Run};
use crate::plan::{self, Plan};
use crate::rewrite::rewrite;
use crate::tar;
use crate::uuid::Uuid;

/// What mvmd chose for a conversion, checked: the boundary hands on this and not the raw start.
#[derive(Debug, Clone)]
pub struct Parameters {
    uuid: Uuid,
    layers: Vec<MediaType>,
    name_limit_bytes: u64,
}

impl Parameters {
    /// The parameters of an image with one layer per entry of `layer_media_types`, in manifest
    /// order. The error names the first thing wrong, a layer type the converter does not take
    /// included, before any layer is read.
    pub fn new(
        filesystem_uuid: &str,
        layer_media_types: &[String],
        name_set_limit_bytes: u64,
    ) -> Result<Self, Error> {
        let uuid = Uuid::parse(filesystem_uuid).ok_or_else(|| {
            Error::Start(format!(
                "{filesystem_uuid:?} is not a UUID in the 8-4-4-4-12 form"
            ))
        })?;
        if layer_media_types.is_empty() {
            return Err(Error::Start("the image has no layers".into()));
        }
        let mut layers = Vec::with_capacity(layer_media_types.len());
        for (layer, media_type) in (0u32..).zip(layer_media_types) {
            layers.push(
                MediaType::parse(media_type).ok_or_else(|| Error::LayerType {
                    layer,
                    media_type: media_type.clone(),
                })?,
            );
        }
        Ok(Self {
            uuid,
            layers,
            name_limit_bytes: name_set_limit_bytes,
        })
    }
}

/// Which pass over a layer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PassKind {
    /// The names of the layer's entries are read and the layer is not kept.
    Names,
    /// The layer is converted into the EROFS.
    Content,
}

/// One pass over one layer, as the client announces it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Pass {
    /// The layer's index.
    pub layer: u32,
    /// Which pass.
    pub kind: PassKind,
}

/// The passes the client streams, each with the bytes of its layer.
pub trait Layers {
    /// Begin the next pass, leaving unread whatever remains of the previous one. `None` where
    /// the client sent no more.
    fn next_pass(&mut self) -> io::Result<Option<Pass>>;

    /// Read bytes of the current pass. Zero where the pass has ended.
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize>;
}

/// The bytes of the current pass as a `Read`.
struct PassData<'a, L>(&'a mut L);

impl<L: Layers> Read for PassData<'_, L> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        self.0.read(buf)
    }
}

/// Convert the image whose layers `layers` streams into an EROFS on `output`, and return its
/// length in bytes. `mkfs` builds it; the first layer a full image and each later one an
/// increment of it.
pub fn convert(
    parameters: &Parameters,
    layers: &mut impl Layers,
    mkfs: &Mkfs,
    output: &Path,
) -> Result<u64, Error> {
    for (index, (layer, &media)) in (0u32..).zip(&parameters.layers).enumerate() {
        begin(layers, layer, PassKind::Names)?;
        let plan = names(layers, layer, media, parameters.name_limit_bytes)?;
        begin(layers, layer, PassKind::Content)?;
        let run = Run {
            uuid: parameters.uuid,
            output,
            incremental: false,
            // With nothing to rewrite, a gzip layer reaches the tool as it came.
            ungzip: media == MediaType::TarGzip && plan.is_empty(),
        };
        mkfs.run(&run, |stdin| {
            feed(media, &plan, PassData(&mut *layers), stdin)
        })
        .map_err(|source| Error::Convert { layer, source })?;
    }
    if layers
        .next_pass()
        .map_err(|error| Error::Stream(error.to_string()))?
        .is_some()
    {
        return Err(Error::Stream("a pass follows the last layer's".into()));
    }
    crate::output::erofs_length(output, &parameters.uuid)
}

/// Require the next pass to be `kind` over `layer`.
fn begin(layers: &mut impl Layers, layer: u32, kind: PassKind) -> Result<(), Error> {
    let expected = Pass { layer, kind };
    match layers
        .next_pass()
        .map_err(|error| Error::Stream(error.to_string()))?
    {
        Some(pass) if pass == expected => Ok(()),
        Some(pass) => Err(Error::Stream(format!(
            "expected {expected:?}, the client began {pass:?}"
        ))),
        None => Err(Error::Stream(format!(
            "the stream ended before {expected:?}"
        ))),
    }
}

/// Read the current pass as a layer of `media` and plan its rewrite.
fn names(
    layers: &mut impl Layers,
    layer: u32,
    media: MediaType,
    limit: u64,
) -> Result<Plan, Error> {
    let reader = media
        .decompress(PassData(layers))
        .map_err(|error| Error::Names {
            layer,
            reason: error.to_string(),
        })?;
    plan::read(reader, limit).map_err(|error| Error::names(layer, error, limit))
}

/// Write the layer on `layer` to `stdin` as `mkfs.erofs` takes it: as it came for a plain tar
/// archive and, with nothing to rewrite, for a gzip one, which the tool filters; otherwise
/// decompressed and, where the plan has actions, rewritten.
fn feed(
    media: MediaType,
    plan: &Plan,
    mut layer: impl Read,
    stdin: &mut impl Write,
) -> io::Result<()> {
    if plan.is_empty() && media != MediaType::TarZstd {
        io::copy(&mut layer, stdin)?;
        return Ok(());
    }
    let mut plain = media.decompress(layer)?;
    if plan.is_empty() {
        io::copy(&mut plain, stdin)?;
        return Ok(());
    }
    rewrite(plain, plan, stdin).map_err(|error| match error {
        tar::Error::Io(error) => error,
        invalid @ tar::Error::Invalid(_) => {
            io::Error::new(io::ErrorKind::InvalidData, invalid.to_string())
        }
    })
}
