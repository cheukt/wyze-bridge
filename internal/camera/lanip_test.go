package camera

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

func TestNormalizeMAC(t *testing.T) {
	tests := []struct{ in, want string }{
		// macOS arp(8) strips leading zeros per octet — the case that makes a
		// raw string compare against Wyze's MAC wrongly report a mismatch.
		{"f0:9:d:cb:4e:ce", "F0090DCB4ECE"},
		{"f0:09:0d:cb:4e:ce", "F0090DCB4ECE"},
		{"F0-09-0D-CB-4E-CE", "F0090DCB4ECE"},
		{"80482CAA9F2F", "80482CAA9F2F"},
		{"80482caa9f2f", "80482CAA9F2F"},
		{"  80482CAA9F2F  ", "80482CAA9F2F"},
		{"", ""},
		{"incomplete", ""},
		{"f0:9:d:cb:4e", ""},       // only 5 octets
		{"f0:9:d:cb:4e:ce:11", ""}, // 7 octets
		{"g0:09:0d:cb:4e:ce", ""},  // not hex
		{"<incomplete>", ""},
	}
	for _, tt := range tests {
		if got := normalizeMAC(tt.in); got != tt.want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseProcNetARP(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.220    0x1         0x2         f0:09:0d:cb:4e:ce     *        eth0
192.168.1.77     0x1         0x2         80:48:2c:aa:9f:2f     *        eth0
192.168.1.5      0x1         0x0         00:00:00:00:00:00     *        eth0
`
	got := parseProcNetARP(table)
	if got["192.168.1.220"] != "F0090DCB4ECE" {
		t.Errorf(".220 = %q, want F0090DCB4ECE", got["192.168.1.220"])
	}
	if got["192.168.1.77"] != "80482CAA9F2F" {
		t.Errorf(".77 = %q, want 80482CAA9F2F", got["192.168.1.77"])
	}
}

func TestParseARPCommand(t *testing.T) {
	const out = `? (192.168.1.220) at f0:9:d:cb:4e:ce on en0 ifscope [ethernet]
? (192.168.1.77) at 80:48:2c:aa:9f:2f on en0 ifscope [ethernet]
? (192.168.1.9) at (incomplete) on en0 ifscope [ethernet]
`
	got := parseARPCommand(out)
	if got["192.168.1.220"] != "F0090DCB4ECE" {
		t.Errorf(".220 = %q, want F0090DCB4ECE", got["192.168.1.220"])
	}
	if got["192.168.1.77"] != "80482CAA9F2F" {
		t.Errorf(".77 = %q, want 80482CAA9F2F", got["192.168.1.77"])
	}
	if _, ok := got["192.168.1.9"]; ok {
		t.Error("incomplete entry should be dropped")
	}
}

func TestLANIPConflict(t *testing.T) {
	// A stranger holds the cloud IP and the camera is nowhere on the segment:
	// nothing to route around, so the operator needs telling.
	stubARP(t, map[string]string{"192.168.1.220": "F0090DCB4ECE"})

	if got := lanIPConflict("192.168.1.220", "80482CAA9F2F"); got == "" {
		t.Error("want a conflict for an IP held by a different MAC")
	} else if !strings.Contains(got, "F0090DCB4ECE") || !strings.Contains(got, "192.168.1.220") {
		t.Errorf("conflict should name both the IP and the squatter, got %q", got)
	}

	// An IP with no neighbour entry proves nothing — must stay silent.
	if got := lanIPConflict("192.168.1.199", "80482CAA9F2F"); got != "" {
		t.Errorf("absent ARP entry must not report a conflict, got %q", got)
	}

	// Missing inputs.
	if got := lanIPConflict("", "80482CAA9F2F"); got != "" {
		t.Errorf("empty IP = %q, want \"\"", got)
	}
	if got := lanIPConflict("192.168.1.220", ""); got != "" {
		t.Errorf("empty MAC = %q, want \"\"", got)
	}
}

// stubARP installs a fixed neighbour table for the duration of a test.
func stubARP(t *testing.T, table map[string]string) {
	t.Helper()
	orig := arpSource
	t.Cleanup(func() { arpSource = orig; resetARPCache() })
	arpSource = func() map[string]string { return table }
	resetARPCache()
}

func TestLANIPForMAC(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.220": "F0090DCB4ECE",
		"192.168.1.222": "80482CAA9F2F",
	})

	if ip, ok := lanIPForMAC(neighbours(), "80:48:2c:aa:9f:2f"); !ok || ip != "192.168.1.222" {
		t.Errorf("lanIPForMAC = (%q, %v), want (192.168.1.222, true)", ip, ok)
	}
	if _, ok := lanIPForMAC(neighbours(), "AABBCCDDEEFF"); ok {
		t.Error("absent MAC should not resolve")
	}
	if _, ok := lanIPForMAC(neighbours(), "not-a-mac"); ok {
		t.Error("malformed MAC should not resolve")
	}
}

// A MAC visible at two addresses is a table caught mid-move; picking one would
// be a coin flip, so it must decline to answer.
func TestLANIPForMAC_ambiguous(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.222": "80482CAA9F2F",
		"192.168.1.223": "80482CAA9F2F",
	})
	if ip, ok := lanIPForMAC(neighbours(), "80482CAA9F2F"); ok {
		t.Errorf("ambiguous MAC resolved to %q, want no answer", ip)
	}
}

func TestEffectiveLANIP(t *testing.T) {
	// The reported incident: cloud says .220, a stranger holds .220, and the
	// camera is really at .222.
	stubARP(t, map[string]string{
		"192.168.1.220": "F0090DCB4ECE",
		"192.168.1.222": "80482CAA9F2F",
	})
	ip, recovered := effectiveLANIP("192.168.1.220", "80482CAA9F2F")
	if ip != "192.168.1.222" || !recovered {
		t.Errorf("effectiveLANIP = (%q, %v), want (192.168.1.222, true)", ip, recovered)
	}
}

func TestEffectiveLANIP_leavesGoodValuesAlone(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.222": "80482CAA9F2F",
		"192.168.1.220": "F0090DCB4ECE",
	})

	// Cloud agrees with the wire — no substitution.
	if ip, rec := effectiveLANIP("192.168.1.222", "80482CAA9F2F"); ip != "192.168.1.222" || rec {
		t.Errorf("matching IP = (%q, %v), want (192.168.1.222, false)", ip, rec)
	}
	// MAC nowhere in the table: we must not invent an address, even though the
	// cloud IP is demonstrably someone else's.
	if ip, rec := effectiveLANIP("192.168.1.220", "AABBCCDDEEFF"); ip != "192.168.1.220" || rec {
		t.Errorf("unlocatable MAC = (%q, %v), want the cloud value unchanged", ip, rec)
	}
	// Missing inputs fall through untouched.
	if ip, rec := effectiveLANIP("", "80482CAA9F2F"); ip != "" || rec {
		t.Errorf("empty IP = (%q, %v)", ip, rec)
	}
	if ip, rec := effectiveLANIP("192.168.1.220", ""); ip != "192.168.1.220" || rec {
		t.Errorf("empty MAC = (%q, %v)", ip, rec)
	}
}

// An empty table (no /proc/net/arp, arp(8) missing, a routed segment) must be
// inert: every camera keeps dialing exactly what discovery gave it.
func TestEffectiveLANIP_emptyTableIsInert(t *testing.T) {
	stubARP(t, nil)
	if ip, rec := effectiveLANIP("192.168.1.220", "80482CAA9F2F"); ip != "192.168.1.220" || rec {
		t.Errorf("empty table = (%q, %v), want the cloud value unchanged", ip, rec)
	}
	if got := lanIPConflict("192.168.1.220", "80482CAA9F2F"); got != "" {
		t.Errorf("empty table conflict = %q, want \"\"", got)
	}
}

// The unresolved-entry guard: a kernel entry with flags 0x0 carries an all-zero
// MAC that would otherwise parse as a real address and match nothing usefully.
func TestParseProcNetARP_skipsUnresolved(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.5      0x1         0x0         00:00:00:00:00:00     *        wlan0
192.168.1.222    0x1         0x2         80:48:2c:aa:9f:2f     *        wlan0
`
	got := parseProcNetARP(table)
	if _, ok := got["192.168.1.5"]; ok {
		t.Error("unresolved entry should be dropped")
	}
	if got["192.168.1.222"] != "80482CAA9F2F" {
		t.Errorf(".222 = %q, want 80482CAA9F2F", got["192.168.1.222"])
	}
}

// End to end: the go2rtc source URL a moved camera gets must carry the address
// the neighbour table found, not the stale one Wyze reported.
func TestStreamSourceFor_dialsRecoveredIP(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.220": "F0090DCB4ECE", // stranger holding the old lease
		"192.168.1.222": "80482CAA9F2F", // the camera, where it actually is
	})

	m := NewManager(&config.Config{Quality: "hd", CamOverrides: map[string]config.CamOverride{}},
		nil, nil, zerolog.Nop())
	cam := NewCamera(wyzeapi.CameraInfo{
		Name: "litterbox", Model: "HL_CAM4",
		MAC: "80482CAA9F2F", LanIP: "192.168.1.220",
		P2PID: "UID01234567890123456", ENR: "enr",
	}, "hd", true, false)
	m.InjectCamera("litterbox", cam)

	url, protocol := m.streamSourceFor(cam)
	if protocol != "tutk" {
		t.Fatalf("protocol = %q, want tutk", protocol)
	}
	if !strings.Contains(url, "wyze://192.168.1.222?") {
		t.Errorf("source = %q, want it to dial 192.168.1.222", url)
	}
	if strings.Contains(url, "192.168.1.220") {
		t.Errorf("source = %q, must not dial the stale 192.168.1.220", url)
	}
	if got := m.DialIP("litterbox"); got != "192.168.1.222" {
		t.Errorf("DialIP = %q, want 192.168.1.222", got)
	}

	// Discovery later overwrites Info with the same stale cloud IP; the
	// substitution is re-derived per dial, so it must survive that.
	cam.UpdateInfo(wyzeapi.CameraInfo{
		Name: "litterbox", Model: "HL_CAM4",
		MAC: "80482CAA9F2F", LanIP: "192.168.1.220",
		P2PID: "UID01234567890123456", ENR: "enr",
	})
	if url, _ := m.streamSourceFor(cam); !strings.Contains(url, "192.168.1.222") {
		t.Errorf("after re-discovery source = %q, want it still dialing .222", url)
	}
}

// The camera sitting at its cloud-reported address is not a conflict, in either
// MAC notation.
func TestLANIPConflict_matchingMAC(t *testing.T) {
	stubARP(t, map[string]string{"192.168.1.222": "80482CAA9F2F"})
	if got := lanIPConflict("192.168.1.222", "80:48:2c:aa:9f:2f"); got != "" {
		t.Errorf("matching MAC should not conflict, got %q", got)
	}
}

// The regression this pass caught: when the camera is findable elsewhere,
// effectiveLANIP dials the recovered address, so reporting the stale lease as
// the reason for a failure points at a problem that was already handled.
func TestLANIPConflict_silentWhenRecoverable(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.220": "F0090DCB4ECE", // stranger on the stale lease
		"192.168.1.222": "80482CAA9F2F", // camera, findable
	})

	// Precondition: this is exactly the case effectiveLANIP routes around.
	if ip, recovered := effectiveLANIP("192.168.1.220", "80482CAA9F2F"); !recovered || ip != "192.168.1.222" {
		t.Fatalf("effectiveLANIP = (%q, %v), want (192.168.1.222, true)", ip, recovered)
	}
	if got := lanIPConflict("192.168.1.220", "80482CAA9F2F"); got != "" {
		t.Errorf("recoverable conflict should stay silent, got %q", got)
	}
}

// A MAC at two addresses is not recoverable (lanIPForMAC declines), so the
// conflict is real and must still be reported.
func TestLANIPConflict_ambiguousMACStillReports(t *testing.T) {
	stubARP(t, map[string]string{
		"192.168.1.220": "F0090DCB4ECE",
		"192.168.1.222": "80482CAA9F2F",
		"192.168.1.223": "80482CAA9F2F",
	})
	if got := lanIPConflict("192.168.1.220", "80482CAA9F2F"); got == "" {
		t.Error("ambiguous MAC is unrecoverable; conflict should be reported")
	}
}

// An unreadable table (arp(8) missing, or its timeout firing) must still be
// cached for the TTL. Leaving it uncached defeats the memo entirely: every
// lookup re-forks arp(8) and waits out arpCmdTimeout, and one failed connect
// makes six lookups.
func TestNeighbours_cachesUnreadableTable(t *testing.T) {
	var calls atomic.Int32
	orig := arpSource
	t.Cleanup(func() { arpSource = orig; resetARPCache() })
	arpSource = func() map[string]string { calls.Add(1); return nil }
	resetARPCache()

	for i := 0; i < 3; i++ {
		effectiveLANIP("192.168.1.220", "80482CAA9F2F")
		lanIPConflict("192.168.1.220", "80482CAA9F2F")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("arpSource called %d times for 6 lookups, want 1 (nil result not cached)", got)
	}
	if n := neighbours(); n == nil {
		t.Error("neighbours() returned nil; callers range over it")
	}
}
