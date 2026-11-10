//! The guest's vCPUs and memory on a Linux kernel, through `/sys` and `/proc`. The kernel brings
//! hot-plugged memory online by itself and a hot-added vCPU not at all, so the agent writes `1`
//! to the `online` file of the lowest present CPUs that read `0`, and only while fewer than the
//! caller's target are online.

use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};

use crate::system::{GuestSize, Resources};

/// Writes `1` to a CPU's `online` file.
type Enable = fn(&Path) -> io::Result<()>;

fn write_one(online: &Path) -> io::Result<()> {
    OpenOptions::new().write(true).open(online)?.write_all(b"1")
}

/// The vCPUs and memory of a Linux kernel.
pub struct LinuxResources {
    cpus: PathBuf,
    meminfo: PathBuf,
    enable: Enable,
}

impl Default for LinuxResources {
    fn default() -> Self {
        Self::at("/sys/devices/system/cpu", "/proc/meminfo")
    }
}

impl LinuxResources {
    /// Resources read from the CPU directory `cpus` and the memory report `meminfo`. Tests point
    /// it at files of their own.
    pub fn at(cpus: impl Into<PathBuf>, meminfo: impl Into<PathBuf>) -> Self {
        Self {
            cpus: cpus.into(),
            meminfo: meminfo.into(),
            enable: write_one,
        }
    }

    /// The same resources, with CPUs enabled through `enable`, for a tree whose `online` files
    /// are plain files and so cannot refuse a write as a kernel does.
    #[cfg(test)]
    fn enabling(mut self, enable: Enable) -> Self {
        self.enable = enable;
        self
    }

    /// The CPUs present, by number with the directory of each. This rests on the kernel
    /// registering a CPU's directory when the CPU becomes present and removing it when it is
    /// unplugged, so a CPU that is possible but not present has none.
    fn present(&self) -> io::Result<Vec<(u32, PathBuf)>> {
        let mut cpus = Vec::new();
        for entry in fs::read_dir(&self.cpus)? {
            let entry = entry?;
            let name = entry.file_name();
            let number = name
                .to_string_lossy()
                .strip_prefix("cpu")
                .and_then(|digits| digits.parse::<u32>().ok());
            if let Some(number) = number {
                cpus.push((number, entry.path()));
            }
        }
        cpus.sort();
        Ok(cpus)
    }

    /// Whether the kernel has the CPU online: the boot CPU has no `online` file and is always on.
    fn is_online(dir: &Path) -> io::Result<bool> {
        match fs::read_to_string(dir.join("online")) {
            Ok(state) => Ok(state.trim() == "1"),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(true),
            Err(error) => Err(error),
        }
    }

    /// `MemTotal` of the memory report, in bytes.
    fn memory_bytes(&self) -> io::Result<u64> {
        let report = fs::read_to_string(&self.meminfo)?;
        let kib: u64 = report
            .lines()
            .find_map(|line| line.strip_prefix("MemTotal:"))
            .and_then(|rest| rest.trim().strip_suffix("kB"))
            .and_then(|digits| digits.trim().parse().ok())
            .ok_or_else(|| io::Error::other("the memory report has no MemTotal in kB"))?;
        Ok(kib * 1024)
    }
}

impl Resources for LinuxResources {
    fn online_vcpus(&self, target: u32) -> io::Result<GuestSize> {
        let mut online = 0;
        let mut offline = Vec::new();
        for (number, dir) in self.present()? {
            let named = |error: io::Error| io::Error::other(format!("cpu{number}: {error}"));
            if Self::is_online(&dir).map_err(named)? {
                online += 1;
            } else {
                offline.push((number, dir));
            }
        }
        for (number, dir) in offline {
            if online >= target {
                break;
            }
            let named = |error: io::Error| io::Error::other(format!("cpu{number}: {error}"));
            (self.enable)(&dir.join("online")).map_err(named)?;
            if !Self::is_online(&dir).map_err(named)? {
                return Err(io::Error::other(format!(
                    "cpu{number} is still offline after the write"
                )));
            }
            online += 1;
        }
        Ok(GuestSize {
            vcpus: online,
            memory_bytes: self.memory_bytes()?,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A fake CPU directory and memory report in a directory, with the boot CPU `cpu0` that has
    /// no `online` file.
    struct Tree {
        dir: tempfile::TempDir,
    }

    impl Tree {
        fn new() -> io::Result<Self> {
            let tree = Self {
                dir: tempfile::tempdir()?,
            };
            fs::create_dir_all(tree.cpus().join("cpu0"))?;
            // Directories that are not CPUs sit beside them, as `cpufreq` and `cpuidle` do.
            fs::create_dir_all(tree.cpus().join("cpufreq"))?;
            fs::create_dir_all(tree.cpus().join("cpuidle"))?;
            fs::write(
                tree.dir.path().join("meminfo"),
                "MemTotal:        2048 kB\nMemFree: 1 kB\n",
            )?;
            Ok(tree)
        }

        fn cpus(&self) -> PathBuf {
            self.dir.path().join("cpus")
        }

        fn resources(&self) -> LinuxResources {
            LinuxResources::at(self.cpus(), self.dir.path().join("meminfo")).enabling(enable)
        }

        /// A present CPU, online or not.
        fn cpu(&self, number: u32, online: bool) -> io::Result<()> {
            let dir = self.cpus().join(format!("cpu{number}"));
            fs::create_dir_all(&dir)?;
            fs::write(dir.join("online"), if online { "1" } else { "0" })
        }

        /// Makes the kernel refuse to bring the CPU online, or stop.
        fn refuse(&self, number: u32, on: bool) -> io::Result<()> {
            let marker = self.cpus().join(format!("cpu{number}")).join("refuse");
            if on {
                fs::write(marker, "")
            } else {
                fs::remove_file(marker)
            }
        }

        fn state(&self, number: u32) -> io::Result<String> {
            fs::read_to_string(self.cpus().join(format!("cpu{number}")).join("online"))
        }
    }

    /// Stands in for the write a kernel can refuse: a CPU whose `refuse` marker exists fails it.
    fn enable(online: &Path) -> io::Result<()> {
        if online.with_file_name("refuse").exists() {
            return Err(io::Error::from_raw_os_error(nix::libc::EIO));
        }
        write_one(online)
    }

    #[test]
    fn offline_cpus_come_online_up_to_the_target_and_the_report_counts_them() -> io::Result<()> {
        let tree = Tree::new()?;
        tree.cpu(1, true)?;
        tree.cpu(2, false)?;
        tree.cpu(3, false)?;
        tree.cpu(4, false)?;
        // The setup produced the state it names.
        assert_eq!(tree.state(2)?, "0");
        assert_eq!(tree.state(4)?, "0");

        let size = tree.resources().online_vcpus(5)?;

        assert_eq!(size.vcpus, 5);
        assert_eq!(size.memory_bytes, 2048 * 1024);
        for number in 1..=4 {
            assert_eq!(tree.state(number)?, "1");
        }
        Ok(())
    }

    #[test]
    fn only_the_lowest_offline_cpus_come_online_when_there_are_more_than_the_target_needs()
    -> io::Result<()> {
        let tree = Tree::new()?;
        for number in 1..=4 {
            tree.cpu(number, false)?;
        }

        let size = tree.resources().online_vcpus(3)?;

        assert_eq!(size.vcpus, 3);
        assert_eq!(tree.state(1)?, "1");
        assert_eq!(tree.state(2)?, "1");
        assert_eq!(tree.state(3)?, "0");
        assert_eq!(tree.state(4)?, "0");
        Ok(())
    }

    #[test]
    fn nothing_is_written_when_the_online_count_is_at_or_above_the_target() -> io::Result<()> {
        let tree = Tree::new()?;
        tree.cpu(1, true)?;
        tree.cpu(2, false)?;
        // A write to cpu2 would fail, so a call that succeeds did not make one.
        tree.refuse(2, true)?;
        let resources = tree.resources();

        let at = resources.online_vcpus(2)?;
        let above = resources.online_vcpus(1)?;

        assert_eq!((at.vcpus, above.vcpus), (2, 2));
        assert_eq!(tree.state(2)?, "0");
        // The control: a target above the online count does try cpu2.
        let error = resources.online_vcpus(3).unwrap_err();
        assert!(error.to_string().contains("cpu2"), "{error}");
        tree.refuse(2, false)?;
        assert_eq!(resources.online_vcpus(3)?.vcpus, 3);
        assert_eq!(tree.state(2)?, "1");
        Ok(())
    }

    #[test]
    fn a_call_with_nothing_to_bring_online_answers_the_same_counts() -> io::Result<()> {
        let tree = Tree::new()?;
        tree.cpu(1, false)?;
        let resources = tree.resources();
        let first = resources.online_vcpus(8)?;

        let second = resources.online_vcpus(8)?;

        assert_eq!(second, first);
        assert_eq!(second.vcpus, 2);
        // The control: a CPU that appears afterwards is counted, so the repeat counted what is
        // there and not what the first call remembered.
        tree.cpu(2, false)?;
        assert_eq!(resources.online_vcpus(8)?.vcpus, 3);
        Ok(())
    }

    #[test]
    fn a_guest_with_only_its_boot_cpu_reports_one() -> io::Result<()> {
        let tree = Tree::new()?;

        assert_eq!(tree.resources().online_vcpus(8)?.vcpus, 1);
        Ok(())
    }

    #[test]
    fn a_cpu_that_does_not_come_online_fails_the_call_naming_it() -> io::Result<()> {
        let tree = Tree::new()?;
        tree.cpu(1, false)?;
        tree.cpu(2, false)?;
        tree.refuse(2, true)?;
        let resources = tree.resources();

        let error = resources.online_vcpus(8).unwrap_err();

        assert!(error.to_string().contains("cpu2"), "{error}");
        assert!(!error.to_string().contains("cpu1"), "{error}");
        // The control: with the refusal lifted the same call succeeds, so cpu2's write was the
        // cause.
        tree.refuse(2, false)?;
        assert_eq!(resources.online_vcpus(8)?.vcpus, 3);
        Ok(())
    }

    #[test]
    fn a_write_the_kernel_accepts_but_does_not_apply_fails_the_call_naming_the_cpu()
    -> io::Result<()> {
        let tree = Tree::new()?;
        tree.cpu(1, false)?;
        let resources =
            LinuxResources::at(tree.cpus(), tree.dir.path().join("meminfo")).enabling(|_| Ok(()));

        let error = resources.online_vcpus(8).unwrap_err();

        assert!(error.to_string().contains("cpu1"), "{error}");
        // The control: a write that applies succeeds.
        assert_eq!(tree.resources().online_vcpus(8)?.vcpus, 2);
        Ok(())
    }

    #[test]
    fn a_memory_report_without_a_total_fails_the_call() -> io::Result<()> {
        let tree = Tree::new()?;
        fs::write(tree.dir.path().join("meminfo"), "MemFree: 1 kB\n")?;

        assert!(tree.resources().online_vcpus(8).is_err());
        // The control: the same tree with a total answers.
        fs::write(tree.dir.path().join("meminfo"), "MemTotal: 4 kB\n")?;
        assert_eq!(tree.resources().online_vcpus(8)?.memory_bytes, 4096);
        Ok(())
    }
}
