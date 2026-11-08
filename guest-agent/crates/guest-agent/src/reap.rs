//! Finding the exited processes nobody has waited for: the zombies that are children of the agent.

use std::fs;
use std::path::Path;

use nix::unistd::Pid;

/// The processes under `proc` (the mount of `/proc`) that have exited and are children of `parent`.
pub(crate) fn zombie_children(proc: &Path, parent: Pid) -> Vec<Pid> {
    let Ok(entries) = fs::read_dir(proc) else {
        return Vec::new();
    };
    entries
        .flatten()
        .filter_map(|entry| {
            let pid: i32 = entry.file_name().to_str()?.parse().ok()?;
            // The process may be gone by the time its file is read.
            let stat = fs::read_to_string(entry.path().join("stat")).ok()?;
            let (state, ppid) = parse_stat(&stat)?;
            (state == 'Z' && ppid == parent.as_raw()).then_some(Pid::from_raw(pid))
        })
        .collect()
}

/// The state and the parent's process ID in the text of a `/proc/<pid>/stat`.
fn parse_stat(stat: &str) -> Option<(char, i32)> {
    // The command name comes second, in parentheses, and may itself hold spaces and parentheses;
    // the fields that follow start after its last closing one.
    let (_, rest) = stat.rsplit_once(')')?;
    let mut fields = rest.split_whitespace();
    let state = fields.next()?.chars().next()?;
    let ppid = fields.next()?.parse().ok()?;
    Some((state, ppid))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stat_gives_the_state_and_parent_even_when_the_name_holds_parentheses() {
        assert_eq!(parse_stat("42 (sleep) Z 7 42 42 0"), Some(('Z', 7)));
        assert_eq!(parse_stat("42 (a) Z 1 (b) S 9 42"), Some(('S', 9)));
        assert_eq!(parse_stat("42 (no state)"), None);
    }

    #[test]
    fn only_zombies_that_are_children_of_the_parent_are_found() {
        let dir = tempfile::tempdir().unwrap();
        for (pid, stat) in [
            ("10", "10 (a) Z 1 10 10 0"),
            ("11", "11 (b) S 1 11 11 0"),
            ("12", "12 (c) Z 5 12 12 0"),
            ("13", "13 (d e) Z 1 13 13 0"),
        ] {
            let process = dir.path().join(pid);
            fs::create_dir(&process).unwrap();
            fs::write(process.join("stat"), stat).unwrap();
        }
        // Entries of /proc that are not processes are passed over.
        fs::create_dir(dir.path().join("self")).unwrap();
        fs::write(dir.path().join("uptime"), "1 1").unwrap();

        let mut found: Vec<i32> = zombie_children(dir.path(), Pid::from_raw(1))
            .into_iter()
            .map(Pid::as_raw)
            .collect();
        found.sort_unstable();
        assert_eq!(found, [10, 13]);
    }
}
