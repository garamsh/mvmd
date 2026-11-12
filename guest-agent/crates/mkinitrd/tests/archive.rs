//! The Guest Agent's initrd holds the agent as `init` and nothing else.

#[test]
fn the_archive_holds_the_agent_as_init() -> Result<(), Box<dyn std::error::Error>> {
    assert_eq!(
        mkinitrd::initrd(b"agent")?,
        initrd::initrd(&[("init", b"agent")])?
    );
    Ok(())
}
