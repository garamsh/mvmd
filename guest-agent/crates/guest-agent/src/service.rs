//! The generated `GuestService`, implemented over `exec` and `files`. This is the one module that
//! names a gRPC status.

use std::io;
use std::sync::Arc;

use nix::sys::signal::Signal;
use prost::{Message, Name};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use crate::boot::Boot;
use crate::description::{BootDescription, CatchUp, NetworkConfig, boot_time};
use crate::error::{BootStep, Error, Expected, Unbooted};
use crate::exec::Execs;
use crate::files::{self, Locks, Reader, Upload};
use crate::system::{DiskError, DiskMount};
use crate::user::UserSpec;
use crate::v1alpha1::guest_service_server::GuestService;
use crate::v1alpha1::{
    BootFailed, BootRequest, BootResponse, BootState, BootStep as ProtoStep, CatchUpRequest,
    CatchUpResponse, DiskInUse, EntryKind, ExecOutputGone, ExecRequest, ExecResponse, FileEntry,
    FindDiskRequest, FindDiskResponse, GrowDiskRequest, GrowDiskResponse, HealthRequest,
    HealthResponse, InfoRequest, InfoResponse, ListFilesRequest, ListFilesResponse, NotBooted,
    OnlineVcpusRequest, OnlineVcpusResponse, PatchFileRequest, PatchFileResponse,
    ReadExecOutputRequest, ReadExecOutputResponse, ReadFileRequest, ReadFileResponse,
    ReleaseDiskRequest, ReleaseDiskResponse, SearchFilesRequest, SearchFilesResponse,
    SignalRequest, SignalResponse, TerminateRequest, TerminateResponse, UpdateNetworkRequest,
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

/// The disk ID of a request, refused when it names none.
fn named(disk_id: String) -> Result<String, Error> {
    if disk_id.is_empty() {
        return Err(Error::Invalid("disk_id is empty".into()));
    }
    Ok(disk_id)
}

/// The failure a disk call's error is.
fn disk_error(context: &str, error: DiskError) -> Error {
    match error {
        DiskError::InUse => Error::PreconditionFailed("the disk is in use".into()),
        DiskError::Unknown => Error::NotFound("the disk is not known to the guest".into()),
        DiskError::MountBusy(path) => {
            Error::PreconditionFailed(format!("the filesystem mounted at {path} is busy"))
        }
        DiskError::Conflict(reason) => Error::PreconditionFailed(reason),
        DiskError::Failed(source) => Error::Io {
            context: context.into(),
            source,
        },
    }
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
            Error::DiskInUse { disk_id, .. } => {
                with_detail(Status::failed_precondition(message), &DiskInUse { disk_id })
            }
            Error::DigestMismatch(_) => Status::data_loss(message),
            Error::Boot { step, detail } => with_detail(
                Status::internal(message),
                &BootFailed {
                    step: match step {
                        BootStep::Clock => ProtoStep::Clock,
                        BootStep::Check => ProtoStep::Check,
                        BootStep::MountImage => ProtoStep::MountImage,
                        BootStep::MountRootDisk => ProtoStep::MountRootDisk,
                        BootStep::Overlay => ProtoStep::Overlay,
                        BootStep::SwitchRoot => ProtoStep::SwitchRoot,
                        BootStep::Network => ProtoStep::Network,
                        BootStep::PlaceFiles => ProtoStep::PlaceFiles,
                        BootStep::MountDisks => ProtoStep::MountDisks,
                        BootStep::ApplyUser => ProtoStep::ApplyUser,
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
        let request = request.into_inner();
        let time = boot_time(&request)?;
        let description = BootDescription::try_from(request)?;
        self.boot.boot(description, time).await?;
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
        let request = request.into_inner();
        let identity = match UserSpec::parse(&request.user)? {
            Some(spec) => {
                let execs = self.execs.clone();
                let resolved = tokio::task::spawn_blocking(move || execs.identity(&spec))
                    .await
                    .map_err(|joined| Error::Io {
                        context: "resolve the user".into(),
                        source: std::io::Error::other(joined),
                    })?;
                Some(resolved.map_err(|error| match error.kind() {
                    std::io::ErrorKind::NotFound => Error::NotFound(error.to_string()),
                    _ => Error::Io {
                        context: "resolve the user".into(),
                        source: error,
                    },
                })?)
            }
            None => None,
        };
        let record = self.execs.start(request, identity.as_ref())?;
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

    async fn find_disk(
        &self,
        request: Request<FindDiskRequest>,
    ) -> Result<Response<FindDiskResponse>, Status> {
        // Not gated on the boot: the calls act on the kernel's device tree.
        let request = request.into_inner();
        let disk_id = named(request.disk_id)?;
        let mount = DiskMount::parse(request.mount_path, request.read_only)?;
        let disks = self.boot.disks()?;
        let device = tokio::task::spawn_blocking(move || {
            disks.find(&disk_id, mount.as_ref().filter(|_| false))
        })
        .await
        .map_err(|joined| Error::Io {
            context: "find the disk".into(),
            source: io::Error::other(joined),
        })?
        .map_err(|error| disk_error("find the disk", error))?;
        Ok(Response::new(FindDiskResponse {
            device: device.name,
            size_bytes: device.size_bytes,
            read_only: device.read_only,
        }))
    }

    async fn release_disk(
        &self,
        request: Request<ReleaseDiskRequest>,
    ) -> Result<Response<ReleaseDiskResponse>, Status> {
        let disk_id = named(request.into_inner().disk_id)?;
        let disks = self.boot.disks()?;
        let target = disk_id.clone();
        tokio::task::spawn_blocking(move || disks.release(&target))
            .await
            .map_err(|joined| Error::Io {
                context: "release the disk".into(),
                source: io::Error::other(joined),
            })?
            .map_err(|error| match error {
                DiskError::InUse => Error::DiskInUse {
                    disk_id,
                    mount: None,
                },
                DiskError::MountBusy(path) => Error::DiskInUse {
                    disk_id,
                    mount: Some(path),
                },
                other => disk_error("release the disk", other),
            })?;
        Ok(Response::new(ReleaseDiskResponse {}))
    }

    async fn grow_disk(
        &self,
        request: Request<GrowDiskRequest>,
    ) -> Result<Response<GrowDiskResponse>, Status> {
        let disk_id = named(request.into_inner().disk_id)?;
        let disks = self.boot.disks()?;
        let size_bytes = tokio::task::spawn_blocking(move || disks.grow(&disk_id))
            .await
            .map_err(|joined| Error::Io {
                context: "grow the disk".into(),
                source: io::Error::other(joined),
            })?
            .map_err(|error| disk_error("grow the disk", error))?;
        Ok(Response::new(GrowDiskResponse { size_bytes }))
    }

    async fn online_vcpus(
        &self,
        request: Request<OnlineVcpusRequest>,
    ) -> Result<Response<OnlineVcpusResponse>, Status> {
        let target = request.into_inner().vcpus;
        if target == 0 {
            return Err(Error::Invalid("vcpus is 0".into()).into());
        }
        // Not gated on the boot: the call acts on the kernel's CPU tree.
        let resources = self.boot.resources()?;
        let size = tokio::task::spawn_blocking(move || resources.online_vcpus(target))
            .await
            .map_err(|joined| Error::Io {
                context: "bring the vCPUs online".into(),
                source: io::Error::other(joined),
            })?
            .map_err(|source| Error::Io {
                context: "bring the vCPUs online".into(),
                source,
            })?;
        Ok(Response::new(OnlineVcpusResponse {
            vcpus: size.vcpus,
            memory_bytes: size.memory_bytes,
        }))
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
