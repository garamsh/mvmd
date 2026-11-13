//! The probes of the workload: parsed from the run configuration, run on their schedules from the
//! entrypoint's start until it ends, and kept as results that `WatchProbes` streams. The agent
//! acts on no result.

use std::collections::{HashMap, VecDeque};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::os::unix::process::ExitStatusExt;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::broadcast;
use tokio::task::JoinSet;
use tokio::time::{Instant, MissedTickBehavior};

use crate::boot::ENTRYPOINT;
use crate::description::NetworkConfig;
use crate::error::Error;
use crate::exec::{Execs, lock};
use crate::user::Identity;
use crate::v1alpha1::{self, ProbeResult, ProbeStatus, probe};

/// The most probes a description carries.
pub const MAX_PROBES: usize = 8;
/// The most bytes of a probe's name.
const MAX_NAME_BYTES: usize = 63;
/// The most bytes of an HTTP probe's path.
const MAX_PATH_BYTES: usize = 2048;
/// The period an unset one means.
const DEFAULT_PERIOD: Duration = Duration::from_secs(10);
/// The timeout an unset one means.
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(1);
/// The threshold of consecutive successes that a 0 means.
const DEFAULT_SUCCESS_THRESHOLD: u32 = 1;
/// The threshold of consecutive failures that a 0 means.
const DEFAULT_FAILURE_THRESHOLD: u32 = 3;
/// The shortest period or timeout.
const MIN_DURATION: Duration = Duration::from_millis(1);
/// The longest initial delay, period or timeout.
const MAX_DURATION: Duration = Duration::from_secs(24 * 60 * 60);
/// The bytes of an exec probe's output a result keeps.
const OUTPUT_BYTES: usize = 1024;
/// The bytes of an HTTP response read to find its status line.
const STATUS_LINE_BYTES: usize = 1024;
/// How long an exec probe's output pipes may stay open after the command exited: a process it
/// left behind can hold them for good.
const DRAIN_GRACE: Duration = Duration::from_millis(100);
/// Changes buffered for a watcher before it is sent the current results instead.
const CHANGES_BUFFERED: usize = 64;

/// One probe, as the description gives it with every default applied, so two descriptions that
/// mean the same compare equal.
#[derive(Debug, Clone, PartialEq)]
pub struct ProbeSpec {
    /// Names the probe in results.
    pub name: String,
    /// What a run does.
    pub kind: ProbeKind,
    /// Time from the entrypoint's start to the first run.
    pub initial_delay: Duration,
    /// Time between the starts of runs.
    pub period: Duration,
    /// How long one run may take.
    pub timeout: Duration,
    /// Consecutive successes that make the probe passing.
    pub success_threshold: u32,
    /// Consecutive failures that make the probe failing.
    pub failure_threshold: u32,
}

/// What a probe's run does.
#[derive(Debug, Clone, PartialEq)]
pub enum ProbeKind {
    /// Run a command; it succeeds on exit code 0.
    Exec {
        /// The program and its arguments; never empty.
        argv: Vec<String>,
    },
    /// Connect to a port of the loopback address.
    Tcp {
        /// The port.
        port: u16,
    },
    /// Get a path of a port of the loopback address; a status from 200 to 399 succeeds.
    HttpGet {
        /// The port.
        port: u16,
        /// The request target.
        path: String,
    },
}

fn refuse(name: &str, why: impl std::fmt::Display) -> Error {
    Error::Invalid(format!("probe {name:?} {why}"))
}

fn port(name: &str, port: u32) -> Result<u16, Error> {
    u16::try_from(port)
        .ok()
        .filter(|port| *port != 0)
        .ok_or_else(|| refuse(name, format!("has port {port}, not from 1 to 65535")))
}

fn duration(
    name: &str,
    field: &str,
    value: Option<prost_types::Duration>,
    default: Duration,
    least: Duration,
) -> Result<Duration, Error> {
    let Some(value) = value else {
        return Ok(default);
    };
    let value = Duration::try_from(value)
        .map_err(|_| refuse(name, format!("has a negative or invalid {field}")))?;
    if value < least || value > MAX_DURATION {
        return Err(refuse(
            name,
            format!("has {field} {value:?}, not from {least:?} to {MAX_DURATION:?}"),
        ));
    }
    Ok(value)
}

impl ProbeSpec {
    fn parse(probe: v1alpha1::Probe) -> Result<Self, Error> {
        let name = probe.name;
        if name.is_empty()
            || name.len() > MAX_NAME_BYTES
            || !name
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"-_.".contains(&byte))
        {
            return Err(refuse(
                &name,
                format!(
                    "has a name that is not 1 to {MAX_NAME_BYTES} letters, digits, '-', '_' or '.'"
                ),
            ));
        }
        let kind = match probe.kind {
            None => return Err(refuse(&name, "has no kind")),
            Some(probe::Kind::Exec(exec)) => {
                if exec.argv.first().is_none_or(String::is_empty) {
                    return Err(refuse(&name, "has an exec argv with no program"));
                }
                ProbeKind::Exec { argv: exec.argv }
            }
            Some(probe::Kind::Tcp(tcp)) => ProbeKind::Tcp {
                port: port(&name, tcp.port)?,
            },
            Some(probe::Kind::HttpGet(get)) => {
                if !get.scheme.is_empty() && get.scheme != "http" {
                    return Err(refuse(
                        &name,
                        format!("has scheme {:?}, only \"http\" is spoken", get.scheme),
                    ));
                }
                if !get.path.starts_with('/')
                    || get.path.len() > MAX_PATH_BYTES
                    || !get.path.bytes().all(|byte| byte.is_ascii_graphic())
                {
                    return Err(refuse(
                        &name,
                        format!(
                            "has a path that does not begin with '/', or is over {MAX_PATH_BYTES} bytes, or holds a space, a control or a non-ASCII character"
                        ),
                    ));
                }
                ProbeKind::HttpGet {
                    port: port(&name, get.port)?,
                    path: get.path,
                }
            }
        };
        let initial_delay = duration(
            &name,
            "initial_delay",
            probe.initial_delay,
            Duration::ZERO,
            Duration::ZERO,
        )?;
        let period = duration(&name, "period", probe.period, DEFAULT_PERIOD, MIN_DURATION)?;
        let timeout = duration(
            &name,
            "timeout",
            probe.timeout,
            DEFAULT_TIMEOUT,
            MIN_DURATION,
        )?;
        let or_default = |value: u32, default: u32| if value == 0 { default } else { value };
        Ok(Self {
            name,
            kind,
            initial_delay,
            period,
            timeout,
            success_threshold: or_default(probe.success_threshold, DEFAULT_SUCCESS_THRESHOLD),
            failure_threshold: or_default(probe.failure_threshold, DEFAULT_FAILURE_THRESHOLD),
        })
    }
}

/// Parse the probes of a run configuration, refusing too many, a name used twice, or a probe that
/// cannot run.
pub(crate) fn parse_probes(probes: Vec<v1alpha1::Probe>) -> Result<Vec<ProbeSpec>, Error> {
    if probes.len() > MAX_PROBES {
        return Err(Error::Invalid(format!(
            "run has {} probes, more than {MAX_PROBES}",
            probes.len()
        )));
    }
    let probes = probes
        .into_iter()
        .map(ProbeSpec::parse)
        .collect::<Result<Vec<_>, _>>()?;
    for (index, probe) in probes.iter().enumerate() {
        if probes
            .iter()
            .take(index)
            .any(|before| before.name == probe.name)
        {
            return Err(Error::Invalid(format!(
                "probe {:?} is named twice",
                probe.name
            )));
        }
    }
    Ok(probes)
}

/// The loopback address probes connect to: `::1` when the network configures IPv6 and no IPv4,
/// else `127.0.0.1`.
pub(crate) fn loopback(network: &NetworkConfig) -> IpAddr {
    if network.ipv4.is_none() && network.ipv6.is_some() {
        IpAddr::V6(Ipv6Addr::LOCALHOST)
    } else {
        IpAddr::V4(Ipv4Addr::LOCALHOST)
    }
}

/// What probe runs need from the booted guest.
pub(crate) struct Context {
    /// The loopback address a tcp or http_get probe connects to.
    pub(crate) host: IpAddr,
    pub(crate) execs: Arc<Execs>,
    /// The run configuration's environment, which an exec probe runs with.
    pub(crate) env: HashMap<String, String>,
    /// The run configuration's working directory, which an exec probe runs in.
    pub(crate) working_dir: String,
    /// The run configuration's user, which an exec probe runs as.
    pub(crate) identity: Option<Identity>,
}

/// The current results of the probes and the changes since, for any number of watchers.
pub(crate) struct Probes {
    state: Mutex<State>,
}

struct State {
    /// One result per probe, in the order of the description.
    results: Vec<ProbeResult>,
    /// Carries each change of status; `None` once the entrypoint has ended.
    changes: Option<broadcast::Sender<ProbeResult>>,
}

/// How one run went.
struct Outcome {
    /// Why it failed; `None` when it succeeded.
    failure: Option<String>,
    /// The first bytes of an exec run's output; `None` for the other kinds.
    output: Option<Vec<u8>>,
}

impl Outcome {
    fn passed() -> Self {
        Self {
            failure: None,
            output: None,
        }
    }

    fn failed(why: impl Into<String>) -> Self {
        Self {
            failure: Some(why.into()),
            output: None,
        }
    }
}

/// Start the probes of `specs` and the task that ends them when the entrypoint does. The
/// entrypoint must already be in `context.execs`.
pub(crate) fn start(specs: Vec<ProbeSpec>, context: Context) -> Arc<Probes> {
    let results = specs
        .iter()
        .map(|spec| ProbeResult {
            name: spec.name.clone(),
            status: ProbeStatus::Unknown.into(),
            ..ProbeResult::default()
        })
        .collect();
    let probes = Arc::new(Probes {
        state: Mutex::new(State {
            results,
            changes: Some(broadcast::Sender::new(CHANGES_BUFFERED)),
        }),
    });
    let context = Arc::new(context);
    let supervisor = probes.clone();
    tokio::spawn(async move {
        let mut runs = JoinSet::new();
        for (index, spec) in specs.into_iter().enumerate() {
            runs.spawn(run_probe(supervisor.clone(), context.clone(), index, spec));
        }
        // The wait fails only if the exec is gone, which also means there is nothing to probe.
        let _ = context.execs.wait(ENTRYPOINT).await;
        // Aborting a run drops its command, which is killed with it.
        runs.shutdown().await;
        supervisor.end();
    });
    probes
}

/// Run one probe on its schedule until it is aborted.
async fn run_probe(probes: Arc<Probes>, context: Arc<Context>, index: usize, spec: ProbeSpec) {
    let mut ticks = tokio::time::interval_at(Instant::now() + spec.initial_delay, spec.period);
    // A run that outlasts the period pushes the next one back; runs never overlap.
    ticks.set_missed_tick_behavior(MissedTickBehavior::Delay);
    loop {
        ticks.tick().await;
        let outcome = match tokio::time::timeout(spec.timeout, attempt(&spec.kind, &context)).await
        {
            Ok(outcome) => outcome,
            Err(_) => Outcome::failed(format!("did not finish within {:?}", spec.timeout)),
        };
        probes.record(index, &spec, outcome);
    }
}

impl Probes {
    pub(crate) fn empty() -> Self {
        Probes {
            state: Mutex::new(State {
                results: Vec::new(),
                changes: None,
            }),
        }
    }

    /// Count a run in its probe's result, and send the result if its status changed.
    fn record(&self, index: usize, spec: &ProbeSpec, outcome: Outcome) {
        let mut guard = lock(&self.state);
        let state = &mut *guard;
        let Some(result) = state.results.get_mut(index) else {
            return;
        };
        if let Some(output) = outcome.output {
            result.last_output = output;
        }
        let turned = match outcome.failure {
            None => {
                result.consecutive_successes = result.consecutive_successes.saturating_add(1);
                result.consecutive_failures = 0;
                (result.status() != ProbeStatus::Passing
                    && result.consecutive_successes >= spec.success_threshold)
                    .then_some(ProbeStatus::Passing)
            }
            Some(why) => {
                result.consecutive_failures = result.consecutive_failures.saturating_add(1);
                result.consecutive_successes = 0;
                result.last_failure = why;
                (result.status() != ProbeStatus::Failing
                    && result.consecutive_failures >= spec.failure_threshold)
                    .then_some(ProbeStatus::Failing)
            }
        };
        let Some(status) = turned else {
            return;
        };
        result.status = status.into();
        result.changed_at = Some(SystemTime::now().into());
        if let Some(changes) = &state.changes {
            // Without a receiver no one is watching.
            let _ = changes.send(result.clone());
        }
    }

    /// The entrypoint has ended: no result changes again, and every watcher ends once it has
    /// been sent the results.
    fn end(&self) {
        lock(&self.state).changes = None;
    }

    /// The current results and a receiver of every change after them, taken together so that no
    /// change is missed or sent twice. The receiver is `None` once the entrypoint has ended.
    fn snapshot(
        &self,
    ) -> (
        VecDeque<ProbeResult>,
        Option<broadcast::Receiver<ProbeResult>>,
    ) {
        let state = lock(&self.state);
        (
            state.results.iter().cloned().collect(),
            state.changes.as_ref().map(broadcast::Sender::subscribe),
        )
    }

    /// Follow the results: the current one of every probe, then each change.
    pub(crate) fn watch(self: &Arc<Self>) -> Watch {
        let (pending, changes) = self.snapshot();
        Watch {
            probes: self.clone(),
            pending,
            changes,
        }
    }
}

/// Follows the results of the probes.
pub(crate) struct Watch {
    probes: Arc<Probes>,
    /// Results to send before any change.
    pending: VecDeque<ProbeResult>,
    changes: Option<broadcast::Receiver<ProbeResult>>,
}

impl Watch {
    /// The next result to send; `None` once the entrypoint has ended and every result is sent.
    pub(crate) async fn next(&mut self) -> Option<ProbeResult> {
        loop {
            if let Some(result) = self.pending.pop_front() {
                return Some(result);
            }
            match self.changes.as_mut()?.recv().await {
                Ok(result) => return Some(result),
                Err(broadcast::error::RecvError::Closed) => return None,
                Err(broadcast::error::RecvError::Lagged(_)) => {
                    // Changes were missed, so the current results stand in for them.
                    (self.pending, self.changes) = self.probes.snapshot();
                }
            }
        }
    }
}

/// One run of a probe.
async fn attempt(kind: &ProbeKind, context: &Context) -> Outcome {
    match kind {
        ProbeKind::Exec { argv } => exec_run(argv, context).await,
        ProbeKind::Tcp { port } => {
            let address = SocketAddr::new(context.host, *port);
            match TcpStream::connect(address).await {
                Ok(_) => Outcome::passed(),
                Err(error) => Outcome::failed(format!("cannot connect to {address}: {error}")),
            }
        }
        ProbeKind::HttpGet { port, path } => {
            let address = SocketAddr::new(context.host, *port);
            match http_get(address, path).await {
                Ok(()) => Outcome::passed(),
                Err(why) => Outcome::failed(why),
            }
        }
    }
}

/// Get `path` from `address` and check the status line. The message of an error never holds
/// what the server sent beyond the status code.
async fn http_get(address: SocketAddr, path: &str) -> Result<(), String> {
    let mut stream = TcpStream::connect(address)
        .await
        .map_err(|error| format!("cannot connect to {address}: {error}"))?;
    let request = format!(
        "GET {path} HTTP/1.1\r\nHost: {address}\r\nUser-Agent: mvmd-guest-agent\r\nConnection: close\r\n\r\n"
    );
    stream
        .write_all(request.as_bytes())
        .await
        .map_err(|error| format!("cannot send the request to {address}: {error}"))?;
    let mut head = Vec::with_capacity(STATUS_LINE_BYTES);
    let mut chunk = [0_u8; 256];
    while !head.contains(&b'\n') && head.len() < STATUS_LINE_BYTES {
        let read = stream
            .read(&mut chunk)
            .await
            .map_err(|error| format!("cannot read the response from {address}: {error}"))?;
        if read == 0 {
            break;
        }
        head.extend_from_slice(&chunk[..read]);
    }
    let line = head.split(|byte| *byte == b'\n').next().unwrap_or_default();
    let status = std::str::from_utf8(line)
        .ok()
        .and_then(|line| line.strip_prefix("HTTP/"))
        .and_then(|rest| rest.split_ascii_whitespace().nth(1))
        .and_then(|code| code.parse::<u16>().ok())
        .ok_or_else(|| "the response does not begin with an HTTP status line".to_owned())?;
    if (200..=399).contains(&status) {
        Ok(())
    } else {
        Err(format!(
            "the response status is {status}, not from 200 to 399"
        ))
    }
}

/// Run the command of an exec probe to its end and keep the first bytes of its output.
async fn exec_run(argv: &[String], context: &Context) -> Outcome {
    let Some((program, args)) = argv.split_first() else {
        return Outcome::failed("the exec argv is empty");
    };
    let mut command = context.execs.command(
        program,
        args,
        &context.env,
        &context.working_dir,
        context.identity.as_ref(),
    );
    // A run that is dropped, by its timeout or the entrypoint's end, kills its command.
    command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(error) => return Outcome::failed(format!("cannot start {program}: {error}")),
    };
    let (stdout, stderr) = (child.stdout.take(), child.stderr.take());
    let output = Mutex::new(Vec::new());
    let readers = async {
        tokio::join!(pump(stdout, &output), pump(stderr, &output));
    };
    tokio::pin!(readers);
    let status = tokio::select! {
        status = child.wait() => {
            // Only a process the command left behind can still hold the pipes open.
            let _ = tokio::time::timeout(DRAIN_GRACE, &mut readers).await;
            status
        }
        () = &mut readers => child.wait().await,
    };
    let failure = match status {
        Ok(status) if status.success() => None,
        Ok(status) => Some(match (status.code(), status.signal()) {
            (Some(code), _) => format!("exited with code {code}"),
            (None, Some(signal)) => format!("terminated by signal {signal}"),
            (None, None) => "did not exit normally".to_owned(),
        }),
        Err(error) => Some(format!("cannot wait for {program}: {error}")),
    };
    let output = std::mem::take(&mut *lock(&output));
    Outcome {
        failure,
        output: Some(output),
    }
}

/// Read `stream` to its end into `output`, keeping the first `OUTPUT_BYTES` of all that is read.
async fn pump(stream: Option<impl AsyncRead + Unpin>, output: &Mutex<Vec<u8>>) {
    let Some(mut stream) = stream else {
        return;
    };
    let mut chunk = [0_u8; 4096];
    while let Ok(read @ 1..) = stream.read(&mut chunk).await {
        let mut kept = lock(output);
        let room = OUTPUT_BYTES.saturating_sub(kept.len());
        kept.extend_from_slice(&chunk[..read.min(room)]);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::description::Addressing;

    fn http(name: &str, path: &str, scheme: &str) -> v1alpha1::Probe {
        v1alpha1::Probe {
            name: name.into(),
            kind: Some(probe::Kind::HttpGet(v1alpha1::HttpGetProbe {
                port: 8080,
                path: path.into(),
                scheme: scheme.into(),
            })),
            ..v1alpha1::Probe::default()
        }
    }

    fn tcp(name: &str, port: u32) -> v1alpha1::Probe {
        v1alpha1::Probe {
            name: name.into(),
            kind: Some(probe::Kind::Tcp(v1alpha1::TcpProbe { port })),
            ..v1alpha1::Probe::default()
        }
    }

    fn refusal(probes: Vec<v1alpha1::Probe>) -> String {
        match parse_probes(probes) {
            Err(Error::Invalid(why)) => why,
            other => panic!("expected a refusal, got {other:?}"),
        }
    }

    #[test]
    fn defaults_apply_to_what_is_unset() {
        let specs = parse_probes(vec![http("web", "/healthz", "")]).expect("parses");
        let [spec] = specs.as_slice() else {
            panic!("one probe");
        };
        assert_eq!(spec.initial_delay, Duration::ZERO);
        assert_eq!(spec.period, Duration::from_secs(10));
        assert_eq!(spec.timeout, Duration::from_secs(1));
        assert_eq!(spec.success_threshold, 1);
        assert_eq!(spec.failure_threshold, 3);
    }

    #[test]
    fn an_explicit_default_equals_an_unset_one() {
        let mut explicit = tcp("db", 5432);
        explicit.period = Some(prost_types::Duration {
            seconds: 10,
            nanos: 0,
        });
        explicit.failure_threshold = 3;
        assert_eq!(
            parse_probes(vec![explicit]).expect("parses"),
            parse_probes(vec![tcp("db", 5432)]).expect("parses")
        );
    }

    #[test]
    fn the_description_refuses_what_cannot_run() {
        let many = (0..=MAX_PROBES)
            .map(|index| tcp(&format!("p{index}"), 80))
            .collect();
        assert!(refusal(many).contains("more than 8"));
        assert!(refusal(vec![tcp("a", 80), tcp("a", 81)]).contains("named twice"));
        assert!(refusal(vec![tcp("", 80)]).contains("name"));
        assert!(refusal(vec![tcp("has space", 80)]).contains("name"));
        assert!(refusal(vec![tcp(&"n".repeat(64), 80)]).contains("name"));
        assert!(refusal(vec![tcp("a", 0)]).contains("port"));
        assert!(refusal(vec![tcp("a", 65536)]).contains("port"));
        assert!(refusal(vec![http("a", "/", "https")]).contains("scheme"));
        assert!(refusal(vec![http("a", "healthz", "")]).contains("path"));
        assert!(refusal(vec![http("a", "/a b", "")]).contains("path"));
        assert!(refusal(vec![http("a", "/a\r\nHost: x", "")]).contains("path"));
        assert!(refusal(vec![http("a", &format!("/{}", "x".repeat(2048)), "")]).contains("path"));
        assert!(refusal(vec![v1alpha1::Probe::default()]).contains("name"));
        let mut no_kind = tcp("a", 80);
        no_kind.kind = None;
        assert!(refusal(vec![no_kind]).contains("no kind"));
        let mut empty_exec = tcp("a", 80);
        empty_exec.kind = Some(probe::Kind::Exec(v1alpha1::ExecProbe { argv: vec![] }));
        assert!(refusal(vec![empty_exec]).contains("argv"));
        let mut quick = tcp("a", 80);
        quick.period = Some(prost_types::Duration {
            seconds: 0,
            nanos: 1,
        });
        assert!(refusal(vec![quick]).contains("period"));
        let mut negative = tcp("a", 80);
        negative.timeout = Some(prost_types::Duration {
            seconds: -1,
            nanos: 0,
        });
        assert!(refusal(vec![negative]).contains("timeout"));
        let mut forever = tcp("a", 80);
        forever.initial_delay = Some(prost_types::Duration {
            seconds: 48 * 60 * 60,
            nanos: 0,
        });
        assert!(refusal(vec![forever]).contains("initial_delay"));
    }

    fn addressing<A>(address: A, gateway: A) -> Addressing<A> {
        Addressing {
            address,
            prefix_len: 24,
            gateway,
        }
    }

    #[test]
    fn the_loopback_is_ipv6_only_when_the_network_has_no_ipv4() {
        let v4 = Some(addressing(
            Ipv4Addr::new(10, 0, 0, 2),
            Ipv4Addr::new(10, 0, 0, 1),
        ));
        let v6 = Some(addressing(
            "fd00::2".parse::<Ipv6Addr>().expect("an address"),
            "fd00::1".parse::<Ipv6Addr>().expect("an address"),
        ));
        let network = |ipv4, ipv6| NetworkConfig {
            ipv4,
            ipv6,
            resolvers: vec![],
        };
        assert_eq!(loopback(&network(v4, v6)), IpAddr::V4(Ipv4Addr::LOCALHOST));
        assert_eq!(
            loopback(&network(v4, None)),
            IpAddr::V4(Ipv4Addr::LOCALHOST)
        );
        assert_eq!(
            loopback(&network(None, None)),
            IpAddr::V4(Ipv4Addr::LOCALHOST)
        );
        assert_eq!(
            loopback(&network(None, v6)),
            IpAddr::V6(Ipv6Addr::LOCALHOST)
        );
    }

    fn spec(success: u32, failure: u32) -> ProbeSpec {
        parse_probes(vec![v1alpha1::Probe {
            success_threshold: success,
            failure_threshold: failure,
            ..tcp("t", 80)
        }])
        .expect("parses")
        .remove(0)
    }

    fn results_of(specs: &[ProbeSpec]) -> Probes {
        Probes {
            state: Mutex::new(State {
                results: specs
                    .iter()
                    .map(|spec| ProbeResult {
                        name: spec.name.clone(),
                        status: ProbeStatus::Unknown.into(),
                        ..ProbeResult::default()
                    })
                    .collect(),
                changes: Some(broadcast::Sender::new(CHANGES_BUFFERED)),
            }),
        }
    }

    #[tokio::test]
    async fn a_watcher_that_fell_behind_is_sent_the_current_results_in_place_of_the_changes() {
        let spec = spec(1, 1);
        let probes = Arc::new(results_of(std::slice::from_ref(&spec)));
        let mut watch = probes.watch();
        let opening = watch.next().await.expect("the opening result");
        assert_eq!(opening.status(), ProbeStatus::Unknown);

        // Twice as many changes as are buffered, ending on a failure.
        for run in 0..CHANGES_BUFFERED * 2 {
            let outcome = if run % 2 == 0 {
                Outcome::passed()
            } else {
                Outcome::failed("no")
            };
            probes.record(0, &spec, outcome);
        }
        let resent = watch.next().await.expect("the current result");
        assert_eq!(resent.status(), ProbeStatus::Failing);
        assert_eq!(resent.consecutive_failures, 1);

        // None of the missed changes follows it.
        probes.end();
        assert!(watch.next().await.is_none());

        // A watch opened after the end gets the current results, then ends.
        let mut late = probes.watch();
        let last = late.next().await.expect("the current result");
        assert_eq!(last.status(), ProbeStatus::Failing);
        assert!(late.next().await.is_none());
    }

    #[test]
    fn a_status_turns_only_at_its_threshold() {
        let spec = spec(2, 3);
        let probes = results_of(std::slice::from_ref(&spec));
        let mut changes = lock(&probes.state)
            .changes
            .as_ref()
            .map(broadcast::Sender::subscribe)
            .expect("open");
        let run = |ok: bool| {
            probes.record(
                0,
                &spec,
                if ok {
                    Outcome::passed()
                } else {
                    Outcome::failed("no")
                },
            );
        };
        let status = || lock(&probes.state).results[0].status();
        run(true);
        assert_eq!(status(), ProbeStatus::Unknown);
        run(true);
        assert_eq!(status(), ProbeStatus::Passing);
        run(false);
        run(false);
        assert_eq!(status(), ProbeStatus::Passing);
        // A success resets the failures in a row.
        run(true);
        run(false);
        run(false);
        assert_eq!(status(), ProbeStatus::Passing);
        run(false);
        assert_eq!(status(), ProbeStatus::Failing);
        assert_eq!(lock(&probes.state).results[0].consecutive_failures, 3);
        run(true);
        assert_eq!(status(), ProbeStatus::Failing);
        run(true);
        assert_eq!(status(), ProbeStatus::Passing);
        let seen: Vec<ProbeStatus> =
            std::iter::from_fn(|| changes.try_recv().ok().map(|result| result.status())).collect();
        assert_eq!(
            seen,
            [
                ProbeStatus::Passing,
                ProbeStatus::Failing,
                ProbeStatus::Passing
            ]
        );
    }
}
