//! The generated `GuestService`, implemented over `exec` and `files`. This is the one module that
//! names a gRPC status.

use std::sync::Arc;

use nix::sys::signal::Signal;
use prost::{Message, Name};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use crate::boot::Boot;
use crate::description::{BootDescription, CatchUp, NetworkConfig};
use crate::error::{BootStep, Error, Expected, Unbooted};
use crate::exec::Execs;
use crate::files::{self, Locks, Reader, Upload};
use crate::v1alpha1::guest_service_server::GuestService;
use crate::v1alpha1::{
    BootFailed, BootRequest, BootResponse, BootState, BootStep as ProtoStep, CatchUpRequest,
    CatchUpResponse, EntryKind, ExecOutputGone, ExecRequest, ExecResponse, FileEntry,
    HealthRequest, HealthResponse, InfoRequest, InfoResponse, ListFilesRequest, ListFilesResponse,
    NotBooted, PatchFileRequest, PatchFileResponse, ReadExecOutputRequest, ReadExecOutputResponse,
    ReadFileRequest, ReadFileResponse, SearchFilesRequest, SearchFilesResponse, SignalRequest,
    SignalResponse, TerminateRequest, TerminateResponse, UpdateNetworkRequest,
    UpdateNetworkResponse, WriteFileRequest, WriteFileResponse, WrongFileKind,
    read_exec_output_response, write_file_request,
};

/// The contract versions the agent serves.
const API_VERSIONS: [&str; 1] = ["v1alpha1"];

/// Messages buffered between a producing task and its stream.
const STREAM_DEPTH: usize = 16;

/// Entries per message of a listing or a search.
const ENTRIES_PER_MESSAGE: usize = 512;

/// The largest request the service accepts, which bounds a patch's diff.
pub(crate) const MAX_REQUEST_BYTES: usize = 16 * 1024 * 1024;

/// The largest response the service sends; a message of a stream is far below it.
pub(crate) const MAX_RESPONSE_BYTES: usize = 4 * 1024 * 1024;

/// A result sent as a stream of messages, each holding a run of entries.
type Batches<M> = tokio_stream::Iter<std::vec::IntoIter<Result<M, Status>>>;

fn batches<M>(entries: Vec<FileEntry>, message: impl Fn(Vec<FileEntry>) -> M) -> Batches<M> {
    let messages: Vec<Result<M, Status>> = entries
        .chunks(ENTRIES_PER_MESSAGE)
        .map(|run| Ok(message(run.to_vec())))
        .collect();
    tokio_stream::iter(messages)
}

/// The guest channel's service.
pub(crate) struct Guest {
    pub(crate) execs: Arc<Execs>,
    pub(crate) locks: Locks,
    pub(crate) boot: Arc<Boot>,
    /// Where `Terminate` sends its `SIGTERM`: the init's supervision. `None` when the agent is
    /// not the guest's init.
    pub(crate) terminate: Option<mpsc::Sender<Signal>>,
}

/// A `google.rpc.Status`, the message the `grpc-status-details-bin` trailer carries. It is spelled
/// out here because a detail of the agent's own is a message tonic's standard error types do not
/// cover.
#[derive(Clone, PartialEq, prost::Message)]
struct RpcStatus {
    #[prost(int32, tag = "1")]
    code: i32,
    #[prost(string, tag = "2")]
    message: String,
    #[prost(message, repeated, tag = "3")]
    details: Vec<Any>,
}

/// A `google.protobuf.Any`.
#[derive(Clone, PartialEq, prost::Message)]
struct Any {
    #[prost(string, tag = "1")]
    type_url: String,
    #[prost(bytes = "vec", tag = "2")]
    value: Vec<u8>,
}

/// `status` with `detail` in its details, named by the full name of the message type.
fn with_detail<M: Message + Name>(status: Status, detail: &M) -> Status {
    let rpc = RpcStatus {
        code: status.code() as i32,
        message: status.message().to_owned(),
        details: vec![Any {
            type_url: format!("type.googleapis.com{}", M::type_url()),
            value: detail.encode_to_vec(),
        }],
    };
    Status::with_details(status.code(), status.message(), rpc.encode_to_vec().into())
}

impl From<Error> for Status {
    fn from(error: Error) -> Self {
        let message = error.to_string();
        match error {
            Error::Invalid(_) => Status::invalid_argument(message),
            Error::WrongKind { path, expected } => with_detail(
                Status::invalid_argument(message),
                &WrongFileKind {
                    path,
                    expected: match expected {
                        Expected::File => EntryKind::File,
                        Expected::Directory => EntryKind::Directory,
                    } as i32,
                },
            ),
            Error::OutOfRange { earliest_offset } => with_detail(
                Status::out_of_range(message),
                &ExecOutputGone { earliest_offset },
            ),
            Error::NotFound(_) => Status::not_found(message),
            Error::PermissionDenied(_) => Status::permission_denied(message),
            Error::PreconditionFailed(_) => Status::failed_precondition(message),
            Error::NotBooted { state, .. } => with_detail(
                Status::failed_precondition(message),
                &NotBooted {
                    state: match state {
                        Unbooted::Pending => BootState::Pending,
                        Unbooted::Failed => BootState::Failed,
                    } as i32,
                },
            ),
            Error::DigestMismatch(_) => Status::data_loss(message),
            Error::Boot { step, detail } => with_detail(
                Status::internal(message),
                &BootFailed {
                    step: match step {
                        BootStep::Check => ProtoStep::Check,
                        BootStep::MountImage => ProtoStep::MountImage,
                        BootStep::MountRootDisk => ProtoStep::MountRootDisk,
                        BootStep::Overlay => ProtoStep::Overlay,
                        BootStep::SwitchRoot => ProtoStep::SwitchRoot,
                        BootStep::Network => ProtoStep::Network,
                        BootStep::Entrypoint => ProtoStep::Entrypoint,
                    } as i32,
                    detail,
                },
            ),
            Error::Io { .. } | Error::Serve(_) => Status::internal(message),
        }
    }
}

#[tonic::async_trait]
impl GuestService for Guest {
    async fn boot(&self, request: Request<BootRequest>) -> Result<Response<BootResponse>, Status> {
        let description = BootDescription::try_from(request.into_inner())?;
        self.boot.boot(description).await?;
        Ok(Response::new(BootResponse {}))
    }

    async fn update_network(
        &self,
        request: Request<UpdateNetworkRequest>,
    ) -> Result<Response<UpdateNetworkResponse>, Status> {
        // A request without a network is not an empty network: applying it would wipe the
        // Machine's.
        let network = request
            .into_inner()
            .network
            .ok_or_else(|| Error::Invalid("network is missing".into()))?;
        let config = NetworkConfig::try_from(network)?;
        self.boot.update_network(config).await?;
        Ok(Response::new(UpdateNetworkResponse {}))
    }

    async fn catch_up(
        &self,
        request: Request<CatchUpRequest>,
    ) -> Result<Response<CatchUpResponse>, Status> {
        let request = CatchUp::try_from(request.into_inner())?;
        self.boot.catch_up(request).await?;
        Ok(Response::new(CatchUpResponse {}))
    }

    async fn exec(&self, request: Request<ExecRequest>) -> Result<Response<ExecResponse>, Status> {
        self.boot.ready()?;
        let record = self.execs.start(request.into_inner())?;
        Ok(Response::new(ExecResponse {
            record: Some(record),
        }))
    }

    type ReadExecOutputStream = ReceiverStream<Result<ReadExecOutputResponse, Status>>;

    async fn read_exec_output(
        &self,
        request: Request<ReadExecOutputRequest>,
    ) -> Result<Response<Self::ReadExecOutputStream>, Status> {
        self.boot.ready()?;
        let request = request.into_inner();
        let mut follower = self.execs.follow(&request.exec_id, request.after_offset)?;
        let (tx, rx) = mpsc::channel(STREAM_DEPTH);
        tokio::spawn(async move {
            loop {
                let output = match follower.next().await {
                    Ok(Some(output)) => output,
                    Ok(None) => return,
                    Err(error) => {
                        // The reader fell behind the retention; it reads again from the offset
                        // the status names.
                        let _ = tx.send(Err(Status::from(error))).await;
                        return;
                    }
                };
                let parts = output
                    .chunks
                    .into_iter()
                    .map(read_exec_output_response::Part::Chunk)
                    .chain(output.record.map(read_exec_output_response::Part::Record));
                for part in parts {
                    let message = ReadExecOutputResponse { part: Some(part) };
                    if tx.send(Ok(message)).await.is_err() {
                        // The client went away; it resumes by offset.
                        return;
                    }
                }
            }
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }

    async fn signal(
        &self,
        request: Request<SignalRequest>,
    ) -> Result<Response<SignalResponse>, Status> {
        self.boot.ready()?;
        let request = request.into_inner();
        self.execs.signal(&request.exec_id, request.signal)?;
        Ok(Response::new(SignalResponse {}))
    }

    type ReadFileStream = ReceiverStream<Result<ReadFileResponse, Status>>;

    async fn read_file(
        &self,
        request: Request<ReadFileRequest>,
    ) -> Result<Response<Self::ReadFileStream>, Status> {
        self.boot.ready()?;
        let mut reader = Reader::open(&request.into_inner().path).await?;
        let (tx, rx) = mpsc::channel(STREAM_DEPTH);
        tokio::spawn(async move {
            loop {
                let message = match reader.next().await {
                    Ok(Some(chunk)) => Ok(ReadFileResponse { chunk }),
                    Ok(None) => return,
                    Err(error) => Err(Status::from(error)),
                };
                let failed = message.is_err();
                if tx.send(message).await.is_err() || failed {
                    return;
                }
            }
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }

    async fn write_file(
        &self,
        request: Request<Streaming<WriteFileRequest>>,
    ) -> Result<Response<WriteFileResponse>, Status> {
        self.boot.ready()?;
        let mut stream = request.into_inner();
        let mut upload: Option<Upload> = None;
        while let Some(message) = stream.message().await? {
            match (message.part, upload.as_mut()) {
                (Some(write_file_request::Part::Start(start)), None) => {
                    upload = Some(Upload::begin(&start.path).await?);
                }
                (Some(write_file_request::Part::Chunk(chunk)), Some(upload)) => {
                    upload.chunk(&chunk).await?;
                }
                (Some(write_file_request::Part::End(end)), Some(_)) => {
                    if let Some(upload) = upload.take() {
                        upload.finish(&self.locks, &end.digest).await?;
                    }
                    return Ok(Response::new(WriteFileResponse {}));
                }
                _ => {
                    return Err(
                        Error::Invalid("a write is a start, chunks, then an end".into()).into(),
                    );
                }
            }
        }
        // The stream ended without an end message: the upload drops and the old content stays.
        Err(Error::Invalid("the write stream ended before its end message".into()).into())
    }

    async fn patch_file(
        &self,
        request: Request<PatchFileRequest>,
    ) -> Result<Response<PatchFileResponse>, Status> {
        self.boot.ready()?;
        let request = request.into_inner();
        let outcome = files::patch(
            &self.locks,
            &request.path,
            &request.base_digest,
            &request.expected_digest,
            &request.patch,
        )
        .await?;
        Ok(Response::new(PatchFileResponse {
            outcome: outcome.into(),
        }))
    }

    type ListFilesStream = Batches<ListFilesResponse>;

    async fn list_files(
        &self,
        request: Request<ListFilesRequest>,
    ) -> Result<Response<Self::ListFilesStream>, Status> {
        self.boot.ready()?;
        let entries = files::list(&request.into_inner().path).await?;
        Ok(Response::new(batches(entries, |entries| {
            ListFilesResponse { entries }
        })))
    }

    type SearchFilesStream = Batches<SearchFilesResponse>;

    async fn search_files(
        &self,
        request: Request<SearchFilesRequest>,
    ) -> Result<Response<Self::SearchFilesStream>, Status> {
        self.boot.ready()?;
        let request = request.into_inner();
        let matches = files::search(&request.path, &request.pattern).await?;
        Ok(Response::new(batches(matches, |matches| {
            SearchFilesResponse { matches }
        })))
    }

    async fn terminate(
        &self,
        _request: Request<TerminateRequest>,
    ) -> Result<Response<TerminateResponse>, Status> {
        // Not gated on the boot: before the entrypoint runs, a termination ends the Machine.
        let terminate = self.terminate.as_ref().ok_or_else(|| {
            Error::PreconditionFailed(
                "this agent is not the guest's init and cannot stop it".into(),
            )
        })?;
        terminate.send(Signal::SIGTERM).await.map_err(|_| {
            Error::PreconditionFailed("the agent is already ending the Machine".into())
        })?;
        Ok(Response::new(TerminateResponse {}))
    }

    async fn health(
        &self,
        _request: Request<HealthRequest>,
    ) -> Result<Response<HealthResponse>, Status> {
        Ok(Response::new(HealthResponse {}))
    }

    async fn info(&self, _request: Request<InfoRequest>) -> Result<Response<InfoResponse>, Status> {
        Ok(Response::new(InfoResponse {
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            api_versions: API_VERSIONS.map(str::to_owned).to_vec(),
        }))
    }
}
