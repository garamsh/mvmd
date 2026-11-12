//! The Guest Agent as the guest's init, driven through the generated client over a Unix socket.
//! The mounts, the network and the power-off are test implementations of the seams; the server,
//! the boot sequence, the exec table and the PID 1 loop are the production ones.

use std::cell::RefCell;
use std::collections::HashMap;
use std::convert::Infallible;
use std::fs;
use std::future::Future;
use std::io;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, SystemTime};

use guest_agent::v1alpha1::guest_service_client::GuestServiceClient;
use guest_agent::v1alpha1::{
    BootFailed, BootRequest, BootState, BootStep as WireStep, CatchUpRequest, DiskInUse,
    ExecRecord, ExecRequest, ExecStatus, FindDiskRequest, HealthRequest, Ipv4Config, Ipv6Config,
    ListFilesRequest, NetworkConfig as WireNetworkConfig, NotBooted, OnlineVcpusRequest,
    PatchFileRequest, ReadExecOutputRequest, ReadFileRequest, ReleaseDiskRequest, RunConfig,
    SearchFilesRequest, SignalRequest, TerminateRequest, UpdateNetworkRequest, WriteFileRequest,
    WriteFileStart, read_exec_output_response, write_file_request,
};
use guest_agent::{
    BootStep, Clock, Console, DiskDevice, DiskError, Disks, Entropy, Filesystem, GuestSize,
    KernelCommandLine, Mounts, Network, NetworkConfig, Power, Resources, System, Uuid,
};
use hyper_util::rt::TokioIo;
use nix::sys::signal::{Signal, kill};
use nix::sys::wait::waitpid;
use nix::unistd::Pid;
use prost_types::Timestamp;
use tempfile::TempDir;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio_stream::StreamExt;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Endpoint};
use tonic::{Code, Streaming};
use tower::service_fn;

#[path = "support/details.rs"]
mod details;

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
    /// Every call fails while this is set.
    failing: AtomicBool,
}

impl Network for TestNetwork {
    fn apply(&self, config: &NetworkConfig) -> io::Result<()> {
        self.log.push("network");
        if self.failing.load(Ordering::SeqCst) {
            return Err(io::Error::other("injected failure"));
        }
        lock(&self.applied).push(config.clone());
        fs::write(&self.marker, "applied")
    }
}

/// The kernel command line of a Machine whose Driver names the power-off.
const POWER_OFF_COMMAND_LINE: &str = "console=ttyS0 mvmd.end=poweroff";

/// A command line that reads as the text, or fails to read when there is none.
struct TestCommandLine(Option<&'static str>);

impl KernelCommandLine for TestCommandLine {
    fn read(&self) -> io::Result<String> {
        self.0
            .map(str::to_owned)
            .ok_or_else(|| io::Error::other("injected failure"))
    }
}

struct TestPower {
    log: Log,
    done: Mutex<Option<oneshot::Sender<()>>>,
    held: Mutex<Option<std::sync::mpsc::Receiver<()>>>,
    /// Both calls fail.
    fail: bool,
    /// The restart fails and the power-off works.
    fail_restart: bool,
}

impl TestPower {
    /// What a call that works does: report it, then wait until the test drops the agent, which
    /// drops the sender. The real call does not return.
    fn works(&self) {
        if let Some(done) = lock(&self.done).take() {
            let _ = done.send(());
        }
        if let Some(held) = lock(&self.held).take() {
            let _ = held.recv();
        }
    }
}

impl Power for TestPower {
    /// Returns, which the real call does only when it has failed.
    fn power_off(&self) -> io::Result<()> {
        self.log.push("power_off");
        if self.fail {
            return Err(io::Error::other("injected failure"));
        }
        self.works();
        Ok(())
    }

    fn restart(&self) -> io::Result<()> {
        self.log.push("restart");
        if self.fail || self.fail_restart {
            return Err(io::Error::other("injected failure"));
        }
        self.works();
        Ok(())
    }

    /// Waits until the test drops the agent, which a halt that works never does.
    fn halt(&self) {
        if let Some(held) = lock(&self.held).take() {
            let _ = held.recv();
        }
    }
}

/// An entropy pool that keeps what it is given and fails while `failing` is set.
struct TestEntropy {
    log: Log,
    mixed: Mutex<Vec<Vec<u8>>>,
    failing: AtomicBool,
}

impl Entropy for TestEntropy {
    fn mix_and_reseed(&self, bytes: &[u8]) -> io::Result<()> {
        self.log.push("entropy");
        if self.failing.load(Ordering::SeqCst) {
            return Err(io::Error::other("injected failure"));
        }
        lock(&self.mixed).push(bytes.to_vec());
        Ok(())
    }
}

/// A clock that keeps what it is set to and fails while `failing` is set.
struct TestClock {
    log: Log,
    set: Mutex<Vec<i64>>,
    failing: AtomicBool,
}

impl Clock for TestClock {
    fn set_realtime(&self, unix_nanos: i64) -> io::Result<()> {
        self.log.push("clock");
        if self.failing.load(Ordering::SeqCst) {
            return Err(io::Error::other("injected failure"));
        }
        lock(&self.set).push(unix_nanos);
        Ok(())
    }
}

/// Disks that remember what was found, refuse to release one that is in use and log each call.
struct TestDisks {
    log: Log,
    found: Mutex<std::collections::HashSet<String>>,
    in_use: AtomicBool,
    no_device: AtomicBool,
    fail_account: bool,
}

impl Disks for TestDisks {
    fn account_for_boot(&self) -> io::Result<()> {
        self.log.push("disk account_for_boot");
        if self.fail_account {
            return Err(io::Error::other("injected failure"));
        }
        Ok(())
    }

    fn find(&self, disk_id: &str) -> std::result::Result<DiskDevice, DiskError> {
        self.log.push(format!("disk find {disk_id}"));
        if self.no_device.load(Ordering::SeqCst) {
            return Err(DiskError::Failed(io::Error::other("no new device")));
        }
        lock(&self.found).insert(disk_id.to_owned());
        Ok(DiskDevice {
            name: "vdb".into(),
            size_bytes: 1 << 20,
            read_only: true,
        })
    }

    fn release(&self, disk_id: &str) -> std::result::Result<(), DiskError> {
        self.log.push(format!("disk release {disk_id}"));
        if !lock(&self.found).contains(disk_id) {
            return Err(DiskError::Unknown);
        }
        if self.in_use.load(Ordering::SeqCst) {
            return Err(DiskError::InUse);
        }
        lock(&self.found).remove(disk_id);
        Ok(())
    }
}

/// Resources that log each call and report two vCPUs and 1 MiB, or fail naming a CPU.
struct TestResources {
    log: Log,
    failing: AtomicBool,
}

impl Resources for TestResources {
    fn online_vcpus(&self, target: u32) -> io::Result<GuestSize> {
        self.log.push(format!("resources online_vcpus {target}"));
        if self.failing.load(Ordering::SeqCst) {
            return Err(io::Error::other("cpu3: Input/output error"));
        }
        Ok(GuestSize {
            vcpus: 2,
            memory_bytes: 1 << 20,
        })
    }
}

/// A console that keeps its lines in the log, as `console: <line>`.
struct TestConsole(Log);

impl Console for TestConsole {
    fn line(&self, text: &str) {
        self.0.push(format!("console: {text}"));
    }
}

#[derive(Default)]
struct Setup {
    fail_mount: Option<&'static str>,
    fail_network: bool,
    fail_clock: bool,
    fail_power: bool,
    fail_restart: bool,
    /// The kernel command line; the power-off's when none.
    command_line: Option<&'static str>,
    /// The command line cannot be read.
    unreadable_command_line: bool,
    fail_account: bool,
    overlay_takes: Duration,
    listener_ends: bool,
    config: guest_agent::Config,
    /// Disks in place of the test ones.
    disks: Option<Arc<dyn Disks>>,
}

struct Agent {
    client: GuestServiceClient<Channel>,
    /// A directory for what the entrypoint and the tests share.
    dir: TempDir,
    _socket_dir: TempDir,
    log: Log,
    network: Arc<TestNetwork>,
    entropy: Arc<TestEntropy>,
    clock: Arc<TestClock>,
    disks: Arc<TestDisks>,
    resources: Arc<TestResources>,
    /// Whether the agent has been asked to listen.
    listened: Arc<AtomicBool>,
    terminate: mpsc::Sender<Signal>,
    powered_off: oneshot::Receiver<()>,
    /// The agent never returns, so this task never finishes.
    run: JoinHandle<Infallible>,
    /// Keeps a power-off that works from returning until the agent is dropped.
    _hold: std::sync::mpsc::Sender<()>,
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
            failing: AtomicBool::new(setup.fail_network),
        });
        let (done, powered_off) = oneshot::channel();
        let (hold, held) = std::sync::mpsc::channel();
        let entropy = Arc::new(TestEntropy {
            log: log.clone(),
            mixed: Mutex::default(),
            failing: AtomicBool::new(false),
        });
        let clock = Arc::new(TestClock {
            log: log.clone(),
            set: Mutex::default(),
            failing: AtomicBool::new(setup.fail_clock),
        });
        let disks = Arc::new(TestDisks {
            log: log.clone(),
            found: Mutex::default(),
            in_use: AtomicBool::new(false),
            no_device: AtomicBool::new(false),
            fail_account: setup.fail_account,
        });
        let resources = Arc::new(TestResources {
            log: log.clone(),
            failing: AtomicBool::new(false),
        });
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
                held: Mutex::new(Some(held)),
                fail: setup.fail_power,
                fail_restart: setup.fail_restart,
            }),
            console: Arc::new(TestConsole(log.clone())),
            entropy: entropy.clone(),
            clock: clock.clone(),
            disks: setup.disks.unwrap_or_else(|| disks.clone()),
            resources: resources.clone(),
            command_line: Arc::new(TestCommandLine(
                (!setup.unreadable_command_line)
                    .then_some(setup.command_line.unwrap_or(POWER_OFF_COMMAND_LINE)),
            )),
            end_method: Arc::default(),
        };
        let socket_dir = tempfile::tempdir()?;
        let socket = socket_dir.path().join("agent.sock");
        let incoming = UnixListenerStream::new(UnixListener::bind(&socket)?);
        let (terminate, requests) = mpsc::channel(4);
        // A listener that accepts nothing and ends stands for a server that stops by itself.
        let accepted = if setup.listener_ends { 0 } else { usize::MAX };
        let listened = Arc::new(AtomicBool::new(false));
        let listening = listened.clone();
        let run = tokio::spawn(guest_agent::run(
            move || {
                listening.store(true, Ordering::SeqCst);
                Ok((incoming.take(accepted), requests))
            },
            setup.config,
            system,
        ));
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
            entropy,
            clock,
            disks,
            resources,
            listened,
            terminate,
            powered_off,
            run,
            _hold: hold,
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
            host_unix_nanos: 0,
        }
    }

    async fn boot(&mut self, script: &str) -> std::result::Result<(), tonic::Status> {
        let request = self.boot_request(script);
        self.client.boot(request).await.map(|_| ())
    }

    async fn powered_off(&mut self) -> Result {
        within(&mut self.powered_off).await??;
        Ok(())
    }

    fn is_powered_off(&self) -> bool {
        self.log.count("power_off") > 0
    }

    /// The lines the console has been given.
    fn console(&self) -> Vec<String> {
        self.log
            .entries()
            .into_iter()
            .filter_map(|entry| entry.strip_prefix("console: ").map(str::to_owned))
            .collect()
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

/// The time an exit line ends with: `head`, then ` at=` and an RFC 3339 UTC time.
fn line_time(line: &str, head: &str) -> Result<SystemTime> {
    let text = line
        .strip_prefix(head)
        .and_then(|rest| rest.strip_prefix(" at="))
        .ok_or_else(|| format!("{line:?} is not {head:?} followed by ` at=`"))?;
    Ok(SystemTime::try_from(Timestamp::from_str(text)?)?)
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

    // The entrypoint waits for the reader, so the agent is still serving when it is read.
    let flag = agent.dir.path().join("read-now");
    agent
        .boot(&format!(
            r#"echo "$GREETING from $(pwd)"; test -e network-applied && echo network-first; \
               while [ ! -e {} ]; do sleep 0.05; done"#,
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
    fs::write(&flag, "")?;
    let (output, record) = within(read_to_record(&mut stream, None)).await??;
    let record = record.ok_or("the stream ended without the record")?;

    // The entrypoint ends at once, and the power-off that follows is another test's subject.
    let steps: Vec<String> = agent
        .log
        .entries()
        .into_iter()
        .filter(|entry| entry != "power_off" && !entry.starts_with("console: "))
        .collect();
    assert_eq!(
        steps,
        [
            "prepare".to_owned(),
            "disk account_for_boot".to_owned(),
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
    let cases: [(&str, BootStep, WireStep, &[&str]); 6] = [
        (
            "find",
            BootStep::Check,
            WireStep::Check,
            &["mount_image", "network"],
        ),
        (
            "mount_image",
            BootStep::MountImage,
            WireStep::MountImage,
            &["mount_root_disk"],
        ),
        (
            "mount_root_disk",
            BootStep::MountRootDisk,
            WireStep::MountRootDisk,
            &["overlay"],
        ),
        (
            "overlay",
            BootStep::Overlay,
            WireStep::Overlay,
            &["switch_root"],
        ),
        (
            "switch_root",
            BootStep::SwitchRoot,
            WireStep::SwitchRoot,
            &["network"],
        ),
        ("network", BootStep::Network, WireStep::Network, &[]),
    ];
    for (operation, step, wire_step, never_ran) in cases {
        let mut agent = Agent::start_with(Setup {
            fail_mount: Some(operation).filter(|name| *name != "network"),
            fail_network: operation == "network",
            ..Setup::default()
        })
        .await?;

        let failure = agent.boot("sleep 30").await.unwrap_err();

        assert_eq!(failure.code(), Code::Internal, "{operation}");
        // The step and its detail are read from the status detail and not from the message.
        let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
        assert_eq!(failed.step(), wire_step, "{operation}");
        assert_eq!(failed.detail, "injected failure", "{operation}");
        // The console carries the same failure, once, in the form the daemon reads.
        assert_eq!(
            agent.console(),
            [format!(
                "mvmd-guest-agent: boot failed step={step} detail=injected failure"
            )],
            "{operation}"
        );
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
        let not_booted: NotBooted = details::detail(&exec).ok_or("no NotBooted detail")?;
        assert_eq!(not_booted.state(), BootState::Failed, "{operation}");
        let repeat = agent.boot("sleep 30").await.unwrap_err();
        assert_eq!(repeat.message(), failure.message(), "{operation}");
        let again: BootFailed =
            details::detail(&repeat).ok_or("no BootFailed detail on a repeat")?;
        assert_eq!(again, failed, "{operation}");
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
    let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
    assert_eq!(failed.step(), WireStep::Entrypoint);
    assert!(
        failed.detail.contains("/no/such/program"),
        "{}",
        failed.detail
    );
    Ok(())
}

#[tokio::test]
async fn termination_after_an_entrypoint_that_did_not_start_powers_the_machine_off() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("");
    request.run = Some(RunConfig {
        argv: vec!["/no/such/program".into()],
        ..RunConfig::default()
    });
    agent.client.boot(request).await.unwrap_err();
    // The control: the failed boot alone does not power the Machine off.
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!agent.is_powered_off());

    agent.terminate.send(Signal::SIGTERM).await?;

    agent.powered_off().await?;
    assert!(
        agent
            .console()
            .iter()
            .any(|line| line.starts_with("mvmd-guest-agent: entrypoint not started reason="))
    );
    Ok(())
}

#[tokio::test]
async fn a_boot_dropped_mid_step_still_finishes_and_a_repeat_finds_its_outcome() -> Result {
    let mut agent = Agent::start_with(Setup {
        overlay_takes: Duration::from_millis(400),
        ..Setup::default()
    })
    .await?;
    let request = agent.boot_request("sleep 30");
    let mut dropped = agent.client.clone();
    let abandoned = tokio::spawn({
        let request = request.clone();
        async move { dropped.boot(request).await }
    });
    // The first call is inside the overlay step when its caller goes away.
    eventually("the boot to reach the overlay", || {
        agent.log.count("mount_root_disk") > 0
    })
    .await?;
    abandoned.abort();
    let _ = abandoned.await;

    within(agent.client.boot(request)).await??;

    assert_eq!(agent.log.count("overlay"), 1);
    assert_eq!(agent.log.count("network"), 1);
    // The repeat is served as a booted guest.
    agent
        .client
        .list_files(ListFilesRequest { path: "/".into() })
        .await?;
    Ok(())
}

fn catch_up_request(restore_id: &str) -> CatchUpRequest {
    CatchUpRequest {
        restore_id: restore_id.into(),
        entropy: (0..32).collect(),
        host_unix_nanos: 1_700_000_000_123_456_789,
        network: Some(network_config("10.9.9.9/32", "fd00::9/128")),
    }
}

#[tokio::test]
async fn catch_up_mixes_the_entropy_sets_the_time_and_applies_the_network_in_that_order() -> Result
{
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    let after_boot = agent.log.entries().len();

    agent.client.catch_up(catch_up_request("restore-1")).await?;

    let steps: Vec<String> = agent.log.entries().split_off(after_boot);
    assert_eq!(steps, ["entropy", "clock", "network"]);
    // What was given is what reached the seams.
    assert_eq!(*lock(&agent.entropy.mixed), [(0..32).collect::<Vec<u8>>()]);
    assert_eq!(*lock(&agent.clock.set), [1_700_000_000_123_456_789]);
    let applied = lock(&agent.network.applied).clone();
    assert_eq!(
        applied[1]
            .ipv4
            .as_ref()
            .ok_or("no IPv4")?
            .address
            .to_string(),
        "10.9.9.9"
    );
    Ok(())
}

#[tokio::test]
async fn a_repeated_catch_up_does_nothing_and_a_new_restore_does_it_again() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    agent.client.catch_up(catch_up_request("restore-1")).await?;

    // The control: another restore is carried out.
    let mut again = catch_up_request("restore-2");
    again.entropy = vec![9; 32];
    again.host_unix_nanos += 5;
    agent.client.catch_up(again).await?;
    assert_eq!(lock(&agent.entropy.mixed).len(), 2);

    // A repeat of the last one, even with a stale time, changes nothing: the clock does not go
    // back.
    let mut stale = catch_up_request("restore-2");
    stale.host_unix_nanos = 1;
    agent.client.catch_up(stale).await?;
    assert_eq!(lock(&agent.entropy.mixed).len(), 2);
    assert_eq!(lock(&agent.clock.set).len(), 2);
    assert_eq!(lock(&agent.network.applied).len(), 3);
    Ok(())
}

#[tokio::test]
async fn two_catch_ups_at_once_with_one_restore_id_do_the_work_once() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    let mut other = agent.client.clone();

    let (first, second) = tokio::join!(
        agent.client.catch_up(catch_up_request("restore-1")),
        other.catch_up(catch_up_request("restore-1"))
    );

    first?;
    second?;
    assert_eq!(lock(&agent.entropy.mixed).len(), 1);
    Ok(())
}

#[tokio::test]
async fn a_catch_up_before_the_boot_is_refused() -> Result {
    let mut agent = Agent::start().await?;

    let refused = agent
        .client
        .catch_up(catch_up_request("restore-1"))
        .await
        .unwrap_err();

    assert_eq!(refused.code(), Code::FailedPrecondition);
    let not_booted: NotBooted = details::detail(&refused).ok_or("no NotBooted detail")?;
    assert_eq!(not_booted.state(), BootState::Pending);
    assert!(lock(&agent.entropy.mixed).is_empty());
    // The control: after the boot the same request is served.
    agent.boot("sleep 30").await?;
    agent.client.catch_up(catch_up_request("restore-1")).await?;
    Ok(())
}

#[tokio::test]
async fn a_catch_up_that_is_not_valid_is_refused_and_changes_nothing() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    let mut requests = Vec::new();
    for change in [
        |r: &mut CatchUpRequest| r.restore_id.clear(),
        |r: &mut CatchUpRequest| r.entropy.truncate(31),
        |r: &mut CatchUpRequest| r.entropy = vec![0; 257],
        |r: &mut CatchUpRequest| r.host_unix_nanos = 0,
        |r: &mut CatchUpRequest| r.network = None,
    ] {
        let mut request = catch_up_request("restore-1");
        change(&mut request);
        requests.push(request);
    }

    for request in requests {
        let refused = agent.client.catch_up(request).await.unwrap_err();
        assert_eq!(refused.code(), Code::InvalidArgument);
    }

    assert!(lock(&agent.entropy.mixed).is_empty());
    assert!(lock(&agent.clock.set).is_empty());
    // The control: a request with every part right is carried out.
    agent.client.catch_up(catch_up_request("restore-1")).await?;
    assert_eq!(lock(&agent.entropy.mixed).len(), 1);
    Ok(())
}

#[tokio::test]
async fn a_step_that_fails_is_named_stops_the_catch_up_and_a_repeat_does_it_all_again() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    agent.entropy.failing.store(true, Ordering::SeqCst);

    let failed = agent
        .client
        .catch_up(catch_up_request("restore-1"))
        .await
        .unwrap_err();

    assert_eq!(failed.code(), Code::Internal);
    assert!(
        failed
            .message()
            .contains("mix entropy into the kernel's pool"),
        "{}",
        failed.message()
    );
    assert!(
        lock(&agent.clock.set).is_empty(),
        "the clock was set after the failure"
    );

    agent.entropy.failing.store(false, Ordering::SeqCst);
    agent.client.catch_up(catch_up_request("restore-1")).await?;
    assert_eq!(lock(&agent.entropy.mixed).len(), 1);
    assert_eq!(lock(&agent.clock.set).len(), 1);
    Ok(())
}

#[tokio::test]
async fn a_catch_up_step_after_the_first_that_fails_is_named_stops_the_rest_and_a_repeat_does_it_all_again()
-> Result {
    // The step that fails, the text naming it, and what must not have run after it.
    for (failing, named, never_ran) in [
        ("clock", "set the wall clock", &["network"][..]),
        ("network", "configure the network", &[][..]),
    ] {
        let mut agent = Agent::start().await?;
        agent.boot("sleep 30").await?;
        let after_boot = agent.log.entries().len();
        if failing == "clock" {
            agent.clock.failing.store(true, Ordering::SeqCst);
        } else {
            agent.network.failing.store(true, Ordering::SeqCst);
        }

        let failed = agent
            .client
            .catch_up(catch_up_request("restore-1"))
            .await
            .unwrap_err();

        assert_eq!(failed.code(), Code::Internal, "{failing}");
        assert!(
            failed.message().contains(named),
            "{failing}: {}",
            failed.message()
        );
        assert!(
            failed.message().contains("injected failure"),
            "{failing}: {}",
            failed.message()
        );
        // The steps before it ran, once each, in order; none after it did.
        let steps: Vec<String> = agent.log.entries().split_off(after_boot);
        let expected: &[&str] = if failing == "clock" {
            &["entropy", "clock"]
        } else {
            &["entropy", "clock", "network"]
        };
        assert_eq!(steps, expected, "{failing}");
        for later in never_ran {
            assert_eq!(
                agent.log.count(later),
                1,
                "{failing} ran {later} after failing"
            );
        }
        // The failed ID was recorded nowhere: the repeat does every step again.
        agent.clock.failing.store(false, Ordering::SeqCst);
        agent.network.failing.store(false, Ordering::SeqCst);
        agent.client.catch_up(catch_up_request("restore-1")).await?;
        let steps: Vec<String> = agent.log.entries().split_off(after_boot + steps.len());
        assert_eq!(steps, ["entropy", "clock", "network"], "{failing}");
        assert_eq!(lock(&agent.entropy.mixed).len(), 2, "{failing}");
        // The clock was set by the failed call as well when only the network failed.
        let clock_sets = if failing == "clock" { 1 } else { 2 };
        assert_eq!(lock(&agent.clock.set).len(), clock_sets, "{failing}");
        assert_eq!(lock(&agent.network.applied).len(), 2, "{failing}");
        // Now recorded: a third call with the same ID does nothing.
        agent.client.catch_up(catch_up_request("restore-1")).await?;
        assert_eq!(lock(&agent.entropy.mixed).len(), 2, "{failing}");
    }
    Ok(())
}

#[tokio::test]
async fn an_update_without_a_network_is_refused_and_changes_nothing() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    let refused = agent
        .client
        .update_network(UpdateNetworkRequest { network: None })
        .await
        .unwrap_err();

    assert_eq!(refused.code(), Code::InvalidArgument);
    assert_eq!(lock(&agent.network.applied).len(), 1);
    // The control: an update that names a network, even an empty one, is applied.
    agent
        .client
        .update_network(UpdateNetworkRequest {
            network: Some(WireNetworkConfig::default()),
        })
        .await?;
    assert_eq!(lock(&agent.network.applied).len(), 2);
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
        let refusal = result.unwrap_err();
        assert_eq!(refusal.code(), Code::FailedPrecondition);
        // Boot has not been called: the boot is pending, not failed.
        let not_booted: NotBooted = details::detail(&refusal).ok_or("no NotBooted detail")?;
        assert_eq!(not_booted.state(), BootState::Pending);
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

type Refusal = std::result::Result<(), tonic::Status>;

/// The gated calls that no other test makes before the boot, each with the name it is reported by.
async fn gated_calls(client: &mut GuestServiceClient<Channel>) -> Vec<(&'static str, Refusal)> {
    let write = vec![WriteFileRequest {
        part: Some(write_file_request::Part::Start(WriteFileStart {
            path: "/no/such/dir/file".into(),
        })),
    }];
    vec![
        (
            "signal",
            client
                .signal(SignalRequest {
                    exec_id: "x".into(),
                    signal: 15,
                })
                .await
                .map(|_| ()),
        ),
        (
            "read_exec_output",
            client
                .read_exec_output(ReadExecOutputRequest {
                    exec_id: "x".into(),
                    after_offset: 0,
                })
                .await
                .map(|_| ()),
        ),
        (
            "read_file",
            client
                .read_file(ReadFileRequest {
                    path: "/no/such/file".into(),
                })
                .await
                .map(|_| ()),
        ),
        (
            "write_file",
            client
                .write_file(tokio_stream::iter(write))
                .await
                .map(|_| ()),
        ),
        (
            "patch_file",
            client
                .patch_file(PatchFileRequest {
                    path: "/no/such/file".into(),
                    ..PatchFileRequest::default()
                })
                .await
                .map(|_| ()),
        ),
        (
            "search_files",
            client
                .search_files(SearchFilesRequest {
                    path: "/no/such/dir".into(),
                    pattern: "*".into(),
                })
                .await
                .map(|_| ()),
        ),
    ]
}

/// Asserts that every gated call was refused as not booted, in `state`.
fn assert_refused_as_not_booted(calls: Vec<(&'static str, Refusal)>, state: BootState) -> Result {
    assert_eq!(calls.len(), 6);
    for (call, result) in calls {
        let refusal = result.err().ok_or_else(|| format!("{call}: was served"))?;
        assert_eq!(refusal.code(), Code::FailedPrecondition, "{call}");
        let not_booted: NotBooted =
            details::detail(&refusal).ok_or_else(|| format!("{call}: no NotBooted detail"))?;
        assert_eq!(not_booted.state(), state, "{call}");
    }
    Ok(())
}

#[tokio::test]
async fn the_signal_exec_output_and_file_calls_are_refused_before_the_boot() -> Result {
    let mut agent = Agent::start().await?;

    let calls = gated_calls(&mut agent.client).await;

    assert_refused_as_not_booted(calls, BootState::Pending)?;
    // The control: once booted the same calls are not refused as not booted. Each fails for its
    // own reason, because the exec, the file and the directory do not exist.
    agent.boot("sleep 30").await?;
    for (call, result) in gated_calls(&mut agent.client).await {
        if let Err(status) = result {
            assert_ne!(status.code(), Code::FailedPrecondition, "{call}");
            assert!(details::detail::<NotBooted>(&status).is_none(), "{call}");
        }
    }
    Ok(())
}

#[tokio::test]
async fn the_signal_exec_output_and_file_calls_are_refused_after_a_failed_boot() -> Result {
    let mut agent = Agent::start_with(Setup {
        fail_network: true,
        ..Setup::default()
    })
    .await?;
    agent.boot("sleep 30").await.unwrap_err();

    let calls = gated_calls(&mut agent.client).await;

    assert_refused_as_not_booted(calls, BootState::Failed)
}

#[tokio::test]
async fn a_description_that_is_not_valid_is_refused_and_the_agent_still_waits() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("sleep 30");
    request.image_uuid = "not-a-uuid".into();

    let refused = agent.client.boot(request).await.unwrap_err();

    assert_eq!(refused.code(), Code::InvalidArgument);
    assert_eq!(agent.log.count("find"), 0);
    // A description that does not parse is not a boot step that failed: no detail.
    assert!(details::detail_types(&refused).is_empty());
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
async fn a_boot_with_a_time_sets_the_clock_once_before_the_first_boot_step() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("sleep 30");
    request.host_unix_nanos = 1_700_000_000_123_456_789;

    agent.client.boot(request).await?;

    assert_eq!(
        *agent.clock.set.lock().unwrap(),
        [1_700_000_000_123_456_789]
    );
    let entries = agent.log.entries();
    let clock = entries
        .iter()
        .position(|e| e == "clock")
        .ok_or("no clock entry")?;
    let first_step = entries
        .iter()
        .position(|e| e.starts_with("find "))
        .ok_or("no boot step")?;
    assert!(clock < first_step, "{entries:?}");
    Ok(())
}

#[tokio::test]
async fn a_boot_whose_clock_step_fails_is_named_and_stops_before_every_other_step() -> Result {
    let mut agent = Agent::start_with(Setup {
        fail_clock: true,
        ..Setup::default()
    })
    .await?;
    let mut request = agent.boot_request("sleep 30");
    request.host_unix_nanos = 1_700_000_000_123_456_789;

    let failure = agent.client.boot(request).await.unwrap_err();

    assert_eq!(failure.code(), Code::Internal);
    let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
    assert_eq!(failed.step(), WireStep::Clock);
    assert_eq!(failed.detail, "injected failure");
    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: boot failed step=set the wall clock detail=injected failure"]
    );
    assert_eq!(agent.log.count("clock"), 1);
    for later in [
        "find",
        "mount_image",
        "mount_root_disk",
        "overlay",
        "switch_root",
        "network",
    ] {
        assert_eq!(
            agent.log.count(later),
            0,
            "{later} ran after the clock failed"
        );
    }
    // The control: the same agent with no time never touches the clock, and boots.
    let mut untimed = Agent::start_with(Setup {
        fail_clock: true,
        ..Setup::default()
    })
    .await?;
    untimed.boot("sleep 30").await?;
    assert_eq!(untimed.log.count("find"), 2);
    Ok(())
}

#[tokio::test]
async fn a_boot_with_no_time_leaves_the_clock_alone() -> Result {
    let mut agent = Agent::start().await?;
    // The control: the same request with a time sets it, so the log can show a set.
    let mut timed = agent.boot_request("sleep 30");
    timed.host_unix_nanos = 5;
    let mut other = Agent::start().await?;

    agent.boot("sleep 30").await?;
    other.client.boot(timed).await?;

    assert!(agent.clock.set.lock().unwrap().is_empty());
    assert_eq!(*other.clock.set.lock().unwrap(), [5]);
    Ok(())
}

#[tokio::test]
async fn a_negative_time_is_refused_and_nothing_is_set() -> Result {
    let mut agent = Agent::start().await?;
    let mut request = agent.boot_request("sleep 30");
    request.host_unix_nanos = -1;

    let refused = agent.client.boot(request).await.unwrap_err();

    assert_eq!(refused.code(), Code::InvalidArgument);
    assert!(agent.clock.set.lock().unwrap().is_empty());
    assert_eq!(agent.log.count("find"), 0);
    // The control: the agent still waits, and the same description with a time boots it.
    let mut valid = agent.boot_request("sleep 30");
    valid.host_unix_nanos = 1;
    agent.client.boot(valid).await?;
    assert_eq!(*agent.clock.set.lock().unwrap(), [1]);
    Ok(())
}

#[tokio::test]
async fn a_repeated_boot_with_another_time_succeeds_and_does_not_set_the_clock_again() -> Result {
    let mut agent = Agent::start().await?;
    let mut first = agent.boot_request("sleep 30");
    first.host_unix_nanos = 100;
    let mut repeat = first.clone();
    repeat.host_unix_nanos = 50;

    agent.client.boot(first).await?;
    agent.client.boot(repeat).await?;

    assert_eq!(*agent.clock.set.lock().unwrap(), [100]);
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

    let before = SystemTime::now();
    fs::write(&flag, "")?;
    let (_, record) = within(read_to_record(&mut stream, None)).await??;
    agent.powered_off().await?;
    let after = SystemTime::now();

    let record = record.ok_or("the stream ended without the record")?;
    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.exit_code, 7);
    let console = agent.console();
    assert_eq!(console.len(), 1);
    let at = line_time(&console[0], "mvmd-guest-agent: entrypoint exited code=7")?;
    assert!(
        before <= at && at <= after,
        "{at:?} is outside {before:?}..{after:?}"
    );
    // The line carries the time of the record the clients read.
    assert_eq!(
        SystemTime::try_from(record.ended_at.ok_or("the record has no ended_at")?)?,
        at
    );
    Ok(())
}

#[tokio::test]
async fn an_entrypoint_killed_by_a_signal_is_reported_with_the_signal() -> Result {
    let mut agent = Agent::start().await?;
    let flag = agent.dir.path().join("kill-now");
    agent
        .boot(&format!(
            "while [ ! -e {} ]; do sleep 0.05; done; kill -KILL $$",
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

    let before = SystemTime::now();
    fs::write(&flag, "")?;
    let (_, record) = within(read_to_record(&mut stream, None)).await??;
    let record = record.ok_or("the stream ended without the record")?;
    agent.powered_off().await?;
    let after = SystemTime::now();

    assert_eq!(record.term_signal, 9);
    let console = agent.console();
    assert_eq!(console.len(), 1);
    let at = line_time(&console[0], "mvmd-guest-agent: entrypoint exited signal=9")?;
    assert!(
        before <= at && at <= after,
        "{at:?} is outside {before:?}..{after:?}"
    );
    Ok(())
}

#[tokio::test]
async fn an_entrypoint_the_agent_lost_is_reported_as_ended_with_the_reason() -> Result {
    let mut agent = Agent::start().await?;
    let pid_file = agent.dir.path().join("entrypoint-pid");
    agent
        .boot(&format!("echo $$ > {}; exec sleep 30", pid_file.display()))
        .await?;
    eventually("the entrypoint's process ID", || {
        fs::read_to_string(&pid_file).is_ok_and(|text| text.ends_with('\n'))
    })
    .await?;
    let pid = Pid::from_raw(fs::read_to_string(&pid_file)?.trim().parse()?);

    // Another waiter takes the entrypoint's status, so the agent's own wait fails.
    let thief = std::thread::spawn(move || waitpid(pid, None));
    tokio::time::sleep(Duration::from_millis(200)).await;
    kill(pid, Signal::SIGKILL)?;
    thief.join().map_err(|_| "the waiting thread panicked")??;
    agent.powered_off().await?;

    let console = agent.console();
    assert_eq!(console.len(), 1, "{console:?}");
    assert!(
        console[0]
            .starts_with("mvmd-guest-agent: entrypoint ended reason=the agent lost the command: "),
        "{console:?}"
    );
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
    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: terminated before the entrypoint ran".to_owned()]
    );
    Ok(())
}

#[tokio::test]
async fn a_terminate_call_forwards_sigterm_to_the_running_entrypoint() -> Result {
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
    // The control: a running entrypoint with no request is neither signalled nor powered off.
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!agent.is_powered_off());

    agent.client.terminate(TerminateRequest {}).await?;
    let (output, record) = within(read_to_record(&mut stream, None)).await??;
    agent.powered_off().await?;

    let record = record.ok_or("no record")?;
    assert_eq!(record.status(), ExecStatus::Exited);
    assert_eq!(record.exit_code, 5);
    assert!(output.contains("got-term"), "{output}");
    assert!(!output.contains("got-int"), "{output}");
    Ok(())
}

#[tokio::test]
async fn a_terminate_call_before_the_entrypoint_runs_powers_the_machine_off() -> Result {
    let mut agent = Agent::start().await?;
    // The control: an agent still waiting for its description serves calls and does not power
    // off by itself.
    agent.client.health(HealthRequest {}).await?;
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!agent.is_powered_off());

    agent.client.terminate(TerminateRequest {}).await?;

    agent.powered_off().await?;
    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: terminated before the entrypoint ran".to_owned()]
    );
    Ok(())
}

#[tokio::test]
async fn a_disk_is_found_and_released_before_the_guest_has_booted() -> Result {
    let mut agent = Agent::start().await?;

    let found = agent
        .client
        .find_disk(FindDiskRequest {
            disk_id: "data".into(),
        })
        .await?
        .into_inner();
    agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await?;

    assert_eq!(
        (found.device.as_str(), found.size_bytes, found.read_only),
        ("vdb", 1 << 20, true)
    );
    assert_eq!(agent.log.count("disk find data"), 1);
    assert_eq!(agent.log.count("disk release data"), 1);
    Ok(())
}

#[tokio::test]
async fn a_release_of_a_disk_in_use_is_refused_with_the_disk_named() -> Result {
    let mut agent = Agent::start().await?;
    agent
        .client
        .find_disk(FindDiskRequest {
            disk_id: "data".into(),
        })
        .await?;
    agent.disks.in_use.store(true, Ordering::SeqCst);

    let refusal = agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await
        .unwrap_err();

    assert_eq!(refusal.code(), Code::FailedPrecondition);
    let in_use: DiskInUse = details::detail(&refusal).ok_or("no DiskInUse detail")?;
    assert_eq!(in_use.disk_id, "data");
    // The control: once nothing uses it, the same call releases it.
    agent.disks.in_use.store(false, Ordering::SeqCst);
    agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await?;
    Ok(())
}

#[tokio::test]
async fn a_disk_the_agent_has_no_record_of_is_not_found_and_a_failed_find_is_not_ok() -> Result {
    let mut agent = Agent::start().await?;

    let unknown = agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "never".into(),
        })
        .await
        .unwrap_err();
    let unnamed = agent
        .client
        .find_disk(FindDiskRequest {
            disk_id: String::new(),
        })
        .await
        .unwrap_err();
    agent.disks.no_device.store(true, Ordering::SeqCst);
    let failed = agent
        .client
        .find_disk(FindDiskRequest {
            disk_id: "data".into(),
        })
        .await
        .unwrap_err();

    assert_eq!(unknown.code(), Code::NotFound);
    assert_eq!(unnamed.code(), Code::InvalidArgument);
    assert_eq!(failed.code(), Code::Internal);
    // Only the call that named a disk reached the disks.
    assert_eq!(agent.log.count("disk find "), 1);
    Ok(())
}

/// A fake `/sys` and `/dev` with the root disk `vda` on the bus, for the agent's own `LinuxDisks`.
struct FakeBus {
    dir: TempDir,
}

impl FakeBus {
    fn new() -> Result<Self> {
        let bus = Self {
            dir: tempfile::tempdir()?,
        };
        fs::create_dir_all(bus.sys().join("class/block"))?;
        fs::create_dir_all(bus.sys().join("bus/pci/devices"))?;
        fs::write(bus.sys().join("bus/pci/rescan"), "")?;
        fs::create_dir_all(bus.dir.path().join("dev"))?;
        bus.plug("vda", "0000:00:04.0")?;
        Ok(bus)
    }

    fn sys(&self) -> PathBuf {
        self.dir.path().join("sys")
    }

    fn remove_file(&self, address: &str) -> PathBuf {
        self.sys()
            .join("bus/pci/devices")
            .join(address)
            .join("remove")
    }

    /// A virtio block device at a PCI address, as the kernel lays it out.
    fn plug(&self, name: &str, address: &str) -> Result {
        let real = self
            .sys()
            .join("devices/pci0000:00")
            .join(address)
            .join("virtio1/block")
            .join(name);
        fs::create_dir_all(&real)?;
        fs::write(real.join("size"), "2048\n")?;
        fs::write(real.join("ro"), "0\n")?;
        fs::create_dir_all(self.sys().join("bus/pci/devices").join(address))?;
        fs::write(self.remove_file(address), "")?;
        fs::write(self.dir.path().join("dev").join(name), "")?;
        std::os::unix::fs::symlink(&real, self.sys().join("class/block").join(name))?;
        Ok(())
    }

    /// The kernel drops the block device `name` once the PCI device at `address` is removed.
    fn unplug_on_remove(
        &self,
        address: &'static str,
        name: &'static str,
    ) -> std::thread::JoinHandle<()> {
        let (remove, device) = (
            self.remove_file(address),
            self.sys().join("class/block").join(name),
        );
        std::thread::spawn(move || {
            while fs::read_to_string(&remove).unwrap_or_default() != "1" {
                std::thread::sleep(Duration::from_millis(10));
            }
            let _ = fs::remove_file(device);
        })
    }
}

#[tokio::test]
async fn disks_found_before_boot_stay_recorded_through_it_and_each_is_released_by_its_id() -> Result
{
    let bus = FakeBus::new()?;
    let disks = guest_agent::LinuxDisks::at(
        bus.sys(),
        bus.dir.path().join("dev"),
        Duration::from_secs(2),
    );
    let mut agent = Agent::start_with(Setup {
        disks: Some(Arc::new(disks)),
        ..Setup::default()
    })
    .await?;
    let wanted = [
        ("one", "vdb", "0000:00:05.0"),
        ("two", "vdc", "0000:00:06.0"),
    ];
    // The agent lists the boot's devices before it listens, so no device is plugged before this.
    agent.client.health(HealthRequest {}).await?;

    for (id, name, address) in wanted {
        bus.plug(name, address)?;
        let found = agent
            .client
            .find_disk(FindDiskRequest { disk_id: id.into() })
            .await?
            .into_inner();
        assert_eq!(found.device, name, "{id}");
    }

    // The entrypoint runs until told to end, so the agent serves while the records are read back.
    let flag = agent.dir.path().join("release-done");
    agent
        .boot(&format!(
            "while [ ! -e {} ]; do sleep 0.05; done",
            flag.display()
        ))
        .await?;
    fs::write(bus.sys().join("bus/pci/rescan"), "untouched")?;
    for (id, name, _) in wanted {
        let again = agent
            .client
            .find_disk(FindDiskRequest { disk_id: id.into() })
            .await?
            .into_inner();
        assert_eq!(again.device, name, "{id} after Boot");
    }
    // A record answers without a rescan, so these answers came from the records.
    assert_eq!(
        fs::read_to_string(bus.sys().join("bus/pci/rescan"))?,
        "untouched"
    );
    for (id, name, address) in wanted {
        let watcher = bus.unplug_on_remove(address, name);
        agent
            .client
            .release_disk(ReleaseDiskRequest { disk_id: id.into() })
            .await?;
        watcher.join().map_err(|_| "watcher")?;
        assert_eq!(fs::read_to_string(bus.remove_file(address))?, "1", "{id}");
    }
    fs::write(&flag, "")?;
    Ok(())
}

#[tokio::test]
async fn the_vcpus_are_brought_online_and_reported_before_the_guest_has_booted() -> Result {
    let mut agent = Agent::start().await?;

    let reported = agent
        .client
        .online_vcpus(OnlineVcpusRequest { vcpus: 2 })
        .await?
        .into_inner();

    assert_eq!((reported.vcpus, reported.memory_bytes), (2, 1 << 20));
    assert_eq!(agent.log.count("resources online_vcpus 2"), 1);
    Ok(())
}

#[tokio::test]
async fn a_target_of_zero_is_refused_and_a_target_of_one_is_accepted() -> Result {
    let mut agent = Agent::start().await?;

    let refused = agent
        .client
        .online_vcpus(OnlineVcpusRequest { vcpus: 0 })
        .await
        .unwrap_err();

    assert_eq!(refused.code(), Code::InvalidArgument);
    // The refused call never reached the resources.
    assert_eq!(agent.log.count("resources online_vcpus "), 0);
    // The control: a target of 1 is accepted and reaches them with the target.
    agent
        .client
        .online_vcpus(OnlineVcpusRequest { vcpus: 1 })
        .await?;
    assert_eq!(agent.log.count("resources online_vcpus 1"), 1);
    Ok(())
}

#[tokio::test]
async fn a_cpu_that_does_not_come_online_fails_the_call_with_its_name() -> Result {
    let mut agent = Agent::start().await?;
    agent.resources.failing.store(true, Ordering::SeqCst);

    let failure = agent
        .client
        .online_vcpus(OnlineVcpusRequest { vcpus: 2 })
        .await
        .unwrap_err();

    assert_eq!(failure.code(), Code::Internal);
    assert!(failure.message().contains("cpu3"), "{}", failure.message());
    // The control: once the kernel accepts the writes, the same call answers.
    agent.resources.failing.store(false, Ordering::SeqCst);
    agent
        .client
        .online_vcpus(OnlineVcpusRequest { vcpus: 2 })
        .await?;
    Ok(())
}

#[tokio::test]
async fn a_power_off_that_fails_falls_back_to_a_restart_and_the_agent_never_returns() -> Result {
    let agent = Agent::start_with(Setup {
        fail_power: true,
        ..Setup::default()
    })
    .await?;
    agent.terminate.send(Signal::SIGTERM).await?;

    eventually("the restart", || agent.log.count("restart") > 0).await?;

    let log = agent.log.entries();
    let power_off = log
        .iter()
        .position(|e| e == "power_off")
        .ok_or("no power-off")?;
    let restart = log
        .iter()
        .position(|e| e == "restart")
        .ok_or("no restart")?;
    assert!(power_off < restart);
    assert!(
        agent
            .console()
            .iter()
            .any(|line| line == "mvmd-guest-agent: failed to power off: injected failure")
    );
    // Both calls have returned, so both have failed; the agent still does not return.
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(!agent.run.is_finished());
    Ok(())
}

/// Ends an agent that has not booted, by a termination request, and returns what it called to
/// end the Machine and what it wrote to the console.
async fn ended_by(
    setup: Setup,
) -> std::result::Result<(Vec<String>, Vec<String>), Box<dyn std::error::Error>> {
    let mut agent = Agent::start_with(setup).await?;
    agent.terminate.send(Signal::SIGTERM).await?;
    agent.powered_off().await?;
    let calls = agent
        .log
        .entries()
        .into_iter()
        .filter(|entry| entry == "power_off" || entry == "restart")
        .collect();
    Ok((calls, agent.console()))
}

#[tokio::test]
async fn the_machine_ends_by_the_method_the_command_line_names() -> Result {
    let (calls, console) = ended_by(Setup {
        command_line: Some("console=ttyS0 reboot=k mvmd.end=reset"),
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["restart"]);
    assert_eq!(
        console,
        ["mvmd-guest-agent: terminated before the entrypoint ran".to_owned()]
    );

    // The control: the other value ends it the other way.
    let (calls, console) = ended_by(Setup {
        command_line: Some("console=ttyS0 reboot=k mvmd.end=poweroff"),
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["power_off"]);
    assert_eq!(
        console,
        ["mvmd-guest-agent: terminated before the entrypoint ran".to_owned()]
    );
    Ok(())
}

#[tokio::test]
async fn an_absent_parameter_is_said_on_the_console_and_the_machine_powers_off() -> Result {
    let (calls, console) = ended_by(Setup {
        command_line: Some("console=ttyS0 reboot=k"),
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["power_off"]);
    assert_eq!(
        console,
        [
            "mvmd-guest-agent: the kernel command line has no mvmd.end; ending by power off"
                .to_owned(),
            "mvmd-guest-agent: terminated before the entrypoint ran".to_owned(),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn an_unknown_value_is_said_on_the_console_and_the_machine_powers_off() -> Result {
    let (calls, console) = ended_by(Setup {
        command_line: Some("console=ttyS0 mvmd.end=halt"),
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["power_off"]);
    assert_eq!(
        console,
        [
            "mvmd-guest-agent: mvmd.end=\"halt\" is neither poweroff nor reset; ending by power off"
                .to_owned(),
            "mvmd-guest-agent: terminated before the entrypoint ran".to_owned(),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn an_unreadable_command_line_is_said_on_the_console_and_the_machine_powers_off() -> Result {
    let (calls, console) = ended_by(Setup {
        unreadable_command_line: true,
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["power_off"]);
    assert_eq!(
        console,
        [
            "mvmd-guest-agent: failed to read the kernel command line: injected failure".to_owned(),
            "mvmd-guest-agent: terminated before the entrypoint ran".to_owned(),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn a_reset_that_fails_falls_back_to_a_power_off() -> Result {
    let (calls, console) = ended_by(Setup {
        command_line: Some("mvmd.end=reset"),
        fail_restart: true,
        ..Setup::default()
    })
    .await?;
    assert_eq!(calls, ["restart", "power_off"]);
    assert!(
        console.contains(&"mvmd-guest-agent: failed to restart: injected failure".to_owned()),
        "{console:?}"
    );
    Ok(())
}

#[tokio::test]
async fn the_entrypoints_end_resets_the_machine_when_the_command_line_says_reset() -> Result {
    let mut agent = Agent::start_with(Setup {
        command_line: Some("mvmd.end=reset"),
        ..Setup::default()
    })
    .await?;
    agent.boot("exit 7").await?;

    agent.powered_off().await?;

    assert_eq!(agent.log.count("restart"), 1);
    assert_eq!(agent.log.count("power_off"), 0);
    let console = agent.console();
    assert_eq!(console.len(), 1);
    line_time(&console[0], "mvmd-guest-agent: entrypoint exited code=7")?;
    Ok(())
}

#[tokio::test]
async fn a_failed_prepare_ends_the_machine_and_says_why() -> Result {
    let mut agent = Agent::start_with(Setup {
        fail_mount: Some("prepare"),
        ..Setup::default()
    })
    .await?;

    agent.powered_off().await?;

    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: failed to prepare the guest: injected failure".to_owned()]
    );
    Ok(())
}

#[tokio::test]
async fn a_failed_account_for_the_boots_disks_ends_the_machine_and_never_listens() -> Result {
    let mut agent = Agent::start_with(Setup {
        fail_account: true,
        ..Setup::default()
    })
    .await?;

    agent.powered_off().await?;

    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: failed to account for the boot's disks: injected failure".to_owned()]
    );
    assert!(!agent.listened.load(Ordering::SeqCst));
    // Control: without the failure the agent listens, as the boot-order test also shows.
    let control = Agent::start().await?;
    within(async {
        while !control.listened.load(Ordering::SeqCst) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await?;
    Ok(())
}

#[tokio::test]
async fn a_server_that_stops_by_itself_ends_the_machine_and_says_why() -> Result {
    let mut agent = Agent::start_with(Setup {
        listener_ends: true,
        ..Setup::default()
    })
    .await?;

    agent.powered_off().await?;

    assert_eq!(
        agent.console(),
        ["mvmd-guest-agent: failed to serve the channel: the server ended".to_owned()]
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
