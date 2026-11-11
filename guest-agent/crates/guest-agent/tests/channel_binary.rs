//! The host-side channel-only binary, run as a process on a temporary socket and driven through the
//! generated client, with the Firecracker host-side handshake in front of the gRPC stream.

use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

use guest_agent::v1alpha1::guest_service_client::GuestServiceClient;
use guest_agent::v1alpha1::{
    BootRequest, ExecRequest, ExecStatus, FindDiskRequest, HealthRequest, NetworkConfig,
    ReadExecOutputRequest, ReleaseDiskRequest, RunConfig, TerminateRequest,
    read_exec_output_response,
};
use hyper_util::rt::TokioIo;
use tempfile::TempDir;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::process::{Child, Command};
use tonic::Code;
use tonic::transport::{Channel, Endpoint};
use tower::service_fn;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

const PORT: u32 = 1024;

/// The binary, started on a socket in a temporary directory.
struct Agent {
    socket: PathBuf,
    _dir: TempDir,
    child: Child,
}

impl Agent {
    async fn start(flags: &[&str]) -> Result<Self> {
        let dir = tempfile::tempdir()?;
        let socket = dir.path().join("agent.sock");
        let child = Command::new(env!("CARGO_BIN_EXE_guest-agent-channel"))
            .args(flags)
            .arg(&socket)
            .kill_on_drop(true)
            .spawn()?;
        // The socket exists once the binary listens.
        for _ in 0..200 {
            if socket.exists() {
                return Ok(Self {
                    socket,
                    _dir: dir,
                    child,
                });
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        Err("the binary never bound its socket".into())
    }

    /// A client that sends the handshake first, as the Driver's dialer does.
    async fn client(&self) -> Result<GuestServiceClient<Channel>> {
        let socket = self.socket.clone();
        let channel = Endpoint::try_from("http://agent.invalid")?
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
        Ok(GuestServiceClient::new(channel))
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

#[tokio::test]
async fn the_handshake_is_answered_and_health_and_exec_are_served_on_the_host() -> Result {
    let agent = Agent::start(&["--handshake-port", &PORT.to_string()]).await?;
    let mut client = agent.client().await?;

    client.health(HealthRequest {}).await?;
    let started = client
        .exec(ExecRequest {
            exec_id: "e1".into(),
            argv: vec!["/bin/sh".into(), "-c".into(), "echo host".into()],
            ..ExecRequest::default()
        })
        .await?
        .into_inner()
        .record
        .ok_or("no record")?;
    assert_eq!(started.status(), ExecStatus::Started);
    let mut stream = client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "e1".into(),
            after_offset: 0,
        })
        .await?
        .into_inner();
    let mut output = Vec::new();
    let mut ended = None;
    while let Some(message) = stream.message().await? {
        match message.part {
            Some(read_exec_output_response::Part::Chunk(chunk)) => output.extend(chunk.data),
            Some(read_exec_output_response::Part::Record(record)) => ended = Some(record),
            None => {}
        }
    }
    assert_eq!(output, b"host\n");
    assert_eq!(ended.ok_or("no final record")?.status(), ExecStatus::Exited);
    // Boot is refused: this agent does not boot a guest.
    let valid = BootRequest {
        image_uuid: "00000000-0000-0000-0000-000000000001".into(),
        root_disk_uuid: "00000000-0000-0000-0000-000000000002".into(),
        run: Some(RunConfig {
            argv: vec!["/bin/true".into()],
            ..RunConfig::default()
        }),
        network: Some(NetworkConfig::default()),
        host_unix_nanos: 0,
    };
    let boot = client.boot(valid).await.unwrap_err();
    assert_eq!(boot.code(), Code::FailedPrecondition);
    Ok(())
}

#[tokio::test]
async fn terminate_is_refused_because_this_agent_is_not_the_guests_init() -> Result {
    let agent = Agent::start(&["--handshake-port", &PORT.to_string()]).await?;
    let mut client = agent.client().await?;

    let refused = client.terminate(TerminateRequest {}).await.unwrap_err();

    assert_eq!(refused.code(), Code::FailedPrecondition);
    // Control: the binary stays up and serves the next call, so the refusal is the call's and
    // not an ended server's.
    client.health(HealthRequest {}).await?;
    Ok(())
}

#[tokio::test]
async fn the_disk_calls_are_refused_because_this_agents_sys_is_the_hosts() -> Result {
    let agent = Agent::start(&["--handshake-port", &PORT.to_string()]).await?;
    let mut client = agent.client().await?;

    let find = client
        .find_disk(FindDiskRequest {
            disk_id: "data".into(),
        })
        .await
        .unwrap_err();
    let release = client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await
        .unwrap_err();

    assert_eq!(find.code(), Code::FailedPrecondition);
    assert_eq!(release.code(), Code::FailedPrecondition);
    // Control: the binary stays up and serves the next call.
    client.health(HealthRequest {}).await?;
    Ok(())
}

#[tokio::test]
async fn a_client_that_sends_the_handshake_is_refused_when_the_flag_is_absent() -> Result {
    let agent = Agent::start(&[]).await?;

    let answer = answer_to(&agent.socket, &format!("CONNECT {PORT}\n")).await?;

    assert!(
        !answer.starts_with(b"OK"),
        "answered the handshake: {answer:?}"
    );
    // Control: the same binary serves a client that sends no handshake.
    let channel = Endpoint::try_from("http://agent.invalid")?
        .connect_with_connector(service_fn({
            let socket = agent.socket.clone();
            move |_| {
                let socket = socket.clone();
                async move { Ok::<_, io::Error>(TokioIo::new(UnixStream::connect(socket).await?)) }
            }
        }))
        .await?;
    GuestServiceClient::new(channel)
        .health(HealthRequest {})
        .await?;
    Ok(())
}

#[tokio::test]
async fn a_handshake_for_another_port_or_none_is_closed_unanswered() -> Result {
    let agent = Agent::start(&["--handshake-port", &PORT.to_string()]).await?;

    let other_port = answer_to(&agent.socket, "CONNECT 1025\n").await?;
    let none = answer_to(&agent.socket, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n").await?;

    assert!(other_port.is_empty(), "{other_port:?}");
    assert!(none.is_empty(), "{none:?}");
    // Control: the right port is answered by the same process.
    let answer = {
        let mut stream = UnixStream::connect(&agent.socket).await?;
        stream
            .write_all(format!("CONNECT {PORT}\n").as_bytes())
            .await?;
        let mut reply = vec![0; format!("OK {PORT}\n").len()];
        stream.read_exact(&mut reply).await?;
        reply
    };
    assert_eq!(answer, format!("OK {PORT}\n").as_bytes());
    Ok(())
}

#[tokio::test]
async fn a_failed_accept_ends_the_binary_with_a_failure_and_says_why() -> Result {
    let dir = tempfile::tempdir()?;
    let socket = dir.path().join("agent.sock");
    // A low descriptor limit, so a burst of connections makes accept fail with EMFILE.
    let mut child = Command::new("sh")
        .args(["-c", "ulimit -n 12 && exec \"$0\" \"$@\""])
        .arg(env!("CARGO_BIN_EXE_guest-agent-channel"))
        .arg(&socket)
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true)
        .spawn()?;
    for _ in 0..200 {
        if socket.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    // Kept open, so the server's descriptors are not freed again.
    let mut held = Vec::new();
    for _ in 0..40 {
        // Once the server has failed it no longer listens, and a connect is refused.
        let Ok(stream) = UnixStream::connect(&socket).await else {
            break;
        };
        held.push(stream);
    }

    let exit = tokio::time::timeout(Duration::from_secs(10), child.wait()).await??;
    let mut stderr = String::new();
    if let Some(mut pipe) = child.stderr.take() {
        pipe.read_to_string(&mut stderr).await?;
    }

    assert!(!exit.success(), "{exit:?}");
    assert!(stderr.contains("accept failed"), "{stderr}");
    drop(held);
    Ok(())
}

#[tokio::test]
async fn a_socket_that_cannot_be_removed_is_reported_and_fails_the_exit() -> Result {
    use std::os::unix::fs::PermissionsExt;

    let mut agent = Agent::start(&[]).await?;
    let dir = agent.socket.parent().ok_or("no directory")?.to_path_buf();
    // Without write permission on the directory an unlink fails, for any user but root.
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o500))?;
    let pid = agent.child.id().ok_or("no process id")?;
    let status = std::process::Command::new("kill")
        .args(["-TERM", &pid.to_string()])
        .status()?;
    assert!(status.success(), "kill");
    let exit = tokio::time::timeout(Duration::from_secs(5), agent.child.wait()).await??;
    let removable = agent.socket.exists();
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))?;

    // Root is not stopped by the mode, and then the socket is removed and the exit is clean.
    assert_eq!(
        exit.success(),
        !removable,
        "{exit:?}, socket left: {removable}"
    );
    Ok(())
}

#[tokio::test]
async fn the_binary_ends_on_sigterm_and_removes_its_socket() -> Result {
    let mut agent = Agent::start(&[]).await?;
    let pid = agent.child.id().ok_or("no process id")?;

    let status = std::process::Command::new("kill")
        .args(["-TERM", &pid.to_string()])
        .status()?;
    assert!(status.success(), "kill");
    let exit = tokio::time::timeout(Duration::from_secs(5), agent.child.wait()).await??;

    assert!(exit.success(), "{exit:?}");
    assert!(!agent.socket.exists());
    Ok(())
}
