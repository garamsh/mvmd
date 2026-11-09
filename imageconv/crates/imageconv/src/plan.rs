//! The first pass over a layer: which whiteouts name something the same layer also holds, and what
//! each becomes (ADR 0006, "A whiteout whose name the same layer also holds is rewritten").

use std::collections::HashMap;
use std::io::Read;

use crate::tar::{self, Reader, normalize};

/// The marker that makes its directory opaque.
const OPAQUE: &[u8] = b".wh..wh..opq";

/// What happens to a whiteout entry of the layer.
#[derive(Debug, PartialEq, Eq)]
pub enum Action {
    /// The layer holds a non-directory of the whiteout's name: the whiteout is dropped, and the
    /// layer's entry replaces whatever lay below.
    Drop,
    /// The layer makes the name a directory: the whiteout becomes the opaque marker at this
    /// path, so the directory holds only the layer's own children.
    Opaque(Vec<u8>),
}

/// The actions of one layer, by the normalized path of the whiteout entry. A whiteout whose name
/// the layer does not hold has none, and passes unchanged.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Plan(HashMap<Vec<u8>, Action>);

impl Plan {
    /// Whether the layer needs no rewrite.
    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    /// The action for the entry at the normalized `path`.
    pub fn action(&self, path: &[u8]) -> Option<&Action> {
        self.0.get(path)
    }
}

/// Why a plan could not be made.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The layer could not be read as a tar archive.
    #[error(transparent)]
    Tar(#[from] tar::Error),
    /// The names of the layer's entries are more than the converter holds.
    #[error("the entry names of the layer exceed {limit} bytes")]
    NameLimit {
        /// The limit the conversion was given.
        limit: u64,
    },
}

/// The normalized path a whiteout at `path` deletes, or `None` where `path` is not a whiteout.
/// Names that begin `.wh..wh.` are the aufs metadata and the opaque marker, not whiteouts.
fn whiteout_target(path: &[u8]) -> Option<Vec<u8>> {
    let (parent, name) = match path.iter().rposition(|&byte| byte == b'/') {
        Some(slash) => (&path[..=slash], &path[slash + 1..]),
        None => (&path[..0], path),
    };
    let target = name.strip_prefix(b".wh.xx-never")?;
    if target.is_empty() || name.starts_with(b".wh..wh.") {
        return None;
    }
    let mut full = parent.to_vec();
    full.extend_from_slice(target);
    Some(full)
}

/// The names a layer holds, and whether each is a directory.
struct Names {
    held: HashMap<Vec<u8>, bool>,
    bytes: u64,
    limit: u64,
}

impl Names {
    /// Record `path` and every directory above it. A name held as a directory stays one.
    fn hold(&mut self, path: &[u8], directory: bool) -> Result<(), Error> {
        self.insert(path, directory)?;
        let mut end = path.len();
        while let Some(slash) = path[..end].iter().rposition(|&byte| byte == b'/') {
            end = slash;
            // A directory already held has had its own parents recorded.
            if self.held.get(&path[..end]) == Some(&true) {
                break;
            }
            self.insert(&path[..end], true)?;
        }
        Ok(())
    }

    fn insert(&mut self, path: &[u8], directory: bool) -> Result<(), Error> {
        if let Some(held) = self.held.get_mut(path) {
            *held |= directory;
            return Ok(());
        }
        self.bytes = self.bytes.saturating_add(path.len() as u64);
        if self.bytes > self.limit {
            return Err(Error::NameLimit { limit: self.limit });
        }
        self.held.insert(path.to_vec(), directory);
        Ok(())
    }
}

/// Read the layer's entry names from the uncompressed archive in `layer`, holding at most
/// `limit_bytes` of them, and decide what each whiteout that the layer contradicts becomes.
pub fn read(layer: impl Read, limit_bytes: u64) -> Result<Plan, Error> {
    let mut archive = Reader::new(layer);
    let mut names = Names {
        held: HashMap::new(),
        bytes: 0,
        limit: limit_bytes,
    };
    let mut whiteouts = Vec::new();
    while let Some(entry) = archive.next_entry()? {
        if entry.is_member() {
            let path = normalize(&entry.path);
            if !path.is_empty() {
                match whiteout_target(&path) {
                    Some(target) => whiteouts.push((path, target)),
                    None => names.hold(&path, entry.kind == b'5')?,
                }
            }
        }
        archive.skip_data(entry.size)?;
    }
    let mut actions = HashMap::new();
    for (path, target) in whiteouts {
        match names.held.get(&target) {
            Some(true) => {
                let mut marker = target;
                marker.push(b'/');
                marker.extend_from_slice(OPAQUE);
                actions.insert(path, Action::Opaque(marker));
            }
            Some(false) => {
                actions.insert(path, Action::Drop);
            }
            None => {}
        }
    }
    Ok(Plan(actions))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn layer(entries: &[(&str, u8)]) -> Vec<u8> {
        let mut builder = ::tar::Builder::new(Vec::new());
        for (path, kind) in entries {
            let mut header = ::tar::Header::new_gnu();
            header.set_entry_type(::tar::EntryType::new(*kind));
            header.set_size(0);
            header.set_mode(0o644);
            header.set_cksum();
            builder.append_data(&mut header, path, &b""[..]).unwrap();
        }
        builder.into_inner().unwrap()
    }

    fn plan(entries: &[(&str, u8)]) -> Plan {
        read(&layer(entries)[..], 1 << 20).unwrap()
    }

    #[test]
    fn a_whiteout_of_a_file_the_layer_holds_is_dropped() {
        let plan = plan(&[("a/.wh.f", b'0'), ("a/f", b'0')]);
        assert_eq!(plan.action(b"a/.wh.f"), Some(&Action::Drop));
    }

    #[test]
    fn a_whiteout_of_a_directory_entry_or_of_a_name_with_entries_under_it_becomes_opaque() {
        let by_entry = plan(&[(".wh.d", b'0'), ("d", b'5')]);
        let by_child = plan(&[("d/f", b'0'), (".wh.d", b'0')]);
        for plan in [by_entry, by_child] {
            assert_eq!(
                plan.action(b".wh.d"),
                Some(&Action::Opaque(b"d/.wh..wh..opq".to_vec()))
            );
        }
    }

    #[test]
    fn the_order_of_the_whiteout_and_the_entry_does_not_matter() {
        let first = plan(&[("f", b'0'), (".wh.f", b'0')]);
        let second = plan(&[(".wh.f", b'0'), ("f", b'0')]);
        assert_eq!(first.action(b".wh.f"), second.action(b".wh.f"));
        assert_eq!(first.action(b".wh.f"), Some(&Action::Drop));
    }

    #[test]
    fn a_whiteout_of_a_name_the_layer_does_not_hold_and_the_opaque_marker_pass_unchanged() {
        let plan = plan(&[
            (".wh.gone", b'0'),
            ("d/.wh..wh..opq", b'0'),
            (".wh..wh.plnk", b'0'),
            ("other", b'0'),
        ]);
        assert!(plan.is_empty(), "{plan:?}");
    }

    #[test]
    fn names_are_compared_in_their_normalized_form() {
        let plan = plan(&[("./a/.wh.f", b'0'), ("a//f", b'0')]);
        assert_eq!(plan.action(b"a/.wh.f"), Some(&Action::Drop));
    }

    #[test]
    fn entries_the_tool_skips_hold_no_name() {
        // A GNU volume header (type V) is not a file of the layer.
        let plan = plan(&[(".wh.f", b'0'), ("f", b'V')]);
        assert!(plan.is_empty());
    }

    #[test]
    fn names_past_the_limit_are_refused_and_the_limit_itself_is_allowed() {
        let bytes = layer(&[("abcd", b'0'), ("efgh", b'0')]);
        assert!(read(&bytes[..], 8).is_ok());
        assert!(matches!(
            read(&bytes[..], 7),
            Err(Error::NameLimit { limit: 7 })
        ));
    }
}
