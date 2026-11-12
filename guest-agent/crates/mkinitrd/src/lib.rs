//! Builds the Guest Agent's initrd: the files the `initrd` library writes into an archive are the
//! agent as `init`.

/// A file the initrd cannot hold: the `initrd` library refused it.
#[derive(Debug, thiserror::Error)]
#[error(transparent)]
pub struct Error(initrd::Error);

/// The archive holding `agent` as `init`.
pub fn initrd(agent: &[u8]) -> Result<Vec<u8>, Error> {
    ::initrd::initrd(&[("init", agent)]).map_err(Error)
}
