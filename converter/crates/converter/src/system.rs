//! What the converter asks of the guest's kernel, as seams: the Linux implementations are here and
//! tests substitute their own.

use std::fs::{self, File};
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use nix::mount::{MsFlags, mount};
use nix::sys::reboot::{RebootMode, reboot};
use nix::unistd::{dup2_stderr, dup2_stdin, dup2_stdout};

/// How long the output device may take to appear, since the kernel registers a virtio device
/// while init already runs.
const DEVICE_WAIT: Duration = Duration::from_secs(10);

/// The mounts of the boot, and the output device they make findable.
pub trait Mounts: Send + Sync {
    /// Mount what the converter needs (device nodes, device information, scratch space for
    /// `mkfs.erofs`) and attach the console to the converter's standard streams.
    fn prepare(&self) -> io::Result<()>;

    /// The one block device the VM was given, which receives the EROFS. An error when there is
    /// none or more than one.
    fn output_device(&self) -> io::Result<PathBuf>;
}

/// Ending the VM. Neither `power_off` nor `restart` returns on success, so a call that returns
/// has failed.
pub trait Power: Send + Sync {
    /// Power the VM off.
    fn power_off(&self) -> io::Result<()>;

    /// Restart the VM, for when powering off did not end it.
    fn restart(&self) -> io::Result<()>;

    /// Wait for ever, once neither of the others has ended the VM. It returns only in a test.
    fn halt(&self);
}

/// Where the converter writes the lines that say why it ended, which the VMM's console carries
/// to the daemon.
pub trait Console: Send + Sync {
    /// Write one line.
    fn line(&self, text: &str);
}

/// The kernel-facing parts the converter is composed with.
#[derive(Clone)]
pub struct System {
    /// The mounts of the boot.
    pub mounts: Arc<dyn Mounts>,
    /// Powering the VM off.
    pub power: Arc<dyn Power>,
    /// The console.
    pub console: Arc<dyn Console>,
}

/// Mounts through the `mount` system call, from the initramfs.
#[derive(Debug, Default)]
pub struct LinuxMounts;

fn mount_at(fstype: &str, target: &str) -> io::Result<()> {
    fs::create_dir_all(target)?;
    mount(
        Some(fstype),
        Path::new(target),
        Some(fstype),
        MsFlags::empty(),
        None::<&str>,
    )
    .map_err(io::Error::from)
}

impl Mounts for LinuxMounts {
    fn prepare(&self) -> io::Result<()> {
        mount_at("devtmpfs", "/dev")?;
        mount_at("sysfs", "/sys")?;
        // `mkfs.erofs` spools the file data of a layer into its temporary directory, which here
        // is memory.
        mount_at("tmpfs", "/tmp")?;
        // Current kernels attach /dev/console to init before it runs; attaching it again through
        // devtmpfs covers a kernel that did not. With no console configured it cannot be
        // opened, and the converter then has no stream to report on.
        if let Ok(console) = File::options().read(true).write(true).open("/dev/console") {
            let _ = dup2_stdin(&console);
            let _ = dup2_stdout(&console);
            let _ = dup2_stderr(&console);
        }
        Ok(())
    }

    fn output_device(&self) -> io::Result<PathBuf> {
        let deadline = std::time::Instant::now() + DEVICE_WAIT;
        loop {
            let mut found = Vec::new();
            for entry in fs::read_dir("/sys/block")? {
                let name = entry?.file_name();
                // The VM has virtio block devices only; the kernel may also list RAM and loop
                // devices it was built with.
                if name.to_string_lossy().starts_with("vd") {
                    found.push(Path::new("/dev").join(name));
                }
            }
            match found.as_slice() {
                [device] => return Ok(device.clone()),
                [] if std::time::Instant::now() < deadline => {
                    std::thread::sleep(Duration::from_millis(50));
                }
                [] => {
                    return Err(io::Error::new(
                        io::ErrorKind::NotFound,
                        "no virtio block device appeared",
                    ));
                }
                several => {
                    return Err(io::Error::other(format!(
                        "{} virtio block devices, expected one",
                        several.len()
                    )));
                }
            }
        }
    }
}

/// Ends the VM through the `reboot` system call.
#[derive(Debug, Default)]
pub struct LinuxPower;

impl Power for LinuxPower {
    fn power_off(&self) -> io::Result<()> {
        nix::unistd::sync();
        reboot(RebootMode::RB_POWER_OFF)
            .map(|never| match never {})
            .map_err(io::Error::from)
    }

    fn restart(&self) -> io::Result<()> {
        nix::unistd::sync();
        reboot(RebootMode::RB_AUTOBOOT)
            .map(|never| match never {})
            .map_err(io::Error::from)
    }

    fn halt(&self) {
        loop {
            std::thread::sleep(Duration::from_secs(3600));
        }
    }
}

/// Writes to the standard error, which `Mounts::prepare` attached to the console.
#[derive(Debug, Default)]
pub struct LinuxConsole;

impl Console for LinuxConsole {
    fn line(&self, text: &str) {
        eprintln!("{text}");
    }
}
