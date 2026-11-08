//! What the boot asks of the guest's kernel, as seams: the real implementations are in `mounts`
//! and `network` and the power-off below, and tests substitute their own.

use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use nix::sys::reboot::{RebootMode, reboot};

use crate::description::NetworkConfig;
use crate::uuid::{Filesystem, Uuid};

/// The mounts of the boot. Where each filesystem is mounted is the implementation's.
pub trait Mounts: Send + Sync {
    /// Mount the pseudo filesystems the agent needs (devices, process information, kernel
    /// information) and attach the console to the agent's standard streams.
    fn prepare(&self) -> io::Result<()>;

    /// The block device whose `filesystem` carries `uuid`; an error when none or several do.
    fn find_device(&self, filesystem: Filesystem, uuid: &Uuid) -> io::Result<PathBuf>;

    /// Mount the image's EROFS from `device`, read-only.
    fn mount_image(&self, device: &Path) -> io::Result<()>;

    /// Mount the root disk's ext4 from `device`, with the directories the overlay needs on it.
    fn mount_root_disk(&self, device: &Path) -> io::Result<()>;

    /// Overlay the root disk on the image.
    fn overlay(&self) -> io::Result<()>;

    /// Make the overlay the root of this process and of every command it starts.
    fn switch_root(&self) -> io::Result<()>;
}

/// The Machine's network, as the boot and a restore configure it.
pub trait Network: Send + Sync {
    /// Make `config` the interface's whole configuration: what an earlier call applied is
    /// removed, so a repeat of one call changes nothing.
    fn apply(&self, config: &NetworkConfig) -> io::Result<()>;
}

/// Ending the Machine. Neither call returns on success, so a call that returns has failed.
pub trait Power: Send + Sync {
    /// Power the Machine off.
    fn power_off(&self) -> io::Result<()>;

    /// Restart the Machine, for when powering off did not end it.
    fn restart(&self) -> io::Result<()>;
}

/// Where the agent writes the lines that say why it ended, which the VMM's console carries to
/// the daemon.
pub trait Console: Send + Sync {
    /// Write one line.
    fn line(&self, text: &str);
}

/// The kernel-facing parts the agent is composed with.
#[derive(Clone)]
pub struct System {
    /// The mounts of the boot.
    pub mounts: Arc<dyn Mounts>,
    /// The Machine's network.
    pub network: Arc<dyn Network>,
    /// Powering the Machine off.
    pub power: Arc<dyn Power>,
    /// The console.
    pub console: Arc<dyn Console>,
}

/// Ends the Machine through the `reboot` system call.
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
}

/// Writes to the standard error, which `Mounts::prepare` attached to the console.
#[derive(Debug, Default)]
pub struct LinuxConsole;

impl Console for LinuxConsole {
    fn line(&self, text: &str) {
        eprintln!("{text}");
    }
}
