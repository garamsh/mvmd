//! Builds the Guest Agent's initrd: a cpio archive in the kernel's "newc" format whose `init` is
//! the agent. The archive is a function of the agent's bytes alone, so two builds of one binary
//! give one digest.

/// The magic that opens every newc header.
const MAGIC: &str = "070701";

/// The regular file type bits of a mode.
const REGULAR_FILE: u32 = 0o100_000;

/// The archive holding `agent` as `init`, executable.
pub fn initrd(agent: &[u8]) -> Vec<u8> {
    let mut archive = Vec::with_capacity(agent.len() + 512);
    entry(&mut archive, 1, "init", REGULAR_FILE | 0o755, agent);
    // The trailer names no file: the kernel stops reading at it.
    entry(&mut archive, 0, "TRAILER!!!", 0, &[]);
    archive
}

/// Append one entry. Owner, group and times are zero and the inode is the caller's counter, so
/// nothing of the machine that builds the archive reaches it.
fn entry(archive: &mut Vec<u8>, inode: u32, name: &str, mode: u32, data: &[u8]) {
    // The name is stored with its terminating NUL.
    let name_size = name.len() + 1;
    let fields = [
        inode,
        mode,
        0, // owner
        0, // group
        1, // links
        0, // modification time
        u32::try_from(data.len()).unwrap_or(u32::MAX),
        0, // device major
        0, // device minor
        0, // rdev major
        0, // rdev minor
        u32::try_from(name_size).unwrap_or(u32::MAX),
        0, // checksum, unused by the "070701" format
    ];
    archive.extend(MAGIC.bytes());
    for field in fields {
        archive.extend(format!("{field:08X}").bytes());
    }
    archive.extend(name.bytes());
    archive.push(0);
    pad(archive);
    archive.extend(data);
    pad(archive);
}

/// Pad to a multiple of four bytes, as the format aligns the name and the data.
fn pad(archive: &mut Vec<u8>) {
    archive.resize(archive.len().next_multiple_of(4), 0);
}
