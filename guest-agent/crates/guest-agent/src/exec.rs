//! Commands the agent runs for the daemon: start once per exec ID, keep the output for resumable
//! reads, report how the command ended.

use std::collections::hash_map::Entry;
use std::collections::{HashMap, VecDeque};
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{ExitStatus, Stdio};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use nix::sys::signal::{Signal, kill};
use nix::sys::wait::{WaitPidFlag, waitpid};
use nix::unistd::Pid;
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Command;
use tokio::sync::watch;

use crate::error::Error;
use crate::reap::zombie_children;
use crate::v1alpha1::{ExecRecord, ExecRequest, ExecStatus, OutputChunk, OutputStream};

/// How long the output pipes may stay open after the command exited. A background process the
/// command left behind can hold them for good, and the record must not wait on it.
const DRAIN_GRACE: Duration = Duration::from_secs(1);

const READ_SIZE: usize = 64 * 1024;

/// The reason a record gives when the agent ends first.
const CUT_SHORT_REASON: &str = "the Guest Agent ended while the command was running";

/// Every command the agent has been asked to run, by exec ID.
pub(crate) struct Execs {
    table: Mutex<HashMap<String, Arc<Exec>>>,
    /// How many of the newest bytes of each command's output are kept.
    retention: u64,
}

/// One command: its record and the output it has produced so far.
struct Exec {
    state: Mutex<State>,
    /// Signalled after every change to `state`.
    changed: watch::Sender<()>,
}

struct State {
    record: ExecRecord,
    /// The running command's process ID, until the command has been reaped. Cleared the moment
    /// `wait` returns, so no signal reaches a process ID the kernel may have reused.
    pid: Option<u32>,
    /// How the command ended, between its being reaped and the record being final.
    ended: Option<Ended>,
    /// The newest output, from `record.earliest_offset` to `len`.
    chunks: VecDeque<OutputChunk>,
    /// The length of the output sequence: stdout and stderr in the order they were read.
    len: u64,
}

enum Ended {
    Exit(ExitStatus),
    Lost(String),
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
    pub(crate) fn new(retention: u64) -> Self {
        Self {
            table: Mutex::default(),
            retention,
        }
    }

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
                let record = {
                    let mut state = lock(&exec.state);
                    state.record.status = ExecStatus::Started.into();
                    state.pid = child.id();
                    state.record.clone()
                };
                tokio::spawn(supervise(exec, self.retention, child, streams));
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
        {
            let state = lock(&exec.state);
            if after > state.len {
                return Err(Error::Invalid(format!(
                    "after_offset {after} is beyond the {} bytes of output",
                    state.len
                )));
            }
            check_held(&state, after)?;
        }
        let changed = exec.changed.subscribe();
        Ok(Follower {
            exec,
            changed,
            after,
            done: false,
        })
    }

    /// The final record of an exec, once the command has ended or failed to start.
    pub(crate) async fn wait(&self, exec_id: &str) -> Result<ExecRecord, Error> {
        let exec = self.find(exec_id)?;
        let mut changed = exec.changed.subscribe();
        loop {
            {
                let state = lock(&exec.state);
                if state.record.status() != ExecStatus::Started {
                    return Ok(state.record.clone());
                }
            }
            changed.changed().await.map_err(|_| Error::Io {
                context: format!("wait for exec {exec_id}"),
                source: std::io::Error::other("the exec was dropped"),
            })?;
        }
    }

    /// Wait for every exited child of `parent` that no exec of this table owns: a process the
    /// agent never started, which was handed to it when its own parent exited.
    pub(crate) fn reap_orphans(&self, parent: Pid) {
        // The table stays locked while the children are waited for. `start` holds it from before
        // a spawn until the new process ID is recorded, so no child of an exec looks like an orphan.
        let table = lock(&self.table);
        let owned: Vec<u32> = table
            .values()
            .filter_map(|exec| lock(&exec.state).pid)
            .collect();
        for pid in zombie_children(Path::new("/proc"), parent) {
            if !u32::try_from(pid.as_raw()).is_ok_and(|raw| owned.contains(&raw)) {
                // The orphan's status goes to no one.
                let _ = waitpid(pid, Some(WaitPidFlag::WNOHANG));
            }
        }
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
                ended: None,
                chunks: VecDeque::new(),
                len: 0,
            }),
            changed: watch::Sender::new(()),
        }
    }

    /// Add output, dropping the oldest bytes beyond `retention`.
    fn append(&self, retention: u64, stream: OutputStream, data: &[u8]) {
        let mut state = lock(&self.state);
        let offset = state.len;
        state.len += data.len() as u64;
        state.chunks.push_back(OutputChunk {
            stream: stream.into(),
            offset,
            data: data.to_vec(),
        });
        let mut excess = (state.len - state.record.earliest_offset).saturating_sub(retention);
        while excess > 0 {
            let Some(front) = state.chunks.front_mut() else {
                break;
            };
            let held = front.data.len() as u64;
            if held <= excess {
                state.chunks.pop_front();
                state.record.earliest_offset += held;
                excess -= held;
            } else {
                // `excess` is below the chunk's length, which fits memory.
                front
                    .data
                    .drain(..usize::try_from(excess).unwrap_or(usize::MAX));
                front.offset += excess;
                state.record.earliest_offset += excess;
                excess = 0;
            }
        }
        drop(state);
        self.changed.send_replace(());
    }

    /// The command has been reaped: note how, and take its process ID out of reach of a signal.
    fn reaped(&self, ended: Ended) {
        let mut state = lock(&self.state);
        if state.pid.take().is_some() {
            state.ended = Some(ended);
        }
    }

    /// The output is drained: the record becomes final, unless the agent ending already did that.
    fn finish(&self) {
        let mut state = lock(&self.state);
        if state.record.status() == ExecStatus::Started {
            match state.ended.take() {
                Some(Ended::Exit(status)) => {
                    state.record.status = ExecStatus::Exited.into();
                    state.record.exit_code = status.code().unwrap_or_default();
                    state.record.term_signal = status.signal().unwrap_or_default();
                }
                Some(Ended::Lost(reason)) => {
                    state.record.status = ExecStatus::CutShort.into();
                    state.record.reason = reason;
                }
                None => {}
            }
        }
        drop(state);
        self.changed.send_replace(());
    }
}

/// Fail a read that starts before the earliest offset still held, naming that offset.
fn check_held(state: &State, after: u64) -> Result<(), Error> {
    let earliest = state.record.earliest_offset;
    if after < earliest {
        return Err(Error::OutOfRange(format!(
            "output before offset {earliest} is no longer held; the earliest held offset is {earliest}"
        )));
    }
    Ok(())
}

impl Follower {
    /// The output not yet delivered, waiting while there is none and the command still runs.
    ///
    /// Fails when the retention has dropped output the follower has not yet been given.
    pub(crate) async fn next(&mut self) -> Result<Option<Output>, Error> {
        while !self.done {
            // Mark the signals so far as seen before reading, so none that follows is missed.
            self.changed.borrow_and_update();
            let output = {
                let state = lock(&self.exec.state);
                check_held(&state, self.after)?;
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
                return Ok(Some(output));
            }
            if self.changed.changed().await.is_err() {
                return Ok(None);
            }
        }
        Ok(None)
    }
}

/// The part of the output sequence after `after`, cutting the chunk that straddles it.
fn chunks_after(chunks: &VecDeque<OutputChunk>, after: u64) -> Vec<OutputChunk> {
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
    retention: u64,
    mut child: tokio::process::Child,
    (stdout, stderr): (
        Option<tokio::process::ChildStdout>,
        Option<tokio::process::ChildStderr>,
    ),
) {
    let mut pumps = tokio::task::JoinSet::new();
    if let Some(stdout) = stdout {
        pumps.spawn(pump(exec.clone(), retention, OutputStream::Stdout, stdout));
    }
    if let Some(stderr) = stderr {
        pumps.spawn(pump(exec.clone(), retention, OutputStream::Stderr, stderr));
    }
    // The process ID is out of reach of a signal from here on, whatever the pipes still hold.
    exec.reaped(match child.wait().await {
        Ok(status) => Ended::Exit(status),
        Err(error) => Ended::Lost(format!("the agent lost the command: {error}")),
    });
    let drained = tokio::time::timeout(DRAIN_GRACE, async {
        while pumps.join_next().await.is_some() {}
    })
    .await;
    if drained.is_err() {
        pumps.abort_all();
    }
    exec.finish();
}

async fn pump(
    exec: Arc<Exec>,
    retention: u64,
    stream: OutputStream,
    mut pipe: impl AsyncRead + Unpin,
) {
    let mut buf = vec![0; READ_SIZE];
    // A read error ends the stream like end of file; the command's own end is still recorded.
    while let Ok(n) = pipe.read(&mut buf).await {
        if n == 0 {
            break;
        }
        exec.append(retention, stream, &buf[..n]);
    }
}
