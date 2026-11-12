//! The Guest Agent's service, driven through the generated client over a Unix socket in a
//! temporary directory. The server code is the production one; only the listener differs.

use std::collections::HashMap;
use std::io;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

use guest_agent::v1alpha1::guest_service_client::GuestServiceClient;
use guest_agent::v1alpha1::{
    EntryKind, ExecOutputGone, ExecRecord, ExecRequest, ExecStatus, FileEntry, HealthRequest,
    InfoRequest, ListFilesRequest, OutputChunk, OutputStream, PatchFileRequest, PatchOutcome,
    ReadExecOutputRequest, ReadFileRequest, SearchFilesRequest, SignalRequest, WriteFileEnd,
    WriteFileRequest, WriteFileStart, WrongFileKind, read_exec_output_response, write_file_request,
};
use hyper_util::rt::TokioIo;
use sha2::{Digest, Sha256};
use tempfile::TempDir;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::oneshot;
use tokio::task::JoinHandle;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Endpoint};
use tonic::{Code, Streaming};
use tower::service_fn;

#[path = "support/details.rs"]
mod details;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

struct Harness {
    client: GuestServiceClient<Channel>,
    /// A directory for the files the tests work on; the socket is elsewhere.
    dir: TempDir,
    _socket_dir: TempDir,
    stop: Option<oneshot::Sender<()>>,
    server: JoinHandle<std::result::Result<(), guest_agent::Error>>,
}

impl Harness {
    async fn start() -> Result<Self> {
        Self::start_with(guest_agent::Config::default()).await
    }

    async fn start_with(config: guest_agent::Config) -> Result<Self> {
        let socket_dir = tempfile::tempdir()?;
        let socket = socket_dir.path().join("agent.sock");
        let incoming = UnixListenerStream::new(UnixListener::bind(&socket)?);
        let (stop, stopped) = oneshot::channel::<()>();
        let server = tokio::spawn(guest_agent::serve(incoming, config, async {
            stopped.await.ok();
        }));
        let channel = Endpoint::try_from("http://agent.invalid")?
            .connect_with_connector(service_fn(move |_| {
                let socket = socket.clone();
                async move { Ok::<_, io::Error>(TokioIo::new(UnixStream::connect(socket).await?)) }
            }))
            .await?;
        Ok(Self {
            client: GuestServiceClient::new(channel),
            dir: tempfile::tempdir()?,
            _socket_dir: socket_dir,
            stop: Some(stop),
            server,
        })
    }

    fn path(&self, name: &str) -> PathBuf {
        self.dir.path().join(name)
    }

    async fn exec(&mut self, id: &str, argv: &[&str]) -> Result<ExecRecord> {
        self.exec_with(id, argv, HashMap::new(), "").await
    }

    async fn exec_with(
        &mut self,
        id: &str,
        argv: &[&str],
        env: HashMap<String, String>,
        working_dir: &str,
    ) -> Result<ExecRecord> {
        let response = self
            .client
            .exec(ExecRequest {
                exec_id: id.into(),
                argv: argv.iter().map(|a| (*a).to_owned()).collect(),
                env,
                working_dir: working_dir.into(),
                ..ExecRequest::default()
            })
            .await?;
        Ok(response.into_inner().record.ok_or("no record")?)
    }

    /// Read the output after an offset until the stream ends with the final record.
    async fn output(&mut self, id: &str, after: u64) -> Result<(Vec<OutputChunk>, ExecRecord)> {
        let mut stream = self
            .client
            .read_exec_output(ReadExecOutputRequest {
                exec_id: id.into(),
                after_offset: after,
            })
            .await?
            .into_inner();
        collect_output(&mut stream).await
    }

    async fn write(&mut self, path: &Path, content: &[u8]) -> Result {
        let digest = digest_of(content);
        self.write_parts(path, content, &digest).await
    }

    async fn write_parts(&mut self, path: &Path, content: &[u8], digest: &str) -> Result {
        let mut parts = vec![WriteFileRequest {
            part: Some(write_file_request::Part::Start(WriteFileStart {
                path: path.to_str().ok_or("path")?.into(),
            })),
        }];
        parts.extend(content.chunks(10).map(|chunk| WriteFileRequest {
            part: Some(write_file_request::Part::Chunk(chunk.to_vec())),
        }));
        parts.push(WriteFileRequest {
            part: Some(write_file_request::Part::End(WriteFileEnd {
                digest: digest.into(),
            })),
        });
        self.client.write_file(tokio_stream::iter(parts)).await?;
        Ok(())
    }

    async fn read(&mut self, path: &Path) -> std::result::Result<Vec<Vec<u8>>, tonic::Status> {
        let mut stream = self
            .client
            .read_file(ReadFileRequest {
                path: path.to_str().unwrap_or_default().into(),
            })
            .await?
            .into_inner();
        let mut chunks = Vec::new();
        while let Some(message) = stream.message().await? {
            chunks.push(message.chunk);
        }
        Ok(chunks)
    }

    async fn patch(
        &mut self,
        path: &Path,
        base: &[u8],
        expected: &[u8],
        diff: &str,
    ) -> std::result::Result<PatchOutcome, tonic::Status> {
        let response = self
            .client
            .patch_file(PatchFileRequest {
                path: path.to_str().unwrap_or_default().into(),
                base_digest: digest_of(base),
                expected_digest: digest_of(expected),
                patch: diff.as_bytes().to_vec(),
            })
            .await?;
        Ok(response.into_inner().outcome())
    }

    /// The messages of a listing, each as its run of entries.
    async fn list(
        &mut self,
        path: &Path,
    ) -> std::result::Result<Vec<Vec<FileEntry>>, tonic::Status> {
        let mut stream = self
            .client
            .list_files(ListFilesRequest {
                path: path.to_str().unwrap_or_default().into(),
            })
            .await?
            .into_inner();
        let mut runs = Vec::new();
        while let Some(message) = stream.message().await? {
            runs.push(message.entries);
        }
        Ok(runs)
    }

    /// The matches of a search, flattened across the messages of its stream.
    async fn search(
        &mut self,
        path: &Path,
        pattern: &str,
    ) -> std::result::Result<Vec<FileEntry>, tonic::Status> {
        let mut stream = self
            .client
            .search_files(SearchFilesRequest {
                path: path.to_str().unwrap_or_default().into(),
                pattern: pattern.into(),
            })
            .await?
            .into_inner();
        let mut matches = Vec::new();
        while let Some(message) = stream.message().await? {
            matches.extend(message.matches);
        }
        Ok(matches)
    }
}

async fn collect_output(
    stream: &mut Streaming<guest_agent::v1alpha1::ReadExecOutputResponse>,
) -> Result<(Vec<OutputChunk>, ExecRecord)> {
    let mut chunks = Vec::new();
    while let Some(message) = stream.message().await? {
        match message.part.ok_or("empty part")? {
            read_exec_output_response::Part::Chunk(chunk) => chunks.push(chunk),
            read_exec_output_response::Part::Record(record) => return Ok((chunks, record)),
        }
    }
    Err("the stream ended without a record".into())
}

fn digest_of(content: &[u8]) -> String {
    let hex: String = Sha256::digest(content)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect();
    format!("sha256:{hex}")
}

/// One stream's bytes out of the chunks.
fn stream_bytes(chunks: &[OutputChunk], stream: OutputStream) -> Vec<u8> {
    chunks
        .iter()
        .filter(|c| c.stream() == stream)
        .flat_map(|c| c.data.clone())
        .collect()
}

fn code(error: &(dyn std::error::Error + 'static)) -> Option<Code> {
    error
        .downcast_ref::<tonic::Status>()
        .map(tonic::Status::code)
}

const SH: &str = "/bin/sh";

#[tokio::test]
async fn exec_reports_output_per_stream_and_the_exit_code() -> Result {
    let mut h = Harness::start().await?;
    let started = h
        .exec("e1", &[SH, "-c", "echo out; echo err >&2; exit 3"])
        .await?;
    assert_eq!(started.status(), ExecStatus::Started);

    let (chunks, record) = h.output("e1", 0).await?;

    assert_eq!(stream_bytes(&chunks, OutputStream::Stdout), b"out\n");
    assert_eq!(stream_bytes(&chunks, OutputStream::Stderr), b"err\n");
    // One sequence: each chunk starts where the previous one ended.
    let mut next = 0;
    for chunk in &chunks {
        assert_eq!(chunk.offset, next);
        next += chunk.data.len() as u64;
    }
    assert_eq!(next, 8);
    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.exit_code, 3);
    assert_eq!(record.term_signal, 0);
    Ok(())
}

#[tokio::test]
async fn an_exited_command_carries_the_guest_clock_between_before_its_end_and_after_its_reap()
-> Result {
    let mut h = Harness::start().await?;
    let dir = tempfile::tempdir()?;
    let flag = dir.path().join("end-now");
    let script = format!("while [ ! -e {} ]; do sleep 0.02; done", flag.display());
    let started = h.exec("e1", &[SH, "-c", &script]).await?;
    // A command that runs has no end time yet.
    assert_eq!(started.status(), ExecStatus::Started);
    assert_eq!(started.ended_at, None);

    let before = SystemTime::now();
    std::fs::write(&flag, "")?;
    let (_, record) = h.output("e1", 0).await?;
    let after = SystemTime::now();

    assert_eq!(record.status(), ExecStatus::Exited);
    let ended = SystemTime::try_from(record.ended_at.ok_or("an exited record has no ended_at")?)?;
    assert!(
        before <= ended,
        "ended_at {ended:?} is before the end {before:?}"
    );
    assert!(
        ended <= after,
        "ended_at {ended:?} is after the reap {after:?}"
    );
    Ok(())
}

#[tokio::test]
async fn a_command_that_did_not_start_has_no_end_time() -> Result {
    let mut h = Harness::start().await?;

    let record = h.exec("e1", &["/nonexistent/program"]).await?;

    assert_eq!(record.status(), ExecStatus::NotStarted);
    assert_eq!(record.ended_at, None);
    Ok(())
}

#[tokio::test]
async fn exec_uses_exactly_the_given_environment_and_working_directory() -> Result {
    let mut h = Harness::start().await?;
    let dir = h.dir.path().canonicalize()?;
    let env = HashMap::from([("GREETING".to_owned(), "hello".to_owned())]);
    h.exec_with(
        "e1",
        &[SH, "-c", "echo $GREETING; echo ${HOME:-unset}; pwd"],
        env,
        dir.to_str().ok_or("dir")?,
    )
    .await?;

    let (chunks, record) = h.output("e1", 0).await?;

    let out = String::from_utf8(stream_bytes(&chunks, OutputStream::Stdout))?;
    assert_eq!(out, format!("hello\nunset\n{}\n", dir.display()));
    assert_eq!(record.exit_code, 0);
    Ok(())
}

#[tokio::test]
async fn repeating_exec_with_the_same_id_does_not_run_the_command_again() -> Result {
    let mut h = Harness::start().await?;
    let log = h.path("log");
    let script = format!("echo ran >> {}", log.display());
    h.exec("e1", &[SH, "-c", &script]).await?;
    let (_, finished) = h.output("e1", 0).await?;

    let repeat = h.exec("e1", &[SH, "-c", &script]).await?;

    // Give a second run, if one started, time to append before the log is read.
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    assert_eq!(std::fs::read_to_string(&log)?, "ran\n");
    assert_eq!(repeat, finished);
    // Control: a new ID runs the same command, so the log would show a second run.
    h.exec("e2", &[SH, "-c", &script]).await?;
    h.output("e2", 0).await?;
    assert_eq!(std::fs::read_to_string(&log)?, "ran\nran\n");
    Ok(())
}

#[tokio::test]
async fn a_command_that_cannot_start_is_recorded_and_repeats_the_same() -> Result {
    let mut h = Harness::start().await?;

    let first = h.exec("e1", &["/nonexistent/program"]).await?;
    let repeat = h.exec("e1", &["/nonexistent/program"]).await?;

    assert_eq!(first.status(), ExecStatus::NotStarted);
    assert!(first.reason.contains("/nonexistent/program"));
    assert_eq!(repeat, first);
    let (chunks, record) = h.output("e1", 0).await?;
    assert!(chunks.is_empty());
    assert_eq!(record, first);
    Ok(())
}

#[tokio::test]
async fn output_resumes_after_an_offset_inside_a_chunk() -> Result {
    let mut h = Harness::start().await?;
    h.exec("e1", &[SH, "-c", "printf 'hello world'"]).await?;
    h.output("e1", 0).await?;

    let (chunks, record) = h.output("e1", 6).await?;

    assert_eq!(chunks.len(), 1);
    assert_eq!(chunks[0].offset, 6);
    assert_eq!(chunks[0].data, b"world");
    assert_eq!(record.status(), ExecStatus::Exited);
    let past_the_end = h
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "e1".into(),
            after_offset: 99,
        })
        .await
        .err()
        .ok_or("a read past the end was accepted")?;
    assert_eq!(past_the_end.code(), Code::InvalidArgument);
    Ok(())
}

#[tokio::test]
async fn output_follows_a_running_command_until_it_ends() -> Result {
    let mut h = Harness::start().await?;
    h.exec("e1", &[SH, "-c", "echo first; sleep 0.3; echo second"])
        .await?;

    let (chunks, record) = h.output("e1", 0).await?;

    assert_eq!(
        stream_bytes(&chunks, OutputStream::Stdout),
        b"first\nsecond\n"
    );
    assert_eq!(record.status(), ExecStatus::Exited);
    Ok(())
}

#[tokio::test]
async fn a_command_ends_even_when_a_process_it_left_behind_holds_its_output_open() -> Result {
    let mut h = Harness::start().await?;
    h.exec("e1", &[SH, "-c", "sleep 3 & echo done"]).await?;

    let (chunks, record) =
        tokio::time::timeout(std::time::Duration::from_secs(2), h.output("e1", 0)).await??;

    assert_eq!(stream_bytes(&chunks, OutputStream::Stdout), b"done\n");
    assert_eq!(record.status(), ExecStatus::Exited);
    Ok(())
}

#[tokio::test]
async fn signal_ends_a_running_command_and_the_record_names_it() -> Result {
    let mut h = Harness::start().await?;
    h.exec("e1", &["/bin/sleep", "30"]).await?;

    h.client
        .signal(SignalRequest {
            exec_id: "e1".into(),
            signal: 15,
        })
        .await?;
    let (_, record) = h.output("e1", 0).await?;

    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.term_signal, 15);
    // The command has ended, so another signal has no process to reach.
    let again = h
        .client
        .signal(SignalRequest {
            exec_id: "e1".into(),
            signal: 15,
        })
        .await
        .err()
        .ok_or("a signal to an ended command was accepted")?;
    assert_eq!(again.code(), Code::FailedPrecondition);
    Ok(())
}

#[tokio::test]
async fn exec_refuses_a_request_that_names_no_command_or_no_id() -> Result {
    let mut h = Harness::start().await?;
    let no_command = h.exec("e1", &[]).await.err().ok_or("empty argv")?;
    let no_id = h.exec("", &["/bin/true"]).await.err().ok_or("empty id")?;
    let unknown = h
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "nope".into(),
            after_offset: 0,
        })
        .await
        .err()
        .ok_or("unknown exec")?;

    assert_eq!(code(&*no_command), Some(Code::InvalidArgument));
    assert_eq!(code(&*no_id), Some(Code::InvalidArgument));
    assert_eq!(unknown.code(), Code::NotFound);
    // None of these is a condition a client acts on beyond its code.
    assert!(details::detail_types(&unknown).is_empty());
    // Control: the same ID with a command is started, and its output is then readable.
    let accepted = h.exec("e1", &["/bin/true"]).await?;
    assert_eq!(accepted.status(), ExecStatus::Started);
    assert_eq!(h.output("e1", 0).await?.1.status(), ExecStatus::Exited);
    Ok(())
}

#[tokio::test]
async fn ended_at_is_the_reap_and_not_the_end_of_the_drain_of_pipes_the_command_outlived() -> Result
{
    let mut h = Harness::start().await?;
    // The shell exits at once; the background sleep holds the output pipes for the drain grace.
    let before = SystemTime::now();
    h.exec("e1", &[SH, "-c", "sleep 3 & echo started"]).await?;
    let (_, record) = h.output("e1", 0).await?;
    let after = SystemTime::now();

    assert_eq!(record.status(), ExecStatus::Exited);
    let ended = SystemTime::try_from(record.ended_at.ok_or("an exited record has no ended_at")?)?;
    assert!(before <= ended, "ended_at {ended:?} is before the start");
    // The record became final a drain grace (one second) after the reap, so a time stamped when
    // the output drained would be that far from the reap.
    let drain = after.duration_since(ended)?;
    assert!(
        drain >= std::time::Duration::from_millis(500),
        "ended_at {ended:?} is only {drain:?} before the record was final at {after:?}"
    );
    Ok(())
}

#[tokio::test]
async fn a_signal_after_the_command_was_reaped_does_not_reach_a_process_id() -> Result {
    let mut h = Harness::start().await?;
    // The shell exits at once; the background sleep holds the output pipes for the drain grace.
    h.exec("e1", &[SH, "-c", "sleep 2 & echo started"]).await?;
    let mut stream = h
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "e1".into(),
            after_offset: 0,
        })
        .await?
        .into_inner();
    let first = stream.message().await?.ok_or("no output")?;
    assert!(matches!(
        first.part,
        Some(read_exec_output_response::Part::Chunk(_))
    ));
    // The shell is reaped by now; the record is not final, as the pipes are still open.
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    let late = h
        .client
        .signal(SignalRequest {
            exec_id: "e1".into(),
            signal: 15,
        })
        .await
        .err()
        .ok_or("a signal to a reaped command was sent")?;

    assert_eq!(late.code(), Code::FailedPrecondition);
    // Control: a command that is still running takes the same signal.
    h.exec("e2", &["/bin/sleep", "30"]).await?;
    h.client
        .signal(SignalRequest {
            exec_id: "e2".into(),
            signal: 15,
        })
        .await?;
    assert_eq!(h.output("e2", 0).await?.1.term_signal, 15);
    Ok(())
}

#[tokio::test]
async fn two_starts_of_one_id_at_once_run_the_command_once() -> Result {
    let mut h = Harness::start().await?;
    let log = h.path("log");
    let script = format!("echo ran >> {}", log.display());
    let request = || ExecRequest {
        exec_id: "e1".into(),
        argv: vec![SH.into(), "-c".into(), script.clone()],
        env: HashMap::new(),
        working_dir: String::new(),
        ..ExecRequest::default()
    };
    let (mut a, mut b) = (h.client.clone(), h.client.clone());

    let (first, second) = tokio::join!(a.exec(request()), b.exec(request()));

    assert_eq!(first?.into_inner().record.ok_or("record")?.exec_id, "e1");
    assert_eq!(second?.into_inner().record.ok_or("record")?.exec_id, "e1");
    h.output("e1", 0).await?;
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    assert_eq!(std::fs::read_to_string(&log)?, "ran\n");
    Ok(())
}

fn retention(bytes: u64) -> guest_agent::Config {
    guest_agent::Config {
        exec_output_retention: bytes,
        ..guest_agent::Config::default()
    }
}

#[tokio::test]
async fn output_older_than_the_retention_is_refused_with_the_earliest_offset_held() -> Result {
    let mut h = Harness::start_with(retention(100)).await?;
    // 1000 bytes in one stream: only the newest 100 are held.
    h.exec("e1", &[SH, "-c", "head -c 1000 /dev/zero | tr '\\0' x"])
        .await?;
    // Reading from 900 is refused as past the end until the output exists, so retry until it does.
    let finished = tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            match h.output("e1", 900).await {
                Ok((_, record)) => return Ok(record),
                Err(e) if code(&*e) == Some(Code::InvalidArgument) => {
                    tokio::time::sleep(std::time::Duration::from_millis(20)).await;
                }
                Err(e) => return Err(e),
            }
        }
    })
    .await??;
    assert_eq!(finished.earliest_offset, 900);

    let old = h
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "e1".into(),
            after_offset: 10,
        })
        .await
        .err()
        .ok_or("a read of dropped output was accepted")?;

    assert_eq!(old.code(), Code::OutOfRange);
    // The earliest offset is read from the detail and not from the message.
    let gone: ExecOutputGone = details::detail(&old).ok_or("no ExecOutputGone detail")?;
    assert_eq!(gone.earliest_offset, 900);
    assert_eq!(
        details::detail_types(&old),
        ["mvmd.guest.v1alpha1.ExecOutputGone"]
    );
    // Control: a read at the earliest held offset, and one inside the held range, succeed.
    let (chunks, record) = h.output("e1", 900).await?;
    assert_eq!(chunks.iter().map(|c| c.data.len()).sum::<usize>(), 100);
    assert_eq!(chunks[0].offset, 900);
    assert_eq!(record.status(), ExecStatus::Exited);
    let (chunks, _) = h.output("e1", 950).await?;
    assert_eq!(chunks[0].offset, 950);
    Ok(())
}

#[tokio::test]
async fn output_within_the_retention_is_all_held() -> Result {
    let mut h = Harness::start_with(retention(100)).await?;
    h.exec("e1", &[SH, "-c", "printf 'short'"]).await?;

    let (chunks, record) = h.output("e1", 0).await?;

    assert_eq!(stream_bytes(&chunks, OutputStream::Stdout), b"short");
    assert_eq!(record.earliest_offset, 0);
    Ok(())
}

#[tokio::test]
async fn a_command_still_running_when_the_agent_ends_is_recorded_as_cut_short() -> Result {
    let mut h = Harness::start().await?;
    let started = h.exec("e1", &["/bin/sleep", "30"]).await?;
    assert_eq!(started.status(), ExecStatus::Started);
    let mut stream = h
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "e1".into(),
            after_offset: 0,
        })
        .await?
        .into_inner();

    if let Some(stop) = h.stop.take() {
        stop.send(()).map_err(|()| "the server was gone")?;
    }
    let (_, record) = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        collect_output(&mut stream),
    )
    .await??;

    assert_eq!(record.status(), ExecStatus::CutShort);
    assert!(!record.reason.is_empty());
    assert_eq!(record.ended_at, None);
    h.server.await??;
    Ok(())
}

#[tokio::test]
async fn write_file_creates_and_replaces_whole_content() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");

    h.write(&file, b"first content, in several chunks").await?;
    assert_eq!(std::fs::read(&file)?, b"first content, in several chunks");
    h.write(&file, b"second").await?;
    // The same content again is a success.
    h.write(&file, b"second").await?;

    assert_eq!(std::fs::read(&file)?, b"second");
    assert_eq!(
        std::fs::read_dir(h.dir.path())?.count(),
        1,
        "no temporary file"
    );
    Ok(())
}

#[tokio::test]
async fn a_write_whose_digest_does_not_match_leaves_the_old_content() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    h.write(&file, b"old").await?;

    let error = h
        .write_parts(&file, b"new, but corrupted in transit", &digest_of(b"new"))
        .await
        .err()
        .ok_or("a corrupted write was accepted")?;

    let status = error.downcast_ref::<tonic::Status>().ok_or("status")?;
    assert_eq!(status.code(), Code::DataLoss);
    assert_eq!(std::fs::read(&file)?, b"old");
    assert_eq!(
        std::fs::read_dir(h.dir.path())?.count(),
        1,
        "no temporary file"
    );
    // Control: the same content with its own digest is committed.
    h.write(&file, b"new, but corrupted in transit").await?;
    assert_eq!(std::fs::read(&file)?, b"new, but corrupted in transit");
    Ok(())
}

#[tokio::test]
async fn a_write_stream_that_ends_before_its_end_message_leaves_the_old_content() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    h.write(&file, b"old").await?;

    let parts = vec![
        WriteFileRequest {
            part: Some(write_file_request::Part::Start(WriteFileStart {
                path: file.to_str().ok_or("path")?.into(),
            })),
        },
        WriteFileRequest {
            part: Some(write_file_request::Part::Chunk(b"new".to_vec())),
        },
    ];
    let error = h
        .client
        .write_file(tokio_stream::iter(parts))
        .await
        .err()
        .ok_or("an unfinished write was accepted")?;

    assert_eq!(error.code(), Code::InvalidArgument);
    assert_eq!(std::fs::read(&file)?, b"old");
    assert_eq!(
        std::fs::read_dir(h.dir.path())?.count(),
        1,
        "no temporary file"
    );
    Ok(())
}

/// A numeric field of `/proc/self/status`, such as `Gid:` (real, effective, saved, fs).
fn status_field(name: &str, radix: u32) -> Result<Vec<u32>> {
    let status = std::fs::read_to_string("/proc/self/status")?;
    let line = status
        .lines()
        .find_map(|line| line.strip_prefix(name))
        .ok_or("no such field in /proc/self/status")?;
    Ok(line
        .split_whitespace()
        .filter_map(|n| u32::from_str_radix(n, radix).ok())
        .collect())
}

#[tokio::test]
async fn a_replaced_file_keeps_its_mode_and_group_and_a_new_file_gets_the_default() -> Result {
    use std::os::unix::fs::{MetadataExt, PermissionsExt};
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, b"old")?;
    std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o640))?;
    // A group other than the primary one, where this process may use one, so a lost group shows.
    let (euid, egid) = (status_field("Uid:", 10)?[1], status_field("Gid:", 10)?[1]);
    let other_gid = status_field("Groups:", 10)?
        .into_iter()
        .find(|gid| *gid != egid)
        .or((euid == 0).then_some(4242));
    if let Some(gid) = other_gid {
        std::os::unix::fs::chown(&file, None, Some(gid))?;
    }
    let before = std::fs::metadata(&file)?;
    assert_eq!(before.mode() & 0o7777, 0o640, "setup: mode");
    assert_eq!(before.gid(), other_gid.unwrap_or(egid), "setup: group");

    h.write(&file, BEFORE).await?;
    assert_eq!(
        h.patch(&file, BEFORE, AFTER, DIFF).await?,
        PatchOutcome::Applied
    );

    let after = std::fs::metadata(&file)?;
    assert_eq!(std::fs::read(&file)?, AFTER);
    assert_eq!(after.mode() & 0o7777, 0o640);
    assert_eq!((after.uid(), after.gid()), (before.uid(), before.gid()));
    // Control: a file that did not exist gets the mode a plain create would give.
    let umask = status_field("Umask:", 8)?[0];
    let fresh = h.path("fresh");
    h.write(&fresh, b"new").await?;
    assert_eq!(std::fs::metadata(&fresh)?.mode() & 0o7777, 0o666 & !umask);
    Ok(())
}

const BEFORE: &[u8] = b"a\nb\nc\n";
const AFTER: &[u8] = b"a\nB\nc\n";
const DIFF: &str = "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n";

#[tokio::test]
async fn patch_applies_when_the_file_is_at_the_base_digest() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, BEFORE)?;

    let outcome = h.patch(&file, BEFORE, AFTER, DIFF).await?;

    assert_eq!(outcome, PatchOutcome::Applied);
    assert_eq!(std::fs::read(&file)?, AFTER);
    Ok(())
}

#[tokio::test]
async fn patch_succeeds_without_a_change_when_the_file_is_at_the_expected_digest() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, BEFORE)?;
    h.patch(&file, BEFORE, AFTER, DIFF).await?;

    // The retry after an unknown outcome: the diff would not apply to the new content.
    let outcome = h.patch(&file, BEFORE, AFTER, DIFF).await?;

    assert_eq!(outcome, PatchOutcome::AlreadyApplied);
    assert_eq!(std::fs::read(&file)?, AFTER);
    Ok(())
}

#[tokio::test]
async fn patch_fails_and_changes_nothing_when_the_file_is_at_neither_digest() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, b"something else\n")?;

    let error = h
        .patch(&file, BEFORE, AFTER, DIFF)
        .await
        .err()
        .ok_or("a patch to an unrelated file was accepted")?;

    assert_eq!(error.code(), Code::FailedPrecondition);
    assert!(error.message().contains("neither"), "{}", error.message());
    assert_eq!(std::fs::read(&file)?, b"something else\n");
    // Control: the same request succeeds once the file is at the base digest.
    std::fs::write(&file, BEFORE)?;
    assert_eq!(
        h.patch(&file, BEFORE, AFTER, DIFF).await?,
        PatchOutcome::Applied
    );
    Ok(())
}

#[tokio::test]
async fn patch_whose_result_is_not_the_expected_digest_changes_nothing() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, BEFORE)?;

    let error = h
        .patch(&file, BEFORE, b"not what the patch makes", DIFF)
        .await
        .err()
        .ok_or("a patch with a wrong expectation was accepted")?;

    assert_eq!(error.code(), Code::FailedPrecondition);
    assert_eq!(std::fs::read(&file)?, BEFORE);
    // Control: the same patch with the digest it does produce is applied.
    assert_eq!(
        h.patch(&file, BEFORE, AFTER, DIFF).await?,
        PatchOutcome::Applied
    );
    assert_eq!(std::fs::read(&file)?, AFTER);
    Ok(())
}

#[tokio::test]
async fn two_patches_from_one_base_apply_once_and_the_other_is_refused() -> Result {
    let h = Harness::start().await?;
    let file = h.path("f");
    let other_diff = "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n b\n-c\n+C\n";
    let other_after: &[u8] = b"a\nb\nC\n";
    for round in 0..20 {
        std::fs::write(&file, BEFORE)?;
        let (mut first, mut second) = (h.client.clone(), h.client.clone());
        let request = |expected: &[u8], diff: &str| PatchFileRequest {
            path: file.to_str().unwrap_or_default().into(),
            base_digest: digest_of(BEFORE),
            expected_digest: digest_of(expected),
            patch: diff.as_bytes().to_vec(),
        };
        let (a, b) = tokio::join!(
            first.patch_file(request(AFTER, DIFF)),
            second.patch_file(request(other_after, other_diff))
        );
        let content = std::fs::read(&file)?;
        // Whichever applied is what the file holds, and the other was told it did not.
        match (a, b) {
            (Ok(_), Err(e)) => {
                assert_eq!(e.code(), Code::FailedPrecondition, "round {round}");
                assert_eq!(content, AFTER, "round {round}");
            }
            (Err(e), Ok(_)) => {
                assert_eq!(e.code(), Code::FailedPrecondition, "round {round}");
                assert_eq!(content, other_after, "round {round}");
            }
            (a, b) => {
                return Err(format!("round {round}: {:?} and {:?}", a.is_ok(), b.is_ok()).into());
            }
        }
    }
    Ok(())
}

#[tokio::test]
async fn read_file_returns_the_content_in_chunks() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("big");
    let content: Vec<u8> = (0..200_000u32).map(|i| (i % 251) as u8).collect();
    std::fs::write(&file, &content)?;

    let chunks = h.read(&file).await?;

    assert!(chunks.len() > 1, "{} chunk(s)", chunks.len());
    assert_eq!(chunks.concat(), content);
    Ok(())
}

#[tokio::test]
async fn read_file_refuses_a_missing_file_a_directory_and_a_relative_path() -> Result {
    let mut h = Harness::start().await?;
    let (missing_path, dir_path) = (h.path("missing"), h.dir.path().to_path_buf());
    let missing = h.read(&missing_path).await.err().ok_or("missing")?;
    let directory = h.read(&dir_path).await.err().ok_or("directory")?;
    let relative = h
        .read(Path::new("relative"))
        .await
        .err()
        .ok_or("relative")?;

    assert_eq!(missing.code(), Code::NotFound);
    assert_eq!(directory.code(), Code::InvalidArgument);
    assert_eq!(relative.code(), Code::InvalidArgument);
    // The directory is a file of the wrong kind; a missing file and a relative path are not.
    let wrong: WrongFileKind = details::detail(&directory).ok_or("no WrongFileKind detail")?;
    assert_eq!(wrong.path, dir_path.to_str().ok_or("path")?);
    assert_eq!(wrong.expected(), EntryKind::File);
    assert_eq!(
        details::detail_types(&directory),
        ["mvmd.guest.v1alpha1.WrongFileKind"]
    );
    assert!(details::detail_types(&missing).is_empty());
    assert!(details::detail_types(&relative).is_empty());
    // Control: a regular file at an absolute path is read.
    let regular = h.path("regular");
    std::fs::write(&regular, b"content")?;
    assert_eq!(h.read(&regular).await?.concat(), b"content");
    Ok(())
}

#[tokio::test]
async fn a_listing_of_a_regular_file_names_a_directory_as_the_kind_expected() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("regular");
    std::fs::write(&file, b"content")?;

    let listed = h.list(&file).await.err().ok_or("a file was listed")?;

    assert_eq!(listed.code(), Code::InvalidArgument);
    let wrong: WrongFileKind = details::detail(&listed).ok_or("no WrongFileKind detail")?;
    assert_eq!(wrong.path, file.to_str().ok_or("path")?);
    assert_eq!(wrong.expected(), EntryKind::Directory);
    // Control: the directory itself is listed.
    let dir = h.dir.path().to_path_buf();
    assert_eq!(h.list(&dir).await?.concat().len(), 1);
    Ok(())
}

#[tokio::test]
async fn read_file_refuses_a_fifo_promptly_beside_a_regular_file() -> Result {
    let mut h = Harness::start().await?;
    let fifo = h.path("fifo");
    let status = std::process::Command::new("mkfifo").arg(&fifo).status()?;
    assert!(status.success(), "mkfifo");
    let regular = h.path("regular");
    std::fs::write(&regular, b"content")?;

    let refused = tokio::time::timeout(std::time::Duration::from_secs(2), h.read(&fifo))
        .await?
        .err()
        .ok_or("a FIFO was read")?;

    assert_eq!(refused.code(), Code::InvalidArgument);
    assert_eq!(h.read(&regular).await?.concat(), b"content");
    Ok(())
}

#[tokio::test]
async fn a_symlink_is_followed_by_read_write_and_patch_and_stays_a_link() -> Result {
    let mut h = Harness::start().await?;
    let real = h.path("real");
    let link = h.path("link");
    std::fs::write(&real, BEFORE)?;
    std::os::unix::fs::symlink(&real, &link)?;

    assert_eq!(h.read(&link).await?.concat(), BEFORE);
    assert_eq!(
        h.patch(&link, BEFORE, AFTER, DIFF).await?,
        PatchOutcome::Applied
    );
    assert_eq!(std::fs::read(&real)?, AFTER);
    h.write(&link, b"written through the link").await?;

    assert_eq!(std::fs::read(&real)?, b"written through the link");
    assert!(
        std::fs::symlink_metadata(&link)?.is_symlink(),
        "the link was replaced"
    );
    // Control: the same write to a path that is not a link creates a regular file.
    let plain = h.path("plain");
    h.write(&plain, b"plain").await?;
    assert!(std::fs::symlink_metadata(&plain)?.is_file());
    Ok(())
}

#[tokio::test]
async fn write_and_patch_refuse_a_target_that_is_not_a_regular_file() -> Result {
    let mut h = Harness::start().await?;
    let directory = h.path("directory");
    std::fs::create_dir(&directory)?;
    std::fs::write(directory.join("inside"), b"kept")?;
    let fifo = h.path("fifo");
    let status = std::process::Command::new("mkfifo").arg(&fifo).status()?;
    assert!(status.success(), "mkfifo");
    let to_directory = h.path("to-directory");
    std::os::unix::fs::symlink(&directory, &to_directory)?;

    let limit = std::time::Duration::from_secs(2);
    for target in [&directory, &fifo, &to_directory] {
        let written = tokio::time::timeout(limit, h.write(target, b"new"))
            .await?
            .err()
            .ok_or("a write was accepted")?;
        assert_eq!(
            code(&*written),
            Some(Code::InvalidArgument),
            "{}",
            target.display()
        );
        let patched = tokio::time::timeout(limit, h.patch(target, BEFORE, AFTER, DIFF))
            .await?
            .err()
            .ok_or("a patch was accepted")?;
        assert_eq!(
            patched.code(),
            Code::InvalidArgument,
            "{}",
            target.display()
        );
        let wrong: WrongFileKind = details::detail(&patched).ok_or("no WrongFileKind detail")?;
        assert_eq!(wrong.expected(), EntryKind::File, "{}", target.display());
    }

    assert_eq!(std::fs::read(directory.join("inside"))?, b"kept");
    assert_eq!(
        std::fs::read_dir(&directory)?.count(),
        1,
        "no temporary file in the directory"
    );
    assert_eq!(
        std::fs::read_dir(h.dir.path())?.count(),
        3,
        "no temporary file beside the targets"
    );
    // Control: a regular file in the same directory is written.
    h.write(&h.path("regular"), b"new").await?;
    assert_eq!(std::fs::read(h.path("regular"))?, b"new");
    Ok(())
}

#[tokio::test]
async fn list_files_names_each_entry_with_its_kind_and_size() -> Result {
    let mut h = Harness::start().await?;
    std::fs::write(h.path("file"), b"12345")?;
    std::fs::create_dir(h.path("dir"))?;
    std::os::unix::fs::symlink("file", h.path("link"))?;

    let dir_path = h.dir.path().to_path_buf();
    let listed = h.list(&dir_path).await?.concat();

    let summary: Vec<_> = listed
        .iter()
        .map(|e| (e.path.clone(), e.kind(), e.size))
        .collect();
    let dir = h.dir.path().display();
    assert_eq!(
        summary,
        vec![
            (format!("{dir}/dir"), EntryKind::Directory, summary[0].2),
            (format!("{dir}/file"), EntryKind::File, 5),
            (format!("{dir}/link"), EntryKind::Symlink, summary[2].2),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn a_listing_and_a_search_larger_than_one_message_arrive_sorted_across_messages() -> Result {
    let mut h = Harness::start().await?;
    for n in 0..1100 {
        std::fs::write(h.path(&format!("f{n:04}")), b"")?;
    }
    let dir = h.dir.path().to_path_buf();

    let runs = h.list(&dir).await?;
    let found = h.search(&dir, "f*").await?;

    assert!(runs.len() > 1, "{} message(s)", runs.len());
    let listed: Vec<String> = runs.concat().into_iter().map(|e| e.path).collect();
    assert_eq!(listed.len(), 1100);
    assert!(listed.is_sorted());
    assert_eq!(
        found.into_iter().map(|e| e.path).collect::<Vec<_>>(),
        listed
    );
    // Control: a directory with few entries is one message.
    let small = h.path("small");
    std::fs::create_dir(&small)?;
    std::fs::write(small.join("only"), b"")?;
    assert_eq!(h.list(&small).await?.len(), 1);
    Ok(())
}

#[tokio::test]
async fn a_request_over_the_size_limit_is_refused_and_one_under_it_is_served() -> Result {
    let mut h = Harness::start().await?;
    let file = h.path("f");
    std::fs::write(&file, BEFORE)?;
    let patch_of = |size: usize| PatchFileRequest {
        path: file.to_str().unwrap_or_default().into(),
        base_digest: digest_of(BEFORE),
        expected_digest: digest_of(AFTER),
        patch: vec![b'x'; size],
    };

    let over = h
        .client
        .patch_file(patch_of(17 * 1024 * 1024))
        .await
        .err()
        .ok_or("an oversized request was accepted")?;
    let under = h
        .client
        // Above tonic's own 4 MiB default, so only the explicit limit lets it through.
        .patch_file(patch_of(5 * 1024 * 1024))
        .await
        .err()
        .ok_or("a junk patch was applied")?;

    assert_eq!(over.code(), Code::OutOfRange, "{}", over.message());
    // The size limit is tonic's own OUT_OF_RANGE, which is not an output read: no detail.
    assert!(details::detail_types(&over).is_empty());
    // The smaller one reached the service, which refused the patch itself.
    assert!(
        matches!(
            under.code(),
            Code::InvalidArgument | Code::FailedPrecondition
        ),
        "{under:?}"
    );
    assert_eq!(std::fs::read(&file)?, BEFORE);
    Ok(())
}

#[tokio::test]
async fn search_files_matches_a_glob_against_paths_below_the_directory() -> Result {
    let mut h = Harness::start().await?;
    std::fs::create_dir_all(h.path("a/b"))?;
    std::fs::write(h.path("a/x.txt"), b"")?;
    std::fs::write(h.path("a/b/y.txt"), b"")?;
    std::fs::write(h.path("z.rs"), b"")?;
    std::fs::write(h.path("top.txt"), b"")?;
    let root: String = h.dir.path().to_str().ok_or("dir")?.into();
    let dir = h.dir.path().to_path_buf();

    let mut search = async |pattern: &str| -> Result<Vec<String>> {
        let matches = h.search(&dir, pattern).await?;
        Ok(matches
            .into_iter()
            .map(|m| m.path.replacen(&root, "", 1))
            .collect())
    };

    assert_eq!(
        search("**/*.txt").await?,
        ["/a/b/y.txt", "/a/x.txt", "/top.txt"]
    );
    // "*" does not cross a directory boundary, so only the top level matches.
    assert_eq!(search("*.txt").await?, ["/top.txt"]);
    assert_eq!(search("*.md").await?, Vec::<String>::new());
    let invalid = search("[")
        .await
        .err()
        .ok_or("an invalid glob was accepted")?;
    let status = invalid.downcast_ref::<tonic::Status>().ok_or("status")?;
    assert_eq!(status.code(), Code::InvalidArgument);
    Ok(())
}

#[tokio::test]
async fn health_answers_and_info_names_the_version_and_the_contracts_served() -> Result {
    let mut h = Harness::start().await?;

    h.client.health(HealthRequest {}).await?;
    let info = h.client.info(InfoRequest {}).await?.into_inner();

    assert_eq!(info.agent_version, env!("CARGO_PKG_VERSION"));
    assert_eq!(info.api_versions, ["v1alpha1"]);
    Ok(())
}
