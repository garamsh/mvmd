//! What a restore needs of the Linux kernel: its random pool, which the agent refills, and its
//! wall clock, which the agent sets.

use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;

use nix::sys::time::{TimeSpec, TimeValLike};
use nix::time::{ClockId, clock_settime};

use crate::description::ENTROPY_BYTES;
use crate::system::{Clock, Entropy};

/// `RNDADDENTROPY`, `_IOW('R', 0x03, int[2])` in the kernel's `include/uapi/linux/random.h`: the
/// size is that of the two integers that open `struct rand_pool_info`, not of what follows.
const RNDADDENTROPY: nix::sys::ioctl::ioctl_num_type = nix::request_code_write!(b'R', 0x03, 8);

nix::ioctl_write_ptr_bad!(rnd_add_entropy, RNDADDENTROPY, PoolInfo);
// `RNDRESEEDCRNG`, `_IO('R', 0x07)`.
nix::ioctl_none!(rnd_reseed_crng, b'R', 0x07);

/// `struct rand_pool_info` with room for the most entropy a catch-up carries.
#[repr(C)]
struct PoolInfo {
    /// The bits of entropy to count.
    entropy_count: i32,
    /// The bytes that follow.
    buf_size: i32,
    buf: [u8; 256],
}

/// The pool entry for `bytes`, counting every bit of them as entropy.
fn pool_info(bytes: &[u8]) -> io::Result<PoolInfo> {
    let refused = || io::Error::other("the entropy is not a size the kernel's pool call takes");
    if !ENTROPY_BYTES.contains(&bytes.len()) {
        return Err(refused());
    }
    let mut info = PoolInfo {
        entropy_count: i32::try_from(bytes.len() * 8).map_err(|_| refused())?,
        buf_size: i32::try_from(bytes.len()).map_err(|_| refused())?,
        buf: [0; 256],
    };
    info.buf[..bytes.len()].copy_from_slice(bytes);
    Ok(info)
}

/// Mixes into the pool with the `RNDADDENTROPY` and `RNDRESEEDCRNG` ioctls of `/dev/urandom`,
/// which need `CAP_SYS_ADMIN`.
#[derive(Debug, Default)]
pub struct LinuxEntropy;

#[expect(
    unsafe_code,
    reason = "the pool's ioctls have no safe wrapper in a dependency"
)]
fn ioctls(random: &File, info: &PoolInfo) -> nix::Result<()> {
    // SAFETY: `random` is an open file for the length of both calls, `info` is a `repr(C)` value
    // laid out as the kernel's `rand_pool_info` with `buf_size` valid bytes after the two
    // integers, and `RNDRESEEDCRNG` takes no argument.
    unsafe {
        rnd_add_entropy(random.as_raw_fd(), info)?;
        rnd_reseed_crng(random.as_raw_fd())?;
    }
    Ok(())
}

impl Entropy for LinuxEntropy {
    fn mix_and_reseed(&self, bytes: &[u8]) -> io::Result<()> {
        let info = pool_info(bytes)?;
        let random = File::options().write(true).open("/dev/urandom")?;
        ioctls(&random, &info).map_err(io::Error::from)
    }
}

/// Sets `CLOCK_REALTIME` with `clock_settime`, which needs `CAP_SYS_TIME`.
#[derive(Debug, Default)]
pub struct LinuxClock;

impl Clock for LinuxClock {
    fn set_realtime(&self, unix_nanos: i64) -> io::Result<()> {
        clock_settime(ClockId::CLOCK_REALTIME, TimeSpec::nanoseconds(unix_nanos))
            .map_err(io::Error::from)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_add_entropy_request_is_the_number_in_the_kernels_header() {
        // `_IOW('R', 0x03, int[2])` is 0x40085203, as `random.h` of Linux v6.12 gives it.
        assert_eq!(RNDADDENTROPY, 0x4008_5203);
    }

    #[test]
    fn the_pool_entry_is_laid_out_as_the_kernels_with_every_bit_counted() -> io::Result<()> {
        assert_eq!(std::mem::offset_of!(PoolInfo, buf), 8);
        let info = pool_info(&[7; 32])?;
        assert_eq!(info.entropy_count, 256);
        assert_eq!(info.buf_size, 32);
        assert_eq!(&info.buf[..32], &[7; 32]);
        assert_eq!(&info.buf[32..], &[0; 224]);
        Ok(())
    }

    #[test]
    fn entropy_outside_the_sizes_a_catch_up_carries_is_refused() {
        assert!(pool_info(&[1; 31]).is_err());
        assert!(pool_info(&[1; 257]).is_err());
        assert!(pool_info(&[1; 256]).is_ok());
    }
}
