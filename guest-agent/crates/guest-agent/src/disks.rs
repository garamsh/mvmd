//! Hot-plugged disks on a Linux kernel: found by a PCI rescan, which the VMM's missing hotplug
//! notification leaves to the agent, and released by removing the device from the PCI bus while
//! holding an exclusive claim on its block device (ADR 0022).

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::thread::sleep;
use std::time::{Duration, Instant};

use crate::system::{DiskDevice, DiskError, Disks};

/// How long a rescan or a removal is given to show in `/sys/class/block`.
const WAIT: Duration = Duration::from_secs(5);
/// How often the block devices are listed while waiting.
const POLL: Duration = Duration::from_millis(50);

/// Where a recorded disk is: the PCI address that removes it and the name it is known by, and
/// whether the agent has released it. A released record stays, so a repeated release succeeds.
struct Record {
    address: String,
    name: String,
    released: bool,
}

/// Claims a block device for the agent alone, or fails.
type Claim = fn(&Path) -> io::Result<File>;

/// Opens the block device exclusively. The kernel refuses with `EBUSY` while anything has claimed
/// the device or one of its partitions, a mounted filesystem or a swap area or a device-mapper
/// table among them, and refuses their claims while this one is held.
fn open_exclusive(device: &Path) -> io::Result<File> {
    OpenOptions::new()
        .read(true)
        .open(device)
}

/// Disks of a Linux kernel, through `/sys` and `/dev`.
pub struct LinuxDisks {
    sys: PathBuf,
    dev: PathBuf,
    wait: Duration,
    claim: Claim,
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
            wait,
            claim: open_exclusive,
            records: Mutex::default(),
        }
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
    fn find(&self, disk_id: &str) -> Result<DiskDevice, DiskError> {
        let mut records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        // A released disk is found by a rescan, as a new one is.
        if let Some(record) = records.get(disk_id).filter(|record| !record.released) {
            return self.describe(&record.name).map_err(DiskError::Failed);
        }
        let before = self.block_devices().map_err(DiskError::Failed)?;
        fs::write(self.sys.join("bus/pci/rescan"), "1").map_err(DiskError::Failed)?;
        let appeared = |devices: &HashMap<String, String>| -> Vec<(String, String)> {
            devices
                .iter()
                .filter(|(name, _)| !before.contains_key(*name))
                .map(|(name, address)| (name.clone(), address.clone()))
                .collect()
        };
        let seen = self.wait_until(|| {
            self.block_devices()
                .is_ok_and(|after| !appeared(&after).is_empty())
        });
        if !seen {
            return Err(failed(
                "no new virtio block device appeared after the rescan",
            ));
        }
        // One more interval, so a second device that came with the first is seen.
        sleep(POLL);
        let mut new = appeared(&self.block_devices().map_err(DiskError::Failed)?);
        let (Some((name, address)), true) = (new.pop(), new.is_empty()) else {
            return Err(failed("more than one new virtio block device appeared"));
        };
        let device = self.describe(&name).map_err(DiskError::Failed)?;
        records.insert(
            disk_id.to_owned(),
            Record {
                address,
                name,
                released: false,
            },
        );
        Ok(device)
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
    use std::os::unix::fs::symlink;
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
            LinuxDisks::at(self.sys(), self.dev(), Duration::from_millis(400)).claiming(claim)
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
        ok(disks.find("data"))?;
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
    fn find_takes_the_device_that_appears_after_the_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);

        let device = ok(disks.find("data"))?;

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
    fn find_without_a_new_device_fails_and_records_nothing() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();

        assert!(matches!(disks.find("data"), Err(DiskError::Failed(_))));
        assert!(matches!(disks.release("data"), Err(DiskError::Unknown)));

        // Control: the same disk ID is found once a device appears.
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);
        assert_eq!(ok(disks.find("data"))?.name, "vdb");
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

        assert!(matches!(disks.find("data"), Err(DiskError::Failed(_))));
        assert!(matches!(disks.release("data"), Err(DiskError::Unknown)));

        // Control: with one device appearing after those two, the same call finds that one.
        appear_later(tree.sys(), vec![("vdd", "0000:00:07.0")]);
        assert_eq!(ok(disks.find("data"))?.name, "vdd");
        Ok(())
    }

    #[test]
    fn a_repeated_find_returns_the_record_without_a_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        fs::write(tree.sys().join("bus/pci/rescan"), "untouched")?;

        let again = ok(disks.find("data"))?;

        assert_eq!(again.name, "vdb");
        assert_eq!(
            fs::read_to_string(tree.sys().join("bus/pci/rescan"))?,
            "untouched"
        );
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

        let again = ok(disks.find("data"))?;

        assert_eq!(again.name, "vdb");
        assert_eq!(fs::read_to_string(tree.sys().join("bus/pci/rescan"))?, "1");
        // It is held again: a claim on it refuses, where a released record would not.
        tree.busy("vdb", true)?;
        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
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
            let sys = tree.sys();
            let (new_name, new_address) = (name.clone(), "0000:00:05.0");
            let appear = std::thread::spawn(move || {
                sleep(Duration::from_millis(120));
                device_in(&sys, &new_name, new_address, "7:0", false)
            });
            ok(disks.find("data"))?;
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
    }
}
