//! Hot-plugged disks on a Linux kernel: found by a PCI rescan, which a VMM that sends no hotplug
//! notification leaves to the agent, as the one virtio block device the agent has not accounted
//! for (ADR 0028), and released by removing the device from the PCI bus while holding an
//! exclusive claim on its block device (ADR 0022). A disk named with a mount has its ext4
//! filesystem mounted in the booted root, and grown online when the disk grows (ADR 0050).

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, OnceLock};
use std::thread::sleep;
use std::time::{Duration, Instant};

use nix::mount::{MsFlags, mount, umount};

use crate::mounts::{create_parents, resolve_target};
use crate::system::{DiskDevice, DiskError, DiskMount, Disks};

/// How long a rescan or a removal is given to show in `/sys/class/block`.
const WAIT: Duration = Duration::from_secs(5);
/// How often the block devices are listed while waiting.
const POLL: Duration = Duration::from_millis(50);

/// Where a recorded disk is: the PCI address that removes it and the name it is known by, whether
/// the agent has released it, the mount it is to have and where that is made. A released record
/// stays, so a repeated release succeeds.
struct Record {
    address: String,
    name: String,
    released: bool,
    mount: Option<DiskMount>,
    /// Where the filesystem is mounted under the root, once the agent has mounted it.
    mounted: Option<PathBuf>,
}

/// Mounts the ext4 filesystem of a device at a directory, read-only or not.
type MountFs = fn(&Path, &Path, bool) -> io::Result<()>;

/// Unmounts the filesystem mounted at a directory.
type UnmountFs = fn(&Path) -> io::Result<()>;

/// Grows the ext4 filesystem mounted at a directory to fill a device of this many bytes.
type GrowFs = fn(&Path, u64) -> io::Result<()>;

fn mount_ext4(device: &Path, target: &Path, read_only: bool) -> io::Result<()> {
    let mut flags = MsFlags::MS_NODEV | MsFlags::MS_NOSUID;
    if read_only {
        flags |= MsFlags::MS_RDONLY;
    }
    mount(Some(device), target, Some("ext4"), flags, None::<&str>).map_err(io::Error::from)
}

fn unmount_fs(target: &Path) -> io::Result<()> {
    umount(target).map_err(io::Error::from)
}

nix::ioctl_write_ptr!(ext4_resize_fs, b'f', 16, u64);

/// Grows the filesystem with `EXT4_IOC_RESIZE_FS`, which takes the new size in blocks and does
/// nothing when the filesystem already has it.
#[expect(
    unsafe_code,
    reason = "EXT4_IOC_RESIZE_FS has no safe wrapper in a dependency"
)]
fn grow_ext4(target: &Path, device_bytes: u64) -> io::Result<()> {
    let directory = File::open(target)?;
    let block = u64::try_from(nix::sys::statfs::statfs(target)?.block_size())
        .map_err(io::Error::other)?
        .max(1);
    let blocks = device_bytes / block;
    // SAFETY: the descriptor is open for the call, and the pointer is to a `u64` that lives
    // through it, which is what the request reads.
    unsafe { ext4_resize_fs(directory.as_raw_fd(), &blocks) }
        .map(|_| ())
        .map_err(io::Error::from)
}

/// Claims a block device for the agent alone, or fails.
type Claim = fn(&Path) -> io::Result<File>;

/// Opens the block device exclusively. The kernel refuses with `EBUSY` while anything has claimed
/// the device or one of its partitions, a mounted filesystem or a swap area or a device-mapper
/// table among them, and refuses their claims while this one is held.
fn open_exclusive(device: &Path) -> io::Result<File> {
    OpenOptions::new()
        .read(true)
        .custom_flags(nix::libc::O_EXCL)
        .open(device)
}

/// Disks of a Linux kernel, through `/sys` and `/dev`.
pub struct LinuxDisks {
    sys: PathBuf,
    dev: PathBuf,
    /// The booted root the mounts are made under: `/` for the agent, a directory for a test.
    root: PathBuf,
    wait: Duration,
    claim: Claim,
    mount: MountFs,
    unmount: UnmountFs,
    grow_fs: GrowFs,
    /// Whether `mount_recorded` has been called, so a record is mounted as it is made. Read and
    /// written with `records` held.
    mounting: AtomicBool,
    /// The virtio block devices present when the agent started, by name with their PCI address,
    /// set once by `account_for_boot`.
    boot: OnceLock<HashMap<String, String>>,
    /// Held for the whole of a call, which serialises the calls and so keeps one rescan's new
    /// device from being taken for another's.
    records: Mutex<HashMap<String, Record>>,
}

impl Default for LinuxDisks {
    fn default() -> Self {
        Self::at("/sys", "/dev", WAIT)
    }
}

impl LinuxDisks {
    /// Disks found under `sys` and `dev`, each wait lasting up to `wait`. Tests point it at a
    /// tree of their own.
    pub fn at(sys: impl Into<PathBuf>, dev: impl Into<PathBuf>, wait: Duration) -> Self {
        Self {
            sys: sys.into(),
            dev: dev.into(),
            root: PathBuf::from("/"),
            wait,
            claim: open_exclusive,
            mount: mount_ext4,
            unmount: unmount_fs,
            grow_fs: grow_ext4,
            mounting: AtomicBool::new(false),
            boot: OnceLock::new(),
            records: Mutex::default(),
        }
    }

    /// The same disks, mounted under `root` instead of `/`.
    #[cfg(test)]
    fn under(mut self, root: impl Into<PathBuf>) -> Self {
        self.root = root.into();
        self
    }

    /// The same disks, mounted, unmounted and grown through these, for a tree whose device nodes
    /// are not block devices.
    #[cfg(test)]
    fn mounting_with(mut self, mount: MountFs, unmount: UnmountFs, grow_fs: GrowFs) -> Self {
        self.mount = mount;
        self.unmount = unmount;
        self.grow_fs = grow_fs;
        self
    }

    /// The same disks, claimed through `claim`, for a tree whose device nodes are not block
    /// devices and so cannot be claimed.
    #[cfg(test)]
    fn claiming(mut self, claim: Claim) -> Self {
        self.claim = claim;
        self
    }

    /// The virtio block devices on the PCI bus, whole devices only, by name with the PCI address
    /// they sit at. A device's address is the path component before its `virtio` one.
    fn block_devices(&self) -> io::Result<HashMap<String, String>> {
        let mut found = HashMap::new();
        for entry in fs::read_dir(self.sys.join("class/block"))? {
            let entry = entry?;
            if entry.path().join("partition").exists() {
                continue;
            }
            let Ok(real) = fs::canonicalize(entry.path()) else {
                continue;
            };
            let parts: Vec<_> = real.components().map(|c| c.as_os_str()).collect();
            let Some(virtio) = parts
                .iter()
                .position(|part| part.to_string_lossy().starts_with("virtio"))
            else {
                continue;
            };
            let Some(address) = virtio.checked_sub(1).map(|i| parts[i].to_string_lossy()) else {
                continue;
            };
            if address.contains(':') && address.contains('.') {
                found.insert(
                    entry.file_name().to_string_lossy().into_owned(),
                    address.into_owned(),
                );
            }
        }
        Ok(found)
    }

    fn describe(&self, name: &str) -> io::Result<DiskDevice> {
        let dir = self.sys.join("class/block").join(name);
        let sectors: u64 = fs::read_to_string(dir.join("size"))?
            .trim()
            .parse()
            .map_err(io::Error::other)?;
        let read_only = fs::read_to_string(dir.join("ro"))?.trim() == "1";
        Ok(DiskDevice {
            name: name.to_owned(),
            size_bytes: sectors * 512,
            read_only,
        })
    }

    /// Mount the record's filesystem at its path in the booted root, creating the directory,
    /// unless it has no mount or is mounted already.
    fn mount_one(&self, record: &mut Record) -> Result<(), DiskError> {
        let Some(wanted) = &record.mount else {
            return Ok(());
        };
        if record.mounted.is_some() {
            return Ok(());
        }
        let at_path = |error: io::Error| {
            DiskError::Failed(io::Error::new(
                error.kind(),
                format!("{}: {error}", wanted.path),
            ))
        };
        let target = resolve_target(&self.root, &wanted.path).map_err(at_path)?;
        create_parents(&target).map_err(at_path)?;
        if !fs::metadata(&target).map_err(at_path)?.is_dir() {
            return Err(at_path(io::Error::new(
                io::ErrorKind::InvalidInput,
                "is not a directory",
            )));
        }
        // A device that cannot be written is mounted read-only whatever was asked for.
        let read_only = wanted.read_only || self.describe(&record.name).map_err(at_path)?.read_only;
        (self.mount)(&self.dev.join(&record.name), &target, read_only).map_err(at_path)?;
        record.mounted = Some(target);
        Ok(())
    }

    fn wait_until(&self, done: impl Fn() -> bool) -> bool {
        let start = Instant::now();
        loop {
            if done() {
                return true;
            }
            if start.elapsed() >= self.wait {
                return false;
            }
            sleep(POLL);
        }
    }
}

fn failed(error: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> DiskError {
    DiskError::Failed(io::Error::other(error))
}

impl Disks for LinuxDisks {
    fn account_for_boot(&self) -> io::Result<()> {
        // Only the first listing is kept: a later call finds the set taken and changes nothing.
        let _ = self.boot.set(self.block_devices()?);
        Ok(())
    }

    fn find(&self, disk_id: &str, mount: Option<&DiskMount>) -> Result<DiskDevice, DiskError> {
        let mut records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        if let Some(wanted) = mount
            && let Some((other, _)) = records.iter().find(|(id, record)| {
                id.as_str() != disk_id
                    && !record.released
                    && record.mount.as_ref().is_some_and(|m| m.path == wanted.path)
            })
        {
            return Err(DiskError::Conflict(format!(
                "disk {other} is already to be mounted at {}",
                wanted.path
            )));
        }
        // A released disk is found by a rescan, as a new one is.
        if let Some(record) = records.get_mut(disk_id).filter(|record| !record.released) {
            if record.mount.as_ref() != mount {
                return Err(DiskError::Conflict(format!(
                    "disk {disk_id} is held with another mount than the one asked for"
                )));
            }
            if self.mounting.load(Ordering::SeqCst) {
                self.mount_one(record)?;
            }
            return self.describe(&record.name).map_err(DiskError::Failed);
        }
        fs::write(self.sys.join("bus/pci/rescan"), "1").map_err(DiskError::Failed)?;
        // Accounted for: the boot's devices and every device a record holds. A released record's
        // device is gone by then, so it holds nothing.
        let unaccounted = || -> io::Result<Vec<(String, String)>> {
            Ok(self
                .block_devices()?
                .into_iter()
                .filter(|(name, address)| {
                    self.boot.get().and_then(|boot| boot.get(name)) != Some(address)
                        && !records.values().any(|record| {
                            !record.released && record.name == *name && record.address == *address
                        })
                })
                .collect())
        };
        if !self.wait_until(|| unaccounted().is_ok_and(|found| !found.is_empty())) {
            return Err(failed(
                "no unaccounted virtio block device appeared after the rescan",
            ));
        }
        // One more interval, so a second device that came with the first is seen.
        sleep(POLL);
        let mut new = unaccounted().map_err(DiskError::Failed)?;
        let (Some((name, address)), true) = (new.pop(), new.is_empty()) else {
            return Err(failed("more than one unaccounted virtio block device"));
        };
        let device = self.describe(&name).map_err(DiskError::Failed)?;
        let record = records.entry(disk_id.to_owned()).insert_entry(Record {
            address,
            name,
            released: false,
            mount: mount.cloned(),
            mounted: None,
        });
        if self.mounting.load(Ordering::SeqCst) {
            self.mount_one(record.into_mut())?;
        }
        Ok(device)
    }

    fn mount_recorded(&self) -> Result<(), DiskError> {
        let mut records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        self.mounting.store(true, Ordering::SeqCst);
        let mut ids: Vec<String> = records.keys().cloned().collect();
        ids.sort();
        for id in ids {
            if let Some(record) = records.get_mut(&id).filter(|record| !record.released) {
                let _ = record;
            }
        }
        Ok(())
    }

    fn grow(&self, disk_id: &str) -> Result<u64, DiskError> {
        let records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        let record = records
            .get(disk_id)
            .filter(|record| !record.released)
            .ok_or(DiskError::Unknown)?;
        let size = self
            .describe(&record.name)
            .map_err(DiskError::Failed)?
            .size_bytes;
        if let Some(target) = &record.mounted {
            (self.grow_fs)(target, size).map_err(DiskError::Failed)?;
        }
        Ok(size)
    }

    fn release(&self, disk_id: &str) -> Result<(), DiskError> {
        let mut records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        let record = records.get_mut(disk_id).ok_or(DiskError::Unknown)?;
        if record.released {
            return Ok(());
        }
        if let Some(target) = &record.mounted {
            (self.unmount)(target).map_err(|error| {
                if error.raw_os_error() == Some(nix::libc::EBUSY) {
                    DiskError::MountBusy(
                        record
                            .mount
                            .as_ref()
                            .map_or_else(String::new, |m| m.path.clone()),
                    )
                } else {
                    DiskError::Failed(error)
                }
            })?;
            record.mounted = None;
        }
        // The claim is held until the device is gone, so nothing can mount it between the check
        // and the removal.
        let _claim = (self.claim)(&self.dev.join(&record.name)).map_err(|error| {
            if error.raw_os_error() == Some(nix::libc::EBUSY) {
                DiskError::InUse
            } else {
                DiskError::Failed(error)
            }
        })?;
        let remove = self
            .sys
            .join("bus/pci/devices")
            .join(&record.address)
            .join("remove");
        fs::write(remove, "1").map_err(DiskError::Failed)?;
        let device = self.sys.join("class/block").join(&record.name);
        if !self.wait_until(|| !Path::new(&device).exists()) {
            return Err(failed("the block device is still there after the removal"));
        }
        record.released = true;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};
    use std::sync::Arc;

    /// A fake `/sys` and `/dev` in a directory, with the root disk `vda` on the bus.
    struct Tree {
        dir: tempfile::TempDir,
    }

    impl Tree {
        fn new() -> io::Result<Self> {
            let tree = Self {
                dir: tempfile::tempdir()?,
            };
            fs::create_dir_all(tree.sys().join("class/block"))?;
            fs::create_dir_all(tree.sys().join("bus/pci/devices"))?;
            fs::write(tree.sys().join("bus/pci/rescan"), "")?;
            fs::create_dir_all(tree.dev())?;
            tree.device("vda", "0000:00:04.0", "254:0", false)?;
            Ok(tree)
        }

        fn sys(&self) -> PathBuf {
            self.dir.path().join("sys")
        }

        fn dev(&self) -> PathBuf {
            self.dir.path().join("dev")
        }

        fn disks(&self) -> LinuxDisks {
            let disks =
                LinuxDisks::at(self.sys(), self.dev(), Duration::from_millis(400)).claiming(claim);
            disks.account_for_boot().expect("the fake tree lists");
            disks
        }

        /// A virtio block device at a PCI address, as the kernel lays it out.
        fn device(&self, name: &str, address: &str, number: &str, ro: bool) -> io::Result<()> {
            device_in(&self.sys(), name, address, number, ro)
        }

        /// Makes something else claim the device `name`, as a mounted filesystem does, or stop.
        fn busy(&self, name: &str, on: bool) -> io::Result<()> {
            marker(&self.dev().join(name), "busy", on)
        }

        /// Makes the device node of `name` refuse to open for a reason other than a claim.
        fn unreadable(&self, name: &str, on: bool) -> io::Result<()> {
            marker(&self.dev().join(name), "unreadable", on)
        }
    }

    fn marker(device: &Path, kind: &str, on: bool) -> io::Result<()> {
        let path = device.with_extension(kind);
        if on {
            fs::write(path, "")
        } else {
            fs::remove_file(path)
        }
    }

    /// Stands in for the exclusive open, which only a block device can refuse: a device whose
    /// `busy` marker exists is claimed by someone else.
    fn claim(device: &Path) -> io::Result<File> {
        if device.with_extension("busy").exists() {
            return Err(io::Error::from_raw_os_error(nix::libc::EBUSY));
        }
        if device.with_extension("unreadable").exists() {
            return Err(io::Error::from_raw_os_error(nix::libc::EACCES));
        }
        OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(device)
    }

    /// Whether this process holds the file open.
    fn held(path: &Path) -> bool {
        let Ok(path) = fs::canonicalize(path) else {
            return false;
        };
        fs::read_dir("/proc/self/fd").is_ok_and(|fds| {
            fds.flatten()
                .any(|fd| fs::read_link(fd.path()).is_ok_and(|target| target == path))
        })
    }

    fn device_in(sys: &Path, name: &str, address: &str, number: &str, ro: bool) -> io::Result<()> {
        let real = sys
            .join("devices/pci0000:00")
            .join(address)
            .join("virtio1/block")
            .join(name);
        fs::create_dir_all(&real)?;
        fs::write(real.join("size"), "2048\n")?;
        fs::write(real.join("ro"), if ro { "1\n" } else { "0\n" })?;
        fs::write(real.join("dev"), format!("{number}\n"))?;
        fs::create_dir_all(sys.join("bus/pci/devices").join(address))?;
        fs::write(sys.join("bus/pci/devices").join(address).join("remove"), "")?;
        symlink(&real, sys.join("class/block").join(name))
    }

    /// Make the devices appear a moment after the rescan was asked for.
    fn appear_later(sys: PathBuf, names: Vec<(&'static str, &'static str)>) {
        std::thread::spawn(move || {
            sleep(Duration::from_millis(120));
            for (index, (name, address)) in names.into_iter().enumerate() {
                let number = format!("254:{}", 16 * (index + 1));
                let _ = device_in(&sys, name, address, &number, true);
            }
        });
    }

    /// The device appears only once the rescan has been asked for, as with a VMM that sends the
    /// guest no notification.
    fn appear_on_rescan(sys: PathBuf, name: &'static str, address: &'static str) {
        std::thread::spawn(move || {
            while fs::read_to_string(sys.join("bus/pci/rescan")).unwrap_or_default() != "1" {
                sleep(Duration::from_millis(10));
            }
            let _ = device_in(&sys, name, address, "254:16", true);
        });
    }

    /// The kernel drops the block device `name` when the PCI device at `address` is removed. The
    /// thread reports whether the device node was held open when the removal was asked for.
    fn unplug_on_remove(
        tree: &Tree,
        address: &'static str,
        name: &'static str,
    ) -> std::thread::JoinHandle<bool> {
        let (sys, dev) = (tree.sys(), tree.dev());
        std::thread::spawn(move || {
            let remove = sys.join("bus/pci/devices").join(address).join("remove");
            while fs::read_to_string(&remove).unwrap_or_default() != "1" {
                sleep(Duration::from_millis(10));
            }
            let claimed = held(&dev.join(name));
            let _ = fs::remove_file(sys.join("class/block").join(name));
            claimed
        })
    }

    fn ok<T>(result: Result<T, DiskError>) -> io::Result<T> {
        result.map_err(|e| io::Error::other(format!("{e:?}")))
    }

    fn found(tree: &Tree) -> io::Result<Arc<LinuxDisks>> {
        let disks = Arc::new(tree.disks());
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);
        ok(disks.find("data", None))?;
        Ok(disks)
    }

    /// Release `data`, whose device `vdb` the kernel drops once it is removed.
    fn released(tree: &Tree, disks: &LinuxDisks) -> io::Result<()> {
        let watcher = unplug_on_remove(tree, "0000:00:05.0", "vdb");
        let result = ok(disks.release("data"));
        watcher.join().map_err(|_| io::Error::other("watcher"))?;
        result
    }

    #[test]
    fn find_takes_the_device_that_appears_only_after_the_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        appear_on_rescan(tree.sys(), "vdb", "0000:00:05.0");

        let device = ok(disks.find("data", None))?;

        assert_eq!(
            device,
            DiskDevice {
                name: "vdb".into(),
                size_bytes: 2048 * 512,
                read_only: true
            }
        );
        assert_eq!(fs::read_to_string(tree.sys().join("bus/pci/rescan"))?, "1");
        Ok(())
    }

    #[test]
    fn find_takes_the_device_the_kernel_added_before_the_call() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        // A VMM that notifies the guest: the kernel has added the device on its own.
        tree.device("vdb", "0000:00:05.0", "254:16", true)?;

        assert_eq!(ok(disks.find("data", None))?.name, "vdb");
        // The rescan is still asked for.
        assert_eq!(fs::read_to_string(tree.sys().join("bus/pci/rescan"))?, "1");
        Ok(())
    }

    #[test]
    fn find_takes_the_device_that_appears_during_the_wait() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        // It comes some time after the call began, whatever the rescan did.
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);

        assert_eq!(ok(disks.find("data", None))?.name, "vdb");
        Ok(())
    }

    #[test]
    fn find_does_not_take_the_device_another_record_holds() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        appear_later(tree.sys(), vec![("vdc", "0000:00:06.0")]);

        assert_eq!(ok(disks.find("more", None))?.name, "vdc");
        Ok(())
    }

    #[test]
    fn find_without_a_new_device_fails_and_records_nothing() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();

        assert!(matches!(
            disks.find("data", None),
            Err(DiskError::Failed(_))
        ));
        assert!(matches!(disks.release("data"), Err(DiskError::Unknown)));

        // Control: the same disk ID is found once a device appears.
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);
        assert_eq!(ok(disks.find("data", None))?.name, "vdb");
        Ok(())
    }

    #[test]
    fn find_with_two_new_devices_fails_and_records_nothing() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        appear_later(
            tree.sys(),
            vec![("vdb", "0000:00:05.0"), ("vdc", "0000:00:06.0")],
        );

        assert!(matches!(
            disks.find("data", None),
            Err(DiskError::Failed(_))
        ));
        assert!(matches!(disks.release("data"), Err(DiskError::Unknown)));

        // Control: with the second device gone, one unaccounted device is left and the same call
        // finds it.
        fs::remove_file(tree.sys().join("class/block/vdc"))?;
        assert_eq!(ok(disks.find("data", None))?.name, "vdb");
        Ok(())
    }

    #[test]
    fn a_repeated_find_returns_the_record_without_a_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        fs::write(tree.sys().join("bus/pci/rescan"), "untouched")?;

        let again = ok(disks.find("data", None))?;

        assert_eq!(again.name, "vdb");
        assert_eq!(
            fs::read_to_string(tree.sys().join("bus/pci/rescan"))?,
            "untouched"
        );
        Ok(())
    }

    #[test]
    fn disks_found_one_after_another_each_get_a_record_and_each_is_released_by_its_id()
    -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        let wanted = [
            ("one", "vdb", "0000:00:05.0"),
            ("two", "vdc", "0000:00:06.0"),
            ("three", "vdd", "0000:00:07.0"),
        ];
        let mut names = Vec::new();
        for (id, name, address) in wanted {
            appear_later(tree.sys(), vec![(name, address)]);
            names.push(ok(disks.find(id, None))?.name);
        }
        let expected: Vec<_> = wanted
            .iter()
            .map(|(_, name, _)| (*name).to_owned())
            .collect();
        assert_eq!(names, expected, "each ID took its own device");
        let remove = |address: &str| {
            tree.sys()
                .join("bus/pci/devices")
                .join(address)
                .join("remove")
        };
        for (_, _, address) in wanted {
            assert_eq!(
                fs::read_to_string(remove(address))?,
                "",
                "nothing removed yet"
            );
        }

        // Released out of order: each release removes its own device and no other.
        for (index, (id, name, address)) in
            [wanted[1], wanted[0], wanted[2]].into_iter().enumerate()
        {
            let watcher = unplug_on_remove(&tree, address, name);
            ok(disks.release(id))?;
            let claimed = watcher.join().map_err(|_| io::Error::other("watcher"))?;
            assert!(claimed, "{id}: the claim was held when the removal began");
            let removed = wanted
                .iter()
                .filter(|(_, _, a)| fs::read_to_string(remove(a)).is_ok_and(|text| text == "1"))
                .count();
            assert_eq!(removed, index + 1, "{id}: one more device removed");
            assert_eq!(fs::read_to_string(remove(address))?, "1", "{id}");
        }
        Ok(())
    }

    #[test]
    fn release_removes_the_device_from_the_bus_while_holding_it() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        let watcher = unplug_on_remove(&tree, "0000:00:05.0", "vdb");

        let result = disks.release("data");
        let held_at_removal = watcher.join().map_err(|_| io::Error::other("watcher"))?;

        assert!(result.is_ok(), "{result:?}");
        assert!(held_at_removal, "the claim was held when the removal began");
        assert!(
            !held(&tree.dev().join("vdb")),
            "and is let go once the call is over"
        );
        Ok(())
    }

    #[test]
    fn release_refuses_a_claimed_device_and_removes_nothing() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        let remove = tree.sys().join("bus/pci/devices/0000:00:05.0/remove");
        // Another disk's claim is no reason to refuse this one.
        tree.busy("vda", true)?;
        tree.busy("vdb", true)?;

        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        assert_eq!(fs::read_to_string(&remove)?, "");
        assert!(!held(&tree.dev().join("vdb")));

        // Control: once the claim is gone, the same call removes it.
        tree.busy("vdb", false)?;
        released(&tree, &disks)?;
        assert_eq!(fs::read_to_string(&remove)?, "1");
        Ok(())
    }

    #[test]
    fn release_fails_and_does_not_refuse_when_the_device_cannot_be_opened() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        tree.unreadable("vdb", true)?;

        assert!(matches!(disks.release("data"), Err(DiskError::Failed(_))));
        assert_eq!(
            fs::read_to_string(tree.sys().join("bus/pci/devices/0000:00:05.0/remove"))?,
            ""
        );

        // Control: a claim that fails with EBUSY is the refusal, so the two are told apart.
        tree.unreadable("vdb", false)?;
        tree.busy("vdb", true)?;
        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        Ok(())
    }

    #[test]
    fn a_repeated_release_succeeds_and_an_unknown_disk_is_unknown() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        released(&tree, &disks)?;
        let remove = tree.sys().join("bus/pci/devices/0000:00:05.0/remove");
        fs::write(&remove, "untouched")?;
        // A repeat claims nothing and removes nothing, so another's claim does not matter to it.
        tree.busy("vdb", true)?;

        assert!(disks.release("data").is_ok());
        assert_eq!(fs::read_to_string(&remove)?, "untouched");
        assert!(matches!(disks.release("never"), Err(DiskError::Unknown)));
        Ok(())
    }

    #[test]
    fn a_find_of_a_released_disk_rescans_and_it_can_be_released_again() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        released(&tree, &disks)?;
        fs::write(tree.sys().join("bus/pci/rescan"), "untouched")?;
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);

        let again = ok(disks.find("data", None))?;

        assert_eq!(again.name, "vdb");
        assert_eq!(fs::read_to_string(tree.sys().join("bus/pci/rescan"))?, "1");
        // It is held again: a claim on it refuses, where a released record would not.
        tree.busy("vdb", true)?;
        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        Ok(())
    }

    /// The mounts of the tests that cannot mount: the mount is a marker file in the directory, and
    /// a `busy` file there makes the unmount refuse, as an open file does.
    fn fake_mount(_device: &Path, target: &Path, read_only: bool) -> io::Result<()> {
        fs::write(target.join(".mounted"), if read_only { "ro" } else { "rw" })
    }

    fn fake_unmount(target: &Path) -> io::Result<()> {
        if target.join("busy").exists() {
            return Err(io::Error::from_raw_os_error(nix::libc::EBUSY));
        }
        fs::remove_file(target.join(".mounted"))
    }

    fn fake_grow(target: &Path, device_bytes: u64) -> io::Result<()> {
        fs::write(target.join(".grown"), device_bytes.to_string())
    }

    /// A writable device appears a moment after the rescan was asked for.
    fn appear_writable(sys: PathBuf, name: &'static str, address: &'static str) {
        std::thread::spawn(move || {
            sleep(Duration::from_millis(120));
            let _ = device_in(&sys, name, address, "254:16", false);
        });
    }

    fn mount_at(path: &str, read_only: bool) -> DiskMount {
        DiskMount {
            path: path.into(),
            read_only,
        }
    }

    /// A disk found with `mount` in a tree whose root is a directory of its own, and the root.
    fn found_with(
        tree: &Tree,
        root: &Path,
        mount: Option<&DiskMount>,
    ) -> io::Result<Arc<LinuxDisks>> {
        let disks = Arc::new(tree.disks().under(root).mounting_with(
            fake_mount,
            fake_unmount,
            fake_grow,
        ));
        appear_writable(tree.sys(), "vdb", "0000:00:05.0");
        ok(disks.find("data", mount))?;
        Ok(disks)
    }

    #[test]
    fn a_disk_found_before_the_mounts_is_mounted_at_its_path_when_they_are_made() -> io::Result<()>
    {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), Some(&mount_at("/mnt/data", true)))?;
        let target = root.path().join("mnt/data");
        assert!(
            !target.exists(),
            "nothing is mounted before the boot says so"
        );

        ok(disks.mount_recorded())?;

        assert_eq!(fs::read_to_string(target.join(".mounted"))?, "ro");
        let mode = fs::metadata(&target)?.permissions().mode() & 0o777;
        assert_eq!(mode, 0o755);
        Ok(())
    }

    #[test]
    fn a_disk_found_with_no_path_is_mounted_nowhere_and_a_writable_one_is_mounted_writable()
    -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), None)?;
        ok(disks.mount_recorded())?;
        assert_eq!(fs::read_dir(root.path())?.count(), 0, "no path, no mount");

        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        found_with(&tree, root.path(), Some(&mount_at("/mnt/data", false)))
            .and_then(|disks| ok(disks.mount_recorded()))?;
        assert_eq!(
            fs::read_to_string(root.path().join("mnt/data/.mounted"))?,
            "rw"
        );
        Ok(())
    }

    #[test]
    fn a_device_that_cannot_be_written_is_mounted_read_only_whatever_was_asked() -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = Arc::new(tree.disks().under(root.path()).mounting_with(
            fake_mount,
            fake_unmount,
            fake_grow,
        ));
        let sys = tree.sys();
        std::thread::spawn(move || {
            sleep(Duration::from_millis(120));
            let _ = device_in(&sys, "vdb", "0000:00:05.0", "254:16", true);
        });
        ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;

        ok(disks.mount_recorded())?;

        assert_eq!(
            fs::read_to_string(root.path().join("mnt/data/.mounted"))?,
            "ro"
        );
        Ok(())
    }

    #[test]
    fn once_the_mounts_are_made_a_find_mounts_at_once_and_a_repeat_mounts_nothing_again()
    -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = Arc::new(tree.disks().under(root.path()).mounting_with(
            fake_mount,
            fake_unmount,
            fake_grow,
        ));
        ok(disks.mount_recorded())?;
        appear_writable(tree.sys(), "vdb", "0000:00:05.0");

        ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;
        let target = root.path().join("mnt/data");
        assert_eq!(fs::read_to_string(target.join(".mounted"))?, "rw");

        // A mount made again would find the marker's write refused: here, by removing it first.
        fs::remove_file(target.join(".mounted"))?;
        ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;
        ok(disks.mount_recorded())?;
        assert!(!target.join(".mounted").exists());
        Ok(())
    }

    #[test]
    fn a_second_disk_for_a_path_and_a_repeat_with_another_mount_are_conflicts() -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), Some(&mount_at("/mnt/data", false)))?;
        appear_writable(tree.sys(), "vdc", "0000:00:06.0");

        let same_path = disks.find("more", Some(&mount_at("/mnt/data", false)));
        let other_mount = disks.find("data", Some(&mount_at("/mnt/other", false)));
        let other_mode = disks.find("data", Some(&mount_at("/mnt/data", true)));
        let none = disks.find("data", None);

        for refused in [same_path, other_mount, other_mode, none] {
            assert!(
                matches!(refused, Err(DiskError::Conflict(_))),
                "{refused:?}"
            );
        }
        // The control: the same call again is a repeat, and so is another path for another disk.
        ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;
        ok(disks.find("more", Some(&mount_at("/mnt/more", false))))?;
        Ok(())
    }

    #[test]
    fn a_path_that_resolves_into_a_reserved_directory_is_not_mounted() -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        fs::create_dir_all(root.path().join("run/mvmd"))?;
        symlink("/run/mvmd", root.path().join("mnt"))?;
        let disks = found_with(&tree, root.path(), Some(&mount_at("/mnt/data", false)))?;

        let refused = disks.mount_recorded();

        assert!(matches!(refused, Err(DiskError::Failed(_))), "{refused:?}");
        assert!(!root.path().join("run/mvmd/data").exists());
        Ok(())
    }

    #[test]
    fn a_release_unmounts_first_and_a_busy_mount_fails_it_naming_the_path_and_removes_nothing()
    -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), Some(&mount_at("/mnt/data", false)))?;
        ok(disks.mount_recorded())?;
        let target = root.path().join("mnt/data");
        fs::write(target.join("busy"), "")?;
        let remove = tree.sys().join("bus/pci/devices/0000:00:05.0/remove");

        let refused = disks.release("data");

        assert!(
            matches!(&refused, Err(DiskError::MountBusy(path)) if path == "/mnt/data"),
            "{refused:?}"
        );
        assert!(target.join(".mounted").exists(), "still mounted");
        assert_eq!(fs::read_to_string(&remove)?, "", "nothing was removed");

        // The control: once nothing holds the mount, the same call unmounts it first.
        fs::remove_file(target.join("busy"))?;
        let watcher = unplug_on_remove(&tree, "0000:00:05.0", "vdb");
        let result = disks.release("data");
        watcher.join().map_err(|_| io::Error::other("watcher"))?;
        assert!(result.is_ok(), "{result:?}");
        assert!(!target.join(".mounted").exists(), "unmounted");
        Ok(())
    }

    #[test]
    fn a_grow_reads_the_size_again_and_grows_only_a_mounted_filesystem() -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), Some(&mount_at("/mnt/data", false)))?;
        ok(disks.mount_recorded())?;
        let size = tree
            .sys()
            .join("devices/pci0000:00/0000:00:05.0/virtio1/block/vdb/size");
        let grown = root.path().join("mnt/data/.grown");

        assert_eq!(ok(disks.grow("data"))?, 2048 * 512);
        assert_eq!(fs::read_to_string(&grown)?, (2048 * 512).to_string());

        fs::write(&size, "4096\n")?;
        assert_eq!(ok(disks.grow("data"))?, 4096 * 512);
        assert_eq!(fs::read_to_string(&grown)?, (4096 * 512).to_string());

        // The control: a disk with no mount has its size read and no filesystem touched.
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let bare = found_with(&tree, root.path(), None)?;
        assert_eq!(ok(bare.grow("data"))?, 2048 * 512);
        assert_eq!(fs::read_dir(root.path())?.count(), 0);
        assert!(matches!(bare.grow("never"), Err(DiskError::Unknown)));
        Ok(())
    }

    #[test]
    fn a_grow_of_a_released_disk_is_unknown() -> io::Result<()> {
        let tree = Tree::new()?;
        let root = tempfile::tempdir()?;
        let disks = found_with(&tree, root.path(), None)?;
        released(&tree, &disks)?;

        assert!(matches!(disks.grow("data"), Err(DiskError::Unknown)));
        Ok(())
    }

    /// The exclusive claim on a real block device, which only the kernel can answer: a loop
    /// device over a file. Mounting and attaching a loop device need root, so each test runs
    /// itself again under passwordless sudo, which the GitHub-hosted runner provides, and where
    /// it can get neither it fails and is not skipped.
    mod real_device {
        use super::*;
        use nix::errno::Errno;
        use nix::mount::{MsFlags, mount, umount};
        use std::os::unix::fs::{FileTypeExt, MetadataExt};
        use std::process::Command;

        /// Whether the caller is root and so goes on. Otherwise it runs the test `name` alone
        /// under sudo, requires that to have passed, and reports that the caller is done.
        fn root_or_rerun(name: &str) -> io::Result<bool> {
            // The owner of /proc/self is the user the process runs as.
            if fs::metadata("/proc/self")?.uid() == 0 {
                return Ok(true);
            }
            let path = module_path!().split_once("::").map_or("", |(_, rest)| rest);
            let full = format!("{path}::{name}");
            let out = Command::new("sudo")
                .arg("-n")
                .arg(std::env::current_exe()?)
                .args(["--exact", &full, "--test-threads=1", "--nocapture"])
                .output()?;
            let text = format!(
                "{}{}",
                String::from_utf8_lossy(&out.stdout),
                String::from_utf8_lossy(&out.stderr)
            );
            assert!(
                out.status.success(),
                "this test needs root or passwordless sudo, and this host gives neither, or the run as root failed: {text}"
            );
            assert!(
                text.contains("1 passed"),
                "the run under sudo did not run the test {full}: {text}"
            );
            Ok(false)
        }

        fn run(program: &str, args: &[&str]) -> io::Result<()> {
            let out = Command::new(program).args(args).output()?;
            if out.status.success() {
                Ok(())
            } else {
                Err(io::Error::other(format!(
                    "{program} {args:?}: {}",
                    String::from_utf8_lossy(&out.stderr)
                )))
            }
        }

        /// An ext4 filesystem on a loop device over a file in a temporary directory.
        struct Loop {
            dir: tempfile::TempDir,
            device: PathBuf,
        }

        impl Loop {
            fn new() -> io::Result<Self> {
                let dir = tempfile::tempdir()?;
                let image = dir.path().join("disk.img");
                File::create(&image)?.set_len(16 << 20)?;
                let out = Command::new("losetup")
                    .args(["--find", "--show"])
                    .arg(&image)
                    .output()?;
                if !out.status.success() {
                    return Err(io::Error::other(format!(
                        "losetup: {}",
                        String::from_utf8_lossy(&out.stderr)
                    )));
                }
                let device = PathBuf::from(String::from_utf8_lossy(&out.stdout).trim());
                let disk = Self { dir, device };
                // The setup is read back: a block device is what the claim is about.
                assert!(
                    fs::metadata(&disk.device)?.file_type().is_block_device(),
                    "{} is not a block device",
                    disk.device.display()
                );
                run("mkfs.ext4", &["-q", &disk.device.to_string_lossy()])?;
                fs::create_dir(disk.target())?;
                Ok(disk)
            }

            fn name(&self) -> String {
                self.device
                    .file_name()
                    .map(|name| name.to_string_lossy().into_owned())
                    .unwrap_or_default()
            }

            fn target(&self) -> PathBuf {
                self.dir.path().join("mnt")
            }

            fn mount(&self) -> Result<Mounted, Errno> {
                mount(
                    Some(&self.device),
                    &self.target(),
                    Some("ext4"),
                    MsFlags::empty(),
                    None::<&str>,
                )?;
                Ok(Mounted(self.target()))
            }
        }

        impl Drop for Loop {
            fn drop(&mut self) {
                let _ = run("losetup", &["--detach", &self.device.to_string_lossy()]);
            }
        }

        /// A mount, let go of when dropped.
        struct Mounted(PathBuf);

        impl Drop for Mounted {
            fn drop(&mut self) {
                let _ = umount(&self.0);
            }
        }

        #[test]
        fn a_mounted_device_refuses_the_claim_and_the_release_reports_it_in_use() -> io::Result<()>
        {
            if !root_or_rerun(
                "a_mounted_device_refuses_the_claim_and_the_release_reports_it_in_use",
            )? {
                return Ok(());
            }
            let disk = Loop::new()?;
            let name = disk.name();

            // Control: nothing mounted, and the claim is given.
            drop(open_exclusive(&disk.device)?);

            let mounted = disk.mount()?;
            let refused = open_exclusive(&disk.device).expect_err("a mounted device is claimed");
            assert_eq!(refused.raw_os_error(), Some(nix::libc::EBUSY), "{refused}");

            // The release path, on a disk found at a fake PCI address whose block device is the
            // real one.
            let tree = Tree::new()?;
            let disks = LinuxDisks::at(tree.sys(), "/dev", Duration::from_millis(400));
            disks.account_for_boot()?;
            let sys = tree.sys();
            let (new_name, new_address) = (name.clone(), "0000:00:05.0");
            let appear = std::thread::spawn(move || {
                sleep(Duration::from_millis(120));
                device_in(&sys, &new_name, new_address, "7:0", false)
            });
            ok(disks.find("data", None))?;
            appear.join().map_err(|_| io::Error::other("appear"))??;
            let remove = tree.sys().join("bus/pci/devices/0000:00:05.0/remove");

            assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
            assert_eq!(fs::read_to_string(&remove)?, "", "nothing was removed");

            // Control: unmounted, the same call goes through, holding the real device.
            drop(mounted);
            let (sys, device) = (tree.sys(), disk.device.clone());
            let unplug = std::thread::spawn(move || {
                while fs::read_to_string(sys.join("bus/pci/devices/0000:00:05.0/remove"))
                    .unwrap_or_default()
                    != "1"
                {
                    sleep(Duration::from_millis(10));
                }
                let claimed = held(&device);
                let _ = fs::remove_file(sys.join("class/block").join(name));
                claimed
            });
            let result = disks.release("data");
            let claimed = unplug.join().map_err(|_| io::Error::other("unplug"))?;
            assert!(result.is_ok(), "{result:?}");
            assert!(claimed, "the real device was held when the removal began");
            Ok(())
        }

        #[test]
        fn a_held_claim_refuses_a_mount_of_the_device() -> io::Result<()> {
            if !root_or_rerun("a_held_claim_refuses_a_mount_of_the_device")? {
                return Ok(());
            }
            let disk = Loop::new()?;

            let claim = open_exclusive(&disk.device)?;
            let refused = disk.mount().err();
            assert_eq!(refused, Some(Errno::EBUSY), "mounting a claimed device");

            // Control: once the claim is dropped, the same mount succeeds.
            drop(claim);
            let _mounted = disk.mount()?;
            Ok(())
        }

        /// Whether the kernel has an ext4 filesystem mounted at `path`.
        fn is_ext4(path: &Path) -> bool {
            nix::sys::statfs::statfs(path)
                .is_ok_and(|stat| stat.filesystem_type() == nix::sys::statfs::EXT4_SUPER_MAGIC)
        }

        /// The fake `/sys` entry of the device `name` at `address` says it has `sectors`.
        fn set_sectors(tree: &Tree, name: &str, address: &str, sectors: u64) -> io::Result<()> {
            let size = tree
                .sys()
                .join("devices/pci0000:00")
                .join(address)
                .join("virtio1/block")
                .join(name)
                .join("size");
            fs::write(size, format!("{sectors}\n"))
        }

        /// A tree whose bus has the loop device of `disk` at `address`, and the disks over it, which
        /// mount under `root` and claim the real device nodes.
        fn disks_over(
            tree: &Tree,
            disk: &Loop,
            address: &str,
            root: &Path,
        ) -> io::Result<LinuxDisks> {
            let disks = LinuxDisks::at(tree.sys(), "/dev", Duration::from_millis(400)).under(root);
            disks.account_for_boot()?;
            device_in(&tree.sys(), &disk.name(), address, "7:0", false)?;
            Ok(disks)
        }

        #[test]
        fn a_disk_is_mounted_at_its_path_with_nodev_nosuid_and_read_only_when_asked()
        -> io::Result<()> {
            if !root_or_rerun(
                "a_disk_is_mounted_at_its_path_with_nodev_nosuid_and_read_only_when_asked",
            )? {
                return Ok(());
            }
            let root = tempfile::tempdir()?;

            // A read-only attachment.
            let tree = Tree::new()?;
            let disk = Loop::new()?;
            let disks = disks_over(&tree, &disk, "0000:00:05.0", root.path())?;
            ok(disks.find("ro", Some(&mount_at("/mnt/ro", true))))?;
            ok(disks.mount_recorded())?;
            let target = root.path().join("mnt/ro");
            let _unmount = Mounted(target.clone());
            assert!(is_ext4(&target), "ext4 is mounted at the path");
            let flags = nix::sys::statvfs::statvfs(&target)?.flags();
            for flag in [
                nix::sys::statvfs::FsFlags::ST_RDONLY,
                nix::sys::statvfs::FsFlags::ST_NODEV,
                nix::sys::statvfs::FsFlags::ST_NOSUID,
            ] {
                assert!(flags.contains(flag), "{flag:?} in {flags:?}");
            }
            assert_eq!(fs::metadata(&target)?.permissions().mode() & 0o777, 0o755);
            let refused = fs::write(target.join("file"), "x").expect_err("a read-only mount");
            assert_eq!(refused.raw_os_error(), Some(nix::libc::EROFS), "{refused}");

            // The control: a writable attachment of another disk is mounted writable, and keeps
            // nodev and nosuid.
            let other_tree = Tree::new()?;
            let other = Loop::new()?;
            let others = disks_over(&other_tree, &other, "0000:00:06.0", root.path())?;
            ok(others.mount_recorded())?;
            ok(others.find("rw", Some(&mount_at("/mnt/rw", false))))?;
            let writable = root.path().join("mnt/rw");
            let _unmount_rw = Mounted(writable.clone());
            fs::write(writable.join("file"), "x")?;
            let flags = nix::sys::statvfs::statvfs(&writable)?.flags();
            assert!(!flags.contains(nix::sys::statvfs::FsFlags::ST_RDONLY));
            assert!(flags.contains(nix::sys::statvfs::FsFlags::ST_NODEV));
            assert!(flags.contains(nix::sys::statvfs::FsFlags::ST_NOSUID));
            Ok(())
        }

        #[test]
        fn a_grow_fills_the_resized_device_with_the_mounted_filesystem_online() -> io::Result<()> {
            if !root_or_rerun("a_grow_fills_the_resized_device_with_the_mounted_filesystem_online")?
            {
                return Ok(());
            }
            let root = tempfile::tempdir()?;
            let tree = Tree::new()?;
            let disk = Loop::new()?;
            let disks = disks_over(&tree, &disk, "0000:00:05.0", root.path())?;
            ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;
            ok(disks.mount_recorded())?;
            let target = root.path().join("mnt/data");
            let _unmount = Mounted(target.clone());
            let blocks = || nix::sys::statvfs::statvfs(&target).map(|stat| stat.blocks());
            let before = blocks()?;

            // The host resizes the disk and the kernel sees it: the filesystem is as it was.
            File::options()
                .write(true)
                .open(disk.dir.path().join("disk.img"))?
                .set_len(64 << 20)?;
            run(
                "losetup",
                &["--set-capacity", &disk.device.to_string_lossy()],
            )?;
            set_sectors(&tree, &disk.name(), "0000:00:05.0", (64 << 20) / 512)?;
            assert_eq!(blocks()?, before, "a resized device alone grows nothing");

            let size = ok(disks.grow("data"))?;

            assert_eq!(size, 64 << 20);
            let after = blocks()?;
            assert!(
                after >= before * 3,
                "the filesystem went from {before} to {after} blocks, of a device four times its size"
            );
            // Growing again changes nothing.
            ok(disks.grow("data"))?;
            assert_eq!(blocks()?, after);
            Ok(())
        }

        #[test]
        fn a_release_unmounts_the_filesystem_and_a_busy_one_fails_it_naming_the_path()
        -> io::Result<()> {
            if !root_or_rerun(
                "a_release_unmounts_the_filesystem_and_a_busy_one_fails_it_naming_the_path",
            )? {
                return Ok(());
            }
            let root = tempfile::tempdir()?;
            let tree = Tree::new()?;
            let disk = Loop::new()?;
            let disks = disks_over(&tree, &disk, "0000:00:05.0", root.path())?;
            ok(disks.find("data", Some(&mount_at("/mnt/data", false))))?;
            ok(disks.mount_recorded())?;
            let target = root.path().join("mnt/data");
            let _unmount = Mounted(target.clone());
            let open = File::create(target.join("held"))?;

            let refused = disks.release("data");

            assert!(
                matches!(&refused, Err(DiskError::MountBusy(path)) if path == "/mnt/data"),
                "{refused:?}"
            );
            assert!(is_ext4(&target), "a busy mount stays");

            // The control: with the file closed, the same call unmounts and releases the disk.
            drop(open);
            let (sys, device, name) = (tree.sys(), disk.device.clone(), disk.name());
            let unplug = std::thread::spawn(move || {
                while fs::read_to_string(sys.join("bus/pci/devices/0000:00:05.0/remove"))
                    .unwrap_or_default()
                    != "1"
                {
                    sleep(Duration::from_millis(10));
                }
                let claimed = held(&device);
                let _ = fs::remove_file(sys.join("class/block").join(name));
                claimed
            });
            let result = disks.release("data");
            unplug.join().map_err(|_| io::Error::other("unplug"))?;
            assert!(result.is_ok(), "{result:?}");
            assert!(!is_ext4(&target), "the filesystem is unmounted");
            Ok(())
        }
    }
}
