//! The Guest Agent's service: exec and the file API of `mvmd.guest.v1alpha1`, served over any
//! incoming connection stream. Production hands it vsock; tests hand it a Unix socket.

mod digest;
mod error;
mod exec;
mod files;
mod service;

use std::future::Future;
use std::io;
use std::sync::Arc;

use futures_core::Stream;
use tokio::io::{AsyncRead, AsyncWrite};
use tonic::transport::Server;
use tonic::transport::server::Connected;

pub use error::Error;

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
}

impl Default for Config {
    fn default() -> Self {
        Self {
            exec_output_retention: 16 * 1024 * 1024,
        }
    }
}

/// Serve `GuestService` on the connections of `incoming` until `shutdown` completes.
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
    let service = v1alpha1::guest_service_server::GuestServiceServer::new(service::Guest {
        execs: execs.clone(),
        locks: files::Locks::default(),
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
