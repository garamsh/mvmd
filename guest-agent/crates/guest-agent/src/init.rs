//! The agent as PID 1: it serves the channel, supervises the entrypoint, forwards termination to
//! it, reaps what nobody else will, and powers the Machine off when the entrypoint is gone.

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
use crate::system::System;
use crate::v1alpha1::{ExecRecord, ExecStatus};
use crate::{Config, serve_with};

/// Why the agent stops serving.
enum Ended {
    /// The entrypoint ended; its record says how.
    Entrypoint(ExecRecord),
    /// A termination request came when no entrypoint was running or ever would.
    Terminated,
}

/// Run as the guest's init: serve `incoming`, boot the guest when mvmd describes it, supervise the
/// entrypoint, and power the Machine off once the entrypoint has ended.
///
/// Each signal received on `terminate` is forwarded to the entrypoint. One that arrives before the
/// entrypoint runs powers the Machine off, since there is nothing to terminate.
///
/// When the entrypoint ends, its record is final for every client still reading it; the agent
/// then stops accepting calls, waits up to `Config::shutdown_grace` for the connected clients,
/// and powers off. It returns only if the power-off fails or the server stops by itself.
pub async fn run<I, IO>(
    incoming: I,
    config: Config,
    system: System,
    mut terminate: mpsc::Receiver<Signal>,
) -> Result<(), Error>
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let mounts = system.mounts.clone();
    tokio::task::spawn_blocking(move || mounts.prepare())
        .await
        .map_err(|joined| io_error("prepare the guest", io::Error::other(joined)))?
        .map_err(|failed| io_error("prepare the guest", failed))?;
    let mut children = signal(SignalKind::child())
        .map_err(|failed| io_error("watch for exited children", failed))?;

    let power = system.power.clone();
    let execs = Arc::new(Execs::new(config.exec_output_retention));
    let boot = Arc::new(Boot::pending(system, execs.clone()));
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
                    // Not found means the entrypoint has not run and never will; any other
                    // refusal means it has already ended, which its own branch reports.
                    if let Err(Error::NotFound(_)) = execs.signal(ENTRYPOINT, requested as i32) {
                        return Ok(Ended::Terminated);
                    }
                }
                record = &mut entrypoint_ended => return record.map(Ended::Entrypoint),
            }
        }
    };
    let ended = tokio::select! {
        served = &mut server => return served,
        ended = supervision => ended?,
    };

    eprintln!("guest-agent: {}; powering off", describe(&ended));
    let _ = stop.send(());
    // A client that never finishes reading must not keep the Machine on.
    let _ = tokio::time::timeout(config.shutdown_grace, &mut server).await;
    tokio::task::spawn_blocking(move || power.power_off())
        .await
        .map_err(|joined| io_error("power off", io::Error::other(joined)))?
        .map_err(|failed| io_error("power off", failed))
}

fn io_error(context: &str, source: io::Error) -> Error {
    Error::Io {
        context: context.into(),
        source,
    }
}

/// What the console says about why the agent is ending.
fn describe(ended: &Ended) -> String {
    match ended {
        Ended::Terminated => "terminated before the entrypoint ran".into(),
        Ended::Entrypoint(record) => match record.status() {
            ExecStatus::Exited if record.term_signal != 0 => {
                format!("the entrypoint was killed by signal {}", record.term_signal)
            }
            ExecStatus::Exited => format!("the entrypoint exited with code {}", record.exit_code),
            _ => format!("the entrypoint ended: {}", record.reason),
        },
    }
}
