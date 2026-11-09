//! Hot-plugged disks on a Linux kernel: found by a PCI rescan, which the VMM's missing hotplug
//! notification leaves to the agent, and released by removing the device from the PCI bus once
//! nothing uses it (ADR 0022).

use std::collections::{BTreeSet, HashMap};
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::thread::sleep;
use std::time::{Duration, Instant};

use crate::system::{DiskDevice, DiskError, Disks};

/// How long a rescan or a removal is given to show in `/sys/class/block`.
const WAIT: Duration = Duration::from_secs(5);
/// How often the block devices are listed while waiting.
const POLL: Duration = Duration::from_millis(50);

/// Where a recorded disk is: the PCI address that removes it and the name it is known by.
struct Record {
    address: String,
    name: String,
}

/// Disks of a Linux kernel, through `/sys` and `/proc`.
pub struct LinuxDisks {
    sys: PathBuf,
    proc: PathBuf,
    wait: Duration,
    /// Held for the whole of a call, which serialises the calls and so keeps one rescan's new
    /// device from being taken for another's.
    records: Mutex<HashMap<String, Record>>,
}

impl Default for LinuxDisks {
    fn default() -> Self {
        Self::at("/sys", "/proc", WAIT)
    }
}

impl LinuxDisks {
    /// Disks found under `sys` and `proc`, each wait lasting up to `wait`. Tests point it at a
    /// tree of their own.
    pub fn at(sys: impl Into<PathBuf>, proc: impl Into<PathBuf>, wait: Duration) -> Self {
        Self {
            sys: sys.into(),
            proc: proc.into(),
            wait,
            records: Mutex::default(),
        }
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

    /// The device numbers and the names of the device and its partitions.
    fn identities(&self, name: &str) -> io::Result<(BTreeSet<String>, Vec<String>)> {
        let mut names = vec![name.to_owned()];
        let dir = self.sys.join("class/block").join(name);
        for entry in fs::read_dir(&dir)? {
            let entry = entry?;
            if entry.path().join("partition").exists() {
                names.push(entry.file_name().to_string_lossy().into_owned());
            }
        }
        let mut numbers = BTreeSet::new();
        for each in &names {
            let number = fs::read_to_string(self.sys.join("class/block").join(each).join("dev"))?;
            numbers.insert(number.trim().to_owned());
        }
        Ok((numbers, names))
    }

    /// Whether a mount of any process, or a holder in sysfs, uses the device or a partition.
    fn in_use(&self, name: &str) -> io::Result<bool> {
        let (numbers, names) = self.identities(name)?;
        for each in &names {
            let holders = self.sys.join("class/block").join(each).join("holders");
            if fs::read_dir(holders).is_ok_and(|mut held| held.next().is_some()) {
                return Ok(true);
            }
        }
        let sources: Vec<String> = names.iter().map(|each| format!("/dev/{each}")).collect();
        for process in fs::read_dir(&self.proc)? {
            let process = process?;
            let numeric = process
                .file_name()
                .to_string_lossy()
                .bytes()
                .all(|b| b.is_ascii_digit());
            if !numeric {
                continue;
            }
            // A process that ended since the listing has no table to read.
            let Ok(table) = fs::read_to_string(process.path().join("mountinfo")) else {
                continue;
            };
            if table.lines().any(|line| mounts(line, &numbers, &sources)) {
                return Ok(true);
            }
        }
        Ok(false)
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

/// Whether one line of a mount table is a mount of a device in `numbers` or of a source in
/// `sources`. The device number is the third field; the source is the second after the ` - `.
fn mounts(line: &str, numbers: &BTreeSet<String>, sources: &[String]) -> bool {
    let mut fields = line.split_whitespace();
    if fields.nth(2).is_some_and(|number| numbers.contains(number)) {
        return true;
    }
    let source = line
        .split_once(" - ")
        .and_then(|(_, tail)| tail.split_whitespace().nth(1));
    source.is_some_and(|source| sources.iter().any(|known| known == source))
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
        if let Some(record) = records.get(disk_id) {
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
        records.insert(disk_id.to_owned(), Record { address, name });
        Ok(device)
    }

    fn release(&self, disk_id: &str) -> Result<(), DiskError> {
        let mut records = self
            .records
            .lock()
            .map_err(|_| failed("the disk records are unusable"))?;
        let record = records.get(disk_id).ok_or(DiskError::Unknown)?;
        if self.in_use(&record.name).map_err(DiskError::Failed)? {
            return Err(DiskError::InUse);
        }
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
        records.remove(disk_id);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::sync::Arc;

    /// A fake `/sys` and `/proc` in a directory, with the root disk `vda` on the bus.
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
            fs::create_dir_all(tree.proc())?;
            tree.device("vda", "0000:00:04.0", "254:0", false)?;
            Ok(tree)
        }

        fn sys(&self) -> PathBuf {
            self.dir.path().join("sys")
        }

        fn proc(&self) -> PathBuf {
            self.dir.path().join("proc")
        }

        fn disks(&self) -> LinuxDisks {
            LinuxDisks::at(self.sys(), self.proc(), Duration::from_millis(400))
        }

        /// A virtio block device at a PCI address, as the kernel lays it out.
        fn device(&self, name: &str, address: &str, number: &str, ro: bool) -> io::Result<()> {
            device_in(&self.sys(), name, address, number, ro)
        }

        fn mounted(&self, number: &str, source: &str) -> io::Result<()> {
            let table = format!(
                "36 1 {number} / /mnt rw - ext4 {source} rw\n37 1 0:5 / /proc rw - proc proc rw\n"
            );
            fs::create_dir_all(self.proc().join("7"))?;
            fs::write(self.proc().join("7/mountinfo"), table)
        }
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

    /// Make `vdb` appear a moment after the rescan was asked for.
    fn appear_later(sys: PathBuf, names: Vec<(&'static str, &'static str)>) {
        std::thread::spawn(move || {
            sleep(Duration::from_millis(120));
            for (index, (name, address)) in names.into_iter().enumerate() {
                let number = format!("254:{}", 16 * (index + 1));
                let _ = device_in(&sys, name, address, &number, true);
            }
        });
    }

    #[test]
    fn find_takes_the_device_that_appears_after_the_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);

        let device = disks
            .find("data")
            .map_err(|e| io::Error::other(format!("{e:?}")))?;

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
        Ok(())
    }

    #[test]
    fn a_repeated_find_returns_the_record_without_a_rescan() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = tree.disks();
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);
        disks
            .find("data")
            .map_err(|e| io::Error::other(format!("{e:?}")))?;
        fs::write(tree.sys().join("bus/pci/rescan"), "untouched")?;

        let again = disks
            .find("data")
            .map_err(|e| io::Error::other(format!("{e:?}")))?;

        assert_eq!(again.name, "vdb");
        assert_eq!(
            fs::read_to_string(tree.sys().join("bus/pci/rescan"))?,
            "untouched"
        );
        Ok(())
    }

    fn found(tree: &Tree) -> io::Result<Arc<LinuxDisks>> {
        let disks = Arc::new(tree.disks());
        appear_later(tree.sys(), vec![("vdb", "0000:00:05.0")]);
        disks
            .find("data")
            .map_err(|e| io::Error::other(format!("{e:?}")))?;
        Ok(disks)
    }

    #[test]
    fn release_removes_the_device_from_the_bus_and_forgets_it() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        // The kernel drops the block device when the PCI device is removed.
        let sys = tree.sys();
        let watcher = std::thread::spawn(move || {
            let remove = sys.join("bus/pci/devices/0000:00:05.0/remove");
            while fs::read_to_string(&remove).unwrap_or_default() != "1" {
                sleep(Duration::from_millis(10));
            }
            let _ = fs::remove_file(sys.join("class/block/vdb"));
        });

        let released = disks.release("data");
        watcher.join().map_err(|_| io::Error::other("watcher"))?;

        assert!(released.is_ok(), "{released:?}");
        assert!(matches!(disks.release("data"), Err(DiskError::Unknown)));
        Ok(())
    }

    #[test]
    fn release_refuses_a_device_mounted_by_number() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        tree.mounted("254:16", "/dev/other")?;

        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        assert_eq!(
            fs::read_to_string(tree.sys().join("bus/pci/devices/0000:00:05.0/remove"))?,
            ""
        );
        Ok(())
    }

    #[test]
    fn release_refuses_a_device_mounted_by_source() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        tree.mounted("0:99", "/dev/vdb")?;

        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        Ok(())
    }

    #[test]
    fn release_refuses_a_device_whose_partition_is_mounted() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        let partition = tree.sys().join("class/block/vdb/vdb1");
        fs::create_dir_all(&partition)?;
        fs::write(partition.join("partition"), "1")?;
        fs::write(partition.join("dev"), "254:17\n")?;
        let listed = tree.sys().join("class/block/vdb1");
        symlink(&partition, listed)?;
        tree.mounted("254:17", "/dev/x")?;

        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        Ok(())
    }

    #[test]
    fn release_refuses_a_device_with_holders() -> io::Result<()> {
        let tree = Tree::new()?;
        let disks = found(&tree)?;
        fs::create_dir_all(tree.sys().join("class/block/vdb/holders/dm-0"))?;

        assert!(matches!(disks.release("data"), Err(DiskError::InUse)));
        Ok(())
    }

    #[test]
    fn release_of_an_unknown_disk_is_unknown() -> io::Result<()> {
        let tree = Tree::new()?;

        assert!(matches!(
            tree.disks().release("nope"),
            Err(DiskError::Unknown)
        ));
        Ok(())
    }
}
