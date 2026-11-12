//! What the boot asks of the guest's kernel, as seams: the real implementations are in `mounts`
//! and `network` and the power-off below, and tests substitute their own.

use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use nix::sys::reboot::{RebootMode, reboot};
use thiserror::Error;

use crate::description::{BootFile, NetworkConfig};
use crate::end_method::EndMethodCell;
use crate::user::Users;
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

    /// Place `files` over the booted root: their content in a filesystem in memory that only the
    /// agent mounts, each bound read-only onto its path, with the directories and the empty target
    /// it needs created. Nothing is written to the root disk, and no error shows content.
    fn place_files(&self, files: &[BootFile]) -> io::Result<()>;
}

/// The Machine's network, as the boot and a restore configure it.
pub trait Network: Send + Sync {
    /// Make `config` the interface's whole configuration: what an earlier call applied is
    /// removed, so a repeat of one call changes nothing.
    fn apply(&self, config: &NetworkConfig) -> io::Result<()>;
}

/// Ending the Machine. Neither `power_off` nor `restart` returns on success, so a call that
/// returns has failed.
pub trait Power: Send + Sync {
    /// Power the Machine off.
    fn power_off(&self) -> io::Result<()>;

    /// Restart the Machine, for when powering off did not end it.
    fn restart(&self) -> io::Result<()>;

    /// Wait for ever, once neither of the others has ended the Machine. It returns only in a
    /// test.
    fn halt(&self);
}

/// The kernel command line the Machine booted with.
pub trait KernelCommandLine: Send + Sync {
    /// The whole line.
    fn read(&self) -> io::Result<String>;
}

/// The kernel's random pool, after a restore.
pub trait Entropy: Send + Sync {
    /// Mix `bytes` into the pool and count them as entropy, then make the kernel reseed its
    /// generator from the pool at once.
    fn mix_and_reseed(&self, bytes: &[u8]) -> io::Result<()>;
}

/// The wall clock.
pub trait Clock: Send + Sync {
    /// Set the wall clock to `unix_nanos` nanoseconds after the Unix epoch.
    fn set_realtime(&self, unix_nanos: i64) -> io::Result<()>;
}

/// What a hot-plugged disk looks like to the guest.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DiskDevice {
    /// The block device's name, for example `vdb`.
    pub name: String,
    /// Its size in bytes.
    pub size_bytes: u64,
    /// Whether it is read-only.
    pub read_only: bool,
}

/// Why a disk call failed.
#[derive(Debug, Error)]
pub enum DiskError {
    /// The kernel has the device claimed: it is mounted, held or in use as swap.
    #[error("the disk is in use")]
    InUse,
    /// The agent never found the disk.
    #[error("the agent has no record of the disk")]
    Unknown,
    /// The kernel's answer was not one device, or a call failed.
    #[error("the disk call failed: {0}")]
    Failed(#[source] io::Error),
}

/// The disks hot-plugged into the Machine. An implementation serialises its calls and keeps the
/// record of which device each disk ID is.
pub trait Disks: Send + Sync {
    /// Record the block devices present now as accounted for, which no `find` takes. Called once,
    /// after the kernel's device tree is mounted and before the agent listens. An error is a
    /// listing that failed, and the agent does not go on without it.
    fn account_for_boot(&self) -> io::Result<()>;

    /// Rescan for the one block device not accounted for and record it as `disk_id`; a `disk_id`
    /// the agent holds returns its record, and one it released is found by a rescan as a new
    /// one is.
    fn find(&self, disk_id: &str) -> Result<DiskDevice, DiskError>;

    /// Remove the device recorded as `disk_id` unless it is in use, and keep the record as
    /// released, so a repeat succeeds. A `disk_id` never found is `Unknown`.
    fn release(&self, disk_id: &str) -> Result<(), DiskError>;
}

/// What the kernel reports of the guest's size.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct GuestSize {
    /// The vCPUs online.
    pub vcpus: u32,
    /// The kernel's total memory, in bytes.
    pub memory_bytes: u64,
}

/// The guest's vCPUs and memory.
pub trait Resources: Send + Sync {
    /// Bring present, offline vCPUs online in number order, only while fewer than `target` are
    /// online, then report what the kernel has. At or above `target` nothing is written. An error
    /// names the CPU that did not come online.
    fn online_vcpus(&self, target: u32) -> io::Result<GuestSize>;
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
    /// The kernel's random pool.
    pub entropy: Arc<dyn Entropy>,
    /// The wall clock.
    pub clock: Arc<dyn Clock>,
    /// The disks hot-plugged into the Machine.
    pub disks: Arc<dyn Disks>,
    /// The guest's vCPUs and memory.
    pub resources: Arc<dyn Resources>,
    /// The kernel command line.
    pub command_line: Arc<dyn KernelCommandLine>,
    /// The guest's accounts, and how a command takes on a user.
    pub users: Arc<dyn Users>,
    /// The method that ends the Machine, set once the agent has read `command_line`.
    pub end_method: EndMethodCell,
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

    fn halt(&self) {
        loop {
            std::thread::sleep(std::time::Duration::from_secs(3600));
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
