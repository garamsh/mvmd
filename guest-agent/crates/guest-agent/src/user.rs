//! The user a command runs as: the OCI image-config form parsed once at the boundary, and
//! resolved against the passwd and group files of the booted root.

use std::io;
use std::process::Command;

use nix::unistd::{Gid, Uid, setgid, setgroups, setuid};

use crate::error::Error;

/// One side of `user:group`: a number, or a name to look up.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Part {
    Id(u32),
    Name(String),
}

impl Part {
    fn parse(text: &str, what: &str) -> Result<Self, Error> {
        if text.is_empty() || text.contains(':') {
            return Err(Error::Invalid(format!(
                "{what} {text:?} is empty or has ':'"
            )));
        }
        if text.bytes().all(|byte| byte.is_ascii_digit()) {
            return text
                .parse()
                .map(Self::Id)
                .map_err(|_| Error::Invalid(format!("{what} {text:?} is not a 32-bit id")));
        }
        Ok(Self::Name(text.to_owned()))
    }
}

/// A user in the form `user`, `uid`, `user:group`, `uid:gid`, `user:gid` or `uid:group`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserSpec {
    user: Part,
    group: Option<Part>,
}

impl UserSpec {
    /// Parse `text`; `None` for the empty string, which runs as root with nothing applied.
    pub(crate) fn parse(text: &str) -> Result<Option<Self>, Error> {
        if text.is_empty() {
            return Ok(None);
        }
        let (user, group) = match text.split_once(':') {
            Some((user, group)) => (user, Some(group)),
            None => (text, None),
        };
        Ok(Some(Self {
            user: Part::parse(user, "user")?,
            group: group.map(|group| Part::parse(group, "group")).transpose()?,
        }))
    }

    /// The identity this names, looked up in `accounts`.
    pub(crate) fn resolve(&self, accounts: &Accounts) -> io::Result<Identity> {
        let not_found = |what: &str, name: &str| {
            io::Error::new(io::ErrorKind::NotFound, format!("no {what} named {name:?}"))
        };
        let passwd = accounts.passwd.lines().filter_map(PasswdEntry::parse);
        let entry = match &self.user {
            Part::Name(name) => Some(
                passwd
                    .into_iter()
                    .find(|entry| entry.name == *name)
                    .ok_or_else(|| not_found("user", name))?,
            ),
            Part::Id(uid) => passwd.into_iter().find(|entry| entry.uid == *uid),
        };
        let uid = match (&self.user, &entry) {
            (Part::Id(uid), _) => *uid,
            (Part::Name(_), Some(entry)) => entry.uid,
            (Part::Name(_), None) => unreachable!("a name that was not found returned above"),
        };
        let gid = match &self.group {
            Some(Part::Id(gid)) => *gid,
            Some(Part::Name(name)) => accounts
                .groups()
                .find(|group| group.name == *name)
                .map(|group| group.gid)
                .ok_or_else(|| not_found("group", name))?,
            None => entry.as_ref().map_or(0, |entry| entry.gid),
        };
        let mut groups = Vec::new();
        if let Part::Name(name) = &self.user {
            for group in accounts
                .groups()
                .filter(|group| group.members.contains(name))
            {
                if !groups.contains(&group.gid) {
                    groups.push(group.gid);
                }
            }
        }
        Ok(Identity {
            uid,
            gid,
            groups,
            home: entry
                .map(|entry| entry.home)
                .filter(|home| !home.is_empty()),
        })
    }
}

/// The user, the primary group and the supplementary groups a command starts with.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Identity {
    /// The user ID.
    pub uid: u32,
    /// The group ID.
    pub gid: u32,
    /// The supplementary group IDs.
    pub groups: Vec<u32>,
    /// The user's home directory from passwd, when it has an entry with one.
    pub home: Option<String>,
}

/// The contents of the passwd and group files of the booted root.
#[derive(Debug, Clone, Default)]
pub struct Accounts {
    /// `/etc/passwd`; empty when the root has none.
    pub passwd: String,
    /// `/etc/group`; empty when the root has none.
    pub group: String,
}

impl Accounts {
    fn groups(&self) -> impl Iterator<Item = GroupEntry> + '_ {
        self.group.lines().filter_map(GroupEntry::parse)
    }
}

struct PasswdEntry {
    name: String,
    uid: u32,
    gid: u32,
    home: String,
}

impl PasswdEntry {
    /// `name:x:uid:gid:gecos:home:shell`; a line that is not that is no entry.
    fn parse(line: &str) -> Option<Self> {
        let mut fields = line.split(':');
        let name = fields.next().filter(|name| !name.is_empty())?;
        let uid = fields.nth(1)?.parse().ok()?;
        let gid = fields.next()?.parse().ok()?;
        let home = fields.nth(1)?;
        Some(Self {
            name: name.to_owned(),
            uid,
            gid,
            home: home.to_owned(),
        })
    }
}

struct GroupEntry {
    name: String,
    gid: u32,
    members: Vec<String>,
}

impl GroupEntry {
    /// `name:x:gid:member,member`; a line that is not that is no entry.
    fn parse(line: &str) -> Option<Self> {
        let mut fields = line.split(':');
        let name = fields.next().filter(|name| !name.is_empty())?;
        let gid = fields.nth(1)?.parse().ok()?;
        let members = fields.next()?;
        Some(Self {
            name: name.to_owned(),
            gid,
            members: members.split(',').map(str::to_owned).collect(),
        })
    }
}

/// The guest's accounts, and how a command takes on an identity.
pub trait Users: Send + Sync {
    /// The passwd and group files of the root the agent now runs in.
    fn accounts(&self) -> io::Result<Accounts>;

    /// Make `command` start with `identity`: `setgroups`, `setgid` and then `setuid`.
    fn run_as(&self, command: &mut Command, identity: &Identity);
}

/// Reads `/etc/passwd` and `/etc/group` and sets the identity in the child before it execs.
#[derive(Debug, Default)]
pub struct LinuxUsers;

fn read_or_empty(path: &str) -> io::Result<String> {
    match std::fs::read_to_string(path) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(String::new()),
        other => other,
    }
}

impl Users for LinuxUsers {
    fn accounts(&self) -> io::Result<Accounts> {
        Ok(Accounts {
            passwd: read_or_empty("/etc/passwd")?,
            group: read_or_empty("/etc/group")?,
        })
    }

    #[expect(
        unsafe_code,
        reason = "setgroups must precede setgid and setuid, and std's own `groups` is unstable"
    )]
    fn run_as(&self, command: &mut Command, identity: &Identity) {
        use std::os::unix::process::CommandExt;
        let groups: Vec<Gid> = identity.groups.iter().copied().map(Gid::from_raw).collect();
        let (gid, uid) = (Gid::from_raw(identity.gid), Uid::from_raw(identity.uid));
        // SAFETY: the closure runs in the forked child before it execs, and calls only the
        // system calls setgroups, setgid and setuid, which allocate nothing and take no lock.
        unsafe {
            command.pre_exec(move || {
                let _ = &groups;
                let _ = setgroups;
                setgid(gid)?;
                setuid(uid)?;
                Ok(())
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PASSWD: &str = "root:x:0:0:root:/root:/bin/sh\n\
        app:x:1000:2000:app:/home/app:/bin/sh\n\
        broken line\n\
        nohome:x:1001:2000:n::/bin/sh\n";
    const GROUP: &str = "root:x:0:\nstaff:x:2000:app\nwheel:x:10:app,other\nsolo:x:3000:\n";

    fn accounts() -> Accounts {
        Accounts {
            passwd: PASSWD.into(),
            group: GROUP.into(),
        }
    }

    fn resolve(text: &str) -> io::Result<Identity> {
        UserSpec::parse(text)
            .map_err(io::Error::other)?
            .ok_or_else(|| io::Error::other("empty"))?
            .resolve(&accounts())
    }

    #[test]
    fn the_empty_string_is_no_user_and_a_malformed_one_is_refused() {
        assert_eq!(UserSpec::parse("").unwrap(), None);
        for bad in [
            ":",
            "app:",
            ":2000",
            "a:b:c",
            "99999999999",
            "app:99999999999",
        ] {
            assert!(UserSpec::parse(bad).is_err(), "{bad:?} was accepted");
        }
        assert!(UserSpec::parse("app:staff").is_ok());
    }

    #[test]
    fn a_name_takes_its_primary_group_its_supplementary_groups_and_its_home() {
        let identity = resolve("app").unwrap();
        assert_eq!(identity.uid, 1000);
        assert_eq!(identity.gid, 2000);
        assert_eq!(identity.groups, [2000, 10]);
        assert_eq!(identity.home.as_deref(), Some("/home/app"));
    }

    #[test]
    fn a_uid_and_gid_resolve_to_themselves_with_no_supplementary_groups() {
        let identity = resolve("1000:3000").unwrap();
        assert_eq!((identity.uid, identity.gid), (1000, 3000));
        assert!(identity.groups.is_empty());
    }

    #[test]
    fn a_uid_with_no_entry_and_no_group_gets_gid_0_and_no_home() {
        let identity = resolve("4242").unwrap();
        assert_eq!((identity.uid, identity.gid), (4242, 0));
        assert_eq!(identity.home, None);
    }

    #[test]
    fn a_bare_uid_with_an_entry_takes_its_primary_group_and_home_but_no_groups() {
        let identity = resolve("1000").unwrap();
        assert_eq!(identity.gid, 2000);
        assert!(identity.groups.is_empty());
        assert_eq!(identity.home.as_deref(), Some("/home/app"));
    }

    #[test]
    fn a_group_name_and_a_user_name_mix() {
        let identity = resolve("app:solo").unwrap();
        assert_eq!((identity.uid, identity.gid), (1000, 3000));
        assert_eq!(identity.groups, [2000, 10]);
        let identity = resolve("1000:wheel").unwrap();
        assert_eq!((identity.uid, identity.gid), (1000, 10));
    }

    #[test]
    fn a_name_that_is_not_there_is_not_found_and_an_empty_home_is_no_home() {
        for bad in ["ghost", "app:ghost", "ghost:1"] {
            let error = resolve(bad).unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::NotFound, "{bad}");
        }
        assert_eq!(resolve("nohome").unwrap().home, None);
    }

    /// Root runs the command as another user; a caller that is not root runs the test again
    /// under passwordless sudo, which the GitHub-hosted runner provides, and where that is not
    /// there the test fails: it does not skip.
    #[test]
    fn a_command_runs_with_the_uid_gid_and_groups_of_its_identity() -> io::Result<()> {
        use std::os::unix::fs::MetadataExt;
        let name = "a_command_runs_with_the_uid_gid_and_groups_of_its_identity";
        // The owner of /proc/self is the user the process runs as.
        if std::fs::metadata("/proc/self")?.uid() != 0 {
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
            return Ok(());
        }
        let identity = Identity {
            uid: 1234,
            gid: 4321,
            groups: vec![4321, 77, 88],
            home: None,
        };
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "id -u; id -g; id -G"]);

        LinuxUsers.run_as(&mut command, &identity);
        let out = command.output()?;

        // The groups are the identity's and not root's: `setgroups` came first, as root.
        assert_eq!(
            String::from_utf8_lossy(&out.stdout),
            "1234\n4321\n4321 77 88\n",
            "{}",
            String::from_utf8_lossy(&out.stderr)
        );
        Ok(())
    }
}
