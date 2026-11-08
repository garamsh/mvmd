//! The Machine's network on a Linux kernel: the addresses without duplicate address detection,
//! the default routes, the resolvers, and rtnetlink messages built here for the kernel.

use std::fs;
use std::io;
use std::net::IpAddr;
use std::os::fd::{AsRawFd, OwnedFd};
use std::sync::Mutex;

use nix::errno::Errno;
use nix::sys::socket::{
    AddressFamily, MsgFlags, SockFlag, SockProtocol, SockType, recv, send, socket,
};

use crate::description::NetworkConfig;
use crate::system::Network;

const RTM_NEWLINK: u16 = 16;
const RTM_NEWADDR: u16 = 20;
const RTM_DELADDR: u16 = 21;
const RTM_NEWROUTE: u16 = 24;

const NLM_F_REQUEST: u16 = 0x1;
const NLM_F_ACK: u16 = 0x4;
const NLM_F_REPLACE: u16 = 0x100;
const NLM_F_CREATE: u16 = 0x400;
const NLMSG_ERROR: u16 = 2;

const IFA_ADDRESS: u16 = 1;
const IFA_LOCAL: u16 = 2;
/// The address is added without duplicate address detection.
const IFA_F_NODAD: u8 = 0x02;
const IFF_UP: u32 = 0x1;

const RTA_OIF: u16 = 4;
const RTA_GATEWAY: u16 = 5;
const RT_TABLE_MAIN: u8 = 254;
const RTPROT_STATIC: u8 = 4;
const RT_SCOPE_UNIVERSE: u8 = 0;
const RTN_UNICAST: u8 = 1;
/// The next hop is on the link even though no address of the interface covers it.
const RTNH_F_ONLINK: u32 = 0x4;

const AF_INET: u8 = 2;
const AF_INET6: u8 = 10;

const RESOLV_CONF: &str = "/etc/resolv.conf";

/// Configures the Machine's one interface through rtnetlink, `/proc/sys` and `/etc`.
#[derive(Debug, Default)]
pub struct LinuxNetwork {
    /// What the last `apply` set, so the next one can remove it.
    applied: Mutex<Option<Applied>>,
}

#[derive(Debug)]
struct Applied {
    index: u32,
    config: NetworkConfig,
}

impl Network for LinuxNetwork {
    fn apply(&self, config: &NetworkConfig) -> io::Result<()> {
        let mut applied = self
            .applied
            .lock()
            .unwrap_or_else(|_| unreachable!("a thread panicked while configuring the network"));
        let netlink = Netlink::open()?;
        netlink.request(&link_up(interface_index("lo")?))?;
        let (name, index) = interface()?;

        if let Some(before) = applied.take() {
            remove_addresses(&netlink, &before)?;
        }
        // Kept before the first change, so a failure part-way still leaves it for the next call.
        *applied = Some(Applied {
            index,
            config: config.clone(),
        });

        // Router advertisements and duplicate address detection are off before the link comes up.
        if config.ipv6.is_some() {
            for setting in ["accept_ra", "accept_dad"] {
                fs::write(format!("/proc/sys/net/ipv6/conf/{name}/{setting}"), "0")?;
            }
        }
        netlink.request(&link_up(index))?;
        if let Some(ipv4) = &config.ipv4 {
            netlink.request(&new_address(index, ipv4.address.into(), ipv4.prefix_len))?;
            netlink.request(&default_route(index, ipv4.gateway.into()))?;
        }
        if let Some(ipv6) = &config.ipv6 {
            netlink.request(&new_address(index, ipv6.address.into(), ipv6.prefix_len))?;
            netlink.request(&default_route(index, ipv6.gateway.into()))?;
        }

        // A symbolic link in the image would point outside what the root disk holds.
        let _ = fs::remove_file(RESOLV_CONF);
        fs::create_dir_all("/etc")?;
        fs::write(RESOLV_CONF, resolv_conf(&config.resolvers))
    }
}

/// Remove the addresses of `before`; one already gone is not an error.
fn remove_addresses(netlink: &Netlink, before: &Applied) -> io::Result<()> {
    let addresses = before
        .config
        .ipv4
        .iter()
        .map(|c| (IpAddr::from(c.address), c.prefix_len))
        .chain(
            before
                .config
                .ipv6
                .iter()
                .map(|c| (IpAddr::from(c.address), c.prefix_len)),
        );
    for (address, prefix_len) in addresses {
        match netlink.request(&delete_address(before.index, address, prefix_len)) {
            Err(error) if error.raw_os_error() != Some(Errno::EADDRNOTAVAIL as i32) => {
                return Err(error);
            }
            _ => {}
        }
    }
    Ok(())
}

/// The Machine's one interface other than loopback, by name and index.
fn interface() -> io::Result<(String, u32)> {
    let mut names = Vec::new();
    for entry in fs::read_dir("/sys/class/net")? {
        let name = entry?.file_name().to_string_lossy().into_owned();
        if name != "lo" {
            names.push(name);
        }
    }
    match names.as_slice() {
        [name] => Ok((name.clone(), interface_index(name)?)),
        [] => Err(io::Error::new(
            io::ErrorKind::NotFound,
            "the Machine has no network interface",
        )),
        several => Err(io::Error::other(format!(
            "the Machine has {} network interfaces, expected one",
            several.len()
        ))),
    }
}

fn interface_index(name: &str) -> io::Result<u32> {
    fs::read_to_string(format!("/sys/class/net/{name}/ifindex"))?
        .trim()
        .parse()
        .map_err(|_| io::Error::other(format!("interface {name} has no readable index")))
}

/// The text of `/etc/resolv.conf` naming `resolvers` in order.
fn resolv_conf(resolvers: &[IpAddr]) -> String {
    resolvers
        .iter()
        .map(|resolver| format!("nameserver {resolver}\n"))
        .collect()
}

/// An rtnetlink socket to the kernel.
struct Netlink(OwnedFd);

impl Netlink {
    fn open() -> io::Result<Self> {
        socket(
            AddressFamily::Netlink,
            SockType::Raw,
            SockFlag::SOCK_CLOEXEC,
            SockProtocol::NetlinkRoute,
        )
        .map(Self)
        .map_err(io::Error::from)
    }

    /// Send one request and wait for the kernel's acknowledgement of it.
    fn request(&self, message: &[u8]) -> io::Result<()> {
        send(self.0.as_raw_fd(), message, MsgFlags::empty()).map_err(io::Error::from)?;
        let mut reply = [0u8; 4096];
        let received =
            recv(self.0.as_raw_fd(), &mut reply, MsgFlags::empty()).map_err(io::Error::from)?;
        acknowledged(&reply[..received])
    }
}

/// The outcome an acknowledgement reports: success, or the error number the kernel gave.
fn acknowledged(reply: &[u8]) -> io::Result<()> {
    // The reply is an nlmsghdr (16 bytes, its type at offset 4) and an NLMSG_ERROR payload
    // that opens with the error number, negative.
    let short = || io::Error::other("the kernel's reply is too short to be an acknowledgement");
    let kind = u16::from_ne_bytes(
        reply
            .get(4..6)
            .ok_or_else(short)?
            .try_into()
            .map_err(|_| short())?,
    );
    if kind != NLMSG_ERROR {
        return Err(io::Error::other(format!(
            "the kernel replied with message type {kind}, not an acknowledgement"
        )));
    }
    let status = i32::from_ne_bytes(
        reply
            .get(16..20)
            .ok_or_else(short)?
            .try_into()
            .map_err(|_| short())?,
    );
    match status {
        0 => Ok(()),
        error => Err(io::Error::from_raw_os_error(-error)),
    }
}

/// An rtnetlink message: the 16-byte header, the fixed `body`, then each attribute padded to four.
fn message(kind: u16, flags: u16, body: &[u8], attributes: &[(u16, Vec<u8>)]) -> Vec<u8> {
    let mut payload = body.to_vec();
    for (attribute, value) in attributes {
        payload.extend(((4 + value.len()) as u16).to_ne_bytes());
        payload.extend(attribute.to_ne_bytes());
        payload.extend(value);
        payload.resize(payload.len().next_multiple_of(4), 0);
    }
    let mut message = Vec::with_capacity(16 + payload.len());
    message.extend(((16 + payload.len()) as u32).to_ne_bytes());
    message.extend(kind.to_ne_bytes());
    message.extend(flags.to_ne_bytes());
    // The sequence number and the port ID are zero: one request is in flight at a time.
    message.extend([0u8; 8]);
    message.extend(payload);
    message
}

fn family(address: IpAddr) -> u8 {
    match address {
        IpAddr::V4(_) => AF_INET,
        IpAddr::V6(_) => AF_INET6,
    }
}

fn octets(address: IpAddr) -> Vec<u8> {
    match address {
        IpAddr::V4(address) => address.octets().to_vec(),
        IpAddr::V6(address) => address.octets().to_vec(),
    }
}

/// `ifaddrmsg`: family, prefix length, flags, scope, interface index.
fn address_body(index: u32, address: IpAddr, prefix_len: u8, flags: u8) -> Vec<u8> {
    let mut body = vec![family(address), prefix_len, flags, RT_SCOPE_UNIVERSE];
    body.extend(index.to_ne_bytes());
    body
}

/// Add `address` to the interface, an IPv6 one without duplicate address detection.
fn new_address(index: u32, address: IpAddr, prefix_len: u8) -> Vec<u8> {
    let flags = if address.is_ipv6() { IFA_F_NODAD } else { 0 };
    message(
        RTM_NEWADDR,
        NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE,
        &address_body(index, address, prefix_len, flags),
        &[(IFA_LOCAL, octets(address)), (IFA_ADDRESS, octets(address))],
    )
}

fn delete_address(index: u32, address: IpAddr, prefix_len: u8) -> Vec<u8> {
    message(
        RTM_DELADDR,
        NLM_F_REQUEST | NLM_F_ACK,
        &address_body(index, address, prefix_len, 0),
        &[(IFA_LOCAL, octets(address)), (IFA_ADDRESS, octets(address))],
    )
}

/// Make `gateway` the default route's next hop of its family, replacing one already there.
fn default_route(index: u32, gateway: IpAddr) -> Vec<u8> {
    // `rtmsg`: family, destination length, source length, TOS, table, protocol, scope, type,
    // then the flags as a 32-bit word.
    let mut body = vec![
        family(gateway),
        0,
        0,
        0,
        RT_TABLE_MAIN,
        RTPROT_STATIC,
        RT_SCOPE_UNIVERSE,
        RTN_UNICAST,
    ];
    let flags = if gateway.is_ipv4() { RTNH_F_ONLINK } else { 0 };
    body.extend(flags.to_ne_bytes());
    message(
        RTM_NEWROUTE,
        NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE,
        &body,
        &[
            (RTA_GATEWAY, octets(gateway)),
            (RTA_OIF, index.to_ne_bytes().to_vec()),
        ],
    )
}

/// Bring the interface up.
fn link_up(index: u32) -> Vec<u8> {
    // `ifinfomsg`: family, padding, type, index, flags, and the mask of the flags to change.
    let mut body = vec![0u8; 4];
    body.extend(index.to_ne_bytes());
    body.extend(IFF_UP.to_ne_bytes());
    body.extend(IFF_UP.to_ne_bytes());
    message(RTM_NEWLINK, NLM_F_REQUEST | NLM_F_ACK, &body, &[])
}

#[cfg(test)]
mod tests {
    use super::*;

    fn header(len: u32, kind: u16, flags: u16) -> Vec<u8> {
        let mut header = len.to_ne_bytes().to_vec();
        header.extend(kind.to_ne_bytes());
        header.extend(flags.to_ne_bytes());
        header.extend([0u8; 8]);
        header
    }

    #[test]
    fn an_ipv6_address_is_added_with_nodad_and_both_attributes() {
        let address: IpAddr = "fd00::2".parse().unwrap();
        let mut expected = header(16 + 8 + 20 + 20, 20, 0x1 | 0x4 | 0x400 | 0x100);
        expected.extend([10, 128, 0x02, 0]);
        expected.extend(3u32.to_ne_bytes());
        for attribute in [2u16, 1] {
            expected.extend(20u16.to_ne_bytes());
            expected.extend(attribute.to_ne_bytes());
            expected.extend(match address {
                IpAddr::V6(a) => a.octets(),
                IpAddr::V4(_) => unreachable!(),
            });
        }
        assert_eq!(new_address(3, address, 128), expected);
    }

    #[test]
    fn an_ipv4_address_carries_no_nodad_flag() {
        let message = new_address(3, "10.1.2.3".parse().unwrap(), 32);
        // The flags are the third byte of the address message, after the 16-byte header.
        assert_eq!(&message[16..20], &[2, 32, 0, 0]);
    }

    #[test]
    fn a_default_route_names_its_gateway_and_interface() {
        let gateway: IpAddr = "fe80::1".parse().unwrap();
        let mut expected = header(16 + 12 + 20 + 8, 24, 0x1 | 0x4 | 0x400 | 0x100);
        expected.extend([10, 0, 0, 0, 254, 4, 0, 1]);
        expected.extend(0u32.to_ne_bytes());
        expected.extend(20u16.to_ne_bytes());
        expected.extend(5u16.to_ne_bytes());
        expected.extend("fe80::1".parse::<std::net::Ipv6Addr>().unwrap().octets());
        expected.extend(8u16.to_ne_bytes());
        expected.extend(4u16.to_ne_bytes());
        expected.extend(3u32.to_ne_bytes());
        assert_eq!(default_route(3, gateway), expected);
    }

    #[test]
    fn an_ipv4_default_route_is_on_link_so_a_host_address_can_reach_it() {
        let message = default_route(3, "10.1.0.1".parse().unwrap());
        // The route's flags follow its eight one-byte fields.
        assert_eq!(&message[24..28], &RTNH_F_ONLINK.to_ne_bytes());
    }

    #[test]
    fn link_up_sets_and_masks_the_up_flag() {
        let mut expected = header(16 + 16, 16, 0x1 | 0x4);
        expected.extend([0, 0, 0, 0]);
        expected.extend(3u32.to_ne_bytes());
        expected.extend(1u32.to_ne_bytes());
        expected.extend(1u32.to_ne_bytes());
        assert_eq!(link_up(3), expected);
    }

    #[test]
    fn an_acknowledgement_is_success_or_the_errno_the_kernel_gave() {
        let mut reply = header(36, NLMSG_ERROR, 0);
        reply.extend(0i32.to_ne_bytes());
        assert!(acknowledged(&reply).is_ok());

        let mut reply = header(36, NLMSG_ERROR, 0);
        reply.extend((-(Errno::EEXIST as i32)).to_ne_bytes());
        let error = acknowledged(&reply).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(Errno::EEXIST as i32));

        assert!(acknowledged(&reply[..10]).is_err());
        assert!(acknowledged(&header(36, 3, 0)).is_err());
    }

    #[test]
    fn resolvers_are_written_in_the_order_given() {
        let resolvers: Vec<IpAddr> =
            vec!["10.0.0.53".parse().unwrap(), "fd00::53".parse().unwrap()];
        assert_eq!(
            resolv_conf(&resolvers),
            "nameserver 10.0.0.53\nnameserver fd00::53\n"
        );
        assert_eq!(resolv_conf(&[]), "");
    }
}
