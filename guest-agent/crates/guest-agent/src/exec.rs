//! Commands the agent runs for the daemon: start once per exec ID, keep the output for resumable
//! reads, report how the command ended.

use std::collections::HashMap;
use std::collections::hash_map::Entry;
use std::os::unix::process::ExitStatusExt;
use std::process::{ExitStatus, Stdio};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use nix::sys::signal::{Signal, kill};
use nix::unistd::Pid;
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Command;
use tokio::sync::watch;

use crate::error::Error;
use crate::v1alpha1::{ExecRecord, ExecRequest, ExecStatus, OutputChunk, OutputStream};

/// How long the output pipes may stay open after the command exited. A background process the
/// command left behind can hold them for good, and the record must not wait on it.
const DRAIN_GRACE: Duration = Duration::from_secs(1);

const READ_SIZE: usize = 64 * 1024;

/// The reason a record gives when the agent ends first.
const CUT_SHORT_REASON: &str = "the Guest Agent ended while the command was running";

/// Every command the agent has been asked to run, by exec ID.
#[derive(Default)]
pub(crate) struct Execs {
    table: Mutex<HashMap<String, Arc<Exec>>>,
}

/// One command: its record and the output it has produced so far.
struct Exec {
    state: Mutex<State>,
    /// Signalled after every change to `state`.
    changed: watch::Sender<()>,
}

struct State {
    record: ExecRecord,
    /// The running command's process ID, until the command has ended.
    pid: Option<u32>,
    chunks: Vec<OutputChunk>,
    /// The length of the output sequence: stdout and stderr in the order they were read.
    len: u64,
}

/// What a read of the output found.
pub(crate) struct Output {
    pub(crate) chunks: Vec<OutputChunk>,
    /// Set once the command has ended and `chunks` ends the output.
    pub(crate) record: Option<ExecRecord>,
}

/// Follows one command's output.
pub(crate) struct Follower {
    exec: Arc<Exec>,
    changed: watch::Receiver<()>,
    after: u64,
    /// Set once the final record has been delivered.
    done: bool,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    // A poisoned lock means a task panicked inside a state update; the program ends then.
    mutex
        .lock()
        .unwrap_or_else(|_| unreachable!("a task panicked while holding an exec lock"))
}

impl Execs {
    /// Start the command under its exec ID, or return the record of the earlier start.
    pub(crate) fn start(&self, request: ExecRequest) -> Result<ExecRecord, Error> {
        if request.exec_id.is_empty() {
            return Err(Error::Invalid("exec_id is empty".into()));
        }
        let Some((program, args)) = request.argv.split_first() else {
            return Err(Error::Invalid("argv is empty".into()));
        };

        // The table stays locked across the spawn, so two starts of one ID cannot both run.
        let mut table = lock(&self.table);
        let exec = match table.entry(request.exec_id.clone()) {
            Entry::Occupied(known) => return Ok(lock(&known.get().state).record.clone()),
            Entry::Vacant(slot) => slot.insert(Arc::new(Exec::new(&request.exec_id))).clone(),
        };

        let mut command = Command::new(program);
        command
            .args(args)
            .env_clear()
            .envs(&request.env)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        if !request.working_dir.is_empty() {
            command.current_dir(&request.working_dir);
        }
        match command.spawn() {
            Ok(mut child) => {
                let streams = (child.stdout.take(), child.stderr.take());
                {
                    let mut state = lock(&exec.state);
                    state.record.status = ExecStatus::Started.into();
                    state.pid = child.id();
                }
                let record = lock(&exec.state).record.clone();
                tokio::spawn(supervise(exec, child, streams));
                Ok(record)
            }
            Err(error) => {
                let mut state = lock(&exec.state);
                state.record.status = ExecStatus::NotStarted.into();
                state.record.reason = format!("cannot start {program}: {error}");
                Ok(state.record.clone())
            }
        }
    }

    /// Follow an exec's output from after the offset.
    pub(crate) fn follow(&self, exec_id: &str, after: u64) -> Result<Follower, Error> {
        let exec = self.find(exec_id)?;
        let len = lock(&exec.state).len;
        if after > len {
            return Err(Error::Invalid(format!(
                "after_offset {after} is beyond the {len} bytes of output"
            )));
        }
        let changed = exec.changed.subscribe();
        Ok(Follower {
            exec,
            changed,
            after,
            done: false,
        })
    }

    /// Deliver a signal to a running command.
    pub(crate) fn signal(&self, exec_id: &str, signal: i32) -> Result<(), Error> {
        let signal = Signal::try_from(signal)
            .map_err(|_| Error::Invalid(format!("{signal} is not a signal number")))?;
        let exec = self.find(exec_id)?;
        // The lock is held while signalling: the command's end is recorded under it, so a
        // process ID is only signalled while the record still says it is running.
        let state = lock(&exec.state);
        let Some(pid) = state.pid else {
            return Err(Error::PreconditionFailed(format!(
                "exec {exec_id} is not running"
            )));
        };
        let pid = Pid::from_raw(i32::try_from(pid).map_err(|_| {
            Error::Invalid(format!("process ID {pid} does not fit a signal target"))
        })?);
        kill(pid, signal).map_err(|errno| Error::Io {
            context: format!("signal exec {exec_id}"),
            source: errno.into(),
        })
    }

    /// End every running command and record that the agent ended before it did.
    pub(crate) fn cut_short(&self) {
        let execs: Vec<Arc<Exec>> = lock(&self.table).values().cloned().collect();
        for exec in execs {
            let mut state = lock(&exec.state);
            if let Some(pid) = state.pid.take() {
                if let Ok(pid) = i32::try_from(pid) {
                    // The command may have ended already; the record decides what is reported.
                    let _ = kill(Pid::from_raw(pid), Signal::SIGKILL);
                }
                state.record.status = ExecStatus::CutShort.into();
                state.record.reason = CUT_SHORT_REASON.into();
                drop(state);
                exec.changed.send_replace(());
            }
        }
    }

    fn find(&self, exec_id: &str) -> Result<Arc<Exec>, Error> {
        lock(&self.table)
            .get(exec_id)
            .cloned()
            .ok_or_else(|| Error::NotFound(format!("exec {exec_id}")))
    }
}

impl Exec {
    fn new(exec_id: &str) -> Self {
        Self {
            state: Mutex::new(State {
                record: ExecRecord {
                    exec_id: exec_id.to_owned(),
                    ..ExecRecord::default()
                },
                pid: None,
                chunks: Vec::new(),
                len: 0,
            }),
            changed: watch::Sender::new(()),
        }
    }

    fn append(&self, stream: OutputStream, data: &[u8]) {
        let mut state = lock(&self.state);
        let offset = state.len;
        state.len += data.len() as u64;
        state.chunks.push(OutputChunk {
            stream: stream.into(),
            offset,
            data: data.to_vec(),
        });
        drop(state);
        self.changed.send_replace(());
    }

    fn finish(&self, status: &ExitStatus) {
        let mut state = lock(&self.state);
        // The record is final once it leaves Started, whether by exit or by the agent ending.
        if state.pid.take().is_some() {
            state.record.status = ExecStatus::Exited.into();
            state.record.exit_code = status.code().unwrap_or_default();
            state.record.term_signal = status.signal().unwrap_or_default();
        }
        drop(state);
        self.changed.send_replace(());
    }
}

impl Follower {
    /// The output not yet delivered, waiting while there is none and the command still runs.
    pub(crate) async fn next(&mut self) -> Option<Output> {
        while !self.done {
            // Mark the signals so far as seen before reading, so none that follows is missed.
            self.changed.borrow_and_update();
            let output = {
                let state = lock(&self.exec.state);
                let finished = state.record.status() != ExecStatus::Started;
                let chunks = chunks_after(&state.chunks, self.after);
                self.after = state.len;
                Output {
                    chunks,
                    record: finished.then(|| state.record.clone()),
                }
            };
            if !output.chunks.is_empty() || output.record.is_some() {
                self.done = output.record.is_some();
                return Some(output);
            }
            self.changed.changed().await.ok()?;
        }
        None
    }
}

/// The part of the output sequence after `after`, cutting the chunk that straddles it.
fn chunks_after(chunks: &[OutputChunk], after: u64) -> Vec<OutputChunk> {
    chunks
        .iter()
        .filter(|chunk| chunk.offset + chunk.data.len() as u64 > after)
        .map(|chunk| {
            let skip = usize::try_from(after.saturating_sub(chunk.offset)).unwrap_or(usize::MAX);
            OutputChunk {
                stream: chunk.stream,
                offset: chunk.offset + skip as u64,
                data: chunk.data[skip..].to_vec(),
            }
        })
        .collect()
}

/// Own the child until it ends: collect its output, then record how it ended.
async fn supervise(
    exec: Arc<Exec>,
    mut child: tokio::process::Child,
    (stdout, stderr): (
        Option<tokio::process::ChildStdout>,
        Option<tokio::process::ChildStderr>,
    ),
) {
    let mut pumps = tokio::task::JoinSet::new();
    if let Some(stdout) = stdout {
        pumps.spawn(pump(exec.clone(), OutputStream::Stdout, stdout));
    }
    if let Some(stderr) = stderr {
        pumps.spawn(pump(exec.clone(), OutputStream::Stderr, stderr));
    }
    let status = child.wait().await;
    let drained = tokio::time::timeout(DRAIN_GRACE, async {
        while pumps.join_next().await.is_some() {}
    })
    .await;
    if drained.is_err() {
        pumps.abort_all();
    }
    match status {
        Ok(status) => exec.finish(&status),
        Err(error) => {
            let mut state = lock(&exec.state);
            if state.pid.take().is_some() {
                state.record.status = ExecStatus::CutShort.into();
                state.record.reason = format!("the agent lost the command: {error}");
            }
            drop(state);
            exec.changed.send_replace(());
        }
    }
}

async fn pump(exec: Arc<Exec>, stream: OutputStream, mut pipe: impl AsyncRead + Unpin) {
    let mut buf = vec![0; READ_SIZE];
    // A read error ends the stream like end of file; the command's own end is still recorded.
    while let Ok(n) = pipe.read(&mut buf).await {
        if n == 0 {
            break;
        }
        exec.append(stream, &buf[..n]);
    }
}
