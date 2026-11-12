//! The mounts of the boot on a Linux kernel, from the initramfs: the image and the root disk
//! found by filesystem UUID, the overlay over them, and the switch to it as the root.

use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use nix::mount::{MsFlags, mount};
use nix::unistd::{chdir, chroot, dup2_stderr, dup2_stdin, dup2_stdout};

use crate::description::{BootFile, is_reserved};
use crate::system::Mounts;
use crate::uuid::{Filesystem, SUPERBLOCK_PROBE_BYTES, Uuid};

const IMAGE: &str = "/mnt/image";
const DISK: &str = "/mnt/disk";
const ROOT: &str = "/mnt/root";
const UPPER: &str = "/mnt/disk/upper";
const WORK: &str = "/mnt/disk/work";
/// The tmpfs that holds the content of the files, which only the agent mounts.
const FILES: &str = "/run/mvmd/files";
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

/// Delete the files of the initramfs at `root`, as `switch_root(8)` does, leaving every mount.
///
/// Nothing is deleted unless `root` is a ramfs or a tmpfs: a root on a disk is not ours to empty.
fn free_initramfs(root: &Path) -> io::Result<()> {
    const RAMFS_MAGIC: u64 = 0x8584_58f6;
    const TMPFS_MAGIC: u64 = 0x0102_1994;
    let kind = u64::try_from(nix::sys::statfs::statfs(root)?.filesystem_type().0).unwrap_or(0);
    if kind != RAMFS_MAGIC && kind != TMPFS_MAGIC {
        return Ok(());
    }
    delete_within(root, fs::metadata(root)?.dev())
}

/// Delete what `dir` holds that is on the filesystem `device`. A directory on another one is a
/// mount, and is neither entered nor removed. A running program's file goes too: it stays open.
fn delete_within(dir: &Path, device: u64) -> io::Result<()> {
    for entry in fs::read_dir(dir)? {
        let path = entry?.path();
        let metadata = fs::symlink_metadata(&path)?;
        if metadata.dev() != device {
            continue;
        }
        if metadata.is_dir() {
            delete_within(&path, device)?;
            // A directory that still holds a mount cannot be removed, and stays.
            let _ = fs::remove_dir(&path);
        } else {
            fs::remove_file(&path)?;
        }
    }
    Ok(())
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
        // Current kernels attach /dev/console to init before it runs; attaching it again through
        // devtmpfs covers a kernel that did not. With no console configured it cannot be
        // opened, and the agent then has no stream to report on.
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
        // What switch_root(8) does: free the initramfs, move the new root over the old one and
        // enter it.
        free_initramfs(Path::new("/"))?;
        chdir(ROOT)?;
        mount(Some("."), "/", None::<&str>, MsFlags::MS_MOVE, None::<&str>)
            .map_err(io::Error::from)?;
        chroot(".")?;
        chdir("/")?;
        Ok(())
    }

    fn place_files(&self, files: &[BootFile]) -> io::Result<()> {
        place_files_in(Path::new("/"), Path::new(FILES), files)
    }
}

/// Name `path` in the error, as the only thing it says about a file: its content may be a secret.
fn at<T>(path: &Path, result: io::Result<T>) -> io::Result<T> {
    result.map_err(|error| io::Error::new(error.kind(), format!("{}: {error}", path.display())))
}

/// Create `dir` and the directories above it that are missing, each 0755 and root's.
fn create_parents(dir: &Path) -> io::Result<()> {
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o755)
        .create(dir)
}

/// Place `files` under `root`, holding their content in a tmpfs mounted at `store`, which only
/// root can enter. Each file is written to the tmpfs, then bound read-only onto its path.
///
/// The file under `root` is created empty where missing, and an existing one is shadowed, never
/// written. An error names the file's path and nothing of its content.
fn place_files_in(root: &Path, store: &Path, files: &[BootFile]) -> io::Result<()> {
    if let Some(parent) = store.parent() {
        create_parents(parent)?;
    }
    fs::DirBuilder::new().mode(0o700).create(store)?;
    mount(
        Some("tmpfs"),
        store,
        Some("tmpfs"),
        MsFlags::MS_NODEV,
        Some("mode=0700"),
    )
    .map_err(io::Error::from)?;
    for (index, file) in files.iter().enumerate() {
        let target = root.join(file.path.trim_start_matches('/'));
        at(
            &target,
            place_one(root, &store.join(index.to_string()), file),
        )?;
    }
    Ok(())
}

/// The most links one path may pass through, as the kernel's own limit.
const MAX_LINKS: usize = 40;

/// The path `path` names in `root`, as a path under `root`: every link in it, the last included,
/// is followed with an absolute target read from `root`, not from the agent's own. A part that
/// does not exist ends the walk, and what follows it is kept as given.
fn resolve_within(root: &Path, path: &str) -> io::Result<PathBuf> {
    use std::collections::VecDeque;
    use std::path::Component;
    let mut rest: VecDeque<std::ffi::OsString> = Path::new(path)
        .components()
        .filter_map(|part| match part {
            Component::Normal(name) => Some(name.to_owned()),
            _ => None,
        })
        .collect();
    let mut resolved = PathBuf::new();
    let mut links = 0;
    while let Some(name) = rest.pop_front() {
        if name == ".." {
            resolved.pop();
            continue;
        }
        if name == "." {
            continue;
        }
        let next = resolved.join(&name);
        match fs::symlink_metadata(root.join(&next)) {
            Ok(meta) if meta.file_type().is_symlink() => {
                links += 1;
                if links > MAX_LINKS {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        "has too many links",
                    ));
                }
                let link = fs::read_link(root.join(&next))?;
                if link.is_absolute() {
                    resolved = PathBuf::new();
                }
                for part in link.components().rev() {
                    match part {
                        Component::Normal(part) => rest.push_front(part.to_owned()),
                        Component::ParentDir => rest.push_front("..".into()),
                        _ => {}
                    }
                }
            }
            Ok(_) => resolved = next,
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                resolved = next;
                resolved.extend(rest.drain(..));
                break;
            }
            Err(error) => return Err(error),
        }
    }
    Ok(root.join(resolved))
}

fn place_one(root: &Path, held: &Path, file: &BootFile) -> io::Result<()> {
    // Resolved before anything is written: a refusal leaves nothing behind.
    let target = resolve_within(root, &file.path)?;
    let inside = Path::new("/").join(target.strip_prefix(root).unwrap_or(&target));
    if inside.to_str().is_none_or(|_| false) {
        let _ = is_reserved;
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "resolves into a directory the agent reserves",
        ));
    }
    let target = target.as_path();
    // Owner and mode are set before the content is written, so it is never more readable than
    // the file's own mode makes it.
    let mut held_file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(held)?;
    // The owner first: changing it clears the set-user-ID bits the mode may carry.
    std::os::unix::fs::fchown(&held_file, Some(file.uid), Some(file.gid))?;
    held_file.set_permissions(fs::Permissions::from_mode(file.mode))?;
    held_file.write_all(&file.content)?;
    drop(held_file);

    if let Some(parent) = target.parent() {
        create_parents(parent)?;
    }
    match fs::symlink_metadata(target) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o644)
                .open(target)?;
        }
        Ok(meta) if meta.file_type().is_file() => {}
        Ok(_) => {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "resolves to something that is not a regular file",
            ));
        }
        Err(error) => return Err(error),
    }
    mount(
        Some(held),
        target,
        None::<&str>,
        MsFlags::MS_BIND,
        None::<&str>,
    )
    .map_err(io::Error::from)?;
    mount(
        None::<&str>,
        target,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REMOUNT | MsFlags::MS_RDONLY | MsFlags::MS_NODEV,
        None::<&str>,
    )
    .map_err(io::Error::from)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn delete_within_empties_a_tree_of_files_links_and_directories() -> io::Result<()> {
        let dir = tempfile::tempdir()?;
        let root = dir.path();
        fs::create_dir_all(root.join("a/b"))?;
        fs::write(root.join("init"), "x")?;
        fs::write(root.join("a/b/file"), "y")?;
        std::os::unix::fs::symlink("/nowhere", root.join("a/link"))?;
        let device = fs::metadata(root)?.dev();

        delete_within(root, device)?;

        assert_eq!(fs::read_dir(root)?.count(), 0);
        Ok(())
    }

    #[test]
    fn delete_within_leaves_what_is_on_another_device() -> io::Result<()> {
        let dir = tempfile::tempdir()?;
        fs::write(dir.path().join("kept"), "x")?;
        // No file here is on this device number, as is the case for the files of a mount.
        let other = fs::metadata(dir.path())?.dev() + 1;

        delete_within(dir.path(), other)?;

        assert!(dir.path().join("kept").exists());
        Ok(())
    }

    /// Tests that mount, which only root may. A caller that is not root runs the test again
    /// under passwordless sudo, which the GitHub-hosted runner provides, and where that is not
    /// there the test fails: it does not skip.
    mod real_files {
        use super::*;
        use nix::mount::{MntFlags, umount2};
        use std::process::Command;

        const SECRET: &[u8] = b"hunter2-the-secret";
        const TMPFS_MAGIC: u64 = 0x0102_1994;

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

        /// A directory standing for the booted root, and the tmpfs store beside it. Dropping it
        /// unmounts what the test mounted, whatever the test did.
        struct Place {
            dir: tempfile::TempDir,
            calls: std::cell::Cell<usize>,
        }

        impl Place {
            fn new() -> io::Result<Self> {
                let dir = tempfile::tempdir()?;
                fs::create_dir_all(dir.path().join("root/etc"))?;
                fs::write(dir.path().join("root/etc/shadowed"), "image content")?;
                Ok(Self {
                    dir,
                    calls: std::cell::Cell::new(0),
                })
            }

            fn root(&self) -> PathBuf {
                self.dir.path().join("root")
            }

            fn store(&self) -> PathBuf {
                self.dir.path().join("run/mvmd/files")
            }

            /// One call of the placing, each with a store of its own as each boot has one.
            fn place(&self, files: &[BootFile]) -> io::Result<()> {
                let call = self.calls.get();
                self.calls.set(call + 1);
                place_files_in(&self.root(), &self.store().join(call.to_string()), files)
            }
        }

        impl Drop for Place {
            fn drop(&mut self) {
                // Every mount under the directory, the deepest first, whatever it is on.
                let mounts = fs::read_to_string("/proc/self/mountinfo").unwrap_or_default();
                let mut points: Vec<&str> = mounts
                    .lines()
                    .filter_map(|line| line.split(' ').nth(4))
                    .filter(|point| Path::new(point).starts_with(self.dir.path()))
                    .collect();
                points.sort_by_key(|point| std::cmp::Reverse(point.len()));
                for point in points {
                    let _ = umount2(point, MntFlags::MNT_DETACH);
                }
            }
        }

        fn file(path: &str, content: &[u8], mode: u32) -> BootFile {
            BootFile {
                path: path.into(),
                content: content.to_vec(),
                mode,
                uid: 1234,
                gid: 4321,
            }
        }

        /// Every file under `dir` that holds `needle`.
        fn holding(dir: &Path, needle: &[u8]) -> io::Result<Vec<PathBuf>> {
            let mut found = Vec::new();
            for entry in fs::read_dir(dir)? {
                let path = entry?.path();
                if fs::symlink_metadata(&path)?.is_dir() {
                    found.extend(holding(&path, needle)?);
                } else if fs::read(&path)?.windows(needle.len()).any(|w| w == needle) {
                    found.push(path);
                }
            }
            Ok(found)
        }

        #[test]
        fn a_file_is_a_read_only_tmpfs_bind_with_its_mode_and_owner_and_the_disk_keeps_none()
        -> io::Result<()> {
            if !root_or_rerun(
                "a_file_is_a_read_only_tmpfs_bind_with_its_mode_and_owner_and_the_disk_keeps_none",
            )? {
                return Ok(());
            }
            let place = Place::new()?;
            let root = place.root();

            place.place(&[
                file("/etc/new", SECRET, 0o640),
                file("/deep/er/still", b"nested", 0o600),
                file("/etc/shadowed", b"mvmd content", 0o644),
            ])?;

            // The file is the content, with the mode and the owner it was sent with.
            let new = root.join("etc/new");
            let meta = fs::metadata(&new)?;
            assert_eq!(fs::read(&new)?, SECRET);
            assert_eq!(meta.mode() & 0o7777, 0o640);
            assert_eq!((meta.uid(), meta.gid()), (1234, 4321));
            // It is on a tmpfs, and read-only.
            let kind = nix::sys::statfs::statfs(&new)?.filesystem_type().0;
            assert_eq!(u64::try_from(kind).ok(), Some(TMPFS_MAGIC));
            let write = fs::OpenOptions::new().write(true).open(&new).unwrap_err();
            assert_eq!(write.kind(), io::ErrorKind::ReadOnlyFilesystem);
            // The directories made for a path are 0755 and root's.
            let parent = fs::metadata(root.join("deep/er"))?;
            assert_eq!(parent.mode() & 0o7777, 0o755);
            assert_eq!(parent.uid(), 0);
            // The store is closed to everyone but the agent.
            let store = fs::metadata(place.store().join("0"))?;
            assert_eq!((store.mode() & 0o7777, store.uid()), (0o700, 0));
            // An image file at the same path is shadowed, not overwritten.
            assert_eq!(fs::read(root.join("etc/shadowed"))?, b"mvmd content");

            // Unmounted, the root disk's side is what it was: nothing holds the content, the
            // file the image had is intact, and the target of a path that had none is empty.
            for mounted in ["etc/new", "deep/er/still", "etc/shadowed"] {
                umount2(&root.join(mounted), MntFlags::MNT_DETACH)?;
            }
            assert_eq!(holding(&root, SECRET)?, Vec::<PathBuf>::new());
            assert_eq!(holding(&root, b"nested")?, Vec::<PathBuf>::new());
            assert_eq!(fs::read(root.join("etc/shadowed"))?, b"image content");
            assert_eq!(fs::read(root.join("etc/new"))?, b"");
            Ok(())
        }

        #[test]
        fn a_file_that_cannot_be_placed_fails_naming_its_path_and_not_its_content() -> io::Result<()>
        {
            if !root_or_rerun(
                "a_file_that_cannot_be_placed_fails_naming_its_path_and_not_its_content",
            )? {
                return Ok(());
            }
            let place = Place::new()?;

            // The parent of the second file is a file of the image, so it cannot be made.
            let error = place
                .place(&[
                    file("/etc/fine", b"fine", 0o600),
                    file("/etc/shadowed/child", SECRET, 0o600),
                ])
                .unwrap_err();

            let said = error.to_string();
            assert!(said.contains("shadowed/child"), "{said}");
            assert!(!said.contains("hunter2"), "{said}");
            // The control: the file before it was placed, so the failure is the one asked for.
            assert_eq!(fs::read(place.root().join("etc/fine"))?, b"fine");
            Ok(())
        }

        /// The file as the parse sees it, which looks at the path as sent and not at what it names.
        fn parses(path: &str) -> bool {
            BootFile::parse(crate::v1alpha1::File {
                path: path.into(),
                content: b"x".to_vec(),
                mode: 0o600,
                uid: 0,
                gid: 0,
            })
            .is_ok()
        }

        fn link(place: &Place, name: &str, to: &str) -> io::Result<()> {
            std::os::unix::fs::symlink(to, place.root().join("etc").join(name))
        }

        #[test]
        fn a_link_into_a_reserved_directory_is_refused_naming_the_path_sent() -> io::Result<()> {
            if !root_or_rerun("a_link_into_a_reserved_directory_is_refused_naming_the_path_sent")? {
                return Ok(());
            }
            let place = Place::new()?;
            link(&place, "absolute", "/proc/self/x")?;
            link(&place, "relative", "../run/mvmd/files/x")?;
            link(&place, "plain", "/etc/shadowed")?;
            // The parse passes each as sent: the refusal is the resolution's.
            for name in ["absolute", "relative", "plain"] {
                assert!(parses(&format!("/etc/{name}")), "{name}");
            }

            for name in ["absolute", "relative"] {
                let path = format!("/etc/{name}");
                let error = place.place(&[file(&path, SECRET, 0o600)]).unwrap_err();
                let said = error.to_string();
                assert!(said.contains(&path), "{said}");
                assert!(said.contains("reserves"), "{said}");
                assert!(!said.contains("hunter2"), "{said}");
            }
            // Nothing was made in the reserved directories of the root.
            assert!(!place.root().join("proc").exists());
            assert!(!place.root().join("run").exists());
            // The control, on a fresh root: a link to a plain file is placed by the same call.
            let control = Place::new()?;
            link(&control, "plain", "/etc/shadowed")?;
            control.place(&[file("/etc/plain", b"ok", 0o600)])?;
            assert_eq!(fs::read(control.root().join("etc/shadowed"))?, b"ok");
            Ok(())
        }

        #[test]
        fn a_link_to_a_plain_file_is_placed_at_its_target_and_the_link_is_kept() -> io::Result<()> {
            if !root_or_rerun(
                "a_link_to_a_plain_file_is_placed_at_its_target_and_the_link_is_kept",
            )? {
                return Ok(());
            }
            let place = Place::new()?;
            link(&place, "absolute", "/etc/shadowed")?;
            link(&place, "relative", "shadowed")?;
            assert!(parses("/etc/absolute") && parses("/etc/relative"));

            // The relative link names the same file, and binds over the first: the later wins.
            place.place(&[
                file("/etc/absolute", b"through the link", 0o640),
                file("/etc/relative", b"through the relative link", 0o640),
            ])?;

            // The links stay and the file they name holds the placed content, read-only.
            assert!(fs::symlink_metadata(place.root().join("etc/absolute"))?.is_symlink());
            assert!(fs::symlink_metadata(place.root().join("etc/relative"))?.is_symlink());
            assert_eq!(
                fs::read(place.root().join("etc/shadowed"))?,
                b"through the relative link"
            );
            assert_eq!(
                fs::metadata(place.root().join("etc/shadowed"))?
                    .permissions()
                    .mode()
                    & 0o777,
                0o640
            );
            assert!(
                fs::OpenOptions::new()
                    .write(true)
                    .open(place.root().join("etc/shadowed"))
                    .is_err()
            );
            Ok(())
        }

        #[test]
        fn a_dangling_link_with_an_existing_parent_gets_its_resolved_file_created_and_placed()
        -> io::Result<()> {
            if !root_or_rerun(
                "a_dangling_link_with_an_existing_parent_gets_its_resolved_file_created_and_placed",
            )? {
                return Ok(());
            }
            let place = Place::new()?;
            link(&place, "dangling", "/etc/made")?;
            assert!(parses("/etc/dangling"));
            assert!(!place.root().join("etc/made").exists());

            place.place(&[file("/etc/dangling", b"made by the placing", 0o600)])?;

            assert!(fs::symlink_metadata(place.root().join("etc/dangling"))?.is_symlink());
            assert_eq!(
                fs::read(place.root().join("etc/made"))?,
                b"made by the placing"
            );
            Ok(())
        }

        #[test]
        fn a_link_to_a_directory_or_a_fifo_is_refused_and_a_plain_file_is_not() -> io::Result<()> {
            if !root_or_rerun("a_link_to_a_directory_or_a_fifo_is_refused_and_a_plain_file_is_not")?
            {
                return Ok(());
            }
            let place = Place::new()?;
            fs::create_dir(place.root().join("etc/adir"))?;
            nix::unistd::mkfifo(
                &place.root().join("etc/afifo"),
                nix::sys::stat::Mode::from_bits_truncate(0o600),
            )?;
            link(&place, "todir", "/etc/adir")?;
            link(&place, "tofifo", "/etc/afifo")?;
            link(&place, "tofile", "/etc/shadowed")?;
            for name in ["todir", "tofifo", "tofile"] {
                assert!(parses(&format!("/etc/{name}")), "{name}");
            }

            for name in ["todir", "tofifo"] {
                let path = format!("/etc/{name}");
                let error = place.place(&[file(&path, SECRET, 0o600)]).unwrap_err();
                let said = error.to_string();
                assert!(said.contains(&path), "{said}");
                assert!(said.contains("not a regular file"), "{said}");
                assert!(!said.contains("hunter2"), "{said}");
            }
            // The control, on a fresh root: the link to a plain file is accepted by the same call.
            let control = Place::new()?;
            link(&control, "tofile", "/etc/shadowed")?;
            control.place(&[file("/etc/tofile", b"ok", 0o600)])?;
            assert_eq!(fs::read(control.root().join("etc/shadowed"))?, b"ok");
            Ok(())
        }
    }
}
