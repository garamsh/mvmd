//! The converter binary, the converter microVM's init: it binds vsock and hands it to
//! `imageconv::run` with the kernel-facing parts of a Linux guest. It never returns, since the
//! kernel panics when init exits.

use std::io;
use std::sync::Arc;

use tokio_vsock::{VMADDR_CID_ANY, VsockAddr, VsockListener};

/// The vsock port the converter listens on.
const PORT: u32 = 1025;

/// Where the initrd puts `mkfs.erofs`.
const MKFS_EROFS: &str = "/mkfs.erofs";

fn listen() -> io::Result<impl futures_core::Stream<Item = io::Result<tokio_vsock::VsockStream>>> {
    Ok(VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, PORT))?.incoming())
}

fn main() {
    let system = imageconv::System {
        mounts: Arc::new(imageconv::LinuxMounts),
        power: Arc::new(imageconv::LinuxPower),
        console: Arc::new(imageconv::LinuxConsole),
    };
    // A panic from here on ends the VM, the runtime failing to build included.
    imageconv::end_on_panic(system.clone());
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => imageconv::fail(&system, "build the runtime", error),
    };
    // `run` never returns, so nothing follows.
    runtime.block_on(imageconv::run(listen, MKFS_EROFS.into(), system));
}
