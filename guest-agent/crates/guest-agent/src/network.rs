//! The Machine's network on a Linux kernel: the addresses without duplicate address detection,
//! the default routes, the resolvers, and rtnetlink messages built here for the kernel.

use std::fs;
use std::io;
use std::net::IpAddr;
use std::os::fd::{AsRawFd, OwnedFd};
use std::path::Path;
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
const RTM_DELROUTE: u16 = 25;

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

const CLASS_NET: &str = "/sys/class/net";
/// The driver of the Machine's network device.
const NET_DRIVER: &str = "virtio_net";
const RESOLV_CONF: &str = "/etc/resolv.conf";

/// Configures the Machine's one interface through rtnetlink, `/proc/sys` and `/etc`.
#[derive(Debug, Default)]
pub struct LinuxNetwork {
    /// What the last `apply` set, so the next one can remove it.
    applied: Mutex<Option<NetworkConfig>>,
}

/// One rtnetlink request, with the errors that mean it was already done.
struct Request {
    message: Vec<u8>,
    tolerated: &'static [Errno],
}

impl Request {
    fn new(message: Vec<u8>) -> Self {
        Self {
            message,
            tolerated: &[],
        }
    }

    fn unless(message: Vec<u8>, tolerated: &'static [Errno]) -> Self {
        Self { message, tolerated }
    }
}

/// What turns one configuration of an interface into the next.
struct Plan {
    /// Removes what the previous configuration set.
    removals: Vec<Request>,
    /// Brings the link up, then sets the new configuration.
    additions: Vec<Request>,
}

/// The requests that replace `before`, if any, with `after` on the interface `index`.
fn plan(before: Option<&NetworkConfig>, after: &NetworkConfig, index: u32) -> io::Result<Plan> {
    let mut removals = Vec::new();
    if let Some(old) = before {
        let ipv4 = old
            .ipv4
            .iter()
            .map(|c| (c.address.into(), c.prefix_len, c.gateway.into()));
        let ipv6 = old
            .ipv6
            .iter()
            .map(|c| (c.address.into(), c.prefix_len, c.gateway.into()));
        for (address, prefix_len, gateway) in ipv4.chain(ipv6) {
            // The default route goes first and goes whether or not the new configuration
            // keeps its family; the address removal alone would leave it.
            removals.push(Request::unless(
                delete_route(index, gateway)?,
                &[Errno::ESRCH],
            ));
            removals.push(Request::unless(
                delete_address(index, address, prefix_len)?,
                &[Errno::EADDRNOTAVAIL],
            ));
        }
    }
    let mut additions = vec![Request::new(link_up(index)?)];
    for (address, prefix_len, gateway) in after
        .ipv4
        .iter()
        .map(|c| {
            (
                IpAddr::from(c.address),
                c.prefix_len,
                IpAddr::from(c.gateway),
            )
        })
        .chain(after.ipv6.iter().map(|c| {
            (
                IpAddr::from(c.address),
                c.prefix_len,
                IpAddr::from(c.gateway),
            )
        }))
    {
        additions.push(Request::new(new_address(index, address, prefix_len)?));
        additions.push(Request::new(default_route(index, gateway)?));
    }
    Ok(Plan {
        removals,
        additions,
    })
}

impl Network for LinuxNetwork {
    fn apply(&self, config: &NetworkConfig) -> io::Result<()> {
        let mut applied = self
            .applied
            .lock()
            .unwrap_or_else(|_| unreachable!("a thread panicked while configuring the network"));
        let netlink = Netlink::open()?;
        netlink.request(&Request::new(link_up(interface_index("lo")?)?))?;
        let (name, index) = interface_in(Path::new(CLASS_NET))?;

        // Router advertisements and duplicate address detection are off before the link comes
        // up, for the link-local address the kernel forms whatever the configuration holds.
        let ipv6_conf = format!("/proc/sys/net/ipv6/conf/{name}");
        if Path::new(&ipv6_conf).exists() {
            for setting in ["accept_ra", "accept_dad"] {
                fs::write(format!("{ipv6_conf}/{setting}"), "0")?;
            }
        }

        let plan = plan(applied.as_ref(), config, index)?;
        for request in &plan.removals {
            netlink.request(request)?;
        }
        // Kept before the first addition, so a failure part-way still leaves it for the next call.
        *applied = Some(config.clone());
        for request in &plan.additions {
            netlink.request(request)?;
        }

        // A symbolic link in the image would point outside what the root disk holds.
        let _ = fs::remove_file(RESOLV_CONF);
        fs::create_dir_all("/etc")?;
        fs::write(RESOLV_CONF, resolv_conf(&config.resolvers))
    }
}

/// The Machine's network interface under `class_net` (`/sys/class/net`): the one whose driver is
/// the virtio network driver, which a kernel's fallback tunnel devices are not.
fn interface_in(class_net: &Path) -> io::Result<(String, u32)> {
    let mut found = Vec::new();
    for entry in fs::read_dir(class_net)? {
        let entry = entry?;
        // A device with no driver link, such as a loopback or a tunnel, is not ours.
        let driver = fs::read_link(entry.path().join("device/driver")).ok();
        if driver.and_then(|d| d.file_name().map(|n| n == NET_DRIVER)) == Some(true) {
            found.push(entry.file_name().to_string_lossy().into_owned());
        }
    }
    match found.as_slice() {
        [name] => Ok((name.clone(), index_in(class_net, name)?)),
        [] => Err(io::Error::new(
            io::ErrorKind::NotFound,
            format!("no network interface is driven by {NET_DRIVER}"),
        )),
        several => Err(io::Error::other(format!(
            "{} network interfaces are driven by {NET_DRIVER}, expected one",
            several.len()
        ))),
    }
}

fn interface_index(name: &str) -> io::Result<u32> {
    index_in(Path::new(CLASS_NET), name)
}

fn index_in(class_net: &Path, name: &str) -> io::Result<u32> {
    fs::read_to_string(class_net.join(name).join("ifindex"))?
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

    /// Send one request and wait for the kernel's acknowledgement of it. An error the request
    /// tolerates counts as an acknowledgement.
    fn request(&self, request: &Request) -> io::Result<()> {
        send(self.0.as_raw_fd(), &request.message, MsgFlags::empty()).map_err(io::Error::from)?;
        let mut reply = [0u8; 4096];
        let received =
            recv(self.0.as_raw_fd(), &mut reply, MsgFlags::empty()).map_err(io::Error::from)?;
        match acknowledged(&reply[..received]) {
            Err(error)
                if request
                    .tolerated
                    .iter()
                    .any(|tolerated| error.raw_os_error() == Some(*tolerated as i32)) =>
            {
                Ok(())
            }
            outcome => outcome,
        }
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
fn message(
    kind: u16,
    flags: u16,
    body: &[u8],
    attributes: &[(u16, Vec<u8>)],
) -> io::Result<Vec<u8>> {
    let too_long = || io::Error::other("an rtnetlink message or attribute is too long");
    let mut payload = body.to_vec();
    for (attribute, value) in attributes {
        let length = u16::try_from(4 + value.len()).map_err(|_| too_long())?;
        payload.extend(length.to_ne_bytes());
        payload.extend(attribute.to_ne_bytes());
        payload.extend(value);
        payload.resize(payload.len().next_multiple_of(4), 0);
    }
    let length = u32::try_from(16 + payload.len()).map_err(|_| too_long())?;
    let mut message = Vec::with_capacity(16 + payload.len());
    message.extend(length.to_ne_bytes());
    message.extend(kind.to_ne_bytes());
    message.extend(flags.to_ne_bytes());
    // The sequence number and the port ID are zero: one request is in flight at a time.
    message.extend([0u8; 8]);
    message.extend(payload);
    Ok(message)
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
fn new_address(index: u32, address: IpAddr, prefix_len: u8) -> io::Result<Vec<u8>> {
    let flags = if address.is_ipv6() { IFA_F_NODAD } else { 0 };
    message(
        RTM_NEWADDR,
        NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE,
        &address_body(index, address, prefix_len, flags),
        &[(IFA_LOCAL, octets(address)), (IFA_ADDRESS, octets(address))],
    )
}

fn delete_address(index: u32, address: IpAddr, prefix_len: u8) -> io::Result<Vec<u8>> {
    message(
        RTM_DELADDR,
        NLM_F_REQUEST | NLM_F_ACK,
        &address_body(index, address, prefix_len, 0),
        &[(IFA_LOCAL, octets(address)), (IFA_ADDRESS, octets(address))],
    )
}

/// `rtmsg` and its attributes for the default route via `gateway`.
fn route(kind: u16, flags: u16, index: u32, gateway: IpAddr) -> io::Result<Vec<u8>> {
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
    let route_flags = if gateway.is_ipv4() { RTNH_F_ONLINK } else { 0 };
    body.extend(route_flags.to_ne_bytes());
    message(
        kind,
        flags,
        &body,
        &[
            (RTA_GATEWAY, octets(gateway)),
            (RTA_OIF, index.to_ne_bytes().to_vec()),
        ],
    )
}

/// Make `gateway` the default route's next hop of its family, replacing one already there.
fn default_route(index: u32, gateway: IpAddr) -> io::Result<Vec<u8>> {
    route(
        RTM_NEWROUTE,
        NLM_F_REQUEST | NLM_F_ACK | NLM_F_CREATE | NLM_F_REPLACE,
        index,
        gateway,
    )
}

/// Remove the default route via `gateway`.
fn delete_route(index: u32, gateway: IpAddr) -> io::Result<Vec<u8>> {
    route(RTM_DELROUTE, NLM_F_REQUEST | NLM_F_ACK, index, gateway)
}

/// Bring the interface up.
fn link_up(index: u32) -> io::Result<Vec<u8>> {
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
    use crate::description::Addressing;

    type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

    fn header(len: u32, kind: u16, flags: u16) -> Vec<u8> {
        let mut header = len.to_ne_bytes().to_vec();
        header.extend(kind.to_ne_bytes());
        header.extend(flags.to_ne_bytes());
        header.extend([0u8; 8]);
        header
    }

    #[test]
    fn an_ipv6_address_is_added_with_nodad_and_both_attributes() -> Result {
        let address: IpAddr = "fd00::2".parse()?;
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
        assert_eq!(new_address(3, address, 128)?, expected);
        Ok(())
    }

    #[test]
    fn an_ipv4_address_carries_no_nodad_flag() -> Result {
        let message = new_address(3, "10.1.2.3".parse()?, 32)?;
        // The flags are the third byte of the address message, after the 16-byte header.
        assert_eq!(&message[16..20], &[2, 32, 0, 0]);
        Ok(())
    }

    #[test]
    fn a_default_route_names_its_gateway_and_interface() -> Result {
        let gateway: IpAddr = "fe80::1".parse()?;
        let mut expected = header(16 + 12 + 20 + 8, 24, 0x1 | 0x4 | 0x400 | 0x100);
        expected.extend([10, 0, 0, 0, 254, 4, 0, 1]);
        expected.extend(0u32.to_ne_bytes());
        expected.extend(20u16.to_ne_bytes());
        expected.extend(5u16.to_ne_bytes());
        expected.extend("fe80::1".parse::<std::net::Ipv6Addr>()?.octets());
        expected.extend(8u16.to_ne_bytes());
        expected.extend(4u16.to_ne_bytes());
        expected.extend(3u32.to_ne_bytes());
        assert_eq!(default_route(3, gateway)?, expected);
        Ok(())
    }

    #[test]
    fn an_ipv4_default_route_is_on_link_so_a_host_address_can_reach_it() -> Result {
        let message = default_route(3, "10.1.0.1".parse()?)?;
        // The route's flags follow its eight one-byte fields.
        assert_eq!(&message[24..28], &RTNH_F_ONLINK.to_ne_bytes());
        Ok(())
    }

    #[test]
    fn a_route_removal_is_the_route_message_with_the_delete_type() -> Result {
        let add = default_route(3, "10.1.0.1".parse()?)?;
        let delete = delete_route(3, "10.1.0.1".parse()?)?;
        assert_eq!(&delete[4..6], &RTM_DELROUTE.to_ne_bytes());
        // Everything but the type and the request flags is the same.
        assert_eq!(&delete[16..], &add[16..]);
        Ok(())
    }

    #[test]
    fn link_up_sets_and_masks_the_up_flag() -> Result {
        let mut expected = header(16 + 16, 16, 0x1 | 0x4);
        expected.extend([0, 0, 0, 0]);
        expected.extend(3u32.to_ne_bytes());
        expected.extend(1u32.to_ne_bytes());
        expected.extend(1u32.to_ne_bytes());
        assert_eq!(link_up(3)?, expected);
        Ok(())
    }

    #[test]
    fn an_attribute_too_long_for_its_length_field_is_refused() {
        let value = vec![0u8; usize::from(u16::MAX)];
        assert!(message(RTM_NEWADDR, 0, &[], &[(IFA_LOCAL, value)]).is_err());
        // The control: one that fits is accepted.
        assert!(message(RTM_NEWADDR, 0, &[], &[(IFA_LOCAL, vec![0; 16])]).is_ok());
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
    fn resolvers_are_written_in_the_order_given() -> Result {
        let resolvers: Vec<IpAddr> = vec!["10.0.0.53".parse()?, "fd00::53".parse()?];
        assert_eq!(
            resolv_conf(&resolvers),
            "nameserver 10.0.0.53\nnameserver fd00::53\n"
        );
        assert_eq!(resolv_conf(&[]), "");
        Ok(())
    }

    fn both() -> Result<NetworkConfig> {
        Ok(NetworkConfig {
            ipv4: Some(Addressing {
                address: "10.1.2.3".parse()?,
                prefix_len: 32,
                gateway: "10.1.0.1".parse()?,
            }),
            ipv6: Some(Addressing {
                address: "fd00::2".parse()?,
                prefix_len: 128,
                gateway: "fe80::1".parse()?,
            }),
            resolvers: Vec::new(),
        })
    }

    /// The type and the family of each request, as the kernel reads them.
    fn kinds(requests: &[Request]) -> Vec<(u16, u8)> {
        requests
            .iter()
            .map(|r| {
                (
                    u16::from_ne_bytes([r.message[4], r.message[5]]),
                    r.message[16],
                )
            })
            .collect()
    }

    #[test]
    fn an_update_that_drops_a_family_removes_its_address_and_route_and_adds_none() -> Result {
        let before = both()?;
        let after = NetworkConfig {
            ipv4: None,
            ..both()?
        };

        let plan = plan(Some(&before), &after, 3)?;

        let removals = kinds(&plan.removals);
        for removed in [
            (RTM_DELROUTE, AF_INET),
            (RTM_DELADDR, AF_INET),
            (RTM_DELROUTE, AF_INET6),
            (RTM_DELADDR, AF_INET6),
        ] {
            assert!(removals.contains(&removed), "{removed:?} in {removals:?}");
        }
        let additions = kinds(&plan.additions);
        assert!(!additions.iter().any(|(kind, family)| {
            *family == AF_INET && (*kind == RTM_NEWADDR || *kind == RTM_NEWROUTE)
        }));
        assert!(additions.contains(&(RTM_NEWADDR, AF_INET6)));
        Ok(())
    }

    #[test]
    fn an_update_that_keeps_a_family_replaces_its_address_and_route() -> Result {
        let plan = plan(Some(&both()?), &both()?, 3)?;

        let additions = kinds(&plan.additions);
        for added in [
            (RTM_NEWADDR, AF_INET),
            (RTM_NEWROUTE, AF_INET),
            (RTM_NEWADDR, AF_INET6),
            (RTM_NEWROUTE, AF_INET6),
        ] {
            assert!(additions.contains(&added), "{added:?} in {additions:?}");
        }
        // The first configuration has nothing to remove.
        assert!(super::plan(None, &both()?, 3)?.removals.is_empty());
        Ok(())
    }

    /// A `/sys/class/net` with the named devices, each with the driver given or none.
    fn class_net(devices: &[(&str, Option<&str>)]) -> Result<tempfile::TempDir> {
        let dir = tempfile::tempdir()?;
        for (at, (name, driver)) in devices.iter().enumerate() {
            let device = dir.path().join(name);
            fs::create_dir_all(device.join("device"))?;
            fs::write(device.join("ifindex"), format!("{}\n", at + 1))?;
            if let Some(driver) = driver {
                let target = dir.path().join("drivers").join(driver);
                fs::create_dir_all(&target)?;
                std::os::unix::fs::symlink(&target, device.join("device/driver"))?;
            }
        }
        Ok(dir)
    }

    #[test]
    fn the_interface_is_the_virtio_one_among_a_kernels_fallback_tunnels() -> Result {
        let dir = class_net(&[
            ("lo", None),
            ("sit0", None),
            ("tunl0", None),
            ("ip6tnl0", None),
            ("eth0", Some("virtio_net")),
        ])?;
        assert_eq!(interface_in(dir.path())?, ("eth0".to_owned(), 5));

        // The control: without the virtio device no interface is found, so the fallback
        // devices were passed over and not chosen.
        let without = class_net(&[("lo", None), ("sit0", None), ("eth1", Some("e1000"))])?;
        assert!(interface_in(without.path()).is_err());
        Ok(())
    }

    #[test]
    fn two_virtio_interfaces_are_refused() -> Result {
        let dir = class_net(&[("eth0", Some("virtio_net")), ("eth1", Some("virtio_net"))])?;
        assert!(interface_in(dir.path()).is_err());
        Ok(())
    }
}
