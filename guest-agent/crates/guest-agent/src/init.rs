//! The agent as PID 1: it serves the channel, supervises the entrypoint, forwards termination to
//! it, reaps what nobody else will, and ends the Machine when the entrypoint is gone. PID 1 never
//! returns: the kernel panics when init exits.

use std::convert::Infallible;
use std::io;
use std::pin::pin;
use std::sync::Arc;

use futures_core::Stream;
use nix::sys::signal::Signal;
use nix::unistd::getpid;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::{mpsc, oneshot};
use tonic::transport::server::Connected;

use crate::boot::{Boot, ENTRYPOINT};
use crate::error::Error;
use crate::exec::Execs;
use crate::system::{Console, System};
use crate::v1alpha1::{ExecRecord, ExecStatus};
use crate::{Config, serve_with};

/// Why the agent stops serving.
enum Ended {
    /// The entrypoint ended or never started; its record says how.
    Entrypoint(ExecRecord),
    /// A termination request came when no entrypoint was running or ever would.
    Terminated,
    /// The agent itself failed at something; the text names it.
    Failed(String),
}

/// Run as the guest's init, and never return.
///
/// `setup` runs once the console is attached. It binds the listener and returns it with the
/// termination requests to forward: each signal received on the receiver is sent to the
/// entrypoint. One that arrives when the entrypoint is not running ends the Machine, since there
/// is nothing to terminate.
///
/// The agent serves `incoming`, boots the guest when mvmd describes it and supervises the
/// entrypoint. When the entrypoint ends, or the agent cannot go on, it writes one line to the
/// console saying why, stops accepting calls, waits up to `Config::shutdown_grace` for the
/// connected clients, and powers the Machine off. If the power-off returns it has failed: the
/// agent then restarts the Machine, and if that fails too it waits for ever.
pub async fn run<I, IO>(
    setup: impl FnOnce() -> io::Result<(I, mpsc::Receiver<Signal>)>,
    config: Config,
    system: System,
) -> Infallible
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    serve_and_supervise(setup, &config, &system).await;
    end(&system).await
}

fn failure_line(context: &str, error: impl std::fmt::Display) -> String {
    format!("mvmd-guest-agent: failed to {context}: {error}")
}

/// End the Machine from async code: power off, then restart, then wait.
async fn end(system: &System) -> Infallible {
    let system = system.clone();
    // The blocking thread that waits for ever is not the runtime's to wait on.
    let _ = tokio::task::spawn_blocking(move || end_now(&system)).await;
    std::future::pending().await
}

/// End the Machine: power off, then restart, then wait. Each of the first two returns only when it
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

/// Write the line for a failure of the agent itself, then end the Machine. For the code that
/// runs before `run` does, with no runtime to await on.
pub fn fail(system: &System, context: &str, error: impl std::fmt::Display) -> ! {
    system.console.line(&failure_line(context, error));
    end_now(system);
    // Only a test's `halt` returns.
    loop {
        std::thread::park();
    }
}

/// Make a panic anywhere in the agent end the Machine as any other failure of the agent does,
/// instead of unwinding or aborting out of PID 1, which panics the guest's kernel.
///
/// The hook runs on the panicking thread before the process aborts, and does not return in
/// production.
pub fn end_on_panic(system: System) {
    std::panic::set_hook(Box::new(move |info| {
        system
            .console
            .line(&format!("mvmd-guest-agent: panic: {info}"));
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

/// Serve and supervise until the agent has a reason to end, write the reason, and stop serving.
async fn serve_and_supervise<I, IO>(
    setup: impl FnOnce() -> io::Result<(I, mpsc::Receiver<Signal>)>,
    config: &Config,
    system: &System,
) where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let console: &dyn Console = system.console.as_ref();
    let mounts = system.mounts.clone();
    let prepared = tokio::task::spawn_blocking(move || mounts.prepare()).await;
    match prepared {
        Ok(Ok(())) => {}
        Ok(Err(error)) => return console.line(&failure_line("prepare the guest", error)),
        Err(error) => return console.line(&failure_line("prepare the guest", error)),
    }
    let (incoming, mut terminate) = match setup() {
        Ok(listening) => listening,
        Err(error) => return console.line(&failure_line("listen for mvmd", error)),
    };
    let mut children = match signal(SignalKind::child()) {
        Ok(children) => children,
        Err(error) => return console.line(&failure_line("watch for exited children", error)),
    };

    let execs = Arc::new(Execs::new(config.exec_output_retention));
    let boot = Arc::new(Boot::pending(system.clone(), execs.clone()));
    let (stop, stopped) = oneshot::channel::<()>();
    let mut server = pin!(serve_with(incoming, execs.clone(), boot.clone(), async {
        // A dropped sender stops the server as a sent one does.
        let _ = stopped.await;
    }));

    let supervision = async {
        let entrypoint_ended = async {
            boot.booted().await;
            execs.wait(ENTRYPOINT).await
        };
        let mut entrypoint_ended = pin!(entrypoint_ended);
        loop {
            tokio::select! {
                _ = children.recv() => execs.reap_orphans(getpid()),
                Some(requested) = terminate.recv() => {
                    match execs.signal(ENTRYPOINT, requested as i32) {
                        Ok(()) | Err(Error::Io { .. }) => {}
                        // The entrypoint is not running: it has ended, never started, or
                        // never will. Its record, if there is one, says which.
                        Err(_) => return match execs.wait(ENTRYPOINT).await {
                            Ok(record) => Ended::Entrypoint(record),
                            Err(_) => Ended::Terminated,
                        },
                    }
                }
                record = &mut entrypoint_ended => return match record {
                    Ok(record) => Ended::Entrypoint(record),
                    Err(error) => Ended::Failed(failure_line("wait for the entrypoint", error)),
                },
            }
        }
    };
    let ended = tokio::select! {
        served = &mut server => {
            let reason = served.err().map_or_else(|| "the server ended".to_owned(), |e| e.to_string());
            return console.line(&failure_line("serve the channel", reason));
        }
        ended = supervision => ended,
    };

    console.line(&describe(&ended));
    let _ = stop.send(());
    // A client that never finishes reading must not keep the Machine on.
    let _ = tokio::time::timeout(config.shutdown_grace, &mut server).await;
}

/// The line the console carries about why the agent is ending.
fn describe(ended: &Ended) -> String {
    match ended {
        Ended::Terminated => "mvmd-guest-agent: terminated before the entrypoint ran".into(),
        Ended::Failed(line) => line.clone(),
        Ended::Entrypoint(record) => match record.status() {
            ExecStatus::Exited if record.term_signal != 0 => format!(
                "mvmd-guest-agent: entrypoint exited signal={}",
                record.term_signal
            ),
            ExecStatus::Exited => format!(
                "mvmd-guest-agent: entrypoint exited code={}",
                record.exit_code
            ),
            ExecStatus::NotStarted => format!(
                "mvmd-guest-agent: entrypoint not started reason={}",
                record.reason
            ),
            _ => format!(
                "mvmd-guest-agent: entrypoint ended reason={}",
                record.reason
            ),
        },
    }
}
