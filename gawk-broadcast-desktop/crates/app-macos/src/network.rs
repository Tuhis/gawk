//! What the broadcast leaves on (docs/57 D7 detection, WU3): whether the
//! link carrying it to the relay is Wi-Fi, and whether `awdl0` — AirDrop,
//! AirPlay, Sidecar, Universal Control — is up. Unprivileged and
//! framework-free: a connected UDP socket names the local address the
//! kernel routes to the relay from, `getifaddrs` maps it to an interface,
//! and `SIOCGIFMEDIA` says whether that interface is 802.11 (what
//! `ifconfig` reads for its media line).
//!
//! **A VPN hides the radio.** A relay reached through a tunnel (`utun*`,
//! point-to-point) routes via an interface with no media at all, while the
//! tunnel's own packets still cross the Wi-Fi AWDL disrupts — the reference
//! Mac reaches the official relay exactly that way. So a tunnel route is
//! looked through: first to the interface a public address routes via (a
//! split tunnel), and failing that to the machine's active links (a full
//! tunnel): Wi-Fi when an associated Wi-Fi link has an address and no
//! wired link does.
//!
//! The one unsafe module in this crate besides `notify`.

use gawk_engine::lossnotice::NetworkFacts;
use std::ffi::CStr;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket};

/// `_IOWR('i', 56, struct ifmediareq)` from `<sys/sockio.h>`; the size in
/// it (0x2c = 44) is pinned by the struct test below.
const SIOCGIFMEDIA: libc::c_ulong = 0xc02c_6938;
/// `<net/if_media.h>`: the network type bits, the two types we tell apart,
/// and the link-status bits.
const IFM_NMASK: libc::c_int = 0xe0;
const IFM_ETHER: libc::c_int = 0x20;
const IFM_IEEE80211: libc::c_int = 0x80;
const IFM_AVALID: libc::c_int = 0x1;
const IFM_ACTIVE: libc::c_int = 0x2;

/// Where a split tunnel's physical route is asked for. `connect` on UDP
/// sends nothing, so these are never contacted.
const PUBLIC_V4: Ipv4Addr = Ipv4Addr::new(1, 1, 1, 1);
const PUBLIC_V6: Ipv6Addr = Ipv6Addr::new(0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111);

/// `struct ifmediareq`, which Darwin declares under `#pragma pack(4)`.
#[repr(C, packed(4))]
struct IfMediaReq {
    ifm_name: [libc::c_char; libc::IFNAMSIZ],
    ifm_current: libc::c_int,
    ifm_mask: libc::c_int,
    ifm_status: libc::c_int,
    ifm_active: libc::c_int,
    ifm_count: libc::c_int,
    ifm_ulist: *mut libc::c_int,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Media {
    /// No media: loopback, tunnels, bridges.
    None,
    Wifi {
        active: bool,
    },
    Wired {
        active: bool,
    },
}

#[derive(Debug, Clone)]
struct Iface {
    name: String,
    up: bool,
    /// `IFF_POINTOPOINT`: a tunnel (utun, ipsec, ppp).
    tunnel: bool,
    addrs: Vec<IpAddr>,
    media: Media,
}

impl Iface {
    /// Carries traffic beyond the link: an address that is not link-local.
    fn routable(&self) -> bool {
        self.addrs.iter().any(|a| match a {
            IpAddr::V4(v4) => !v4.is_loopback() && !v4.is_link_local(),
            IpAddr::V6(v6) => !v6.is_loopback() && (v6.segments()[0] & 0xffc0) != 0xfe80,
        })
    }
}

/// `None` when the route or the interfaces could not be read — the notice
/// then stays off rather than guess.
pub fn probe(relay: SocketAddr) -> Option<NetworkFacts> {
    let relay_ip = relay.ip().to_canonical();
    let ifaces = interfaces()?;
    let route = iface_for(local_ip_towards(relay_ip)?, &ifaces)?;
    let public: IpAddr = if relay_ip.is_ipv4() {
        PUBLIC_V4.into()
    } else {
        PUBLIC_V6.into()
    };
    let public_route = local_ip_towards(public).and_then(|ip| iface_for(ip, &ifaces));
    Some(classify(&ifaces, route, public_route))
}

fn iface_for(local: IpAddr, ifaces: &[Iface]) -> Option<&str> {
    ifaces
        .iter()
        .find(|i| i.addrs.contains(&local))
        .map(|i| i.name.as_str())
}

/// The decision, pure: `route` carries the broadcast; `public_route` is
/// where a public address would go (a split tunnel's physical link).
fn classify(ifaces: &[Iface], route: &str, public_route: Option<&str>) -> NetworkFacts {
    let find = |name: &str| ifaces.iter().find(|i| i.name == name);
    let awdl_up = find("awdl0").is_some_and(|i| i.up);
    let Some(r) = find(route) else {
        return NetworkFacts {
            awdl_up,
            ..Default::default()
        };
    };
    if !r.tunnel {
        return NetworkFacts {
            wifi: matches!(r.media, Media::Wifi { .. }),
            vpn: false,
            awdl_up,
        };
    }
    let wifi = match public_route.and_then(find).filter(|p| !p.tunnel) {
        Some(p) => matches!(p.media, Media::Wifi { .. }),
        None => {
            let live = |want: fn(Media) -> bool| {
                ifaces
                    .iter()
                    .any(|i| i.up && !i.tunnel && want(i.media) && i.routable())
            };
            live(|m| m == Media::Wifi { active: true })
                && !live(|m| m == Media::Wired { active: true })
        }
    };
    NetworkFacts {
        wifi,
        vpn: true,
        awdl_up,
    }
}

/// The local address the kernel picks for `dest`. `connect` on UDP sends
/// nothing; it only resolves the route.
fn local_ip_towards(dest: IpAddr) -> Option<IpAddr> {
    // quinn's dual-stack socket reports IPv4 peers as `::ffff:a.b.c.d`.
    let ip = dest.to_canonical();
    let bind: SocketAddr = if ip.is_ipv4() {
        ([0, 0, 0, 0], 0).into()
    } else {
        ([0u16; 8], 0).into()
    };
    let sock = UdpSocket::bind(bind).ok()?;
    sock.connect((ip, 4433)).ok()?;
    Some(sock.local_addr().ok()?.ip())
}

fn interfaces() -> Option<Vec<Iface>> {
    let mut head: *mut libc::ifaddrs = std::ptr::null_mut();
    // SAFETY: getifaddrs fills `head` with a list we free below.
    if unsafe { libc::getifaddrs(&mut head) } != 0 {
        return None;
    }
    let mut out: Vec<Iface> = Vec::new();
    let mut cur = head;
    while !cur.is_null() {
        // SAFETY: a non-null node of the list getifaddrs returned; its name
        // is a NUL-terminated string and its addr, when set, is a sockaddr
        // of the family it declares.
        let (name, flags, addr) = unsafe {
            let ifa = &*cur;
            let name = CStr::from_ptr(ifa.ifa_name).to_string_lossy().into_owned();
            (name, ifa.ifa_flags, sockaddr_ip(ifa.ifa_addr))
        };
        let i = match out.iter().position(|i| i.name == name) {
            Some(i) => i,
            None => {
                out.push(Iface {
                    media: media(&name),
                    name,
                    up: flags & libc::IFF_UP as u32 != 0,
                    tunnel: flags & libc::IFF_POINTOPOINT as u32 != 0,
                    addrs: Vec::new(),
                });
                out.len() - 1
            }
        };
        out[i].addrs.extend(addr);
        // SAFETY: same list.
        cur = unsafe { (*cur).ifa_next };
    }
    // SAFETY: the list getifaddrs allocated, freed once.
    unsafe { libc::freeifaddrs(head) };
    Some(out)
}

/// # Safety
/// `sa` is null or points at a sockaddr of the family it declares.
unsafe fn sockaddr_ip(sa: *const libc::sockaddr) -> Option<IpAddr> {
    if sa.is_null() {
        return None;
    }
    // SAFETY: the caller's contract; the family selects the layout.
    unsafe {
        match i32::from((*sa).sa_family) {
            libc::AF_INET => {
                let v4 = &*(sa as *const libc::sockaddr_in);
                Some(IpAddr::from(u32::from_be(v4.sin_addr.s_addr).to_be_bytes()))
            }
            libc::AF_INET6 => {
                let v6 = &*(sa as *const libc::sockaddr_in6);
                Some(IpAddr::from(v6.sin6_addr.s6_addr))
            }
            _ => None,
        }
    }
}

fn media(name: &str) -> Media {
    let bytes = name.as_bytes();
    if bytes.len() >= libc::IFNAMSIZ {
        return Media::None;
    }
    let mut req = IfMediaReq {
        ifm_name: [0; libc::IFNAMSIZ],
        ifm_current: 0,
        ifm_mask: 0,
        ifm_status: 0,
        ifm_active: 0,
        ifm_count: 0,
        ifm_ulist: std::ptr::null_mut(),
    };
    for (d, s) in req.ifm_name.iter_mut().zip(bytes) {
        *d = *s as libc::c_char;
    }
    // SAFETY: a plain datagram socket, closed below; SIOCGIFMEDIA reads the
    // name and writes the fixed fields (ifm_count 0 = no list wanted).
    unsafe {
        let fd = libc::socket(libc::AF_INET, libc::SOCK_DGRAM, 0);
        if fd < 0 {
            return Media::None;
        }
        let rc = libc::ioctl(fd, SIOCGIFMEDIA, &mut req as *mut IfMediaReq);
        libc::close(fd);
        if rc != 0 {
            return Media::None;
        }
    }
    let (current, status) = (req.ifm_current, req.ifm_status);
    let active = status & IFM_AVALID != 0 && status & IFM_ACTIVE != 0;
    match current & IFM_NMASK {
        IFM_IEEE80211 => Media::Wifi { active },
        IFM_ETHER => Media::Wired { active },
        _ => Media::None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn iface(name: &str, tunnel: bool, addrs: &[&str], media: Media) -> Iface {
        Iface {
            name: name.into(),
            up: true,
            tunnel,
            addrs: addrs.iter().map(|a| a.parse().unwrap()).collect(),
            media,
        }
    }

    const WIFI_ON: Media = Media::Wifi { active: true };
    const WIRED_ON: Media = Media::Wired { active: true };

    /// The reference Mac, 2026-09-29: Wi-Fi en0, a WireGuard utun7 carrying
    /// the homelab (and the relay) as a split tunnel, AWDL and llw0 up with
    /// link-local addresses only, idle Ethernet adapters.
    fn reference_mac() -> Vec<Iface> {
        vec![
            iface("lo0", false, &["127.0.0.1"], Media::None),
            iface("en0", false, &["192.168.10.23", "fe80::1"], WIFI_ON),
            iface("awdl0", false, &["fe80::2"], WIFI_ON),
            iface("llw0", false, &["fe80::3"], WIFI_ON),
            iface("en4", false, &[], Media::Wired { active: false }),
            iface("utun7", true, &["10.10.102.4"], Media::None),
        ]
    }

    #[test]
    fn a_direct_wifi_route_is_wifi() {
        let f = classify(&reference_mac(), "en0", Some("en0"));
        assert_eq!((f.wifi, f.vpn, f.awdl_up), (true, false, true));
    }

    // The bug the first on-hardware run found: the relay behind a split
    // tunnel read as "not Wi-Fi" while every packet crossed en0.
    #[test]
    fn a_split_tunnel_is_looked_through_to_its_wifi() {
        let f = classify(&reference_mac(), "utun7", Some("en0"));
        assert_eq!((f.wifi, f.vpn), (true, true));
    }

    #[test]
    fn a_full_tunnel_over_wifi_alone_is_wifi() {
        let f = classify(&reference_mac(), "utun7", Some("utun7"));
        assert_eq!((f.wifi, f.vpn), (true, true));
    }

    // Both links live and everything in the tunnel: which one carries it
    // is the service order's call, so don't claim Wi-Fi.
    #[test]
    fn a_full_tunnel_with_a_live_cable_is_not_claimed_as_wifi() {
        let mut ifs = reference_mac();
        ifs.push(iface("en5", false, &["192.168.10.40"], WIRED_ON));
        let f = classify(&ifs, "utun7", None);
        assert_eq!((f.wifi, f.vpn), (false, true));
    }

    #[test]
    fn a_split_tunnel_over_a_cable_is_not_wifi() {
        let mut ifs = reference_mac();
        ifs.push(iface("en5", false, &["192.168.10.40"], WIRED_ON));
        let f = classify(&ifs, "utun7", Some("en5"));
        assert!(!f.wifi);
    }

    #[test]
    fn a_wired_route_is_not_wifi() {
        let mut ifs = reference_mac();
        ifs.push(iface("en5", false, &["192.168.10.40"], WIRED_ON));
        let f = classify(&ifs, "en5", Some("en5"));
        assert_eq!((f.wifi, f.vpn), (false, false));
    }

    #[test]
    fn awdl_down_is_reported_down() {
        let mut ifs = reference_mac();
        ifs.iter_mut().find(|i| i.name == "awdl0").unwrap().up = false;
        assert!(!classify(&ifs, "en0", Some("en0")).awdl_up);
    }

    // The ioctl number encodes the struct size; a layout drift would have
    // the kernel read past (or short of) our buffer.
    #[test]
    fn ifmediareq_matches_darwin_layout() {
        assert_eq!(std::mem::size_of::<IfMediaReq>(), 44);
        assert_eq!((SIOCGIFMEDIA >> 16) & 0x1fff, 44);
    }

    #[test]
    fn loopback_is_not_wifi() {
        assert_eq!(media("lo0"), Media::None);
        let facts = probe("127.0.0.1:4433".parse().unwrap()).expect("loopback resolves");
        assert!(!facts.wifi);
    }

    #[test]
    fn a_v4_mapped_peer_resolves_like_its_v4_self() {
        let mapped: IpAddr = "::ffff:127.0.0.1".parse().unwrap();
        assert_eq!(local_ip_towards(mapped), Some(IpAddr::from([127, 0, 0, 1])));
    }

    #[test]
    fn nonsense_names_have_no_media() {
        assert_eq!(media("no-such-if0"), Media::None);
        assert_eq!(media("a-name-longer-than-ifnamsiz"), Media::None);
    }

    // Manual: what this Mac reports towards an address. `GAWK_PROBE=<ip>
    // cargo test -p gawk-broadcast-app-macos -- --ignored --nocapture
    // this_macs_facts`; compare with `route -n get <ip>` and `ifconfig
    // awdl0`.
    #[test]
    #[ignore]
    fn this_macs_facts() {
        let ip = std::env::var("GAWK_PROBE").unwrap_or_else(|_| "1.1.1.1".into());
        let facts = probe(SocketAddr::new(ip.parse().unwrap(), 4433));
        println!("{ip}: {facts:?}");
        assert!(facts.is_some());
    }
}
