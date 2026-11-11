//! The boot description mvmd sends over the guest channel, parsed once at the boundary into
//! types the rest of the boot trusts.

use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::str::FromStr;

use crate::error::Error;
use crate::uuid::Uuid;
use crate::v1alpha1;

/// Everything mvmd tells the agent about the Machine at boot.
#[derive(Debug, Clone, PartialEq)]
pub struct BootDescription {
    /// The filesystem UUID of the MachineImage's EROFS.
    pub image: Uuid,
    /// The filesystem UUID of the root disk.
    pub root_disk: Uuid,
    /// The entrypoint.
    pub run: RunConfig,
    /// The Machine's network.
    pub network: NetworkConfig,
}

/// The entrypoint's command line, environment and working directory.
#[derive(Debug, Clone, PartialEq)]
pub struct RunConfig {
    /// The program and its arguments; never empty.
    pub argv: Vec<String>,
    /// The entrypoint's whole environment.
    pub env: HashMap<String, String>,
    /// The working directory in the booted root; empty keeps `/`.
    pub working_dir: String,
}

/// What the agent configures on the Machine's interface. A family that is `None` is not served.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct NetworkConfig {
    /// The IPv4 address and its gateway.
    pub ipv4: Option<Addressing<Ipv4Addr>>,
    /// The IPv6 address and its gateway.
    pub ipv6: Option<Addressing<Ipv6Addr>>,
    /// The resolvers, in the order they are written.
    pub resolvers: Vec<IpAddr>,
}

/// An address of one family with its prefix length and the next hop of its default route.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Addressing<A> {
    /// The Machine's address.
    pub address: A,
    /// The address's prefix length.
    pub prefix_len: u8,
    /// The default route's next hop.
    pub gateway: A,
}

impl TryFrom<v1alpha1::BootRequest> for BootDescription {
    type Error = Error;

    fn try_from(request: v1alpha1::BootRequest) -> Result<Self, Error> {
        let run = request
            .run
            .ok_or_else(|| Error::Invalid("run is missing".into()))?;
        if run.argv.is_empty() {
            return Err(Error::Invalid("run.argv is empty".into()));
        }
        if !run.working_dir.is_empty() && !run.working_dir.starts_with('/') {
            return Err(Error::Invalid(
                "run.working_dir is not an absolute path".into(),
            ));
        }
        Ok(Self {
            image: Uuid::parse(&request.image_uuid)?,
            root_disk: Uuid::parse(&request.root_disk_uuid)?,
            run: RunConfig {
                argv: run.argv,
                env: run.env,
                working_dir: run.working_dir,
            },
            network: request.network.unwrap_or_default().try_into()?,
        })
    }
}

/// The host's wall clock a `Boot` request carries, when it asks for the clock to be set: `None`
/// for 0, which leaves the clock as the kernel set it.
pub fn boot_time(request: &v1alpha1::BootRequest) -> Result<Option<i64>, Error> {
    match request.host_unix_nanos {
        0 => Ok(None),
        nanos if nanos > 0 => Ok(Some(nanos)),
        _ => Err(Error::Invalid("host_unix_nanos is negative".into())),
    }
}

/// What mvmd sends to catch a restored guest up.
#[derive(Debug, Clone, PartialEq)]
pub struct CatchUp {
    /// Unique to the restore.
    pub restore_id: String,
    /// Fresh random bytes for the kernel's pool.
    pub entropy: Vec<u8>,
    /// The host's wall clock, in nanoseconds since the Unix epoch; positive.
    pub host_unix_nanos: i64,
    /// The Machine's network after the restore.
    pub network: NetworkConfig,
}

/// The fewest and the most bytes of entropy a catch-up carries.
pub const ENTROPY_BYTES: std::ops::RangeInclusive<usize> = 32..=256;

impl TryFrom<v1alpha1::CatchUpRequest> for CatchUp {
    type Error = Error;

    fn try_from(request: v1alpha1::CatchUpRequest) -> Result<Self, Error> {
        if request.restore_id.is_empty() {
            return Err(Error::Invalid("restore_id is empty".into()));
        }
        if !ENTROPY_BYTES.contains(&request.entropy.len()) {
            return Err(Error::Invalid(format!(
                "entropy is {} bytes, not from {} to {}",
                request.entropy.len(),
                ENTROPY_BYTES.start(),
                ENTROPY_BYTES.end()
            )));
        }
        if request.host_unix_nanos <= 0 {
            return Err(Error::Invalid("host_unix_nanos is not positive".into()));
        }
        let network = request
            .network
            .ok_or_else(|| Error::Invalid("network is missing".into()))?;
        Ok(Self {
            restore_id: request.restore_id,
            entropy: request.entropy,
            host_unix_nanos: request.host_unix_nanos,
            network: network.try_into()?,
        })
    }
}

impl TryFrom<v1alpha1::NetworkConfig> for NetworkConfig {
    type Error = Error;

    fn try_from(config: v1alpha1::NetworkConfig) -> Result<Self, Error> {
        Ok(Self {
            ipv4: config
                .ipv4
                .map(|c| Addressing::parse(&c.address, &c.gateway, 32))
                .transpose()?,
            ipv6: config
                .ipv6
                .map(|c| Addressing::parse(&c.address, &c.gateway, 128))
                .transpose()?,
            resolvers: config
                .resolvers
                .iter()
                .map(|text| parse_address(text))
                .collect::<Result<_, _>>()?,
        })
    }
}

impl<A: FromStr> Addressing<A> {
    /// Parse `address` as "address/prefix" with a prefix of at most `max_prefix`, and `gateway`.
    fn parse(address: &str, gateway: &str, max_prefix: u8) -> Result<Self, Error> {
        let (address_text, prefix_text) = address
            .split_once('/')
            .ok_or_else(|| Error::Invalid(format!("{address:?} has no prefix length")))?;
        let prefix_len: u8 = prefix_text
            .parse()
            .ok()
            .filter(|prefix| *prefix <= max_prefix)
            .ok_or_else(|| Error::Invalid(format!("{address:?} has a bad prefix length")))?;
        Ok(Self {
            address: parse_address(address_text)?,
            prefix_len,
            gateway: parse_address(gateway)?,
        })
    }
}

fn parse_address<A: FromStr>(text: &str) -> Result<A, Error> {
    text.parse()
        .map_err(|_| Error::Invalid(format!("{text:?} is not an address of the expected family")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn network(ipv4: (&str, &str), ipv6: (&str, &str)) -> v1alpha1::NetworkConfig {
        v1alpha1::NetworkConfig {
            ipv4: Some(v1alpha1::Ipv4Config {
                address: ipv4.0.into(),
                gateway: ipv4.1.into(),
            }),
            ipv6: Some(v1alpha1::Ipv6Config {
                address: ipv6.0.into(),
                gateway: ipv6.1.into(),
            }),
            resolvers: vec!["10.0.0.53".into(), "fd00::53".into()],
        }
    }

    #[test]
    fn a_network_config_parses_into_typed_addresses() {
        let parsed: NetworkConfig =
            network(("10.1.2.3/32", "10.1.0.1"), ("fd00::2/128", "fe80::1"))
                .try_into()
                .unwrap();
        let ipv4 = parsed.ipv4.unwrap();
        assert_eq!(ipv4.address, Ipv4Addr::new(10, 1, 2, 3));
        assert_eq!(ipv4.prefix_len, 32);
        assert_eq!(ipv4.gateway, Ipv4Addr::new(10, 1, 0, 1));
        let ipv6 = parsed.ipv6.unwrap();
        assert_eq!(ipv6.address, "fd00::2".parse::<Ipv6Addr>().unwrap());
        assert_eq!(ipv6.prefix_len, 128);
        assert_eq!(ipv6.gateway, "fe80::1".parse::<Ipv6Addr>().unwrap());
        assert_eq!(
            parsed.resolvers,
            [
                "10.0.0.53".parse::<IpAddr>().unwrap(),
                "fd00::53".parse().unwrap()
            ]
        );
    }

    #[test]
    fn a_network_config_refuses_what_is_not_an_address_of_its_family() {
        for (ipv4, ipv6) in [
            (("10.1.2.3", "10.1.0.1"), ("fd00::2/128", "fe80::1")),
            (("10.1.2.3/33", "10.1.0.1"), ("fd00::2/128", "fe80::1")),
            (("fd00::2/32", "10.1.0.1"), ("fd00::2/128", "fe80::1")),
            (("10.1.2.3/32", "fe80::1"), ("fd00::2/128", "fe80::1")),
            (("10.1.2.3/32", "10.1.0.1"), ("fd00::2/129", "fe80::1")),
            (("10.1.2.3/32", "10.1.0.1"), ("10.1.2.3/128", "fe80::1")),
        ] {
            assert!(
                NetworkConfig::try_from(network(ipv4, ipv6)).is_err(),
                "{ipv4:?} {ipv6:?} was accepted"
            );
        }
        // The control: the same shapes with every part right are accepted.
        assert!(
            NetworkConfig::try_from(network(
                ("10.1.2.3/32", "10.1.0.1"),
                ("fd00::2/128", "fe80::1")
            ))
            .is_ok()
        );
    }

    #[test]
    fn a_family_without_a_message_is_not_served() {
        let parsed = NetworkConfig::try_from(v1alpha1::NetworkConfig::default()).unwrap();
        assert_eq!(parsed, NetworkConfig::default());
    }
}
