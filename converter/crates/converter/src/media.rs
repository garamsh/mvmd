//! The layer media types the converter accepts, and how each is read.

use std::io::{self, BufRead, BufReader, Read};

use ruzstd::decoding::{FrameDecoder, StreamingDecoder};

/// A layer's encoding: the OCI image specification's `tar`, `tar+gzip` and `tar+zstd`,
/// distributable or not.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MediaType {
    /// An uncompressed tar archive.
    Tar,
    /// A gzip-compressed tar archive.
    TarGzip,
    /// A zstd-compressed tar archive.
    TarZstd,
}

impl MediaType {
    /// The encoding the media type names, or `None` for any other type.
    pub fn parse(media_type: &str) -> Option<Self> {
        match media_type {
            "application/vnd.oci.image.layer.v1.tar"
            | "application/vnd.oci.image.layer.nondistributable.v1.tar" => Some(Self::Tar),
            "application/vnd.oci.image.layer.v1.tar+gzip"
            | "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip" => Some(Self::TarGzip),
            "application/vnd.oci.image.layer.v1.tar+zstd"
            | "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd" => Some(Self::TarZstd),
            _ => None,
        }
    }

    /// The uncompressed tar archive in `layer`.
    pub fn decompress<'a>(self, layer: impl Read + 'a) -> io::Result<Box<dyn Read + 'a>> {
        Ok(match self {
            Self::Tar => Box::new(layer),
            Self::TarGzip => Box::new(flate2::read::MultiGzDecoder::new(layer)),
            Self::TarZstd => Box::new(ZstdFrames::new(layer)?),
        })
    }
}

fn invalid(error: impl ToString) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, error.to_string())
}

/// A zstd stream of one frame or several, read to its end.
struct ZstdFrames<R: Read> {
    /// The decoder of the current frame; `None` once the stream has ended.
    decoder: Option<StreamingDecoder<BufReader<R>, FrameDecoder>>,
}

impl<R: Read> ZstdFrames<R> {
    fn new(layer: R) -> io::Result<Self> {
        let decoder = StreamingDecoder::new(BufReader::new(layer)).map_err(invalid)?;
        Ok(Self {
            decoder: Some(decoder),
        })
    }
}

impl<R: Read> Read for ZstdFrames<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if buf.is_empty() {
            return Ok(0);
        }
        loop {
            let Some(decoder) = self.decoder.as_mut() else {
                return Ok(0);
            };
            let read = decoder.read(buf)?;
            if read > 0 {
                return Ok(read);
            }
            // The frame has ended; another follows where the stream has more bytes.
            let Some(finished) = self.decoder.take() else {
                return Ok(0);
            };
            let (mut source, frame_decoder) = finished.into_parts();
            if source.fill_buf()?.is_empty() {
                return Ok(0);
            }
            self.decoder =
                Some(StreamingDecoder::new_with_decoder(source, frame_decoder).map_err(invalid)?);
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io::Write;

    use super::*;

    fn read_all(media: MediaType, bytes: &[u8]) -> io::Result<Vec<u8>> {
        let mut out = Vec::new();
        media.decompress(bytes)?.read_to_end(&mut out)?;
        Ok(out)
    }

    #[test]
    fn the_six_oci_layer_types_are_accepted_and_others_are_not() {
        let accepted = [
            ("application/vnd.oci.image.layer.v1.tar", MediaType::Tar),
            (
                "application/vnd.oci.image.layer.v1.tar+gzip",
                MediaType::TarGzip,
            ),
            (
                "application/vnd.oci.image.layer.v1.tar+zstd",
                MediaType::TarZstd,
            ),
            (
                "application/vnd.oci.image.layer.nondistributable.v1.tar",
                MediaType::Tar,
            ),
            (
                "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip",
                MediaType::TarGzip,
            ),
            (
                "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd",
                MediaType::TarZstd,
            ),
        ];
        for (text, media) in accepted {
            assert_eq!(MediaType::parse(text), Some(media), "{text}");
        }
        for text in [
            "",
            "application/vnd.docker.image.rootfs.diff.tar.gzip",
            "application/vnd.oci.image.layer.v1.tar+xz",
            "application/vnd.oci.image.layer.v1.tar+gzip ",
        ] {
            assert_eq!(MediaType::parse(text), None, "{text:?}");
        }
    }

    #[test]
    fn gzip_reads_every_member_of_the_stream() {
        let mut bytes = Vec::new();
        for part in [&b"first "[..], b"second"] {
            let mut encoder =
                flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
            encoder.write_all(part).unwrap();
            bytes.extend(encoder.finish().unwrap());
        }
        assert_eq!(
            read_all(MediaType::TarGzip, &bytes).unwrap(),
            b"first second"
        );
    }

    /// A zstd frame holding `text` as one raw block, written from the format's description.
    fn zstd_frame(text: &[u8]) -> Vec<u8> {
        let mut frame = vec![0x28, 0xb5, 0x2f, 0xfd]; // magic
        frame.push(0x20); // frame header: single segment, no checksum, no dictionary
        frame.push(u8::try_from(text.len()).unwrap()); // content size, one byte
        let header = (u32::try_from(text.len()).unwrap() << 3) | 1; // raw block, last
        frame.extend_from_slice(&header.to_le_bytes()[..3]);
        frame.extend_from_slice(text);
        frame
    }

    #[test]
    fn zstd_reads_what_the_frame_holds_and_refuses_what_is_not_zstd() {
        assert_eq!(
            read_all(MediaType::TarZstd, &zstd_frame(b"hello")).unwrap(),
            b"hello"
        );
        assert!(read_all(MediaType::TarZstd, b"not a zstd stream at all").is_err());
    }

    #[test]
    fn zstd_reads_frames_that_follow_one_another() {
        let mut bytes = zstd_frame(b"one ");
        bytes.extend(zstd_frame(b"two"));
        assert_eq!(read_all(MediaType::TarZstd, &bytes).unwrap(), b"one two");
    }
}
