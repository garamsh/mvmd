//! A host-side Guest Agent that serves only the channel, for tests: `GuestService` on a Unix socket
//! the caller names, with none of the init's duties. It never mounts, reaps or powers off, `Boot`
//! is refused, and exec and the file calls act on the host as the user who runs it.
//!
//! `guest-agent-channel [--handshake-port <port>] <socket>`. With the flag, each connection
//! must open with `CONNECT <port>\n`, which is answered `OK <port>\n` before the gRPC stream
//! begins: the handshake a Firecracker host-side vsock socket speaks. A connection that opens
//! with anything else is closed. This binary is not shipped and is not in the initrd.

use std::io;
use std::path::Path;
use std::process::ExitCode;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{UnixListener, UnixStream};
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

/// How long a client has to complete the handshake before its connection is dropped.
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(5);

/// The longest handshake line accepted, newline included.
const MAX_LINE: usize = 64;

const USAGE: &str = "usage: guest-agent-channel [--handshake-port <port>] <socket>";

struct Arguments {
    socket: String,
    handshake_port: Option<u32>,
}

fn parse(mut args: impl Iterator<Item = String>) -> Result<Arguments, String> {
    let mut socket = None;
    let mut handshake_port = None;
    while let Some(arg) = args.next() {
        if arg == "--handshake-port" {
            let value = args.next().ok_or("--handshake-port needs a port")?;
            let port = value
                .parse()
                .map_err(|_| format!("{value:?} is not a port"))?;
            handshake_port = Some(port);
        } else if arg.starts_with("--") || socket.is_some() {
            return Err(format!("unexpected argument {arg:?}"));
        } else {
            socket = Some(arg);
        }
    }
    Ok(Arguments {
        socket: socket.ok_or("no socket path")?,
        handshake_port,
    })
}

/// Read the line `CONNECT <port>` byte by byte, so nothing of the gRPC stream behind it is
/// consumed, and answer `OK <port>`.
async fn handshake(stream: &mut UnixStream, port: u32) -> io::Result<()> {
    let mut line = Vec::new();
    loop {
        let byte = stream.read_u8().await?;
        if byte == b'\n' {
            break;
        }
        line.push(byte);
        if line.len() >= MAX_LINE {
            return Err(io::Error::other("the handshake line is too long"));
        }
    }
    if line != format!("CONNECT {port}").as_bytes() {
        return Err(io::Error::other(
            "the handshake is not CONNECT for the port",
        ));
    }
    stream.write_all(format!("OK {port}\n").as_bytes()).await
}

async fn run(arguments: Arguments) -> io::Result<()> {
    let listener = UnixListener::bind(&arguments.socket)?;
    let (ready, incoming) = mpsc::channel(16);
    // Held until the outcome is known, so the end of the accept loop is not read as an end of
    // the connection stream before its error is.
    let keep_open = ready.clone();
    // The accept loop ends only with the error that stopped it, which ends the server.
    let mut accept = tokio::spawn(async move {
        loop {
            let mut stream = match listener.accept().await {
                Ok((stream, _)) => stream,
                Err(error) => return error,
            };
            let Some(port) = arguments.handshake_port else {
                if ready.send(Ok(stream)).await.is_err() {
                    return io::Error::other("the server stopped");
                }
                continue;
            };
            // A task per connection, so a client that stalls in the handshake holds up no other.
            let ready = ready.clone();
            tokio::spawn(async move {
                let done =
                    tokio::time::timeout(HANDSHAKE_TIMEOUT, handshake(&mut stream, port)).await;
                if matches!(done, Ok(Ok(()))) {
                    // The send fails only once the server has stopped and dropped the receiver,
                    // when there is no one left to tell.
                    ready.send(Ok(stream)).await.ok();
                }
            });
        }
    });
    let mut interrupt = signal(SignalKind::interrupt())?;
    let mut terminate = signal(SignalKind::terminate())?;
    let shutdown = async {
        tokio::select! {
            _ = interrupt.recv() => {}
            _ = terminate.recv() => {}
        }
    };
    let served = guest_agent::serve(
        ReceiverStream::new(incoming),
        guest_agent::Config::default(),
        shutdown,
    );
    let outcome = tokio::select! {
        served = served => served.map_err(io::Error::other),
        stopped = &mut accept => Err(match stopped {
            Ok(error) => io::Error::new(error.kind(), format!("accept failed: {error}")),
            Err(joined) => io::Error::other(joined),
        }),
    };
    drop(keep_open);
    accept.abort();
    // A socket already gone is no failure; any other is reported, and does not hide an earlier one.
    let removed = match std::fs::remove_file(Path::new(&arguments.socket)) {
        Err(error) if error.kind() != io::ErrorKind::NotFound => Err(error),
        _ => Ok(()),
    };
    outcome.and(removed)
}

fn main() -> ExitCode {
    let arguments = match parse(std::env::args().skip(1)) {
        Ok(arguments) => arguments,
        Err(message) => {
            eprintln!("guest-agent-channel: {message}\n{USAGE}");
            return ExitCode::from(2);
        }
    };
    let result = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .and_then(|runtime| runtime.block_on(run(arguments)));
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("guest-agent-channel: {error}");
            ExitCode::FAILURE
        }
    }
}
