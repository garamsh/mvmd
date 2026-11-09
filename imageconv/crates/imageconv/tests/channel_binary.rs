//! The host-side channel-only binary, run as a process on a temporary socket and driven through the
//! generated client, with the Firecracker host-side handshake in front of the gRPC stream.

#[expect(
    dead_code,
    reason = "this test uses part of what the test binaries share"
)]
#[path = "support/image.rs"]
mod image;

use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

use hyper_util::rt::TokioIo;
use image::{Entry, Result, TAR, UUID, layer, tool};
use imageconv::v1alpha1::convert_request::Message;
use imageconv::v1alpha1::convert_response::Result as Outcome;
use imageconv::v1alpha1::convert_service_client::ConvertServiceClient;
use imageconv::v1alpha1::{ConvertRequest, Pass, PassKind, Start};
use tempfile::TempDir;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::process::{Child, Command};
use tonic::transport::{Channel, Endpoint};
use tower::service_fn;

const PORT: u32 = 1025;

/// The binary, started on a socket in a temporary directory.
struct Converter {
    socket: PathBuf,
    _dir: TempDir,
    _child: Child,
}

impl Converter {
    async fn start() -> Result<Self> {
        let dir = tempfile::tempdir()?;
        let socket = dir.path().join("converter.sock");
        let output = image::output_file(dir.path())?;
        let child = Command::new(env!("CARGO_BIN_EXE_imageconv-channel"))
            .arg("--mkfs")
            .arg(tool("MKFS_EROFS"))
            .arg("--output")
            .arg(&output)
            .args(["--handshake-port", &PORT.to_string()])
            .arg(&socket)
            .kill_on_drop(true)
            .spawn()?;
        // The socket exists once the binary listens.
        for _ in 0..200 {
            if socket.exists() {
                return Ok(Self {
                    socket,
                    _dir: dir,
                    _child: child,
                });
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        Err("the binary never bound its socket".into())
    }

    /// A client that sends the handshake first, as the Driver's dialer does.
    async fn client(&self) -> Result<ConvertServiceClient<Channel>> {
        let socket = self.socket.clone();
        let channel = Endpoint::try_from("http://converter.invalid")?
            .connect_with_connector(service_fn(move |_| {
                let socket = socket.clone();
                async move {
                    let mut stream = UnixStream::connect(socket).await?;
                    stream
                        .write_all(format!("CONNECT {PORT}\n").as_bytes())
                        .await?;
                    let mut reply = vec![0; format!("OK {PORT}\n").len()];
                    stream.read_exact(&mut reply).await?;
                    if reply != format!("OK {PORT}\n").as_bytes() {
                        return Err(io::Error::other("the handshake was not answered OK"));
                    }
                    Ok::<_, io::Error>(TokioIo::new(stream))
                }
            }))
            .await?;
        Ok(ConvertServiceClient::new(channel))
    }
}

/// What the binary sends back when `line` is sent to it and the write side is then closed.
async fn answer_to(socket: &Path, line: &str) -> Result<Vec<u8>> {
    let mut stream = UnixStream::connect(socket).await?;
    stream.write_all(line.as_bytes()).await?;
    stream.shutdown().await?;
    let mut answer = Vec::new();
    // A close with the client's bytes unread arrives as a reset, which is also no answer.
    match tokio::time::timeout(Duration::from_secs(5), stream.read_to_end(&mut answer)).await? {
        Err(e) if e.kind() == io::ErrorKind::ConnectionReset => {}
        read => {
            read?;
        }
    }
    Ok(answer)
}

fn message(message: Message) -> ConvertRequest {
    ConvertRequest {
        message: Some(message),
    }
}

#[tokio::test]
async fn the_handshake_is_answered_and_one_conversion_is_served_on_the_host() -> Result {
    let converter = Converter::start().await?;

    // Control: a connection that opens with anything but the handshake is closed unanswered.
    assert!(answer_to(&converter.socket, "HELLO\n").await?.is_empty());

    let bytes = layer(&[Entry::File("a", "A")]);
    let messages = vec![
        message(Message::Start(Start {
            filesystem_uuid: UUID.into(),
            layer_media_types: vec![TAR.into()],
            name_set_limit_bytes: 1 << 20,
        })),
        message(Message::Pass(Pass {
            layer: 0,
            kind: PassKind::Names.into(),
        })),
        message(Message::Data(bytes.clone())),
        message(Message::Pass(Pass {
            layer: 0,
            kind: PassKind::Content.into(),
        })),
        message(Message::Data(bytes)),
    ];
    let response = converter
        .client()
        .await?
        .convert(tokio_stream::iter(messages))
        .await?
        .into_inner();
    match response.result {
        Some(Outcome::Converted(converted)) => assert!(converted.length > 0),
        other => panic!("expected a conversion, got {other:?}"),
    }
    Ok(())
}
