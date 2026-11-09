//! One `mkfs.erofs` run over one layer.

use std::ffi::OsString;
use std::io::{self, Read};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::thread::JoinHandle;

use crate::uuid::Uuid;

/// How much of what `mkfs.erofs` wrote to its standard error the failure keeps: the end of it.
const STDERR_KEPT: usize = 4096;

/// Why a run failed.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The program could not be started.
    #[error("could not start {path}: {source}")]
    Start {
        /// The program's path.
        path: PathBuf,
        /// Why it did not start.
        source: io::Error,
    },
    /// The program ended unsuccessfully; `stderr` is the end of what it wrote.
    #[error("mkfs.erofs {status}: {stderr}")]
    Failed {
        /// How it ended.
        status: String,
        /// The end of its standard error.
        stderr: String,
    },
    /// The layer could not be read or converted into the stream the program takes.
    #[error("the layer could not be fed to mkfs.erofs: {0}")]
    Feed(io::Error),
}

/// The pinned `mkfs.erofs`.
#[derive(Debug, Clone)]
pub struct Mkfs {
    program: PathBuf,
    /// Arguments that come before the ones a run adds.
    leading: Vec<OsString>,
}

/// What one run builds.
pub struct Run<'a> {
    /// The filesystem UUID.
    pub uuid: Uuid,
    /// The file or device the image is written to.
    pub output: &'a Path,
    /// Whether the run adds to the image already on the output, as a later layer does.
    pub incremental: bool,
    /// Whether the stream on standard input is gzip, which the program filters itself.
    pub ungzip: bool,
}

impl Mkfs {
    /// The program at `program`.
    pub fn new(program: impl Into<PathBuf>) -> Self {
        Self {
            program: program.into(),
            leading: Vec::new(),
        }
    }

    /// The program at `program`, started with `leading` before the arguments of a run. A test
    /// stands a script in for the tool by naming an interpreter and the script, so the script is
    /// read and never executed itself.
    pub fn with_leading_args<A: Into<OsString>>(
        program: impl Into<PathBuf>,
        leading: impl IntoIterator<Item = A>,
    ) -> Self {
        Self {
            program: program.into(),
            leading: leading.into_iter().map(Into::into).collect(),
        }
    }

    /// Run the program, hand `feed` its standard input, and wait for it. The build time is fixed
    /// and file times are kept, so the same layers give the same image.
    pub fn run(
        &self,
        run: &Run<'_>,
        feed: impl FnOnce(&mut ChildStdin) -> io::Result<()>,
    ) -> Result<(), Error> {
        let mut command = Command::new(&self.program);
        command
            .args(&self.leading)
            .args([
                "--quiet",
                "-T0",
                "--mkfs-time",
                "--tar=f",
                "--aufs",
                "--ovlfs-strip=1",
            ])
            .arg(format!("-U{}", run.uuid));
        if run.incremental {
            command.arg("--incremental=data");
        }
        if run.ungzip {
            command.arg("--ungzip");
        }
        let mut child = command
            .arg(run.output)
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|source| Error::Start {
                path: self.program.clone(),
                source,
            })?;
        let stderr = child.stderr.take().map(tail);
        let fed = match child.stdin.take() {
            Some(mut stdin) => feed(&mut stdin),
            None => Err(io::Error::other("the program's standard input is not open")),
        };
        // The input handle ended with the match above, so the program sees the end of its input.
        let outcome = finish(&mut child, fed);
        let stderr = stderr
            .and_then(|reader| reader.join().ok())
            .unwrap_or_default();
        match outcome {
            Ok(()) => Ok(()),
            Err(Ended::Status(status)) => Err(Error::Failed { status, stderr }),
            Err(Ended::Feed(error)) => Err(Error::Feed(error)),
        }
    }
}

/// How a run ended when it did not succeed.
enum Ended {
    /// The program ended unsuccessfully.
    Status(String),
    /// Feeding it failed while it was still running.
    Feed(io::Error),
}

/// Wait for the program, having fed it. A feed that failed because the program had ended is the
/// program's failure; one that failed on its own ends the program.
fn finish(child: &mut Child, fed: io::Result<()>) -> Result<(), Ended> {
    match fed {
        Ok(()) => match child.wait() {
            Ok(status) if status.success() => Ok(()),
            Ok(status) => Err(Ended::Status(status.to_string())),
            Err(error) => Err(Ended::Feed(error)),
        },
        Err(error) if error.kind() == io::ErrorKind::BrokenPipe => match child.wait() {
            Ok(status) => Err(Ended::Status(status.to_string())),
            Err(error) => Err(Ended::Feed(error)),
        },
        Err(error) => {
            // The program is not told why; it is ended, and the layer's failure is reported.
            let _ = child.kill();
            let _ = child.wait();
            Err(Ended::Feed(error))
        }
    }
}

/// Collect the end of `stream` on its own thread, so a program that writes much to it never
/// blocks on a full pipe while the converter writes to its input.
fn tail(mut stream: impl Read + Send + 'static) -> JoinHandle<String> {
    std::thread::spawn(move || {
        let mut kept = Vec::new();
        let mut buffer = [0u8; 1024];
        while let Ok(read) = stream.read(&mut buffer) {
            if read == 0 {
                break;
            }
            kept.extend_from_slice(&buffer[..read]);
            if kept.len() > STDERR_KEPT {
                kept.drain(..kept.len() - STDERR_KEPT);
            }
        }
        String::from_utf8_lossy(&kept).trim().to_owned()
    })
}
