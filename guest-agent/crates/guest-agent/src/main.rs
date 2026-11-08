//! The Guest Agent binary: binds vsock and serves the guest channel. Init duties (PID 1, the boot
//! order, reaping) are not here.

use std::process::ExitCode;

use tokio::signal::unix::{SignalKind, signal};
use tokio_vsock::{VMADDR_CID_ANY, VsockAddr, VsockListener};

/// The vsock port the agent listens on.
const PORT: u32 = 1024;

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("guest-agent: {message}");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), String> {
    let listener = VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, PORT))
        .map_err(|e| format!("cannot listen on vsock port {PORT}: {e}"))?;
    let mut terminate =
        signal(SignalKind::terminate()).map_err(|e| format!("cannot watch SIGTERM: {e}"))?;
    let shutdown = async move {
        terminate.recv().await;
    };
    guest_agent::serve(
        listener.incoming(),
        guest_agent::Config::default(),
        shutdown,
    )
    .await
    .map_err(|e| e.to_string())
}
