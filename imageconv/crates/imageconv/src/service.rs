//! The converter channel's service: it feeds the streamed passes to `convert` on a blocking
//! thread and answers the call with its outcome. The one place that builds a gRPC status or a
//! report.

use std::io;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::Duration;

use futures_core::Stream;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::{mpsc, watch};
use tonic::transport::Server;
use tonic::transport::server::Connected;
use tonic::{Request, Response, Status, Streaming};

use crate::convert::{Layers, Parameters, Pass, PassKind, convert};
use crate::error::{Error, Step};
use crate::mkfs::Mkfs;
use crate::v1alpha1::convert_request::Message;
use crate::v1alpha1::convert_service_server::{ConvertService, ConvertServiceServer};
use crate::v1alpha1::{
    ConvertRequest, ConvertResponse, Converted, Failure, FailureStep, PassKind as WirePassKind,
    convert_response,
};

/// The largest message the service accepts: a `Data` message holds at most 1 MiB, and the rest of
/// a message is small.
const MAX_MESSAGE_BYTES: usize = 2 << 20;

/// How many messages wait between the stream and the conversion, so a conversion slower than the
/// client slows the client and holds no more than a few chunks.
const QUEUE_DEPTH: usize = 4;

/// How long the server stays up after the conversion's answer, for a client that has not read it.
const ANSWER_GRACE: Duration = Duration::from_secs(5);

/// What a conversion runs with.
#[derive(Debug, Clone)]
pub struct Converter {
    /// The pinned `mkfs.erofs`.
    pub mkfs: Mkfs,
    /// The output block device, or a file in its place.
    pub output: PathBuf,
}

/// Why the server stopped before it served a conversion.
#[derive(Debug, thiserror::Error)]
#[error("serving failed: {0}")]
pub struct ServeError(String);

/// Serve one conversion on the connections of `incoming`: the server answers the first `Convert`
/// call, refuses any other, and stops once the answer has been sent or after a grace period.
pub async fn serve<I, IO>(incoming: I, converter: Converter) -> Result<(), ServeError>
where
    I: Stream<Item = io::Result<IO>>,
    IO: AsyncRead + AsyncWrite + Connected + Unpin + Send + 'static,
    IO::ConnectInfo: Clone + Send + Sync + 'static,
{
    let (answered, mut stop) = watch::channel(false);
    let mut graceless = stop.clone();
    let service = ConvertServiceServer::new(Service {
        converter,
        answered: Mutex::new(Some(answered)),
    })
    .max_decoding_message_size(MAX_MESSAGE_BYTES)
    .max_encoding_message_size(MAX_MESSAGE_BYTES);
    let server = Server::builder()
        .add_service(service)
        .serve_with_incoming_shutdown(incoming, async move {
            let _ = stop.wait_for(|answered| *answered).await;
        });
    let grace = async move {
        let _ = graceless.wait_for(|answered| *answered).await;
        tokio::time::sleep(ANSWER_GRACE).await;
    };
    tokio::select! {
        served = server => served.map_err(|error| ServeError(error.to_string())),
        () = grace => Ok(()),
    }
}

struct Service {
    converter: Converter,
    /// Taken by the one call that is served.
    answered: Mutex<Option<watch::Sender<bool>>>,
}

#[tonic::async_trait]
impl ConvertService for Service {
    async fn convert(
        &self,
        request: Request<Streaming<ConvertRequest>>,
    ) -> Result<Response<ConvertResponse>, Status> {
        let answered = self
            .answered
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take()
            .ok_or_else(|| Status::already_exists("the converter serves one conversion"))?;
        let answer = self.converse(request.into_inner()).await;
        // The server stops once this answer has gone out, whichever it is.
        let _ = answered.send(true);
        answer.map(Response::new)
    }
}

impl Service {
    /// Read the start, run the conversion while the rest of the stream feeds it, and answer.
    async fn converse(
        &self,
        mut stream: Streaming<ConvertRequest>,
    ) -> Result<ConvertResponse, Status> {
        let parameters = match parse_start(stream.message().await?) {
            Ok(parameters) => parameters,
            Err(error) => return Ok(failure(&error)),
        };
        let (queue, received) = mpsc::channel(QUEUE_DEPTH);
        let converter = self.converter.clone();
        let mut worker = tokio::task::spawn_blocking(move || {
            convert(
                &parameters,
                &mut Queued::new(received),
                &converter.mkfs,
                &converter.output,
            )
        });
        let mut queue = Some(queue);
        let mut broken = None;
        let joined = loop {
            tokio::select! {
                joined = &mut worker => break joined,
                message = stream.message(), if queue.is_some() => match message {
                    Ok(Some(message)) => {
                        if let Some(sender) = &queue
                            && sender.send(item(message)).await.is_err()
                        {
                            // The conversion has ended; its outcome is next.
                            queue = None;
                        }
                    }
                    Ok(None) => queue = None,
                    Err(status) => {
                        broken = Some(status);
                        queue = None;
                    }
                },
            }
        };
        let outcome = joined
            .map_err(|error| Status::internal(format!("the conversion panicked: {error}")))?;
        match (outcome, broken) {
            (Ok(length), _) => Ok(ConvertResponse {
                result: Some(convert_response::Result::Converted(Converted { length })),
            }),
            // The call itself broke, so the failure that followed is not the answer.
            (Err(_), Some(status)) => Err(status),
            (Err(error), None) => Ok(failure(&error)),
        }
    }
}

/// The parameters the first message carries, or the failure that says it does not.
fn parse_start(first: Option<ConvertRequest>) -> Result<Parameters, Error> {
    match first.and_then(|request| request.message) {
        Some(Message::Start(start)) => Parameters::new(
            &start.filesystem_uuid,
            &start.layer_media_types,
            start.name_set_limit_bytes,
        ),
        _ => Err(Error::Stream("the first message is not a Start".into())),
    }
}

fn failure(error: &Error) -> ConvertResponse {
    let step = match error.step() {
        Step::Start => FailureStep::Start,
        Step::LayerType => FailureStep::LayerType,
        Step::Stream => FailureStep::Stream,
        Step::Names => FailureStep::Names,
        Step::NameLimit => FailureStep::NameLimit,
        Step::Convert => FailureStep::Convert,
        Step::Output => FailureStep::Output,
    };
    ConvertResponse {
        result: Some(convert_response::Result::Failure(Failure {
            step: step.into(),
            message: error.to_string(),
        })),
    }
}

/// What the stream delivers to the conversion.
enum Item {
    Pass(Pass),
    Data(Vec<u8>),
    /// A message the call does not allow here; the text says which.
    Misplaced(&'static str),
}

fn item(request: ConvertRequest) -> Item {
    match request.message {
        Some(Message::Pass(pass)) => {
            let kind = match WirePassKind::try_from(pass.kind) {
                Ok(WirePassKind::Names) => PassKind::Names,
                Ok(WirePassKind::Content) => PassKind::Content,
                _ => return Item::Misplaced("a pass of unspecified kind"),
            };
            Item::Pass(Pass {
                layer: pass.layer,
                kind,
            })
        }
        Some(Message::Data(data)) => Item::Data(data),
        Some(Message::Start(_)) => Item::Misplaced("a second Start"),
        None => Item::Misplaced("an empty message"),
    }
}

/// The passes as the queue delivers them, read on the conversion's thread.
struct Queued {
    received: mpsc::Receiver<Item>,
    /// A pass announced while the previous one was being read.
    announced: Option<Pass>,
    chunk: Vec<u8>,
    taken: usize,
    started: bool,
    /// The stream has ended.
    closed: bool,
}

impl Queued {
    fn new(received: mpsc::Receiver<Item>) -> Self {
        Self {
            received,
            announced: None,
            chunk: Vec::new(),
            taken: 0,
            started: false,
            closed: false,
        }
    }
}

impl Layers for Queued {
    fn next_pass(&mut self) -> io::Result<Option<Pass>> {
        self.chunk.clear();
        self.taken = 0;
        if let Some(pass) = self.announced.take() {
            return Ok(Some(pass));
        }
        loop {
            match self.received.blocking_recv() {
                None => {
                    self.closed = true;
                    return Ok(None);
                }
                Some(Item::Pass(pass)) => {
                    self.started = true;
                    return Ok(Some(pass));
                }
                // What a pass left unread.
                Some(Item::Data(_)) if self.started => {}
                Some(Item::Data(_)) => {
                    return Err(io::Error::other("data comes before the first pass"));
                }
                Some(Item::Misplaced(what)) => return Err(io::Error::other(what)),
            }
        }
    }

    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        // Once this pass has ended, it stays ended, however often it is read.
        if self.announced.is_some() || self.closed {
            return Ok(0);
        }
        while self.taken == self.chunk.len() {
            match self.received.blocking_recv() {
                // The stream ended, or the next pass began: this pass has ended.
                None => {
                    self.closed = true;
                    return Ok(0);
                }
                Some(Item::Pass(pass)) => {
                    self.announced = Some(pass);
                    return Ok(0);
                }
                Some(Item::Data(data)) => {
                    self.chunk = data;
                    self.taken = 0;
                }
                Some(Item::Misplaced(what)) => return Err(io::Error::other(what)),
            }
        }
        let count = buf.len().min(self.chunk.len() - self.taken);
        buf[..count].copy_from_slice(&self.chunk[self.taken..self.taken + count]);
        self.taken += count;
        Ok(count)
    }
}
