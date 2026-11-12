//! `mkinitrd <converter> <mkfs.erofs> <output>`: writes the converter's initrd with the binary at
//! `<converter>` as its `init` and the one at `<mkfs.erofs>` beside it.

use std::error::Error;
use std::fs;
use std::process::ExitCode;

fn build(converter: &str, mkfs_erofs: &str, output: &str) -> Result<(), Box<dyn Error>> {
    let archive = mkinitrd::initrd(&fs::read(converter)?, &fs::read(mkfs_erofs)?)?;
    Ok(fs::write(output, archive)?)
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let [converter, mkfs_erofs, output] = args.as_slice() else {
        eprintln!("usage: mkinitrd <converter> <mkfs.erofs> <output>");
        return ExitCode::FAILURE;
    };
    match build(converter, mkfs_erofs, output) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("mkinitrd: {error}");
            ExitCode::FAILURE
        }
    }
}
