//! The converter's initrd holds the converter as `init` and `mkfs.erofs` where the converter looks
//! for it.

use mkinitrd::MKFS_EROFS_PATH;

#[test]
fn the_archive_holds_the_converter_as_init_and_mkfs_erofs_at_its_path()
-> Result<(), Box<dyn std::error::Error>> {
    assert_eq!(
        mkinitrd::initrd(b"converter", b"tool")?,
        initrd::initrd(&[("init", b"converter"), ("mkfs.erofs", b"tool")])?
    );
    assert_eq!(MKFS_EROFS_PATH, "/mkfs.erofs");
    Ok(())
}
