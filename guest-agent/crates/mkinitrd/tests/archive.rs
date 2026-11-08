//! The initrd archive, read back with a reader written from the newc format's description and
//! not from the writer.

use mkinitrd::initrd;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

/// One entry as the kernel reads it: name, mode, owner, group, modification time and content.
type Entry = (String, u32, u32, u32, u32, Vec<u8>);

fn field(archive: &[u8], at: usize, index: usize) -> Result<u32> {
    let start = at + 6 + index * 8;
    let text = std::str::from_utf8(&archive[start..start + 8])?;
    Ok(u32::from_str_radix(text, 16)?)
}

fn align(offset: usize) -> usize {
    (offset + 3) & !3
}

/// Every entry before the trailer, and the offset where the archive ends.
fn read(archive: &[u8]) -> Result<(Vec<Entry>, usize)> {
    let mut entries = Vec::new();
    let mut at = 0;
    loop {
        assert_eq!(&archive[at..at + 6], b"070701", "header at {at}");
        let (mode, owner, group, mtime) = (
            field(archive, at, 1)?,
            field(archive, at, 2)?,
            field(archive, at, 3)?,
            field(archive, at, 5)?,
        );
        let size = field(archive, at, 6)? as usize;
        let name_size = field(archive, at, 11)? as usize;
        let name_at = at + 110;
        let name = std::str::from_utf8(&archive[name_at..name_at + name_size - 1])?;
        assert_eq!(archive[name_at + name_size - 1], 0, "the name ends in NUL");
        let data_at = align(name_at + name_size);
        at = align(data_at + size);
        if name == "TRAILER!!!" {
            return Ok((entries, at));
        }
        entries.push((
            name.to_owned(),
            mode,
            owner,
            group,
            mtime,
            archive[data_at..data_at + size].to_vec(),
        ));
    }
}

#[test]
fn the_archive_holds_the_agent_as_an_executable_init_and_ends_at_its_trailer() -> Result {
    let agent = b"\x7fELF and some more bytes";
    let archive = initrd(agent);

    let (entries, end) = read(&archive)?;

    assert_eq!(
        entries,
        [("init".to_owned(), 0o100_755, 0, 0, 0, agent.to_vec())]
    );
    assert_eq!(end, archive.len());
    assert_eq!(archive.len() % 4, 0);
    Ok(())
}

#[test]
fn an_agent_of_any_length_keeps_the_alignment_and_its_content() -> Result {
    for length in 0..9 {
        let agent: Vec<u8> = (0..length).map(|byte| byte + 1).collect();
        let archive = initrd(&agent);
        let (entries, end) = read(&archive)?;
        assert_eq!(entries[0].5, agent, "length {length}");
        assert_eq!(end, archive.len(), "length {length}");
    }
    Ok(())
}

#[test]
fn the_archive_is_a_function_of_the_agent_alone() {
    let agent = b"agent";
    assert_eq!(initrd(agent), initrd(agent));
    // The control: a different agent gives a different archive, so equality means something.
    assert_ne!(initrd(agent), initrd(b"agent!"));
}
