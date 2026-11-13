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
use std::process::Command;
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, SystemTime};

use guest_agent::v1alpha1::guest_service_client::GuestServiceClient;
use guest_agent::v1alpha1::{
    BootFailed, BootRequest, BootState, BootStep as WireStep, CatchUpRequest, DiskInUse,
    ExecRecord, ExecRequest, ExecStatus, File as WireFile, FindDiskRequest, GrowDiskRequest,
    HealthRequest, Ipv4Config, Ipv6Config, ListFilesRequest, NetworkConfig as WireNetworkConfig,
    NotBooted, OnlineVcpusRequest, PatchFileRequest, ReadExecOutputRequest, ReadFileRequest,
    ReleaseDiskRequest, RunConfig, SearchFilesRequest, SignalRequest, TerminateRequest,
    UpdateNetworkRequest, WatchProbesRequest, WatchProbesResponse, WriteFileRequest,
    WriteFileStart, read_exec_output_response, write_file_request,
};
use guest_agent::{
    Accounts, BootFile, BootStep, Clock, Console, DiskDevice, DiskError, DiskMount, Disks, Entropy,
    Filesystem, GuestSize, Identity, KernelCommandLine, Mounts, Network, NetworkConfig, Power,
    Resources, System, Users, Uuid,
};
use hyper_util::rt::TokioIo;
use nix::sys::signal::Signal;
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
    /// The files `place_files` was given, in order.
    placed: Mutex<Vec<BootFile>>,
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

    fn place_files(&self, files: &[BootFile]) -> io::Result<()> {
        let paths: Vec<&str> = files.iter().map(|file| file.path.as_str()).collect();
        self.step("place_files", &paths.join(","))?;
        lock(&self.placed).extend_from_slice(files);
        Ok(())
    }
}

/// Users that read their accounts from the text given, and keep the identity each command was
/// started with: the test cannot change its own user, so the identity is what it can see.
struct TestUsers {
    log: Log,
    passwd: &'static str,
    group: &'static str,
    ran_as: Mutex<Vec<Identity>>,
}

impl Users for TestUsers {
    fn accounts(&self) -> io::Result<Accounts> {
        self.log.push("users accounts");
        Ok(Accounts {
            passwd: self.passwd.into(),
            group: self.group.into(),
        })
    }

    fn run_as(&self, _command: &mut Command, identity: &Identity) {
        self.log.push("users run_as");
        lock(&self.ran_as).push(identity.clone());
    }
}

/// The accounts of the image the user tests boot.
const PASSWD: &str = "root:x:0:0:root:/root:/bin/sh\napp:x:1000:2000:app:/home/app:/bin/sh\n";
const GROUP: &str = "root:x:0:\nstaff:x:2000:app\nwheel:x:10:app\n";

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

/// Disks that remember what was found and mounted, refuse to release one that is in use or whose
/// mount is busy, and log each call.
struct TestDisks {
    log: Log,
    found: Mutex<std::collections::HashSet<String>>,
    /// The mounts asked for and not made yet, by disk ID.
    wanted: Mutex<Vec<(String, DiskMount)>>,
    /// The mounts made, by disk ID.
    mounted: Mutex<Vec<(String, DiskMount)>>,
    /// Whether the boot has mounted the disks, from which `find` mounts at once.
    mounting: AtomicBool,
    in_use: AtomicBool,
    busy: AtomicBool,
    no_device: AtomicBool,
    fail_account: bool,
    fail_mount: AtomicBool,
}

impl TestDisks {
    fn mount(&self, disk_id: &str, mount: DiskMount) -> std::result::Result<(), DiskError> {
        if self.fail_mount.load(Ordering::SeqCst) {
            return Err(DiskError::Failed(io::Error::other("injected failure")));
        }
        self.log.push(format!(
            "disk mount {disk_id} {} ro={}",
            mount.path, mount.read_only
        ));
        lock(&self.mounted).push((disk_id.to_owned(), mount));
        Ok(())
    }
}

impl Disks for TestDisks {
    fn account_for_boot(&self) -> io::Result<()> {
        self.log.push("disk account_for_boot");
        if self.fail_account {
            return Err(io::Error::other("injected failure"));
        }
        Ok(())
    }

    fn find(
        &self,
        disk_id: &str,
        mount: Option<&DiskMount>,
    ) -> std::result::Result<DiskDevice, DiskError> {
        self.log.push(format!("disk find {disk_id}"));
        if self.no_device.load(Ordering::SeqCst) {
            return Err(DiskError::Failed(io::Error::other("no new device")));
        }
        let first = lock(&self.found).insert(disk_id.to_owned());
        if let (true, Some(mount)) = (first, mount) {
            if self.mounting.load(Ordering::SeqCst) {
                self.mount(disk_id, mount.clone())?;
            } else {
                lock(&self.wanted).push((disk_id.to_owned(), mount.clone()));
            }
        }
        Ok(DiskDevice {
            name: "vdb".into(),
            size_bytes: 1 << 20,
            read_only: true,
        })
    }

    fn mount_recorded(&self) -> std::result::Result<(), DiskError> {
        self.log.push("disk mount_recorded");
        self.mounting.store(true, Ordering::SeqCst);
        let wanted: Vec<_> = lock(&self.wanted).drain(..).collect();
        for (disk_id, mount) in wanted {
            self.mount(&disk_id, mount)?;
        }
        Ok(())
    }

    fn release(&self, disk_id: &str) -> std::result::Result<(), DiskError> {
        self.log.push(format!("disk release {disk_id}"));
        if !lock(&self.found).contains(disk_id) {
            return Err(DiskError::Unknown);
        }
        let mount = lock(&self.mounted)
            .iter()
            .find(|(id, _)| id == disk_id)
            .map(|(_, mount)| mount.path.clone());
        if let Some(path) = mount {
            if self.busy.load(Ordering::SeqCst) {
                return Err(DiskError::MountBusy(path));
            }
            self.log.push(format!("disk unmount {disk_id} {path}"));
            lock(&self.mounted).retain(|(id, _)| id != disk_id);
        }
        if self.in_use.load(Ordering::SeqCst) {
            return Err(DiskError::InUse);
        }
        lock(&self.found).remove(disk_id);
        Ok(())
    }

    fn grow(&self, disk_id: &str) -> std::result::Result<u64, DiskError> {
        self.log.push(format!("disk grow {disk_id}"));
        if !lock(&self.found).contains(disk_id) {
            return Err(DiskError::Unknown);
        }
        Ok(2 << 20)
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
    /// The image's `/etc/passwd` and `/etc/group`; empty when not set.
    passwd: &'static str,
    group: &'static str,
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
    mounts: Arc<TestMounts>,
    users: Arc<TestUsers>,
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
            wanted: Mutex::default(),
            mounted: Mutex::default(),
            mounting: AtomicBool::new(false),
            in_use: AtomicBool::new(false),
            busy: AtomicBool::new(false),
            no_device: AtomicBool::new(false),
            fail_account: setup.fail_account,
            fail_mount: AtomicBool::new(false),
        });
        let resources = Arc::new(TestResources {
            log: log.clone(),
            failing: AtomicBool::new(false),
        });
        let mounts = Arc::new(TestMounts {
            log: log.clone(),
            fail: setup.fail_mount,
            overlay_takes: setup.overlay_takes,
            placed: Mutex::default(),
        });
        let users = Arc::new(TestUsers {
            log: log.clone(),
            passwd: setup.passwd,
            group: setup.group,
            ran_as: Mutex::default(),
        });
        let system = System {
            mounts: mounts.clone(),
            users: users.clone(),
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
            mounts,
            users,
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
                ..RunConfig::default()
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
            ..ReadExecOutputRequest::default()
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
            "disk mount_recorded".to_owned(),
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
                    ..ReadExecOutputRequest::default()
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
        (
            "watch_probes",
            client.watch_probes(WatchProbesRequest {}).await.map(|_| ()),
        ),
    ]
}

/// Asserts that every gated call was refused as not booted, in `state`.
fn assert_refused_as_not_booted(calls: Vec<(&'static str, Refusal)>, state: BootState) -> Result {
    assert_eq!(calls.len(), 7);
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
async fn a_tail_of_the_entrypoint_starts_before_the_end_of_its_output() -> Result {
    let mut agent = Agent::start().await?;
    let flag = agent.dir.path().join("exit-now");
    agent
        .boot(&format!(
            "printf abcdef; while [ ! -e {} ]; do sleep 0.05; done; printf ghi",
            flag.display()
        ))
        .await?;
    let mut held = agent
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: guest_agent::ENTRYPOINT.into(),
            ..ReadExecOutputRequest::default()
        })
        .await?
        .into_inner();
    within(read_to_record(&mut held, Some("abcdef"))).await??;

    let mut stream = agent
        .client
        .read_exec_output(ReadExecOutputRequest {
            exec_id: guest_agent::ENTRYPOINT.into(),
            tail_bytes: 3,
            ..ReadExecOutputRequest::default()
        })
        .await?
        .into_inner();
    fs::write(&flag, "")?;
    let (output, record) = within(read_to_record(&mut stream, None)).await??;

    assert_eq!(output, "defghi");
    assert_eq!(
        record
            .ok_or("the stream ended without the record")?
            .status(),
        ExecStatus::Exited
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
            ..ReadExecOutputRequest::default()
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
            ..ReadExecOutputRequest::default()
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
                ..ReadExecOutputRequest::default()
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
            ..ReadExecOutputRequest::default()
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
            ..Default::default()
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
            ..Default::default()
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
            ..Default::default()
        })
        .await
        .unwrap_err();
    agent.disks.no_device.store(true, Ordering::SeqCst);
    let failed = agent
        .client
        .find_disk(FindDiskRequest {
            disk_id: "data".into(),
            ..Default::default()
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
            .find_disk(FindDiskRequest {
                disk_id: id.into(),
                ..Default::default()
            })
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
            .find_disk(FindDiskRequest {
                disk_id: id.into(),
                ..Default::default()
            })
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
            ..ReadExecOutputRequest::default()
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

/// The setup of an image with the accounts above.
fn with_accounts() -> Setup {
    Setup {
        passwd: PASSWD,
        group: GROUP,
        ..Setup::default()
    }
}

/// A boot request whose entrypoint runs as `user` and writes `$HOME` to `home` in the shared
/// directory, then stays.
fn user_request(agent: &Agent, user: &str) -> BootRequest {
    let mut request = agent.boot_request(r#"echo "${HOME-unset}" > home; sleep 30"#);
    if let Some(run) = request.run.as_mut() {
        run.user = user.into();
    }
    request
}

fn identity(uid: u32, gid: u32, groups: &[u32], home: Option<&str>) -> Identity {
    Identity {
        uid,
        gid,
        groups: groups.to_vec(),
        home: home.map(str::to_owned),
    }
}

/// The `$HOME` the entrypoint wrote, once it has. The shell creates `home` empty before `echo`
/// writes to it, so the wait is for the line's newline, not for the file.
async fn entrypoint_home(agent: &Agent) -> Result<String> {
    let home = agent.dir.path().join("home");
    eventually("the entrypoint to write HOME", || {
        fs::read_to_string(&home).is_ok_and(|text| text.ends_with('\n'))
    })
    .await?;
    Ok(fs::read_to_string(home)?.trim_end().to_owned())
}

#[tokio::test]
async fn a_named_user_runs_the_entrypoint_with_its_ids_groups_and_home() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    let request = user_request(&agent, "app");

    agent.client.boot(request).await?;

    // The primary group is the passwd one, and the supplementary groups are the ones that list
    // the user, the primary group among them.
    assert_eq!(
        *lock(&agent.users.ran_as),
        [identity(1000, 2000, &[2000, 10], Some("/home/app"))]
    );
    assert_eq!(entrypoint_home(&agent).await?, "/home/app");
    Ok(())
}

#[tokio::test]
async fn a_uid_with_a_gid_runs_as_exactly_those_and_takes_no_groups() -> Result {
    for (user, expected, home) in [
        (
            "1000:3000",
            identity(1000, 3000, &[], Some("/home/app")),
            "/home/app",
        ),
        ("4242:7", identity(4242, 7, &[], None), "unset"),
        (
            "app:7",
            identity(1000, 7, &[2000, 10], Some("/home/app")),
            "/home/app",
        ),
        (
            "1000:wheel",
            identity(1000, 10, &[], Some("/home/app")),
            "/home/app",
        ),
        // A uid with no passwd entry has group 0.
        ("4242", identity(4242, 0, &[], None), "unset"),
    ] {
        let mut agent = Agent::start_with(with_accounts()).await?;
        let request = user_request(&agent, user);

        agent.client.boot(request).await?;

        assert_eq!(*lock(&agent.users.ran_as), [expected], "{user}");
        assert_eq!(entrypoint_home(&agent).await?, home, "{user}");
    }
    Ok(())
}

#[tokio::test]
async fn a_home_in_the_environment_is_kept() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    let mut request = user_request(&agent, "app");
    if let Some(run) = request.run.as_mut() {
        run.env.insert("HOME".into(), "/set/by/mvmd".into());
    }

    agent.client.boot(request).await?;

    assert_eq!(entrypoint_home(&agent).await?, "/set/by/mvmd");
    Ok(())
}

#[tokio::test]
async fn an_empty_user_runs_the_entrypoint_as_root_and_looks_nothing_up() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    let request = user_request(&agent, "");

    agent.client.boot(request).await?;

    assert!(lock(&agent.users.ran_as).is_empty());
    assert_eq!(agent.log.count("users"), 0);
    // Nothing sets HOME for root: the description's environment is the whole environment.
    assert_eq!(entrypoint_home(&agent).await?, "unset");
    Ok(())
}

#[tokio::test]
async fn a_name_that_does_not_resolve_fails_the_boot_at_apply_the_user() -> Result {
    // The control: the same accounts resolve a name that is in them.
    let mut control = Agent::start_with(with_accounts()).await?;
    let request = user_request(&control, "app");
    control.client.boot(request).await?;

    for user in ["ghost", "app:ghost", "ghost:2000"] {
        let mut agent = Agent::start_with(with_accounts()).await?;
        let request = user_request(&agent, user);

        let failure = agent.client.boot(request).await.unwrap_err();

        assert_eq!(failure.code(), Code::Internal, "{user}");
        let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
        assert_eq!(failed.step(), WireStep::ApplyUser, "{user}");
        assert!(failed.detail.contains("ghost"), "{user}: {}", failed.detail);
        assert!(
            failure.message().contains("boot step apply the user"),
            "{user}: {}",
            failure.message()
        );
        // The entrypoint never started.
        assert!(lock(&agent.users.ran_as).is_empty(), "{user}");
        assert!(!agent.dir.path().join("home").exists(), "{user}");
    }
    Ok(())
}

#[tokio::test]
async fn a_user_that_is_not_in_the_form_is_refused_at_the_parse() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    for user in [":", "app:", ":2000", "a:b:c", "99999999999"] {
        let request = user_request(&agent, user);

        let refused = agent.client.boot(request).await.unwrap_err();

        assert_eq!(refused.code(), Code::InvalidArgument, "{user}");
    }
    assert_eq!(agent.log.count("find"), 0);
    // The control: the agent still waits, and a valid user boots it.
    let request = user_request(&agent, "app:staff");
    agent.client.boot(request).await?;
    Ok(())
}

#[tokio::test]
async fn an_exec_with_a_user_runs_as_it_and_an_empty_one_runs_as_root() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    agent.boot("sleep 30").await?;
    assert!(lock(&agent.users.ran_as).is_empty());

    for (id, user) in [("named", "app"), ("numbers", "1000:3000")] {
        let mut request = exec_request(id, "true");
        request.user = user.into();
        let record = agent.client.exec(request).await?.into_inner().record;
        assert_eq!(record.ok_or("no record")?.status(), ExecStatus::Started);
    }
    let mut root = exec_request("root", "true");
    root.user = String::new();
    agent.client.exec(root).await?;

    // The empty user added no identity; the other two added theirs, in order.
    assert_eq!(
        *lock(&agent.users.ran_as),
        [
            identity(1000, 2000, &[2000, 10], Some("/home/app")),
            identity(1000, 3000, &[], Some("/home/app")),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn an_exec_with_a_name_that_does_not_resolve_is_not_found_and_does_not_start() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    agent.boot("sleep 30").await?;
    let mut request = exec_request("ghosted", "true");
    request.user = "ghost".into();

    let refused = agent.client.exec(request.clone()).await.unwrap_err();

    assert_eq!(refused.code(), Code::NotFound);
    assert!(refused.message().contains("ghost"), "{}", refused.message());
    // The exec ID was not taken: the same ID starts with a user that resolves.
    request.user = "app".into();
    let record = agent.client.exec(request).await?.into_inner().record;
    assert_eq!(record.ok_or("no record")?.status(), ExecStatus::Started);
    // A name that is not in the form is invalid, not missing.
    let mut malformed = exec_request("malformed", "true");
    malformed.user = "a:b:c".into();
    let invalid = agent.client.exec(malformed).await.unwrap_err();
    assert_eq!(invalid.code(), Code::InvalidArgument);
    Ok(())
}

fn wire_file(path: &str, content: &[u8]) -> WireFile {
    WireFile {
        path: path.into(),
        content: content.to_vec(),
        mode: 0o640,
        uid: 1000,
        gid: 2000,
    }
}

fn files_request(agent: &Agent, files: Vec<WireFile>) -> BootRequest {
    let mut request = agent.boot_request("sleep 30");
    if let Some(run) = request.run.as_mut() {
        run.files = files;
    }
    request
}

fn placed(path: &str, content: &[u8]) -> BootFile {
    BootFile {
        path: path.into(),
        content: content.to_vec(),
        mode: 0o640,
        uid: 1000,
        gid: 2000,
    }
}

#[tokio::test]
async fn files_are_placed_after_the_network_and_before_the_user_and_the_entrypoint() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    let mut request = files_request(
        &agent,
        vec![
            wire_file("/etc/app/config", b"one"),
            wire_file("/var/secret", b"two"),
        ],
    );
    if let Some(run) = request.run.as_mut() {
        run.user = "app".into();
    }

    agent.client.boot(request).await?;

    let steps: Vec<String> = agent
        .log
        .entries()
        .into_iter()
        .skip_while(|entry| entry != "switch_root")
        .collect();
    assert_eq!(
        steps[..6],
        [
            "switch_root".to_owned(),
            "network".to_owned(),
            "place_files /etc/app/config,/var/secret".to_owned(),
            "disk mount_recorded".to_owned(),
            "users accounts".to_owned(),
            "users run_as".to_owned(),
        ]
    );
    // The seam is handed each file whole, with its mode and owner, in the order sent.
    assert_eq!(
        *lock(&agent.mounts.placed),
        [
            placed("/etc/app/config", b"one"),
            placed("/var/secret", b"two")
        ]
    );
    Ok(())
}

#[tokio::test]
async fn a_boot_with_no_files_places_none() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    assert_eq!(agent.log.count("place_files"), 0);
    assert!(lock(&agent.mounts.placed).is_empty());
    Ok(())
}

#[tokio::test]
async fn a_repeated_boot_with_other_files_is_refused_and_with_the_same_files_places_nothing_again()
-> Result {
    let mut agent = Agent::start().await?;
    let files = || vec![wire_file("/etc/a", b"one")];
    agent.client.boot(files_request(&agent, files())).await?;
    let steps = agent.log.entries();

    agent.client.boot(files_request(&agent, files())).await?;
    let other_content = agent
        .client
        .boot(files_request(&agent, vec![wire_file("/etc/a", b"two")]))
        .await
        .unwrap_err();
    let other_mode = {
        let mut file = wire_file("/etc/a", b"one");
        file.mode = 0o600;
        agent
            .client
            .boot(files_request(&agent, vec![file]))
            .await
            .unwrap_err()
    };
    let none = agent
        .client
        .boot(files_request(&agent, Vec::new()))
        .await
        .unwrap_err();

    assert_eq!(agent.log.entries(), steps);
    for refused in [other_content, other_mode, none] {
        assert_eq!(refused.code(), Code::FailedPrecondition);
    }
    Ok(())
}

#[tokio::test]
async fn a_failed_place_the_files_step_is_named_and_the_entrypoint_never_runs() -> Result {
    let mut agent = Agent::start_with(Setup {
        fail_mount: Some("place_files"),
        ..with_accounts()
    })
    .await?;
    let request = files_request(&agent, vec![wire_file("/etc/a", b"one")]);

    let failure = agent.client.boot(request).await.unwrap_err();

    assert_eq!(failure.code(), Code::Internal);
    let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
    assert_eq!(failed.step(), WireStep::PlaceFiles);
    assert_eq!(failed.detail, "injected failure");
    assert_eq!(agent.log.count("users"), 0);
    assert!(lock(&agent.mounts.placed).is_empty());
    Ok(())
}

/// A description each of whose files sits on one limit's far side, with what is wrong with it.
fn refused_file_sets() -> Vec<(&'static str, Vec<WireFile>)> {
    let one = |path: &str| vec![wire_file(path, b"x")];
    let many = |count: usize, size: usize| -> Vec<WireFile> {
        (0..count)
            .map(|index| wire_file(&format!("/data/{index}"), &vec![b'x'; size]))
            .collect()
    };
    vec![
        ("a relative path", one("etc/a")),
        ("an empty path", one("")),
        ("the root", one("/")),
        ("a trailing slash", one("/etc/a/")),
        ("an empty part", one("/etc//a")),
        ("a dot part", one("/etc/./a")),
        ("a dot-dot part", one("/etc/../a")),
        ("a path under /proc", one("/proc/a")),
        ("/proc itself", one("/proc")),
        ("a path under /sys", one("/sys/kernel/a")),
        ("a path under /dev", one("/dev/null")),
        ("a path under /run/mvmd", one("/run/mvmd/files/0")),
        ("/run/mvmd itself", one("/run/mvmd")),
        (
            "the same path twice",
            vec![wire_file("/etc/a", b"x"), wire_file("/etc/a", b"y")],
        ),
        ("65 files", many(65, 1)),
        // Together one byte over 1 MiB, each file within it.
        ("more than 1 MiB", {
            let mut files = many(64, 16384);
            files[0].content.push(b'x');
            files
        }),
    ]
}

#[tokio::test]
async fn a_file_set_beyond_a_limit_is_refused_at_the_parse_and_one_within_boots() -> Result {
    let mut agent = Agent::start().await?;
    for (what, files) in refused_file_sets() {
        let request = files_request(&agent, files);

        let refused = agent.client.boot(request).await.unwrap_err();

        assert_eq!(refused.code(), Code::InvalidArgument, "{what}");
        // A description that does not parse is not a boot step that failed: nothing ran.
        assert!(details::detail_types(&refused).is_empty(), "{what}");
    }
    assert_eq!(agent.log.count("find"), 0);
    assert_eq!(agent.log.count("place_files"), 0);

    // The control: on the near side of every limit the same agent boots: 64 files holding
    // exactly 1 MiB, with paths that only look like the reserved directories.
    let mut files: Vec<WireFile> = (0..64)
        .map(|index| wire_file(&format!("/data/{index}"), &[b'x'; 16384]))
        .collect();
    files[0].path = "/procfs/a".into();
    files[1].path = "/run/mvmdx/a".into();
    files[2].path = "/devices/a".into();
    files[3].path = "/sysroot/a".into();
    let request = files_request(&agent, files);
    agent.client.boot(request).await?;
    assert_eq!(lock(&agent.mounts.placed).len(), 64);
    Ok(())
}

/// What the status of a refused or failed boot says, in every place it says anything.
fn everything_said(status: &tonic::Status, console: &[String]) -> Vec<u8> {
    let mut said = status.message().as_bytes().to_vec();
    said.extend_from_slice(status.details());
    said.extend(console.iter().flat_map(|line| line.bytes()));
    said
}

fn contains(haystack: &[u8], needle: &[u8]) -> bool {
    haystack
        .windows(needle.len())
        .any(|window| window == needle)
}

#[tokio::test]
async fn a_refused_or_failed_file_is_named_by_its_path_and_never_by_its_content() -> Result {
    const SECRET: &[u8] = b"hunter2-the-secret";
    // Refused at the parse: for the path, for the mode, for the total size and for the count.
    let mut bad_mode = wire_file("/etc/secret", SECRET);
    bad_mode.mode = 0o20000;
    let mut too_many: Vec<WireFile> = (0..65)
        .map(|index| wire_file(&format!("/etc/{index}"), SECRET))
        .collect();
    too_many[7].path = "/etc/the-seventh".into();
    let mut too_big = vec![wire_file("/etc/big", &vec![b'x'; 1 << 20])];
    too_big.push(wire_file("/etc/secret", SECRET));
    let refused_sets = [
        ("a relative path", vec![wire_file("etc/secret", SECRET)]),
        ("a reserved path", vec![wire_file("/proc/secret", SECRET)]),
        ("a mode beyond the permissions", vec![bad_mode]),
        (
            "a repeated path",
            vec![
                wire_file("/etc/secret", SECRET),
                wire_file("/etc/secret", SECRET),
            ],
        ),
        ("too many files", too_many),
        ("too many bytes", too_big),
    ];
    let mut agent = Agent::start().await?;
    for (what, files) in refused_sets {
        let request = files_request(&agent, files);

        let refused = agent.client.boot(request).await.unwrap_err();

        assert_eq!(refused.code(), Code::InvalidArgument, "{what}");
        assert!(
            !contains(&everything_said(&refused, &agent.console()), SECRET),
            "{what} said the content"
        );
    }
    // The control: a refusal for a path says the path, so the check above can fail.
    let request = files_request(&agent, vec![wire_file("etc/secret", SECRET)]);
    let refused = agent.client.boot(request).await.unwrap_err();
    assert!(
        refused.message().contains("etc/secret"),
        "{}",
        refused.message()
    );

    // Failed at its step, with the seam failing: the status, its details and the console.
    let mut failing = Agent::start_with(Setup {
        fail_mount: Some("place_files"),
        ..Setup::default()
    })
    .await?;
    let request = files_request(&failing, vec![wire_file("/etc/secret", SECRET)]);
    let failure = failing.client.boot(request).await.unwrap_err();
    assert_eq!(failure.code(), Code::Internal);
    let said = everything_said(&failure, &failing.console());
    assert!(!contains(&said, SECRET), "the failed step said the content");
    // The control: what was said is the failure, so there was something to leak into.
    assert!(contains(&said, b"place the files"));
    // The content is what the seam was given; the log of the seam's calls names paths only.
    assert!(
        failing
            .log
            .entries()
            .iter()
            .all(|entry| !contains(entry.as_bytes(), SECRET))
    );
    assert!(
        failing
            .log
            .entries()
            .contains(&"place_files /etc/secret".to_owned())
    );
    Ok(())
}

/// `FindDisk` naming a disk, the path to mount it at and whether the mount is read-only.
fn find_at(disk_id: &str, mount_path: &str, read_only: bool) -> FindDiskRequest {
    FindDiskRequest {
        disk_id: disk_id.into(),
        mount_path: mount_path.into(),
        read_only,
    }
}

#[tokio::test]
async fn a_disk_found_after_the_boot_is_mounted_at_its_path_read_only_when_asked() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    agent
        .client
        .find_disk(find_at("data", "/mnt/data", true))
        .await?;
    agent
        .client
        .find_disk(find_at("scratch", "/mnt/scratch", false))
        .await?;
    // The control: a disk found with no path is recorded and mounted nowhere.
    agent.client.find_disk(find_at("bare", "", false)).await?;

    assert_eq!(
        *lock(&agent.disks.mounted),
        [
            (
                "data".to_owned(),
                DiskMount {
                    path: "/mnt/data".into(),
                    read_only: true
                }
            ),
            (
                "scratch".to_owned(),
                DiskMount {
                    path: "/mnt/scratch".into(),
                    read_only: false
                }
            ),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn a_mount_path_the_rules_refuse_is_refused_before_the_disks_are_asked() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;

    for path in [
        "mnt/data",
        "/",
        "/mnt/../etc",
        "/mnt//data",
        "/mnt/data/",
        "/proc/data",
        "/sys",
        "/dev/disk",
        "/run/mvmd/data",
    ] {
        let refused = agent
            .client
            .find_disk(find_at("data", path, false))
            .await
            .unwrap_err();
        assert_eq!(refused.code(), Code::InvalidArgument, "{path}");
        assert!(refused.message().contains(path), "{path}: {refused:?}");
    }
    assert_eq!(agent.log.count("disk find"), 0);
    assert!(lock(&agent.disks.mounted).is_empty());

    // The control: a path the rules allow, with the same disk, is found and mounted.
    agent
        .client
        .find_disk(find_at("data", "/mnt/data", false))
        .await?;
    assert_eq!(agent.log.count("disk find data"), 1);
    assert_eq!(lock(&agent.disks.mounted).len(), 1);
    Ok(())
}

#[tokio::test]
async fn a_disk_named_at_create_is_mounted_before_the_entrypoint_and_after_the_files() -> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    agent
        .client
        .find_disk(find_at("data", "/mnt/data", true))
        .await?;
    // Found before the boot, it is recorded and not mounted yet.
    assert!(lock(&agent.disks.mounted).is_empty());

    let mut request = files_request(&agent, vec![wire_file("/etc/a", b"one")]);
    if let Some(run) = request.run.as_mut() {
        run.user = "app".into();
    }
    agent.client.boot(request).await?;

    let steps: Vec<String> = agent
        .log
        .entries()
        .into_iter()
        .skip_while(|entry| entry != "place_files /etc/a")
        .collect();
    assert_eq!(
        steps[..4],
        [
            "place_files /etc/a".to_owned(),
            "disk mount_recorded".to_owned(),
            "disk mount data /mnt/data ro=true".to_owned(),
            "users accounts".to_owned(),
        ]
    );
    Ok(())
}

#[tokio::test]
async fn a_failed_mount_of_a_disk_fails_the_boot_at_its_step_and_the_entrypoint_never_runs()
-> Result {
    let mut agent = Agent::start_with(with_accounts()).await?;
    agent
        .client
        .find_disk(find_at("data", "/mnt/data", false))
        .await?;
    agent.disks.fail_mount.store(true, Ordering::SeqCst);

    let failure = agent
        .client
        .boot(user_request(&agent, "app"))
        .await
        .unwrap_err();

    assert_eq!(failure.code(), Code::Internal);
    let failed: BootFailed = details::detail(&failure).ok_or("no BootFailed detail")?;
    assert_eq!(failed.step(), WireStep::MountDisks);
    assert_eq!(agent.log.count("users"), 0);
    Ok(())
}

#[tokio::test]
async fn a_release_unmounts_the_disk_first_and_a_busy_mount_fails_it_naming_the_path() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    agent
        .client
        .find_disk(find_at("data", "/mnt/data", false))
        .await?;
    agent.disks.busy.store(true, Ordering::SeqCst);

    let refusal = agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await
        .unwrap_err();

    assert_eq!(refusal.code(), Code::FailedPrecondition);
    assert!(refusal.message().contains("/mnt/data"), "{refusal:?}");
    let in_use: DiskInUse = details::detail(&refusal).ok_or("no DiskInUse detail")?;
    assert_eq!(in_use.disk_id, "data");
    assert_eq!(lock(&agent.disks.mounted).len(), 1);

    // The control: once the mount is not busy, the same call unmounts it and releases the disk.
    agent.disks.busy.store(false, Ordering::SeqCst);
    agent
        .client
        .release_disk(ReleaseDiskRequest {
            disk_id: "data".into(),
        })
        .await?;
    assert!(lock(&agent.disks.mounted).is_empty());
    assert_eq!(agent.log.count("disk unmount data /mnt/data"), 1);
    Ok(())
}

#[tokio::test]
async fn a_grow_answers_the_new_size_and_a_disk_never_found_is_not_found() -> Result {
    let mut agent = Agent::start().await?;
    agent.boot("sleep 30").await?;
    agent
        .client
        .find_disk(find_at("data", "/mnt/data", false))
        .await?;

    let grown = agent
        .client
        .grow_disk(GrowDiskRequest {
            disk_id: "data".into(),
        })
        .await?
        .into_inner();
    let unknown = agent
        .client
        .grow_disk(GrowDiskRequest {
            disk_id: "never".into(),
        })
        .await
        .unwrap_err();
    let unnamed = agent
        .client
        .grow_disk(GrowDiskRequest {
            disk_id: String::new(),
        })
        .await
        .unwrap_err();

    assert_eq!(grown.size_bytes, 2 << 20);
    assert_eq!(unknown.code(), Code::NotFound);
    assert_eq!(unnamed.code(), Code::InvalidArgument);
    assert_eq!(agent.log.count("disk grow data"), 1);
    Ok(())
}

/// The probes the workload is run with: each kind, the schedule, and the stream of results.
mod probe_tests {
    use std::sync::atomic::AtomicU16;
    use std::time::Instant;

    use guest_agent::v1alpha1::{
        ExecProbe, HttpGetProbe, Probe, ProbeResult, ProbeStatus, TcpProbe, probe,
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    use super::*;

    /// An agent that keeps serving for a minute after the entrypoint ends, so that a stream
    /// ends only because the probes did, not because the server shut down.
    async fn start_patient() -> Result<Agent> {
        Agent::start_with(Setup {
            config: guest_agent::Config {
                shutdown_grace: Duration::from_secs(60),
                ..guest_agent::Config::default()
            },
            ..Setup::default()
        })
        .await
    }

    fn ms(millis: i64) -> Option<prost_types::Duration> {
        Some(prost_types::Duration {
            seconds: millis / 1000,
            nanos: i32::try_from(millis % 1000).unwrap_or(0) * 1_000_000,
        })
    }

    /// A probe that runs every 50 ms and turns on one run, whatever its kind. Its first run is
    /// 300 ms after the entrypoint starts, which leaves time to open a watch while the probe is
    /// still unknown.
    fn probe(name: &str, kind: probe::Kind) -> Probe {
        Probe {
            name: name.into(),
            kind: Some(kind),
            initial_delay: ms(300),
            period: ms(50),
            timeout: ms(2000),
            success_threshold: 1,
            failure_threshold: 1,
        }
    }

    fn sh_probe(name: &str, script: &str) -> Probe {
        probe(
            name,
            probe::Kind::Exec(ExecProbe {
                argv: vec![SH.into(), "-c".into(), script.into()],
            }),
        )
    }

    fn tcp_probe(name: &str, port: u16) -> Probe {
        probe(
            name,
            probe::Kind::Tcp(TcpProbe {
                port: u32::from(port),
            }),
        )
    }

    fn http_probe(name: &str, port: u16, path: &str) -> Probe {
        probe(
            name,
            probe::Kind::HttpGet(HttpGetProbe {
                port: u32::from(port),
                path: path.into(),
                scheme: String::new(),
            }),
        )
    }

    async fn boot_with(agent: &mut Agent, script: &str, probes: Vec<Probe>) -> Result {
        let mut request = agent.boot_request(script);
        request.run.as_mut().ok_or("no run")?.probes = probes;
        agent.client.boot(request).await?;
        Ok(())
    }

    async fn watch(agent: &mut Agent) -> Result<Streaming<WatchProbesResponse>> {
        Ok(agent
            .client
            .watch_probes(WatchProbesRequest {})
            .await?
            .into_inner())
    }

    async fn next(stream: &mut Streaming<WatchProbesResponse>) -> Result<ProbeResult> {
        let message = within(stream.message()).await??.ok_or("the stream ended")?;
        Ok(message.result.ok_or("the message has no result")?)
    }

    /// Asserts that the stream is open and sends nothing for a while.
    async fn quiet(stream: &mut Streaming<WatchProbesResponse>) -> Result {
        match tokio::time::timeout(Duration::from_millis(400), stream.message()).await {
            Err(_) => Ok(()),
            Ok(message) => Err(format!("the stream sent or ended: {message:?}").into()),
        }
    }

    /// Asserts that the stream ends without another message.
    async fn ended(stream: &mut Streaming<WatchProbesResponse>) -> Result {
        match tokio::time::timeout(Duration::from_secs(5), stream.message()).await {
            Err(_) => Err("the WatchProbes stream stayed open after the entrypoint ended".into()),
            Ok(message) => match message? {
                None => Ok(()),
                Some(message) => {
                    Err(format!("the stream sent {message:?} instead of ending").into())
                }
            },
        }
    }

    fn at(result: &ProbeResult) -> Result<SystemTime> {
        Ok(SystemTime::try_from(
            result.changed_at.ok_or("the result has no changed_at")?,
        )?)
    }

    /// The code of the refusal a boot came to.
    fn refusal_code(outcome: Result) -> Result<Code> {
        let error = outcome.err().ok_or("the boot was accepted")?;
        Ok(error
            .downcast_ref::<tonic::Status>()
            .ok_or("the boot failed, but not with a status")?
            .code())
    }

    /// A server of HTTP on a loopback port, answering every request with `status`.
    struct Http {
        port: u16,
        status: Arc<AtomicU16>,
        requests: Arc<Mutex<Vec<String>>>,
    }

    async fn http_server(status: u16) -> Result<Http> {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let server = Http {
            port: listener.local_addr()?.port(),
            status: Arc::new(AtomicU16::new(status)),
            requests: Arc::default(),
        };
        let (status, requests) = (server.status.clone(), server.requests.clone());
        tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                let (status, requests) = (status.clone(), requests.clone());
                tokio::spawn(async move {
                    let mut request = [0_u8; 1024];
                    let Ok(read) = stream.read(&mut request).await else {
                        return;
                    };
                    let text = String::from_utf8_lossy(&request[..read]).into_owned();
                    lock(&requests).push(text);
                    let status = status.load(Ordering::SeqCst);
                    let response = format!(
                        "HTTP/1.1 {status} Reason\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                    );
                    let _ = stream.write_all(response.as_bytes()).await;
                });
            }
        });
        Ok(server)
    }

    #[tokio::test]
    async fn a_tcp_probe_is_unknown_then_passes_while_a_listener_accepts_and_fails_when_it_goes()
    -> Result {
        let mut agent = Agent::start().await?;
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let port = listener.local_addr()?.port();
        let before = SystemTime::now();
        boot_with(&mut agent, "sleep 30", vec![tcp_probe("db", port)]).await?;
        let mut stream = watch(&mut agent).await?;

        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        let passing = next(&mut stream).await?;
        let after_pass = SystemTime::now();
        drop(listener);
        let failing = next(&mut stream).await?;
        let after_fail = SystemTime::now();

        assert_eq!(passing.name, "db");
        assert_eq!(passing.status(), ProbeStatus::Passing);
        assert!(before <= at(&passing)? && at(&passing)? <= after_pass);
        assert_eq!(failing.status(), ProbeStatus::Failing);
        assert!(after_pass <= at(&failing)? && at(&failing)? <= after_fail);
        assert!(failing.last_failure.contains("cannot connect"));
        assert_eq!(failing.consecutive_successes, 0);
        // The agent acts on no result: the entrypoint runs on and the Machine stays up.
        assert!(!agent.is_powered_off());
        let record = agent
            .client
            .exec(exec_request(guest_agent::ENTRYPOINT, "true"))
            .await?
            .into_inner()
            .record
            .ok_or("no record")?;
        assert_eq!(record.status(), ExecStatus::Started);
        Ok(())
    }

    #[tokio::test]
    async fn the_stream_opens_with_unknown_and_no_time_and_the_first_run_waits_the_initial_delay()
    -> Result {
        let mut agent = Agent::start().await?;
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let mut probe = tcp_probe("late", listener.local_addr()?.port());
        probe.initial_delay = ms(600);
        let before = SystemTime::now();
        boot_with(&mut agent, "sleep 30", vec![probe]).await?;
        let mut stream = watch(&mut agent).await?;

        let unknown = next(&mut stream).await?;
        let passing = next(&mut stream).await?;

        assert_eq!(unknown.status(), ProbeStatus::Unknown);
        assert!(unknown.changed_at.is_none());
        assert_eq!(
            (unknown.consecutive_successes, unknown.consecutive_failures),
            (0, 0)
        );
        assert_eq!(passing.status(), ProbeStatus::Passing);
        assert!(
            at(&passing)? >= before + Duration::from_millis(600),
            "the first run came {:?} after the boot began",
            at(&passing)?.duration_since(before)
        );
        Ok(())
    }

    #[tokio::test]
    async fn an_http_probe_passes_on_a_status_from_200_to_399_and_fails_on_the_rest() -> Result {
        let mut agent = Agent::start().await?;
        let server = http_server(200).await?;
        boot_with(
            &mut agent,
            "sleep 30",
            vec![http_probe("web", server.port, "/healthz?full=1")],
        )
        .await?;
        let mut stream = watch(&mut agent).await?;

        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        // 200 and 399 are the ends of success; 199 and 400 the first outside it.
        let mut seen = vec![];
        for status in [200, 503, 399, 400, 302, 199] {
            server.status.store(status, Ordering::SeqCst);
            let result = next(&mut stream).await?;
            if result.status() == ProbeStatus::Failing {
                assert!(
                    result.last_failure.contains(&status.to_string()),
                    "{}",
                    result.last_failure
                );
            }
            seen.push(result.status());
        }

        assert_eq!(
            seen,
            [
                ProbeStatus::Passing,
                ProbeStatus::Failing,
                ProbeStatus::Passing,
                ProbeStatus::Failing,
                ProbeStatus::Passing,
                ProbeStatus::Failing
            ]
        );
        let requests = lock(&server.requests).clone();
        let first = requests.first().ok_or("the server saw no request")?;
        assert!(
            first.starts_with("GET /healthz?full=1 HTTP/1.1\r\n"),
            "{first}"
        );
        assert!(first.contains("Host: 127.0.0.1:"), "{first}");
        Ok(())
    }

    #[tokio::test]
    async fn an_exec_probe_passes_on_exit_0_and_fails_otherwise_and_keeps_the_start_of_its_output()
    -> Result {
        let mut agent = Agent::start().await?;
        let flag = agent.dir.path().join("ready");
        let script = format!(
            "echo out-$GREETING; head -c 3000 /dev/zero | tr '\\0' x; test -e {}",
            flag.display()
        );
        boot_with(&mut agent, "sleep 30", vec![sh_probe("cmd", &script)]).await?;
        let mut stream = watch(&mut agent).await?;

        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        let failing = next(&mut stream).await?;
        fs::write(&flag, "")?;
        let passing = next(&mut stream).await?;

        assert_eq!(failing.status(), ProbeStatus::Failing);
        assert_eq!(failing.last_failure, "exited with code 1");
        assert_eq!(passing.status(), ProbeStatus::Passing);
        // Only the first 1 KiB of the output is kept, from its start; the message of a failure
        // holds none of it.
        assert_eq!(passing.last_output.len(), 1024);
        assert!(passing.last_output.starts_with(b"out-hello\nxxx"));
        assert!(!failing.last_failure.contains("out-"));
        Ok(())
    }

    #[tokio::test]
    async fn an_exec_probe_runs_with_the_run_configs_environment_and_working_directory() -> Result {
        let mut agent = Agent::start().await?;
        fs::write(agent.dir.path().join("here"), "")?;
        // The environment is the run config's alone: GREETING is set and HOME is not.
        let script = r#"test -e here && test "$GREETING" = hello && test -z "${HOME+x}""#;
        boot_with(&mut agent, "sleep 30", vec![sh_probe("env", script)]).await?;
        let mut stream = watch(&mut agent).await?;

        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        let result = next(&mut stream).await?;

        assert_eq!(
            result.status(),
            ProbeStatus::Passing,
            "{}",
            result.last_failure
        );
        Ok(())
    }

    #[tokio::test]
    async fn the_thresholds_are_consecutive_runs() -> Result {
        let mut agent = Agent::start().await?;
        let flag = agent.dir.path().join("ready");
        let mut probe = sh_probe("t", &format!("test -e {}", flag.display()));
        probe.success_threshold = 3;
        probe.failure_threshold = 2;
        probe.initial_delay = ms(600);
        boot_with(&mut agent, "sleep 30", vec![probe]).await?;
        let mut stream = watch(&mut agent).await?;
        let opened = next(&mut stream).await?;
        assert_eq!(opened.status(), ProbeStatus::Unknown);
        assert_eq!(opened.consecutive_failures, 0);

        // A probe turns at its threshold and not before, so the message after the opening one
        // is the passing one, at exactly three successes.
        fs::write(&flag, "")?;
        let passing = next(&mut stream).await?;
        fs::remove_file(&flag)?;
        let failing = next(&mut stream).await?;

        assert_eq!(passing.status(), ProbeStatus::Passing);
        assert_eq!(passing.consecutive_successes, 3);
        assert_eq!(passing.consecutive_failures, 0);
        assert_eq!(failing.status(), ProbeStatus::Failing);
        assert_eq!(failing.consecutive_failures, 2);
        assert_eq!(failing.consecutive_successes, 0);
        Ok(())
    }

    #[tokio::test]
    async fn a_probe_that_hangs_fails_at_its_timeout_and_its_command_is_killed() -> Result {
        let mut agent = Agent::start().await?;
        let pid_file = agent.dir.path().join("pid");
        let mut exec = sh_probe(
            "slow",
            &format!("echo $$ > {}; sleep 60", pid_file.display()),
        );
        exec.timeout = ms(300);
        // A server that accepts and never answers.
        let silent = TcpListener::bind("127.0.0.1:0").await?;
        let mut web = http_probe("mute", silent.local_addr()?.port(), "/");
        web.timeout = ms(300);
        let held = tokio::spawn(async move {
            let mut streams = vec![];
            while let Ok((stream, _)) = silent.accept().await {
                streams.push(stream);
            }
        });
        let before = Instant::now();
        boot_with(&mut agent, "sleep 30", vec![exec, web]).await?;
        let mut stream = watch(&mut agent).await?;

        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        let first = next(&mut stream).await?;
        let second = next(&mut stream).await?;

        let mut names = vec![first.name.clone(), second.name.clone()];
        names.sort();
        assert_eq!(names, ["mute", "slow"]);
        for result in [&first, &second] {
            assert_eq!(result.status(), ProbeStatus::Failing, "{}", result.name);
            assert!(
                result.last_failure.starts_with("did not finish within"),
                "{}: {}",
                result.name,
                result.last_failure
            );
        }
        assert!(before.elapsed() >= Duration::from_millis(300));
        let pid: u32 = fs::read_to_string(&pid_file)?.trim().parse()?;
        eventually("the probe's command to be killed", || {
            !Path::new(&format!("/proc/{pid}")).exists()
        })
        .await?;
        held.abort();
        Ok(())
    }

    #[tokio::test]
    async fn a_watch_opened_later_starts_with_the_current_result_of_every_probe_in_order() -> Result
    {
        let mut agent = Agent::start().await?;
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let up = tcp_probe("zz-up", listener.local_addr()?.port());
        let down = sh_probe("aa-down", "exit 3");
        boot_with(&mut agent, "sleep 30", vec![up, down]).await?;
        let mut first = watch(&mut agent).await?;
        let mut turned = HashMap::new();
        while turned.len() < 2 {
            let result = next(&mut first).await?;
            if result.status() != ProbeStatus::Unknown {
                turned.insert(result.name.clone(), result);
            }
        }

        let mut second = watch(&mut agent).await?;
        let (one, two) = (next(&mut second).await?, next(&mut second).await?);

        // The description's order, not the names'.
        assert_eq!((one.name.as_str(), two.name.as_str()), ("zz-up", "aa-down"));
        assert_eq!(one.status(), ProbeStatus::Passing);
        assert_eq!(two.status(), ProbeStatus::Failing);
        assert_eq!(one.changed_at, turned["zz-up"].changed_at);
        assert_eq!(two.changed_at, turned["aa-down"].changed_at);
        // Nothing more arrives while nothing changes.
        quiet(&mut second).await?;
        Ok(())
    }

    #[tokio::test]
    async fn the_stream_ends_after_the_entrypoint_ends() -> Result {
        let mut agent = start_patient().await?;
        let flag = agent.dir.path().join("exit-now");
        let script = format!("while [ ! -e {} ]; do sleep 0.05; done", flag.display());
        boot_with(&mut agent, &script, vec![sh_probe("ok", "true")]).await?;
        let mut stream = watch(&mut agent).await?;
        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Passing);
        // The control: with the entrypoint running the stream stays open.
        quiet(&mut stream).await?;

        fs::write(&flag, "")?;
        ended(&mut stream).await?;
        Ok(())
    }

    #[tokio::test]
    async fn no_probes_send_nothing_and_the_stream_ends_with_the_entrypoint() -> Result {
        let mut agent = start_patient().await?;
        let flag = agent.dir.path().join("exit-now");
        let script = format!("while [ ! -e {} ]; do sleep 0.05; done", flag.display());
        boot_with(&mut agent, &script, vec![]).await?;
        let mut stream = watch(&mut agent).await?;

        quiet(&mut stream).await?;
        fs::write(&flag, "")?;
        ended(&mut stream).await?;
        Ok(())
    }

    #[tokio::test]
    async fn a_probe_stops_running_once_the_entrypoint_ended() -> Result {
        let mut agent = start_patient().await?;
        let flag = agent.dir.path().join("exit-now");
        let counter = agent.dir.path().join("runs");
        let script = format!("while [ ! -e {} ]; do sleep 0.05; done", flag.display());
        let probe = sh_probe("count", &format!("echo >> {}", counter.display()));
        boot_with(&mut agent, &script, vec![probe]).await?;
        let mut stream = watch(&mut agent).await?;
        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Unknown);
        assert_eq!(next(&mut stream).await?.status(), ProbeStatus::Passing);

        fs::write(&flag, "")?;
        ended(&mut stream).await?;
        let runs = fs::read_to_string(&counter)?.lines().count();
        tokio::time::sleep(Duration::from_millis(400)).await;

        assert_eq!(fs::read_to_string(&counter)?.lines().count(), runs);
        Ok(())
    }

    #[tokio::test]
    async fn a_repeated_boot_with_other_probes_is_refused_and_with_the_same_ones_accepted() -> Result
    {
        let mut agent = Agent::start().await?;
        let mut first = tcp_probe("db", 5432);
        first.period = None;
        boot_with(&mut agent, "sleep 30", vec![first.clone()]).await?;
        let steps = agent.log.entries();

        // The same probes, one of them with a default written out.
        let mut explicit = first.clone();
        explicit.period = ms(10_000);
        boot_with(&mut agent, "sleep 30", vec![first.clone()]).await?;
        boot_with(&mut agent, "sleep 30", vec![explicit]).await?;
        let mut other_port = first.clone();
        other_port.kind = Some(probe::Kind::Tcp(TcpProbe { port: 5433 }));
        let mut other_name = first.clone();
        other_name.name = "cache".into();
        let mut other_period = first.clone();
        other_period.period = ms(60);
        let mut other_threshold = first.clone();
        other_threshold.failure_threshold = 2;

        for (what, probes) in [
            ("port", vec![other_port]),
            ("name", vec![other_name]),
            ("period", vec![other_period]),
            ("threshold", vec![other_threshold]),
            ("none", vec![]),
            ("more", vec![first.clone(), tcp_probe("extra", 1)]),
        ] {
            let outcome = boot_with(&mut agent, "sleep 30", probes).await;
            assert_eq!(refusal_code(outcome)?, Code::FailedPrecondition, "{what}");
        }
        assert_eq!(agent.log.entries(), steps);
        Ok(())
    }

    #[tokio::test]
    async fn probes_that_cannot_run_refuse_the_boot_and_the_agent_still_waits() -> Result {
        let mut agent = Agent::start().await?;
        let nine: Vec<Probe> = (0..9).map(|n| tcp_probe(&format!("p{n}"), 80)).collect();
        let mut bad_scheme = http_probe("a", 80, "/");
        bad_scheme.kind = Some(probe::Kind::HttpGet(HttpGetProbe {
            port: 80,
            path: "/".into(),
            scheme: "https".into(),
        }));
        let kindless = Probe {
            name: "a".into(),
            ..Probe::default()
        };
        let refused = [
            (
                "duplicate name",
                vec![tcp_probe("a", 80), tcp_probe("a", 81)],
            ),
            ("more than 8", nine),
            ("scheme", vec![bad_scheme]),
            ("path", vec![http_probe("a", 80, "no-slash")]),
            ("port", vec![tcp_probe("a", 0)]),
            ("name", vec![tcp_probe("bad name", 80)]),
            ("kind", vec![kindless]),
        ];

        for (what, probes) in refused {
            let outcome = boot_with(&mut agent, "sleep 30", probes).await;
            assert_eq!(refusal_code(outcome)?, Code::InvalidArgument, "{what}");
        }
        // Nothing booted, and the agent still takes a valid description.
        assert_eq!(agent.log.count("find"), 0);
        boot_with(&mut agent, "sleep 30", vec![tcp_probe("a", 80)]).await?;
        Ok(())
    }
}
