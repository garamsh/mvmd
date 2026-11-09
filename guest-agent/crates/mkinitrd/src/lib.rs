//! Builds the Guest Agent's initrd: the files the `initrd` library writes into an archive are the
//! agent as `init`.

/// The archive holding `agent` as `init`.
pub fn initrd(agent: &[u8]) -> Vec<u8> {
    ::initrd::initrd(&[("init", agent)])
}
