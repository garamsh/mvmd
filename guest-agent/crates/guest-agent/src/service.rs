//! The generated `GuestService`, implemented over `exec` and `files`. This is the one module that
//! names a gRPC status.

use std::sync::Arc;

use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use crate::error::Error;
use crate::exec::Execs;
use crate::files::{self, Reader, Upload};
use crate::v1alpha1::guest_service_server::GuestService;
use crate::v1alpha1::{
    ExecRequest, ExecResponse, HealthRequest, HealthResponse, InfoRequest, InfoResponse,
    ListFilesRequest, ListFilesResponse, PatchFileRequest, PatchFileResponse,
    ReadExecOutputRequest, ReadExecOutputResponse, ReadFileRequest, ReadFileResponse,
    SearchFilesRequest, SearchFilesResponse, SignalRequest, SignalResponse, WriteFileRequest,
    WriteFileResponse, read_exec_output_response, write_file_request,
};

/// The contract versions the agent serves.
const API_VERSIONS: [&str; 1] = ["v1alpha1"];

/// Messages buffered between a producing task and its stream.
const STREAM_DEPTH: usize = 16;

/// The guest channel's service.
pub(crate) struct Guest {
    pub(crate) execs: Arc<Execs>,
}

impl From<Error> for Status {
    fn from(error: Error) -> Self {
        let message = error.to_string();
        match error {
            Error::Invalid(_) => Status::invalid_argument(message),
            Error::NotFound(_) => Status::not_found(message),
            Error::PermissionDenied(_) => Status::permission_denied(message),
            Error::PreconditionFailed(_) => Status::failed_precondition(message),
            Error::DigestMismatch(_) => Status::data_loss(message),
            Error::Io { .. } | Error::Serve(_) => Status::internal(message),
        }
    }
}

#[tonic::async_trait]
impl GuestService for Guest {
    async fn exec(&self, request: Request<ExecRequest>) -> Result<Response<ExecResponse>, Status> {
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
        let request = request.into_inner();
        let mut follower = self.execs.follow(&request.exec_id, request.after_offset)?;
        let (tx, rx) = mpsc::channel(STREAM_DEPTH);
        tokio::spawn(async move {
            while let Some(output) = follower.next().await {
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
        let request = request.into_inner();
        self.execs.signal(&request.exec_id, request.signal)?;
        Ok(Response::new(SignalResponse {}))
    }

    type ReadFileStream = ReceiverStream<Result<ReadFileResponse, Status>>;

    async fn read_file(
        &self,
        request: Request<ReadFileRequest>,
    ) -> Result<Response<Self::ReadFileStream>, Status> {
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
                        upload.finish(&end.digest).await?;
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
        let request = request.into_inner();
        let outcome = files::patch(
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

    async fn list_files(
        &self,
        request: Request<ListFilesRequest>,
    ) -> Result<Response<ListFilesResponse>, Status> {
        let entries = files::list(&request.into_inner().path).await?;
        Ok(Response::new(ListFilesResponse { entries }))
    }

    async fn search_files(
        &self,
        request: Request<SearchFilesRequest>,
    ) -> Result<Response<SearchFilesResponse>, Status> {
        let request = request.into_inner();
        let matches = files::search(&request.path, &request.pattern).await?;
        Ok(Response::new(SearchFilesResponse { matches }))
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
