package camera

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// arpSource returns the host's IP→MAC neighbour table. Overridden in tests.
var arpSource = readARPTable

var (
	arpMu    sync.Mutex
	arpTable map[string]string
	arpAt    time.Time
	arpTTL   = 5 * time.Second
)

// arpCmdTimeout bounds the arp(8) fallback. neighbours() holds arpMu across the
// read, so an unbounded command would wedge every caller — and those callers
// run on the single discovery-loop goroutine.
const arpCmdTimeout = 2 * time.Second

// neighbours returns the neighbour table, re-reading it at most once per arpTTL.
// ConnectAll fans out across the whole fleet at once, so without this every
// camera would read /proc/net/arp (or fork arp(8)) for the same answer.
//
// The returned map is shared and must not be mutated; each refresh builds a new
// one, so a caller ranging over an older map still sees a consistent snapshot.
func neighbours() map[string]string {
	arpMu.Lock()
	defer arpMu.Unlock()
	if arpTable != nil && time.Since(arpAt) < arpTTL {
		return arpTable
	}
	t := arpSource()
	if t == nil {
		// Cache the failure as an empty table. An unreadable table is a stable
		// condition, and leaving arpTable nil would defeat the TTL entirely —
		// every lookup would re-fork arp(8) and wait out arpCmdTimeout again.
		t = map[string]string{}
	}
	arpTable, arpAt = t, time.Now()
	return arpTable
}

// resetARPCache drops the memoized table. Tests call it after swapping arpSource.
func resetARPCache() {
	arpMu.Lock()
	arpTable, arpAt = nil, time.Time{}
	arpMu.Unlock()
}

// readARPTable reads the OS neighbour table, keyed by IP with bare uppercase
// hex MACs. Linux exposes it as /proc/net/arp; elsewhere we shell out to arp(8).
// A missing or unparseable table is not an error worth surfacing — callers
// treat an absent entry as "unknown", so an empty result is correct. Returns
// nil on failure; neighbours() normalizes that to an empty map before caching.
func readARPTable() map[string]string {
	if b, err := os.ReadFile("/proc/net/arp"); err == nil {
		return parseProcNetARP(string(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), arpCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "arp", "-an").Output()
	if err != nil {
		return nil
	}
	return parseARPCommand(string(out))
}

// parseProcNetARP parses Linux /proc/net/arp:
//
//	IP address       HW type     Flags       HW address            Mask     Device
//	192.168.1.220    0x1         0x2         f0:09:0d:cb:4e:ce     *        eth0
func parseProcNetARP(s string) map[string]string {
	table := make(map[string]string)
	for i, line := range strings.Split(s, "\n") {
		if i == 0 { // header
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		// Flags 0x0 is an unresolved entry; its MAC column reads all zeros.
		if f[2] == "0x0" {
			continue
		}
		if mac := normalizeMAC(f[3]); mac != "" && mac != "000000000000" {
			table[f[0]] = mac
		}
	}
	return table
}

// parseARPCommand parses BSD/macOS `arp -an`:
//
//	? (192.168.1.220) at f0:9:d:cb:4e:ce on en0 ifscope [ethernet]
//
// Note the zero-stripped octets — macOS prints "9" for "09", which is why every
// MAC goes through normalizeMAC rather than a raw string compare.
func parseARPCommand(s string) map[string]string {
	table := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		open := strings.Index(line, "(")
		closeIdx := strings.Index(line, ")")
		if open < 0 || closeIdx < open {
			continue
		}
		ip := line[open+1 : closeIdx]
		f := strings.Fields(line[closeIdx+1:])
		if len(f) < 2 || f[0] != "at" {
			continue
		}
		if mac := normalizeMAC(f[1]); mac != "" {
			table[ip] = mac
		}
	}
	return table
}

// normalizeMAC reduces a MAC in any common notation to bare uppercase hex,
// zero-padding short octets. Returns "" if the input isn't a MAC.
//
// The zero-padding matters: macOS arp(8) strips leading zeros per octet, so
// "f0:9:d:cb:4e:ce" and "F0090DCB4ECE" are the same address despite comparing
// unequal as strings.
func normalizeMAC(s string) string {
	s = strings.TrimSpace(s)
	if sep := strings.IndexAny(s, ":-"); sep >= 0 {
		parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == '-' })
		if len(parts) != 6 {
			return ""
		}
		var b strings.Builder
		for _, p := range parts {
			if len(p) == 0 || len(p) > 2 {
				return ""
			}
			if len(p) == 1 {
				b.WriteByte('0')
			}
			b.WriteString(p)
		}
		s = b.String()
	}
	s = strings.ToUpper(s)
	if len(s) != 12 {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return s
}

// lanIPConflict reports that the discovered LAN IP demonstrably belongs to some
// other device, returning a ready-to-log explanation ("" when there is no such
// proof). It exists because Wyze's cloud serves the last IP a camera reported
// before it dropped off the network, and keeps serving it after DHCP has handed
// that address to something else — the bridge then dials a stranger and gets an
// opaque "discovery timeout" forever.
//
// Deliberately advisory. It never blocks a dial, and it stays silent unless the
// neighbour table positively contradicts discovery: an absent entry proves
// nothing (the OS only caches addresses it has recently talked to), and if a
// model ever reports a device MAC that differs from its wifi MAC, a false
// mismatch degrades to a spurious log line rather than a broken camera.
//
// It also stays silent when effectiveLANIP could route around the conflict — we
// dialed the recovered address, so blaming the failure on the stale lease would
// point at a problem already handled.
func lanIPConflict(ip, camMAC string) string {
	want := normalizeMAC(camMAC)
	if ip == "" || want == "" {
		return ""
	}
	table := neighbours()
	got, ok := table[ip]
	if !ok || got == want {
		return ""
	}
	if _, recoverable := lanIPForMAC(table, camMAC); recoverable {
		return ""
	}
	return "discovered LAN IP " + ip + " belongs to " + got + ", not this camera (" +
		want + "), and this camera's MAC is absent from the neighbour table — " +
		"it is likely off the network or on another segment"
}

// lanIPForMAC returns the address table associates with mac. ok is false when
// the MAC is absent, or when it appears at more than one address — a MAC in two
// places is a table caught mid-move, and guessing which entry is current would
// be worse than leaving the caller on its existing value.
//
// Takes the table rather than calling neighbours() so that a caller combining
// this with its own lookup sees one consistent snapshot across both.
func lanIPForMAC(table map[string]string, mac string) (string, bool) {
	want := normalizeMAC(mac)
	if want == "" {
		return "", false
	}
	var found string
	for ip, m := range table {
		if m != want {
			continue
		}
		if found != "" && found != ip {
			return "", false
		}
		found = ip
	}
	return found, found != ""
}

// effectiveLANIP returns the address to dial for a camera, and whether that
// differs from the one Wyze's cloud reported.
//
// Wyze serves the last IP a camera reported before it dropped off the network
// and keeps serving it afterwards, so a DHCP pool reshuffle (a router reboot
// moves the whole fleet at once) leaves every wyze:// URL pointing at whoever
// inherited the old lease. The host's own neighbour table knows better: it maps
// the camera's MAC to where it actually is now.
//
// Overriding requires positively locating the MAC — we never invent an address,
// and an unlocatable MAC falls back to the cloud value unchanged, so this can
// only ever redirect a dial that was already going to the wrong place.
func effectiveLANIP(cloudIP, mac string) (string, bool) {
	want := normalizeMAC(mac)
	if want == "" || cloudIP == "" {
		return cloudIP, false
	}
	table := neighbours()
	if got, ok := table[cloudIP]; ok && got == want {
		return cloudIP, false // cloud agrees with the wire
	}
	if ip, ok := lanIPForMAC(table, mac); ok && ip != cloudIP {
		return ip, true
	}
	return cloudIP, false
}

// streamURLFor builds a camera's go2rtc source URL against an explicit LAN IP,
// leaving its discovered Info untouched. Keeping the substitution out of Info
// means the next Discover can't clobber it, and the recovery re-derives from
// the live neighbour table on every dial.
func streamURLFor(c *Camera, ip string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info := c.Info
	if ip != "" {
		info.LanIP = ip
	}
	return info.StreamURL(c.Quality)
}

// tutkSource builds the wyze:// source URL for a TUTK camera, dialing where the
// camera actually is rather than where Wyze last saw it. streamSourceFor
// delegates here in one line, so upstream-owned manager.go stays a one-line
// diff while the logic lives in this file. Pure: see logStaleLANIP.
func tutkSource(cam *Camera) string {
	info := cam.GetInfo()
	ip, _ := effectiveLANIP(info.LanIP, info.MAC)
	return streamURLFor(cam, ip)
}

// logStaleLANIP warns when a camera's dial address had to be recovered from the
// neighbour table because Wyze reported a stale one.
//
// Lives on the connect path rather than inside tutkSource so that
// streamSourceFor stays free of side effects — HealthCheck calls it purely for
// a protocol label, and upstream documents it as idempotent. connectCamera runs
// only on connect/reconnect, so a recovered camera warns about once; one that
// keeps failing re-warns each backoff, which is a condition worth repeating.
func (m *Manager) logStaleLANIP(cam *Camera) {
	info := cam.GetInfo()
	ip, recovered := effectiveLANIP(info.LanIP, info.MAC)
	if !recovered {
		return
	}
	m.log.Warn().
		Str("cam", cam.Name()).
		Str("cloud_ip", info.LanIP).
		Str("dial_ip", ip).
		Str("mac", info.MAC).
		Msg("Wyze reports a stale LAN IP; dialing where the neighbour table finds this MAC")
}

// logLANIPConflict emits a diagnostic when a camera's discovered LAN IP is
// provably some other device's and we could not locate the camera to route
// around it. Silent otherwise, so it costs nothing on the healthy path.
//
// A separate log line rather than a field on the caller's event: that keeps
// manager.go's failure paths at a one-line addition each.
func (m *Manager) logLANIPConflict(cam *Camera) {
	info := cam.GetInfo()
	if conflict := lanIPConflict(info.LanIP, info.MAC); conflict != "" {
		m.log.Warn().Str("cam", cam.Name()).Msg(conflict)
	}
}

// LANIPConflict returns a diagnostic when the host's neighbour table shows a
// camera's discovered LAN IP is held by a different device, or "" when there is
// no such evidence. Advisory only — see lanIPConflict.
func (m *Manager) LANIPConflict(name string) string {
	info, ok := m.cameraInfo(name)
	if !ok {
		return ""
	}
	return lanIPConflict(info.LanIP, info.MAC)
}

// DialIP returns the LAN address the manager would dial for a camera, which
// differs from the discovered one when the neighbour table locates the camera
// elsewhere. Empty for an unknown camera.
func (m *Manager) DialIP(name string) string {
	info, ok := m.cameraInfo(name)
	if !ok {
		return ""
	}
	ip, _ := effectiveLANIP(info.LanIP, info.MAC)
	return ip
}

// cameraInfo returns a named camera's discovered info, or ok=false if no such
// camera is registered.
func (m *Manager) cameraInfo(name string) (wyzeapi.CameraInfo, bool) {
	cam := m.GetCamera(name)
	if cam == nil {
		return wyzeapi.CameraInfo{}, false
	}
	return cam.GetInfo(), true
}
