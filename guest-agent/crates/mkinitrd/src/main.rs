//! `mkinitrd <agent> <output>`: writes the Guest Agent's initrd with the binary at `<agent>` as
//! its `init`.

use std::fs;
use std::process::ExitCode;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let [agent, output] = args.as_slice() else {
        eprintln!("usage: mkinitrd <agent> <output>");
        return ExitCode::FAILURE;
    };
    match fs::read(agent).and_then(|binary| fs::write(output, mkinitrd::initrd(&binary))) {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("mkinitrd: {error}");
            ExitCode::FAILURE
        }
    }
}
