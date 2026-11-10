//! The method that ends the Machine, which the Driver names on the kernel command line.

use std::io;
use std::sync::{Arc, OnceLock};

use thiserror::Error;

use crate::system::KernelCommandLine;

/// The kernel command line parameter that names the method.
const PARAMETER: &str = "mvmd.end";

/// How the agent ends the Machine, so that the VMM process ends with it.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub enum EndMethod {
    /// `reboot(RB_POWER_OFF)`, and the default.
    #[default]
    PowerOff,
    /// `reboot(RB_AUTOBOOT)`.
    Reset,
}

/// Why the command line names no method.
#[derive(Debug, PartialEq, Eq, Error)]
pub enum EndMethodError {
    /// The command line has no `mvmd.end`.
    #[error("the kernel command line has no {PARAMETER}")]
    Absent,
    /// `mvmd.end` has a value other than `poweroff` and `reset`.
    #[error("{PARAMETER}={0:?} is neither poweroff nor reset")]
    Unknown(String),
}

impl EndMethod {
    /// Parse the method from a kernel command line. The last `mvmd.end` counts, as it does for
    /// the kernel's own parameters.
    pub fn from_command_line(line: &str) -> Result<Self, EndMethodError> {
        let value = line
            .split_whitespace()
            .filter_map(|word| {
                let (name, value) = word.split_once('=').unwrap_or((word, ""));
                (name == PARAMETER).then_some(value)
            })
            .next_back()
            .ok_or(EndMethodError::Absent)?;
        match value {
            "poweroff" => Ok(Self::PowerOff),
            "reset" => Ok(Self::Reset),
            other => Err(EndMethodError::Unknown(other.to_owned())),
        }
    }
}

/// The method the kernel command line names, held for the rest of the agent's life. Empty until
/// the agent has read the command line, which needs `/proc`; an end before that is a power-off.
pub type EndMethodCell = Arc<OnceLock<EndMethod>>;

/// Reads the kernel command line from `/proc/cmdline`.
#[derive(Debug, Default)]
pub struct LinuxCommandLine;

impl KernelCommandLine for LinuxCommandLine {
    fn read(&self) -> io::Result<String> {
        std::fs::read_to_string("/proc/cmdline")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_power_off_and_reset() {
        assert_eq!(
            EndMethod::from_command_line("console=ttyS0 mvmd.end=poweroff reboot=k"),
            Ok(EndMethod::PowerOff)
        );
        assert_eq!(
            EndMethod::from_command_line("console=ttyS0 mvmd.end=reset\n"),
            Ok(EndMethod::Reset)
        );
    }

    #[test]
    fn an_absent_parameter_is_absent() {
        assert_eq!(
            EndMethod::from_command_line("console=ttyS0 reboot=k"),
            Err(EndMethodError::Absent)
        );
        // A parameter that only contains the name is another parameter.
        assert_eq!(
            EndMethod::from_command_line("mvmd.endx=reset xmvmd.end=reset"),
            Err(EndMethodError::Absent)
        );
    }

    #[test]
    fn an_unknown_value_is_unknown() {
        assert_eq!(
            EndMethod::from_command_line("mvmd.end=halt"),
            Err(EndMethodError::Unknown("halt".into()))
        );
        assert_eq!(
            EndMethod::from_command_line("mvmd.end"),
            Err(EndMethodError::Unknown(String::new()))
        );
    }

    #[test]
    fn the_last_occurrence_counts() {
        assert_eq!(
            EndMethod::from_command_line("mvmd.end=poweroff mvmd.end=reset"),
            Ok(EndMethod::Reset)
        );
    }
}
