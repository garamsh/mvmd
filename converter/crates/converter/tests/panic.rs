//! A panic ends the VM through the converter's own path. This is its own test binary because the
//! panic hook is the process's.

use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, PoisonError};

use converter::{Console, Mounts, Power, System};

type Log = Arc<Mutex<Vec<String>>>;

fn push(log: &Log, entry: impl Into<String>) {
    log.lock()
        .unwrap_or_else(PoisonError::into_inner)
        .push(entry.into());
}

/// Seams that do nothing the test looks at, except the ones that record the end of the VM.
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

    fn output_device(&self) -> io::Result<PathBuf> {
        Err(io::Error::other("unused"))
    }
}

fn entries(log: &Log) -> Vec<String> {
    log.lock().unwrap_or_else(PoisonError::into_inner).clone()
}

#[test]
fn a_panic_on_any_thread_ends_the_vm_through_the_end_path() {
    let log = Log::default();
    converter::end_on_panic(System {
        mounts: Arc::new(Unused),
        power: Arc::new(Recorder(log.clone())),
        console: Arc::new(Recorder(log.clone())),
    });

    // The control: nothing has ended the VM before the panic.
    assert!(entries(&log).is_empty());

    let panicked = std::thread::spawn(|| {
        panic!("a thread of the converter failed");
    })
    .join();

    // The converter's hook stays no longer, so a failed assertion below is reported.
    drop(std::panic::take_hook());
    // In a test the hook returns, so the thread unwinds; in the converter it does not return.
    assert!(panicked.is_err());
    let entries = entries(&log);
    assert_eq!(entries.len(), 6, "{entries:?}");
    assert!(
        entries[0].starts_with("console: mvmd-converter: panic: ")
            && entries[0].contains("a thread of the converter failed"),
        "{entries:?}"
    );
    assert_eq!(
        entries[1..],
        [
            "power_off",
            "console: mvmd-converter: failed to power off: injected failure",
            "restart",
            "console: mvmd-converter: failed to restart: injected failure",
            "halt",
        ]
    );
}
