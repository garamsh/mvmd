//! The converter as PID 1: it prepares the VM, serves one conversion, and powers the VM off.
//! PID 1 never returns: the kernel panics when init exits.

use std::convert::Infallible;
use std::io;
use std::path::PathBuf;

use futures_core::Stream;
use tokio::io::{AsyncRead, AsyncWrite};
use tonic::transport::server::Connected;

use crate::mkfs::Mkfs;
use crate::service::{Converter, serve};
use crate::system::System;

fn failure_line(context: &str, error: impl std::fmt::Display) -> String {
    format!("mvmd-converter: failed to {context}: {error}")
}

/// Run as the VM's init, and never return.
///
/// The converter prepares the VM, finds its output device, binds the listener `setup` returns,
/// and serves one conversion with the `mkfs.erofs` at `mkfs_erofs`. Then it powers the VM off.
/// A step that fails is written to the console as one line, and the VM is powered off too. If
/// the power-off returns it has failed: the converter then restarts the VM, and if that fails
/// too it waits for ever.
pub async fn run<I, IO>(
    setup: impl FnOnce() -> io::Result<I>,
    mkfs_erofs: PathBuf,
    system: System,
) -> Infallible
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    if let Err(line) = serve_one(setup, mkfs_erofs, &system).await {
        system.console.line(&line);
    }
    end(&system).await
}

/// Prepare, listen and serve, and return the console line for what failed.
async fn serve_one<I, IO>(
    setup: impl FnOnce() -> io::Result<I>,
    mkfs_erofs: PathBuf,
    system: &System,
) -> Result<(), String>
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let mounts = system.mounts.clone();
    let output = tokio::task::spawn_blocking(move || {
        mounts
            .prepare()
            .map_err(|error| failure_line("prepare the VM", error))?;
        mounts
            .output_device()
            .map_err(|error| failure_line("find the output device", error))
    })
    .await
    .map_err(|error| failure_line("prepare the VM", error))??;
    let incoming = setup().map_err(|error| failure_line("listen for mvmd", error))?;
    let converter = Converter {
        mkfs: Mkfs::new(mkfs_erofs),
        output,
    };
    serve(incoming, converter)
        .await
        .map_err(|error| failure_line("serve the conversion", error))
}

/// End the VM from async code: power off, then restart, then wait.
async fn end(system: &System) -> Infallible {
    let system = system.clone();
    // The blocking thread that waits for ever is not the runtime's to wait on.
    let _ = tokio::task::spawn_blocking(move || end_now(&system)).await;
    std::future::pending().await
}

/// End the VM: power off, then restart, then wait. Each of the first two returns only when it
/// has failed, and the console says so.
fn end_now(system: &System) {
    let outcome = system.power.power_off();
    system
        .console
        .line(&failure_line("power off", describe_return(outcome)));
    let outcome = system.power.restart();
    system
        .console
        .line(&failure_line("restart", describe_return(outcome)));
    system.power.halt();
}

/// Write the line for a failure of the converter itself, then end the VM. For the code that runs
/// before `run` does, with no runtime to await on.
pub fn fail(system: &System, context: &str, error: impl std::fmt::Display) -> ! {
    system.console.line(&failure_line(context, error));
    end_now(system);
    // Only a test's `halt` returns.
    loop {
        std::thread::park();
    }
}

/// Make a panic anywhere in the converter end the VM as any other failure of the converter does,
/// instead of unwinding or aborting out of PID 1, which panics the guest's kernel.
///
/// The hook runs on the panicking thread before the process aborts, and does not return in
/// production.
pub fn end_on_panic(system: System) {
    std::panic::set_hook(Box::new(move |info| {
        system
            .console
            .line(&format!("mvmd-converter: panic: {info}"));
        end_now(&system);
    }));
}

/// What a return from a call that does not return on success says.
fn describe_return(result: io::Result<()>) -> String {
    match result {
        Ok(()) => "the call returned".into(),
        Err(error) => error.to_string(),
    }
}
