//! The converter's service and its init, driven through the generated client over a Unix socket in
//! a temporary directory. The server code is the production one; only the listener differs, and
//! `mkfs.erofs` is the pinned one.

#[path = "support/image.rs"]
mod image;

use std::fs;
use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use converter::v1alpha1::convert_request::Message;
use converter::v1alpha1::convert_response::Result as Outcome;
use converter::v1alpha1::convert_service_client::ConvertServiceClient;
use converter::v1alpha1::{ConvertRequest, ConvertResponse, FailureStep, Pass, PassKind, Start};
use converter::{Console, Converter, Mkfs, Mounts, Power, System};
use hyper_util::rt::TokioIo;
use image::{Entry, GZIP, Result, TAR, UUID, ZSTD, expect, gzip, layer, zstd};
use tempfile::TempDir;
use tokio::net::{UnixListener, UnixStream};
use tokio::task::JoinHandle;
use tokio_stream::wrappers::{ReceiverStream, UnixListenerStream};
use tonic::Code;
use tonic::transport::{Channel, Endpoint};
use tower::service_fn;

/// A layer as a client holds it: its media type and bytes.
type Blob = (&'static str, Vec<u8>);

/// The messages of one conversion: the start, then for each layer both passes, the bytes cut into
/// chunks of `chunk` bytes.
fn conversion(uuid: &str, image: &[Blob], chunk: usize) -> Vec<ConvertRequest> {
    let mut messages = vec![start(uuid, image)];
    for (layer, (_, bytes)) in (0u32..).zip(image) {
        for kind in [PassKind::Names, PassKind::Content] {
            messages.push(message(Message::Pass(Pass {
                layer,
                kind: kind.into(),
            })));
            messages.extend(
                bytes
                    .chunks(chunk)
                    .map(|part| message(Message::Data(part.to_vec()))),
            );
        }
    }
    messages
}

fn message(message: Message) -> ConvertRequest {
    ConvertRequest {
        message: Some(message),
    }
}

fn start(uuid: &str, image: &[Blob]) -> ConvertRequest {
    message(Message::Start(Start {
        filesystem_uuid: uuid.into(),
        layer_media_types: image.iter().map(|(media, _)| (*media).to_owned()).collect(),
        name_set_limit_bytes: 1 << 20,
    }))
}

struct Harness {
    client: ConvertServiceClient<Channel>,
    dir: TempDir,
    output: PathBuf,
    server: JoinHandle<std::result::Result<(), converter::ServeError>>,
}

impl Harness {
    async fn start() -> Result<Self> {
        let dir = TempDir::new()?;
        let output = image::output_file(dir.path())?;
        let socket = dir.path().join("converter.sock");
        let incoming = UnixListenerStream::new(UnixListener::bind(&socket)?);
        let converter = Converter {
            mkfs: Mkfs::new(image::tool("MKFS_EROFS")),
            output: output.clone(),
        };
        let server = tokio::spawn(converter::serve(incoming, converter));
        Ok(Self {
            client: connect(socket).await?,
            dir,
            output,
            server,
        })
    }

    async fn convert(&mut self, messages: Vec<ConvertRequest>) -> Result<ConvertResponse> {
        Ok(self
            .client
            .convert(tokio_stream::iter(messages))
            .await?
            .into_inner())
    }

    fn tree(&self) -> Result<image::Tree> {
        image::tree(&self.output, self.dir.path())
    }
}

async fn connect(socket: PathBuf) -> Result<ConvertServiceClient<Channel>> {
    let channel = Endpoint::try_from("http://converter.invalid")?
        .connect_with_connector(service_fn(move |_| {
            let socket = socket.clone();
            async move { Ok::<_, io::Error>(TokioIo::new(UnixStream::connect(socket).await?)) }
        }))
        .await?;
    Ok(ConvertServiceClient::new(channel))
}

fn converted(response: ConvertResponse) -> u64 {
    match response.result {
        Some(Outcome::Converted(converted)) => converted.length,
        other => panic!("expected a conversion, got {other:?}"),
    }
}

fn failed(response: ConvertResponse) -> (FailureStep, String) {
    match response.result {
        Some(Outcome::Failure(failure)) => (failure.step(), failure.message),
        other => panic!("expected a failure, got {other:?}"),
    }
}

#[tokio::test]
async fn an_image_streamed_in_small_chunks_is_converted_and_its_length_reported() -> Result {
    let mut harness = Harness::start().await?;
    let image = [
        (
            TAR,
            layer(&[
                Entry::Dir("d"),
                Entry::File("d/a", "A"),
                Entry::File("x", "X"),
            ]),
        ),
        (
            GZIP,
            gzip(&layer(&[
                Entry::Whiteout("x"),
                Entry::Dir("d"),
                Entry::Opaque("d"),
                Entry::File("d/c", "C"),
                Entry::File("f", "new"),
            ])),
        ),
        (
            ZSTD,
            zstd(&layer(&[Entry::Whiteout("f"), Entry::File("f", "newer")])),
        ),
    ];

    let length = converted(harness.convert(conversion(UUID, &image, 700)).await?);

    assert_eq!(length, fs::metadata(&harness.output)?.len());
    assert_eq!(harness.tree()?, expect(&["d/", "d/c=C", "f=newer"]));
    // The server ends once it has answered.
    let ended = tokio::time::timeout(Duration::from_secs(3), harness.server).await??;
    assert!(ended.is_ok());
    Ok(())
}

#[tokio::test]
async fn a_media_type_the_converter_does_not_take_is_a_failure_that_names_the_layer_type_step()
-> Result {
    let mut harness = Harness::start().await?;
    let image = [
        (TAR, layer(&[Entry::File("a", "A")])),
        (
            "application/vnd.docker.image.rootfs.diff.tar.gzip",
            gzip(&layer(&[])),
        ),
    ];

    let (step, message) = failed(harness.convert(conversion(UUID, &image, 4096)).await?);

    assert_eq!(step, FailureStep::LayerType);
    assert!(message.contains("layer 1"), "{message}");
    // Nothing was written: the output is still the empty file the test made.
    assert!(fs::read(&harness.output)?.iter().all(|&byte| byte == 0));
    Ok(())
}

#[tokio::test]
async fn each_failure_names_its_step() -> Result {
    let good = |bytes: Vec<u8>| (TAR, bytes);
    let a = layer(&[Entry::File("a", "A")]);
    let cases: Vec<(Vec<ConvertRequest>, FailureStep)> = vec![
        // The first message is not a Start.
        (
            vec![message(Message::Data(vec![1, 2, 3]))],
            FailureStep::Stream,
        ),
        // A Start with a bad UUID.
        (
            conversion("not a uuid", &[good(a.clone())], 4096),
            FailureStep::Start,
        ),
        // A layer that is not gzip, in a gzip layer's names pass.
        (
            conversion(UUID, &[(GZIP, b"plainly not gzip".to_vec())], 4096),
            FailureStep::Names,
        ),
        // A stream that ends before the content pass.
        (
            conversion(UUID, &[good(a.clone())], 4096)
                .into_iter()
                .take(3)
                .collect(),
            FailureStep::Stream,
        ),
        // A pass of unspecified kind.
        (
            vec![
                start(UUID, &[good(a.clone())]),
                message(Message::Pass(Pass { layer: 0, kind: 0 })),
            ],
            FailureStep::Stream,
        ),
    ];

    for (messages, expected) in cases {
        let mut harness = Harness::start().await?;
        let (step, message) = failed(harness.convert(messages).await?);
        assert_eq!(step, expected, "{message}");
    }
    Ok(())
}

#[tokio::test]
async fn a_second_call_while_one_is_served_is_refused() -> Result {
    let harness = Harness::start().await?;
    let image = [(TAR, layer(&[Entry::File("a", "A")]))];
    // Two calls, each held open after its Start, so the server meets both before either ends.
    let mut a = Held::open(&harness, start(UUID, &image)).await?;
    let mut b = Held::open(&harness, start(UUID, &image)).await?;

    // The call the server serves is held open, so the first to end is the refused one.
    let (refused, served) = tokio::select! {
        ended = &mut a.call => (ended?, b),
        ended = &mut b.call => (ended?, a),
    };
    assert_eq!(refused.unwrap_err().code(), Code::AlreadyExists);

    // The served call is unaffected: it carries on and converts.
    for message in conversion(UUID, &image, 4096).into_iter().skip(1) {
        served.sender.send(message).await?;
    }
    drop(served.sender);
    converted(served.call.await??);
    Ok(())
}

/// A call whose stream the test feeds, and the task that carries it to the answer.
struct Held {
    sender: tokio::sync::mpsc::Sender<ConvertRequest>,
    call: JoinHandle<std::result::Result<ConvertResponse, tonic::Status>>,
}

impl Held {
    async fn open(harness: &Harness, first: ConvertRequest) -> Result<Self> {
        let (sender, receiver) = tokio::sync::mpsc::channel(8);
        let mut client = harness.client.clone();
        let call = tokio::spawn(async move {
            client
                .convert(ReceiverStream::new(receiver))
                .await
                .map(tonic::Response::into_inner)
        });
        sender.send(first).await?;
        Ok(Self { sender, call })
    }
}

/// What the init's seams record, in order.
#[derive(Clone, Default)]
struct Log(Arc<Mutex<Vec<String>>>);

impl Log {
    fn push(&self, entry: impl Into<String>) {
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .push(entry.into());
    }

    fn entries(&self) -> Vec<String> {
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    async fn until(&self, entry: &str) -> Vec<String> {
        for _ in 0..500 {
            let entries = self.entries();
            if entries.iter().any(|seen| seen == entry) {
                return entries;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("{entry:?} never happened: {:?}", self.entries());
    }
}

struct Seams {
    log: Log,
    output: Option<PathBuf>,
}

impl Mounts for Seams {
    fn prepare(&self) -> io::Result<()> {
        self.log.push("prepare");
        match self.output {
            Some(_) => Ok(()),
            None => Err(io::Error::other("no sysfs")),
        }
    }

    fn output_device(&self) -> io::Result<PathBuf> {
        self.log.push("output_device");
        self.output
            .clone()
            .ok_or_else(|| io::Error::other("unreachable"))
    }
}

impl Power for Seams {
    fn power_off(&self) -> io::Result<()> {
        self.log.push("power_off");
        Err(io::Error::other("injected failure"))
    }

    fn restart(&self) -> io::Result<()> {
        self.log.push("restart");
        Err(io::Error::other("injected failure"))
    }

    fn halt(&self) {
        self.log.push("halt");
    }
}

impl Console for Seams {
    fn line(&self, text: &str) {
        self.log.push(format!("console: {text}"));
    }
}

fn system(log: &Log, output: Option<PathBuf>) -> System {
    let seams = Arc::new(Seams {
        log: log.clone(),
        output,
    });
    System {
        mounts: seams.clone(),
        power: seams.clone(),
        console: seams,
    }
}

#[tokio::test]
async fn init_prepares_serves_one_conversion_and_powers_off() -> Result {
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let socket = dir.path().join("converter.sock");
    let incoming = UnixListenerStream::new(UnixListener::bind(&socket)?);
    let log = Log::default();
    let init = tokio::spawn(converter::run(
        move || Ok::<_, io::Error>(incoming),
        image::tool("MKFS_EROFS"),
        system(&log, Some(output.clone())),
    ));
    let mut client = connect(socket).await?;
    let image = [(TAR, layer(&[Entry::File("a", "A")]))];

    let response = client
        .convert(tokio_stream::iter(conversion(UUID, &image, 4096)))
        .await?
        .into_inner();
    let entries = log.until("halt").await;
    init.abort();

    converted(response);
    assert_eq!(
        entries,
        [
            "prepare",
            "output_device",
            "power_off",
            "console: mvmd-converter: failed to power off: injected failure",
            "restart",
            "console: mvmd-converter: failed to restart: injected failure",
            "halt",
        ]
    );
    assert_eq!(image::tree(&output, dir.path())?, expect(&["a=A"]));
    Ok(())
}

#[tokio::test]
async fn init_reports_a_step_that_failed_on_the_console_and_powers_off() -> Result {
    let log = Log::default();
    let never = || Ok::<_, io::Error>(tokio_stream::empty::<io::Result<UnixStream>>());
    let init = tokio::spawn(converter::run(never, "/absent".into(), system(&log, None)));

    let entries = log.until("halt").await;
    init.abort();

    assert_eq!(
        entries[..3],
        [
            "prepare",
            "console: mvmd-converter: failed to prepare the VM: no sysfs",
            "power_off"
        ]
    );
    Ok(())
}

#[tokio::test]
async fn init_reports_a_listener_that_could_not_be_bound() -> Result {
    let log = Log::default();
    let dir = TempDir::new()?;
    let output = image::output_file(dir.path())?;
    let unbound = || -> io::Result<tokio_stream::Empty<io::Result<UnixStream>>> {
        Err(io::Error::other("no vsock"))
    };
    let init = tokio::spawn(converter::run(
        unbound,
        "/absent".into(),
        system(&log, Some(output)),
    ));

    let entries = log.until("halt").await;
    init.abort();

    assert_eq!(
        entries[..4],
        [
            "prepare",
            "output_device",
            "console: mvmd-converter: failed to listen for mvmd: no vsock",
            "power_off"
        ]
    );
    Ok(())
}
