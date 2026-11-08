//! The mounts of the boot on a Linux kernel, from the initramfs: the image and the root disk
//! found by filesystem UUID, the overlay over them, and the switch to it as the root.

use std::fs::{self, File};
use std::io::{self, Read};
use std::path::{Path, PathBuf};

use nix::mount::{MsFlags, mount};
use nix::unistd::{chdir, chroot, dup2_stderr, dup2_stdin, dup2_stdout};

use crate::system::Mounts;
use crate::uuid::{Filesystem, SUPERBLOCK_PROBE_BYTES, Uuid};

const IMAGE: &str = "/mnt/image";
const DISK: &str = "/mnt/disk";
const ROOT: &str = "/mnt/root";
const UPPER: &str = "/mnt/disk/upper";
const WORK: &str = "/mnt/disk/work";
const OVERLAY_OPTIONS: &str = "lowerdir=/mnt/image,upperdir=/mnt/disk/upper,workdir=/mnt/disk/work";

/// Mounts through the `mount` system call.
#[derive(Debug, Default)]
pub struct LinuxMounts;

fn mount_at(
    source: Option<&str>,
    target: &Path,
    fstype: Option<&str>,
    flags: MsFlags,
    options: Option<&str>,
) -> io::Result<()> {
    fs::create_dir_all(target)?;
    mount(source, target, fstype, flags, options).map_err(io::Error::from)
}

/// Create `dir`, which may already hold what an earlier boot of this root disk left.
fn create_dir_if_absent(dir: &str) -> io::Result<()> {
    match fs::create_dir(dir) {
        Err(error) if error.kind() != io::ErrorKind::AlreadyExists => Err(error),
        _ => Ok(()),
    }
}

impl Mounts for LinuxMounts {
    fn prepare(&self) -> io::Result<()> {
        let none = MsFlags::empty();
        mount_at(
            Some("devtmpfs"),
            Path::new("/dev"),
            Some("devtmpfs"),
            none,
            None,
        )?;
        mount_at(Some("proc"), Path::new("/proc"), Some("proc"), none, None)?;
        mount_at(Some("sysfs"), Path::new("/sys"), Some("sysfs"), none, None)?;
        // The kernel opens /dev/console for init only when the initramfs holds the node, which
        // it need not: attach it here. With no console configured it cannot be opened, and the
        // agent then has no stream to report on.
        if let Ok(console) = File::options().read(true).write(true).open("/dev/console") {
            let _ = dup2_stdin(&console);
            let _ = dup2_stdout(&console);
            let _ = dup2_stderr(&console);
        }
        Ok(())
    }

    fn find_device(&self, filesystem: Filesystem, uuid: &Uuid) -> io::Result<PathBuf> {
        let mut found = Vec::new();
        for entry in fs::read_dir("/sys/class/block")? {
            let device = Path::new("/dev").join(entry?.file_name());
            let mut head = [0u8; SUPERBLOCK_PROBE_BYTES];
            // A device that cannot be read, or is smaller than the probe, holds no filesystem
            // of ours.
            let probed = File::open(&device).and_then(|mut file| file.read_exact(&mut head));
            if probed.is_ok() && filesystem.uuid_in(&head) == Some(*uuid) {
                found.push(device);
            }
        }
        match found.as_slice() {
            [device] => Ok(device.clone()),
            [] => Err(io::Error::new(
                io::ErrorKind::NotFound,
                format!("no block device carries a {filesystem:?} with UUID {uuid}"),
            )),
            several => Err(io::Error::other(format!(
                "{} block devices carry a {filesystem:?} with UUID {uuid}",
                several.len()
            ))),
        }
    }

    fn mount_image(&self, device: &Path) -> io::Result<()> {
        mount_at(
            device.to_str(),
            Path::new(IMAGE),
            Some("erofs"),
            MsFlags::MS_RDONLY,
            None,
        )
    }

    fn mount_root_disk(&self, device: &Path) -> io::Result<()> {
        mount_at(
            device.to_str(),
            Path::new(DISK),
            Some("ext4"),
            MsFlags::empty(),
            None,
        )?;
        create_dir_if_absent(UPPER)?;
        create_dir_if_absent(WORK)
    }

    fn overlay(&self) -> io::Result<()> {
        mount_at(
            Some("overlay"),
            Path::new(ROOT),
            Some("overlay"),
            MsFlags::empty(),
            Some(OVERLAY_OPTIONS),
        )
    }

    fn switch_root(&self) -> io::Result<()> {
        // The pseudo filesystems move into the new root, which creates their mount points in the
        // overlay's upper layer when the image has none.
        for name in ["dev", "proc", "sys"] {
            let target = Path::new(ROOT).join(name);
            fs::create_dir_all(&target)?;
            mount(
                Some(Path::new("/").join(name).as_path()),
                &target,
                None::<&str>,
                MsFlags::MS_MOVE,
                None::<&str>,
            )
            .map_err(io::Error::from)?;
        }
        // What switch_root(8) does: move the new root over the old one and enter it.
        chdir(ROOT)?;
        mount(Some("."), "/", None::<&str>, MsFlags::MS_MOVE, None::<&str>)
            .map_err(io::Error::from)?;
        chroot(".")?;
        chdir("/")?;
        Ok(())
    }
}
