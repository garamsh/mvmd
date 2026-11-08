//! The Guest Agent binary, the guest's init: it binds vsock, forwards termination requests, and
//! hands both to `guest_agent::run` with the kernel-facing parts of a Linux guest. It never
//! returns, since the kernel panics when init exits.

use std::io;
use std::sync::Arc;

use nix::sys::signal::Signal;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;
use tokio_vsock::{VMADDR_CID_ANY, VsockAddr, VsockListener, VsockStream};

/// The vsock port the agent listens on.
const PORT: u32 = 1024;

/// The signals that ask init to end: the one `kill` sends, and the one the kernel sends init on
/// a ctrl-alt-del.
const TERMINATION: [(SignalKind, Signal); 2] = [
    (SignalKind::terminate(), Signal::SIGTERM),
    (SignalKind::interrupt(), Signal::SIGINT),
];

/// Bind the listener and start forwarding the termination signals.
fn listen() -> io::Result<(
    impl futures_core::Stream<Item = io::Result<VsockStream>>,
    mpsc::Receiver<Signal>,
)> {
    let listener = VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, PORT))?;
    let (requests, terminate) = mpsc::channel(8);
    for (kind, forwarded) in TERMINATION {
        let mut received = signal(kind)?;
        let requests = requests.clone();
        tokio::spawn(async move {
            while received.recv().await.is_some() {
                if requests.send(forwarded).await.is_err() {
                    return;
                }
            }
        });
    }
    Ok((listener.incoming(), terminate))
}

#[tokio::main]
async fn main() {
    let system = guest_agent::System {
        mounts: Arc::new(guest_agent::LinuxMounts),
        network: Arc::new(guest_agent::LinuxNetwork::default()),
        power: Arc::new(guest_agent::LinuxPower),
        console: Arc::new(guest_agent::LinuxConsole),
    };
    match guest_agent::run(listen, guest_agent::Config::default(), system).await {}
}
