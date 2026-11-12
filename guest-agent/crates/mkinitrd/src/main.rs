//! `mkinitrd <agent> <output>`: writes the Guest Agent's initrd with the binary at `<agent>` as
//! its `init`.

use std::error::Error;
use std::fs;
use std::process::ExitCode;

fn build(agent: &str, output: &str) -> Result<(), Box<dyn Error>> {
    let archive = mkinitrd::initrd(&fs::read(agent)?)?;
    Ok(fs::write(output, archive)?)
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let [agent, output] = args.as_slice() else {
        eprintln!("usage: mkinitrd <agent> <output>");
        return ExitCode::FAILURE;
    };
    match build(agent, output) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("mkinitrd: {error}");
            ExitCode::FAILURE
        }
    }
}
