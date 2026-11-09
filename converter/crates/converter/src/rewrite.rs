//! The second pass over a layer that needs one: the archive again, with each whiteout the plan
//! names dropped or made the opaque marker of its directory, and every other entry as it was.

use std::io::{Read, Write};

use crate::plan::{Action, Plan};
use crate::tar::{self, Reader, empty_file_entry, normalize};

/// Write the uncompressed archive in `layer` to `out`, rewritten by `plan`.
pub fn rewrite(layer: impl Read, plan: &Plan, out: &mut impl Write) -> Result<(), tar::Error> {
    let mut archive = Reader::new(layer);
    while let Some(entry) = archive.next_entry()? {
        let action = if entry.is_member() {
            plan.action(&normalize(&entry.path))
        } else {
            None
        };
        match action {
            None => {
                out.write_all(&entry.raw)?;
                archive.copy_data(entry.size, out)?;
            }
            Some(Action::Drop) => archive.skip_data(entry.size)?,
            Some(Action::Opaque(marker)) => {
                out.write_all(&empty_file_entry(marker))?;
                archive.skip_data(entry.size)?;
            }
        }
    }
    // The end of the archive: two blocks of zeros.
    out.write_all(&[0u8; 1024])?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::plan;

    fn layer(entries: &[(&str, u8, &[u8])]) -> Vec<u8> {
        let mut builder = ::tar::Builder::new(Vec::new());
        for (path, kind, data) in entries {
            let mut header = ::tar::Header::new_gnu();
            header.set_entry_type(::tar::EntryType::new(*kind));
            header.set_size(data.len() as u64);
            header.set_mode(0o644);
            header.set_cksum();
            builder.append_data(&mut header, path, *data).unwrap();
        }
        builder.into_inner().unwrap()
    }

    fn rewritten(bytes: &[u8]) -> Vec<(String, u8, Vec<u8>)> {
        let plan = plan::read(bytes, 1 << 20).unwrap();
        let mut out = Vec::new();
        rewrite(bytes, &plan, &mut out).unwrap();
        let mut archive = ::tar::Archive::new(&out[..]);
        archive
            .entries()
            .unwrap()
            .map(|entry| {
                let mut entry = entry.unwrap();
                let path = entry.path().unwrap().to_string_lossy().into_owned();
                let kind = entry.header().entry_type().as_byte();
                let mut data = Vec::new();
                entry.read_to_end(&mut data).unwrap();
                (path, kind, data)
            })
            .collect()
    }

    #[test]
    fn a_layer_with_nothing_to_rewrite_comes_out_with_the_same_entries_and_data() {
        let bytes = layer(&[
            ("a", b'5', b""),
            ("a/f", b'0', b"data"),
            (".wh.x", b'0', b""),
        ]);
        assert_eq!(
            rewritten(&bytes),
            [
                ("a".to_owned(), b'5', vec![]),
                ("a/f".to_owned(), b'0', b"data".to_vec()),
                (".wh.x".to_owned(), b'0', vec![]),
            ]
        );
    }

    #[test]
    fn a_whiteout_of_a_held_file_is_dropped_and_the_data_after_it_is_kept() {
        let bytes = layer(&[
            (".wh.f", b'0', b""),
            ("f", b'0', b"new"),
            ("g", b'0', b"gg"),
        ]);
        assert_eq!(
            rewritten(&bytes),
            [
                ("f".to_owned(), b'0', b"new".to_vec()),
                ("g".to_owned(), b'0', b"gg".to_vec()),
            ]
        );
    }

    #[test]
    fn a_whiteout_of_a_held_directory_becomes_the_opaque_marker_in_its_place() {
        let bytes = layer(&[
            ("d/f", b'0', b"x"),
            ("p/.wh.d", b'0', b""),
            ("p/d/g", b'0', b"y"),
        ]);
        let bytes_dir = layer(&[("d/f", b'0', b"x"), (".wh.d", b'0', b"")]);
        assert_eq!(
            rewritten(&bytes_dir),
            [
                ("d/f".to_owned(), b'0', b"x".to_vec()),
                ("d/.wh..wh..opq".to_owned(), b'0', vec![]),
            ]
        );
        assert_eq!(
            rewritten(&bytes),
            [
                ("d/f".to_owned(), b'0', b"x".to_vec()),
                ("p/d/.wh..wh..opq".to_owned(), b'0', vec![]),
                ("p/d/g".to_owned(), b'0', b"y".to_vec()),
            ]
        );
    }

    #[test]
    fn an_archive_that_breaks_while_rewriting_is_an_error() {
        let bytes = layer(&[(".wh.f", b'0', b""), ("f", b'0', &[7u8; 700])]);
        let plan = plan::read(&bytes[..], 1 << 20).unwrap();
        let truncated = &bytes[..1024 + 100];
        let mut out = Vec::new();
        assert!(rewrite(truncated, &plan, &mut out).is_err());
    }
}
