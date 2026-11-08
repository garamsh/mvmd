//! The Guest Agent binary, the guest's init: it binds vsock, forwards termination requests, and
//! hands both to `guest_agent::run` with the kernel-facing parts of a Linux guest.

use std::process::ExitCode;
use std::sync::Arc;

use nix::sys::signal::Signal;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;
use tokio_vsock::{VMADDR_CID_ANY, VsockAddr, VsockListener};

/// The vsock port the agent listens on.
const PORT: u32 = 1024;

/// The signals that ask init to end: the one `kill` sends, and the one the kernel sends init on
/// a ctrl-alt-del.
const TERMINATION: [(SignalKind, Signal); 2] = [
    (SignalKind::terminate(), Signal::SIGTERM),
    (SignalKind::interrupt(), Signal::SIGINT),
];

#[tokio::main]
async fn main() -> ExitCode {
    match start().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("guest-agent: {message}");
            ExitCode::FAILURE
        }
    }
}

async fn start() -> Result<(), String> {
    let listener = VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, PORT))
        .map_err(|e| format!("cannot listen on vsock port {PORT}: {e}"))?;
    let (requests, terminate) = mpsc::channel(8);
    for (kind, forwarded) in TERMINATION {
        let mut received = signal(kind).map_err(|e| format!("cannot watch {forwarded}: {e}"))?;
        let requests = requests.clone();
        tokio::spawn(async move {
            while received.recv().await.is_some() {
                if requests.send(forwarded).await.is_err() {
                    return;
                }
            }
        });
    }
    let system = guest_agent::System {
        mounts: Arc::new(guest_agent::LinuxMounts),
        network: Arc::new(guest_agent::LinuxNetwork::default()),
        power: Arc::new(guest_agent::LinuxPower),
    };
    guest_agent::run(
        listener.incoming(),
        guest_agent::Config::default(),
        system,
        terminate,
    )
    .await
    .map_err(|e| e.to_string())
}
