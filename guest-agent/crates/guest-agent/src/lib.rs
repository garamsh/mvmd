//! The Guest Agent: the service of `mvmd.guest.v1alpha1` (exec, the file API and the boot) served
//! over any incoming connection stream, and the duties of the guest's init. Production hands it
//! vsock; tests hand it a Unix socket.

mod boot;
mod description;
mod digest;
mod error;
mod exec;
mod files;
mod init;
mod mounts;
mod network;
mod reap;
mod restore;
mod service;
mod system;
mod uuid;

use std::future::Future;
use std::io;
use std::sync::Arc;
use std::time::Duration;

use futures_core::Stream;
use nix::sys::signal::Signal;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::mpsc;
use tonic::transport::Server;
use tonic::transport::server::Connected;

pub use boot::ENTRYPOINT;
pub use description::{Addressing, NetworkConfig};
pub use error::{BootStep, Error};
pub use init::{end_on_panic, fail, run};
pub use mounts::LinuxMounts;
pub use network::LinuxNetwork;
pub use restore::{LinuxClock, LinuxEntropy};
pub use system::{
    Clock, Console, Entropy, LinuxConsole, LinuxPower, Mounts, Network, Power, System,
};
pub use uuid::{Filesystem, Uuid};

/// The generated contract: messages, the client and the server of `GuestService`.
pub mod v1alpha1 {
    #![expect(
        clippy::allow_attributes,
        clippy::allow_attributes_without_reason,
        reason = "tonic-prost-build writes #[allow] without a reason"
    )]
    tonic::include_proto!("mvmd.guest.v1alpha1");
}

/// What the agent is configured with.
#[derive(Debug, Clone)]
pub struct Config {
    /// How many of the newest bytes of each command's output the agent keeps.
    pub exec_output_retention: u64,
    /// How long `init::run` lets connected clients finish reading before it powers off.
    pub shutdown_grace: Duration,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            exec_output_retention: 16 * 1024 * 1024,
            shutdown_grace: Duration::from_secs(5),
        }
    }
}

/// Serve `GuestService` on the connections of `incoming` until `shutdown` completes, without
/// booting the guest: `Boot` is refused and every other call is served from the start.
///
/// A command still running when `shutdown` completes is ended, and its record says the agent
/// ended first.
pub async fn serve<I, IO>(
    incoming: I,
    config: Config,
    shutdown: impl Future<Output = ()>,
) -> Result<(), Error>
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let execs = Arc::new(exec::Execs::new(config.exec_output_retention));
    let boot = Arc::new(boot::Boot::disabled(execs.clone()));
    serve_with(incoming, execs, boot, None, shutdown).await
}

/// Serve `GuestService` over `execs`, with `boot` deciding when the guest is ready. `terminate`
/// carries the `Terminate` calls to the init's supervision; without it the agent is not the
/// guest's init and refuses them.
async fn serve_with<I, IO>(
    incoming: I,
    execs: Arc<exec::Execs>,
    boot: Arc<boot::Boot>,
    terminate: Option<mpsc::Sender<Signal>>,
    shutdown: impl Future<Output = ()>,
) -> Result<(), Error>
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let service = v1alpha1::guest_service_server::GuestServiceServer::new(service::Guest {
        execs: execs.clone(),
        locks: files::Locks::default(),
        boot,
        terminate,
    })
    .max_decoding_message_size(service::MAX_REQUEST_BYTES)
    .max_encoding_message_size(service::MAX_RESPONSE_BYTES);
    let signal = async move {
        shutdown.await;
        // Streams that follow a command end with its record, which lets the drain finish.
        execs.cut_short();
    };
    Server::builder()
        .add_service(service)
        .serve_with_incoming_shutdown(incoming, signal)
        .await
        .map_err(|e| Error::Serve(e.into()))
}
