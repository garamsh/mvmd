//! The Guest Agent as the guest's init, driven through the generated client over a Unix socket.
//! The mounts, the network and the power-off are test implementations of the seams; the server,
//! the boot sequence, the exec table and the PID 1 loop are the production ones.

use std::cell::RefCell;
use std::collections::HashMap;
use std::fs;
use std::future::Future;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use guest_agent::v1alpha1::guest_service_client::GuestServiceClient;
use guest_agent::v1alpha1::{
    BootRequest, ExecRecord, ExecRequest, ExecStatus, HealthRequest, Ipv4Config, Ipv6Config,
    ListFilesRequest, NetworkConfig as WireNetworkConfig, ReadExecOutputRequest, RunConfig,
    UpdateNetworkRequest, WriteFileRequest, WriteFileStart, read_exec_output_response,
    write_file_request,
};
use guest_agent::{BootStep, Filesystem, Mounts, Network, NetworkConfig, Power, System, Uuid};
use hyper_util::rt::TokioIo;
use nix::sys::signal::Signal;
use tempfile::TempDir;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Endpoint};
use tonic::{Code, Streaming};
use tower::service_fn;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

const SH: &str = "/bin/sh";
const IMAGE_UUID: &str = "11111111-1111-1111-1111-111111111111";
const DISK_UUID: &str = "22222222-2222-2222-2222-222222222222";
const IMAGE_DEVICE: &str = "/dev/vda";
const DISK_DEVICE: &str = "/dev/vdb";

/// A running agent reaps every exited child its process has that no command of its own holds, and
/// this process holds the commands of every agent a test runs. So the tests take turns.
static ONE_AGENT_AT_A_TIME: Mutex<()> = Mutex::new(());

thread_local! {
    /// The turn the current test thread holds. It is released when the thread ends, which is
    /// after the test's runtime has been dropped with the agent's tasks in it.
    static TURN: RefCell<Option<MutexGuard<'static, ()>>> = const { RefCell::new(None) };
}

/// Wait for this test's turn to run an agent.
fn take_turn() {
    TURN.with_borrow_mut(|turn| {
        if turn.is_none() {
            *turn = Some(lock(&ONE_AGENT_AT_A_TIME));
        }
    });
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// What the seams were asked to do, in order.
#[derive(Clone, Default)]
struct Log(Arc<Mutex<Vec<String>>>);

impl Log {
    fn push(&self, entry: impl Into<String>) {
        lock(&self.0).push(entry.into());
    }

    fn entries(&self) -> Vec<String> {
        lock(&self.0).clone()
    }

    fn count(&self, prefix: &str) -> usize {
        self.entries()
            .iter()
            .filter(|entry| entry.starts_with(prefix))
            .count()
    }
}

/// Test mounts: two devices named by UUID, and a failure on request.
struct TestMounts {
    log: Log,
    /// The operation that fails, by the name it logs.
    fail: Option<&'static str>,
    /// How long `overlay` takes, so two boots can overlap.
    overlay_takes: Duration,
}

impl TestMounts {
    fn step(&self, name: &str, detail: &str) -> io::Result<()> {
        self.log.push(format!("{name} {detail}").trim_end());
        if self.fail == Some(name) {
            return Err(io::Error::other("injected failure"));
        }
        Ok(())
    }
}

impl Mounts for TestMounts {
    fn prepare(&self) -> io::Result<()> {
        self.step("prepare", "")
    }

    fn find_device(&self, filesystem: Filesystem, uuid: &Uuid) -> io::Result<PathBuf> {
        self.step("find", &format!("{filesystem:?} {uuid}"))?;
        let device = match filesystem {
            Filesystem::Erofs => (IMAGE_UUID, IMAGE_DEVICE),
            Filesystem::Ext4 => (DISK_UUID, DISK_DEVICE),
        };
        if uuid.to_string() == device.0 {
            Ok(device.1.into())
        } else {
            Err(io::Error::new(io::ErrorKind::NotFound, "no such device"))
        }
    }

    fn mount_image(&self, device: &Path) -> io::Result<()> {
        self.step("mount_image", &device.display().to_string())
    }

    fn mount_root_disk(&self, device: &Path) -> io::Result<()> {
        self.step("mount_root_disk", &device.display().to_string())
    }

    fn overlay(&self) -> io::Result<()> {
        std::thread::sleep(self.overlay_takes);
        self.step("overlay", "")
    }

    fn switch_root(&self) -> io::Result<()> {
        self.step("switch_root", "")
    }
}

/// A network that records what it was given and, when it has a marker file, creates it.
struct TestNetwork {
    log: Log,
    applied: Mutex<Vec<NetworkConfig>>,
    marker: PathBuf,
    fail: bool,
}

impl Network for TestNetwork {
    fn apply(&self, config: &NetworkConfig) -> io::Result<()> {
        self.log.push("network");
        if self.fail {
            return Err(io::Error::other("injected failure"));
        }
        lock(&self.applied).push(config.clone());
        fs::write(&self.marker, "applied")
    }
}

struct TestPower {
    log: Log,
    done: Mutex<Option<oneshot::Sender<()>>>,
    fail: bool,
}

impl Power for TestPower {
    fn power_off(&self) -> io::Result<()> {
        self.log.push("power_off");
        if let Some(done) = lock(&self.done).take() {
            let _ = done.send(());
        }
        if self.fail {
            return Err(io::Error::other("injected failure"));
        }
        Ok(())
    }
}

#[derive(Default)]
struct Setup {
    fail_mount: Option<&'static str>,
    fail_network: bool,
    fail_power: bool,
    overlay_takes: Duration,
    config: guest_agent::Config,
}

struct Agent {
    client: GuestServiceClient<Channel>,
    /// A directory for what the entrypoint and the tests share.
    dir: TempDir,
    _socket_dir: TempDir,
    log: Log,
    network: Arc<TestNetwork>,
    terminate: mpsc::Sender<Signal>,
    powered_off: oneshot::Receiver<()>,
    run: JoinHandle<std::result::Result<(), guest_agent::Error>>,
}

impl Agent {
    async fn start() -> Result<Self> {
        Self::start_with(Setup::default()).await
    }

    async fn start_with(setup: Setup) -> Result<Self> {
        take_turn();
        let dir = tempfile::tempdir()?;
        let log = Log::default();
        let network = Arc::new(TestNetwork {
            log: log.clone(),
            applied: Mutex::default(),
            marker: dir.path().join("network-applied"),
            fail: setup.fail_network,
        });
        let (done, powered_off) = oneshot::channel();
        let system = System {
            mounts: Arc::new(TestMounts {
                log: log.clone(),
                fail: setup.fail_mount,
                overlay_takes: setup.overlay_takes,
            }),
            network: network.clone(),
            power: Arc::new(TestPower {
                log: log.clone(),
                done: Mutex::new(Some(done)),
                fail: setup.fail_power,
            }),
        };
        let socket_dir = tempfile::tempdir()?;
        let socket = socket_dir.path().join("agent.sock");
        let incoming = UnixListenerStream::new(UnixListener::bind(&socket)?);
        let (terminate, requests) = mpsc::channel(4);
        let run = tokio::spawn(guest_agent::run(incoming, setup.config, system, requests));
        let channel = Endpoint::try_from("http://agent.invalid")?.connect_with_connector_lazy(
            service_fn(move |_| {
                let socket = socket.clone();
                async move { Ok::<_, io::Error>(TokioIo::new(UnixStream::connect(socket).await?)) }
            }),
        );
        Ok(Self {
            client: GuestServiceClient::new(channel),
            dir,
            _socket_dir: socket_dir,
            log,
            network,
            terminate,
            powered_off,
            run,
        })
    }

    /// The boot description of a Machine whose entrypoint is `script` run by `sh`.
    fn boot_request(&self, script: &str) -> BootRequest {
        BootRequest {
            image_uuid: IMAGE_UUID.into(),
            root_disk_uuid: DISK_UUID.into(),
            run: Some(RunConfig {
                argv: vec![SH.into(), "-c".into(), script.into()],
                env: HashMap::from([("GREETING".to_owned(), "hello".to_owned())]),
                working_dir: self.dir.path().display().to_string(),
            }),
            network: Some(network_config("10.1.2.3/32", "fd00::2/128")),
        }
    }

    async fn boot(&mut self, script: &str) -> std::result::Result<(), tonic::Status> {
        let request = self.boot_request(script);
        self.client.boot(request).await.map(|_| ())
    }

    /// Read the entrypoint's output until the stream ends with its final record.
    async fn entrypoint_output(&mut self) -> Result<(String, ExecRecord)> {
        let mut stream = self
            .client
            .read_exec_output(ReadExecOutputRequest {
                exec_id: guest_agent::ENTRYPOINT.into(),
                after_offset: 0,
            })
            .await?
            .into_inner();
        let (output, record) = within(read_to_record(&mut stream, None)).await??;
        Ok((output, record.ok_or("the stream ended without a record")?))
    }

    async fn powered_off(&mut self) -> Result {
        within(&mut self.powered_off).await??;
        Ok(())
    }

    fn is_powered_off(&self) -> bool {
        self.log.count("power_off") > 0
    }
}

fn network_config(ipv4: &str, ipv6: &str) -> WireNetworkConfig {
    WireNetworkConfig {
        ipv4: Some(Ipv4Config {
            address: ipv4.into(),
            gateway: "10.1.0.1".into(),
        }),
        ipv6: Some(Ipv6Config {
            address: ipv6.into(),
            gateway: "fe80::1".into(),
        }),
        resolvers: vec!["10.0.0.53".into()],
    }
}

/// Bound a wait, so a test that would hang fails.
async fn within<T>(work: impl Future<Output = T>) -> Result<T> {
    tokio::time::timeout(Duration::from_secs(20), work)
        .await
        .map_err(|_| "timed out".into())
}

/// Read output until the stream carries `until`, or ends. Returns the text read so far, and the
/// final record if the stream reached it.
async fn read_to_record(
    stream: &mut Streaming<guest_agent::v1alpha1::ReadExecOutputResponse>,
    until: Option<&str>,
) -> Result<(String, Option<ExecRecord>)> {
    let mut output = String::new();
    while let Some(message) = stream.message().await? {
        match message.part.ok_or("empty part")? {
            read_exec_output_response::Part::Chunk(chunk) => {
                output.push_str(&String::from_utf8_lossy(&chunk.data));
                if until.is_some_and(|marker| output.contains(marker)) {
                    return Ok((output, None));
                }
            }
            read_exec_output_response::Part::Record(record) => return Ok((output, Some(record))),
        }
    }
    Ok((output, None))
}

async fn eventually(what: &str, mut condition: impl FnMut() -> bool) -> Result {
    for _ in 0..400 {
        if condition() {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    Err(format!("timed out waiting for {what}").into())
}

#[tokio::test]
async fn boot_takes_the_steps_in_order_and_runs_the_entrypoint_as_described() -> Result {
    let mut agent = Agent::start().await?;
    let dir = agent.dir.path().canonicalize()?;

    agent
        .boot(r#"echo "$GREETING from $(pwd)"; test -e network-applied && echo network-first"#)
        .await?;
    let (output, record) = agent.entrypoint_output().await?;

    // The entrypoint ends at once, and the power-off that follows is another test's subject.
    let steps: Vec<String> = agent
        .log
        .entries()
        .into_iter()
        .filter(|entry| entry != "power_off")
        .collect();
    assert_eq!(
        steps,
        [
            "prepare".to_owned(),
            format!("find Erofs {IMAGE_UUID}"),
            format!("find Ext4 {DISK_UUID}"),
            format!("mount_image {IMAGE_DEVICE}"),
            format!("mount_root_disk {DISK_DEVICE}"),
            "overlay".to_owned(),
            "switch_root".to_owned(),
            "network".to_owned(),
        ]
    );
    // The entrypoint ran after the network was applied (it finds the marker the network wrote),
    // with exactly the described environment and working directory.
    assert_eq!(
        output,
        format!("hello from {}\nnetwork-first\n", dir.display())
    );
    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.exit_code, 0);
    Ok(())
}

#[tokio::test]
async fn boot_hands_the_network_the_parsed_description() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    let applied = agent.network.applied.lock().unwrap().clone();
    assert_eq!(applied.len(), 1);
    let ipv4 = applied[0].ipv4.as_ref().ok_or("no IPv4")?;
    assert_eq!(ipv4.address.to_string(), "10.1.2.3");
    assert_eq!(ipv4.prefix_len, 32);
    assert_eq!(ipv4.gateway.to_string(), "10.1.0.1");
    let ipv6 = applied[0].ipv6.as_ref().ok_or("no IPv6")?;
    assert_eq!(ipv6.address.to_string(), "fd00::2");
    assert_eq!(ipv6.gateway.to_string(), "fe80::1");
    assert_eq!(applied[0].resolvers[0].to_string(), "10.0.0.53");
    Ok(())
}

#[tokio::test]
async fn each_step_that_fails_is_named_over_the_channel_and_ends_the_boot() -> Result {
    // The operations that fail, with the step each is reported as and what must not have run.
    let cases: [(&str, BootStep, &[&str]); 6] = [
        ("find", BootStep::Check, &["mount_image", "network"]),
        ("mount_image", BootStep::MountImage, &["mount_root_disk"]),
        ("mount_root_disk", BootStep::MountRootDisk, &["overlay"]),
        ("overlay", BootStep::Overlay, &["switch_root"]),
        ("switch_root", BootStep::SwitchRoot, &["network"]),
        ("network", BootStep::Network, &[]),
    ];
    for (operation, step, never_ran) in cases {
        let mut agent = Agent::start_with(Setup {
            fail_mount: Some(operation).filter(|name| *name != "network"),
            fail_network: operation == "network",
            ..Setup::default()
        })
        .await?;

        let failure = agent.boot("sleep 30").await.unwrap_err();

        assert_eq!(failure.code(), Code::Internal, "{operation}");
        assert!(
            failure.message().contains(&format!("boot step {step}")),
            "{operation}: {}",
            failure.message()
        );
        assert!(
            failure.message().contains("injected failure"),
            "{operation}"
        );
        for later in never_ran {
            assert_eq!(
                agent.log.count(later),
                0,
                "{operation} ran {later} after failing"
            );
        }
        // The entrypoint never ran, and nothing but a repeat of the failure is served.
        let exec = agent
            .client
            .exec(exec_request("x", "true"))
            .await
            .unwrap_err();
        assert_eq!(exec.code(), Code::FailedPrecondition, "{operation}");
        assert!(
            exec.message().contains(&format!("boot step {step}")),
            "{operation}"
        );
        let repeat = agent.boot("sleep 30").await.unwrap_err();
        assert_eq!(repeat.message(), failure.message(), "{operation}");
        assert_eq!(agent.log.count(operation), 1, "{operation} was tried again");
        assert!(!agent.is_powered_off(), "{operation} powered off");
    }
    Ok(())
}

#[tokio::test]
async fn an_entrypoint_that_cannot_start_fails_the_boot_at_its_step() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("");
    request.run = Some(RunConfig {
        argv: vec!["/no/such/program".into()],
        ..RunConfig::default()
    });

    let failure = agent.client.boot(request).await.unwrap_err();

    assert_eq!(failure.code(), Code::Internal);
    assert!(
        failure
            .message()
            .contains(&format!("boot step {}", BootStep::Entrypoint))
    );
    assert!(failure.message().contains("/no/such/program"));
    Ok(())
}

fn exec_request(id: &str, program: &str) -> ExecRequest {
    ExecRequest {
        exec_id: id.into(),
        argv: vec![program.into()],
        ..ExecRequest::default()
    }
}

#[tokio::test]
async fn the_guest_is_served_only_once_it_has_booted_except_health_and_info() -> Result {
    let mut agent = Agent::start().await?;

    agent.client.health(HealthRequest {}).await?;
    let refused = [
        agent
            .client
            .exec(exec_request("a", "true"))
            .await
            .map(|_| ()),
        agent
            .client
            .list_files(ListFilesRequest { path: "/".into() })
            .await
            .map(|_| ()),
        agent
            .client
            .update_network(UpdateNetworkRequest {
                network: Some(network_config("10.1.2.4/32", "fd00::3/128")),
            })
            .await
            .map(|_| ()),
    ];
    for result in refused {
        assert_eq!(result.unwrap_err().code(), Code::FailedPrecondition);
    }

    // The control: the same calls are served once Boot has succeeded.
    agent.boot("sleep 30").await?;
    assert_eq!(
        agent
            .client
            .exec(exec_request("a", "true"))
            .await?
            .into_inner()
            .record
            .ok_or("no record")?
            .status(),
        ExecStatus::Started
    );
    agent
        .client
        .list_files(ListFilesRequest { path: "/".into() })
        .await?;
    Ok(())
}

#[tokio::test]
async fn a_description_that_is_not_valid_is_refused_and_the_agent_still_waits() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("sleep 30");
    request.image_uuid = "not-a-uuid".into();

    let refused = agent.client.boot(request).await.unwrap_err();

    assert_eq!(refused.code(), Code::InvalidArgument);
    assert_eq!(agent.log.count("find"), 0);
    // The control: the agent has not given up, and the valid description boots it.
    agent.boot("sleep 30").await?;
    Ok(())
}

#[tokio::test]
async fn a_repeated_boot_succeeds_without_running_the_steps_again() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    let steps = agent.log.entries();

    agent.boot("sleep 30").await?;
    let different = agent.boot("sleep 31").await.unwrap_err();

    assert_eq!(agent.log.entries(), steps);
    assert_eq!(different.code(), Code::FailedPrecondition);
    Ok(())
}

#[tokio::test]
async fn two_boots_at_once_run_the_steps_once() -> Result {
    let mut agent = Agent::start_with(Setup {
        overlay_takes: Duration::from_millis(300),
        ..Setup::default()
    })
    .await?;
    let mut other = agent.client.clone();
    let request = agent.boot_request("sleep 30");

    let (first, second) = tokio::join!(
        agent.client.boot(request.clone()),
        other.boot(request.clone())
    );

    first?;
    second?;
    assert_eq!(agent.log.count("overlay"), 1);
    assert_eq!(agent.log.count("network"), 1);
    Ok(())
}

#[tokio::test]
async fn update_network_applies_the_new_configuration_after_the_boot() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    agent
        .client
        .update_network(UpdateNetworkRequest {
            network: Some(network_config("10.9.9.9/32", "fd00::9/128")),
        })
        .await?;

    let applied = agent.network.applied.lock().unwrap().clone();
    assert_eq!(applied.len(), 2);
    assert_eq!(
        applied[1]
            .ipv4
            .as_ref()
            .ok_or("no IPv4")?
            .address
            .to_string(),
        "10.9.9.9"
    );
    assert_eq!(
        applied[1]
            .ipv6
            .as_ref()
            .ok_or("no IPv6")?
            .address
            .to_string(),
        "fd00::9"
    );
    Ok(())
}

#[tokio::test]
async fn the_entrypoints_exit_is_reported_and_the_machine_powers_off() -> Result {
    let mut agent = Agent::start().await?;
    let flag = agent.dir.path().join("exit-now");
    agent
        .boot(&format!(
            "echo bye; while [ ! -e {} ]; do sleep 0.05; done; exit 7",
            flag.display()
        ))
        .await?;
    let mut stream = agent
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: guest_agent::ENTRYPOINT.into(),
            after_offset: 0,
        })
        .await?
        .into_inner();
    let (seen, _) = within(read_to_record(&mut stream, Some("bye"))).await??;
    assert_eq!(seen, "bye\n");
    // The control: a running entrypoint does not power the Machine off.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(!agent.is_powered_off());

    fs::write(&flag, "")?;
    let (_, record) = within(read_to_record(&mut stream, None)).await??;
    agent.powered_off().await?;

    let record = record.ok_or("the stream ended without the record")?;
    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.exit_code, 7);
    within(agent.run).await???;
    Ok(())
}

#[tokio::test]
async fn a_client_that_never_finishes_cannot_keep_the_machine_on() -> Result {
    let mut agent = Agent::start_with(Setup {
        config: guest_agent::Config {
            shutdown_grace: Duration::from_millis(300),
            ..guest_agent::Config::default()
        },
        ..Setup::default()
    })
    .await?;
    let flag = agent.dir.path().join("exit-now");
    agent
        .boot(&format!(
            "while [ ! -e {} ]; do sleep 0.05; done",
            flag.display()
        ))
        .await?;
    // A write that starts and never ends holds its connection's request open.
    let (keep_open, parts) = mpsc::channel::<WriteFileRequest>(1);
    let target = agent.dir.path().join("never-finished");
    keep_open
        .send(WriteFileRequest {
            part: Some(write_file_request::Part::Start(WriteFileStart {
                path: target.display().to_string(),
            })),
        })
        .await?;
    let mut writer = agent.client.clone();
    let stuck = tokio::spawn(async move {
        writer
            .write_file(tokio_stream::wrappers::ReceiverStream::new(parts))
            .await
    });
    tokio::time::sleep(Duration::from_millis(200)).await;

    fs::write(&flag, "")?;

    agent.powered_off().await?;
    stuck.abort();
    Ok(())
}

#[tokio::test]
async fn termination_is_forwarded_to_the_entrypoint_as_the_signal_received() -> Result {
    for (signal, code) in [(Signal::SIGTERM, 5), (Signal::SIGINT, 6)] {
        let mut agent = Agent::start().await?;
        agent
            .boot(
                "trap 'echo got-term; exit 5' TERM; trap 'echo got-int; exit 6' INT; \
                 echo ready; while :; do sleep 0.05; done",
            )
            .await?;
        let mut stream = agent
            .client
            .read_exec_output(ReadExecOutputRequest {
                exec_id: guest_agent::ENTRYPOINT.into(),
                after_offset: 0,
            })
            .await?
            .into_inner();
        within(read_to_record(&mut stream, Some("ready"))).await??;
        assert!(!agent.is_powered_off(), "{signal} not yet sent");

        agent.terminate.send(signal).await?;
        let (output, record) = within(read_to_record(&mut stream, None)).await??;
        agent.powered_off().await?;

        let record = record.ok_or("no record")?;
        assert_eq!(record.status(), ExecStatus::Exited, "{signal}");
        assert_eq!(record.exit_code, code, "{signal}");
        assert!(
            output.contains(if code == 5 { "got-term" } else { "got-int" }),
            "{signal}: {output}"
        );
    }
    Ok(())
}

#[tokio::test]
async fn termination_before_the_entrypoint_runs_powers_the_machine_off() -> Result {
    let mut agent = Agent::start().await?;
    // The control: an agent still waiting for its description does not power off by itself.
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!agent.is_powered_off());

    agent.terminate.send(Signal::SIGTERM).await?;

    agent.powered_off().await?;
    within(agent.run).await???;
    Ok(())
}

#[tokio::test]
async fn a_failed_power_off_is_returned() -> Result {
    let agent = Agent::start_with(Setup {
        fail_power: true,
        ..Setup::default()
    })
    .await?;
    agent.terminate.send(Signal::SIGTERM).await?;
    let outcome = within(agent.run).await??;
    assert!(
        matches!(outcome, Err(guest_agent::Error::Io { .. })),
        "{outcome:?}"
    );
    Ok(())
}

#[tokio::test]
async fn a_failed_prepare_stops_the_agent_before_it_serves() -> Result {
    let agent = Agent::start_with(Setup {
        fail_mount: Some("prepare"),
        ..Setup::default()
    })
    .await?;
    let outcome = within(agent.run).await??;
    assert!(
        matches!(outcome, Err(guest_agent::Error::Io { .. })),
        "{outcome:?}"
    );
    Ok(())
}

fn process_state(pid: u32) -> Option<(char, u32)> {
    let stat = fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let rest = stat.rsplit_once(')')?.1;
    let mut fields = rest.split_whitespace();
    Some((fields.next()?.chars().next()?, fields.next()?.parse().ok()?))
}

#[tokio::test]
async fn processes_orphaned_onto_init_are_reaped_and_a_commands_own_status_is_kept() -> Result {
    // Orphans are handed to this process as they are to PID 1 in a guest.
    nix::sys::prctl::set_child_subreaper(true)?;
    let mut agent = Agent::start().await?;
    let pid_file = agent.dir.path().join("orphan.pid");
    let flag = agent.dir.path().join("exit-now");
    agent
        .boot(&format!(
            "(sleep 1 >/dev/null 2>&1 & echo $! > {pid}.tmp; mv {pid}.tmp {pid}); \
             while [ ! -e {flag} ]; do sleep 0.05; done",
            pid = pid_file.display(),
            flag = flag.display()
        ))
        .await?;
    eventually("the orphan's process ID", || pid_file.exists()).await?;
    let orphan: u32 = fs::read_to_string(&pid_file)?.trim().parse()?;
    // The setup took: the orphan now belongs to this process, as it would to init.
    eventually("the orphan to be adopted", || {
        process_state(orphan).is_some_and(|(_, parent)| parent == std::process::id())
    })
    .await?;

    // A command of the agent's own ends while the reaping goes on; its status is not taken.
    agent
        .client
        .exec(ExecRequest {
            exec_id: "own".into(),
            argv: vec![SH.into(), "-c".into(), "sleep 1.2; exit 9".into()],
            ..ExecRequest::default()
        })
        .await?;
    let mut stream = agent
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: "own".into(),
            after_offset: 0,
        })
        .await?
        .into_inner();
    let (_, own) = within(read_to_record(&mut stream, None)).await??;

    // The orphan ended a while ago; it is gone and not left as a zombie.
    eventually("the orphan to be reaped", || {
        process_state(orphan).is_none()
    })
    .await?;
    let own = own.ok_or("no record")?;
    assert_eq!(own.status(), ExecStatus::Exited);
    assert_eq!(own.exit_code, 9);
    fs::write(&flag, "")?;
    agent.powered_off().await?;
    Ok(())
}
