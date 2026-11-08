//! A panic ends the Machine through the agent's own path. This is its own test binary because the
//! panic hook is the process's.

use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, PoisonError};

use guest_agent::{
    Clock, Console, Entropy, Filesystem, Mounts, Network, NetworkConfig, Power, System, Uuid,
};

type Log = Arc<Mutex<Vec<String>>>;

fn push(log: &Log, entry: impl Into<String>) {
    log.lock()
        .unwrap_or_else(PoisonError::into_inner)
        .push(entry.into());
}

/// Seams that do nothing the test looks at, except the ones that record the end of the Machine.
struct Recorder(Log);

impl Console for Recorder {
    fn line(&self, text: &str) {
        push(&self.0, format!("console: {text}"));
    }
}

impl Power for Recorder {
    fn power_off(&self) -> io::Result<()> {
        push(&self.0, "power_off");
        Err(io::Error::other("injected failure"))
    }

    fn restart(&self) -> io::Result<()> {
        push(&self.0, "restart");
        Err(io::Error::other("injected failure"))
    }

    fn halt(&self) {
        push(&self.0, "halt");
    }
}

struct Unused;

impl Mounts for Unused {
    fn prepare(&self) -> io::Result<()> {
        Ok(())
    }
    fn find_device(&self, _: Filesystem, _: &Uuid) -> io::Result<PathBuf> {
        Err(io::Error::other("unused"))
    }
    fn mount_image(&self, _: &Path) -> io::Result<()> {
        Ok(())
    }
    fn mount_root_disk(&self, _: &Path) -> io::Result<()> {
        Ok(())
    }
    fn overlay(&self) -> io::Result<()> {
        Ok(())
    }
    fn switch_root(&self) -> io::Result<()> {
        Ok(())
    }
}

impl Network for Unused {
    fn apply(&self, _: &NetworkConfig) -> io::Result<()> {
        Ok(())
    }
}

impl Entropy for Unused {
    fn mix_and_reseed(&self, _: &[u8]) -> io::Result<()> {
        Ok(())
    }
}

impl Clock for Unused {
    fn set_realtime(&self, _: i64) -> io::Result<()> {
        Ok(())
    }
}

fn system(log: &Log) -> System {
    System {
        mounts: Arc::new(Unused),
        network: Arc::new(Unused),
        power: Arc::new(Recorder(log.clone())),
        console: Arc::new(Recorder(log.clone())),
        entropy: Arc::new(Unused),
        clock: Arc::new(Unused),
    }
}

fn entries(log: &Log) -> Vec<String> {
    log.lock().unwrap_or_else(PoisonError::into_inner).clone()
}

#[test]
fn a_panic_on_any_thread_ends_the_machine_through_the_end_path() {
    let log = Log::default();
    guest_agent::end_on_panic(system(&log));

    // The control: nothing has ended the Machine before the panic.
    assert!(entries(&log).is_empty());

    let panicked = std::thread::spawn(|| {
        panic!("a thread of the agent failed");
    })
    .join();

    // The agent's hook stays no longer, so a failed assertion below is reported.
    drop(std::panic::take_hook());
    // In a test the hook returns, so the thread unwinds; in the agent it does not return.
    assert!(panicked.is_err());
    let entries = entries(&log);
    assert_eq!(entries.len(), 6, "{entries:?}");
    assert!(
        entries[0].starts_with("console: mvmd-guest-agent: panic: ")
            && entries[0].contains("a thread of the agent failed"),
        "{entries:?}"
    );
    assert_eq!(
        entries[1..],
        [
            "power_off",
            "console: mvmd-guest-agent: failed to power off: injected failure",
            "restart",
            "console: mvmd-guest-agent: failed to restart: injected failure",
            "halt",
        ]
    );
}
