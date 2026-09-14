package camera

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// The reported incident: Wyze still reports staleIP, a stranger now holds that
// lease, and the camera has moved to camIP.
const (
	camMAC      = "80482CAA9F2F"
	strangerMAC = "F0090DCB4ECE"
	staleIP     = "192.168.1.220"
	camIP       = "192.168.1.222"
)

// stubARP installs a fixed neighbour table for the duration of a test.
func stubARP(t *testing.T, table map[string]string) {
	t.Helper()
	orig := arpSource
	t.Cleanup(func() { arpSource = orig; resetARPCache() })
	arpSource = func() map[string]string { return table }
	resetARPCache()
}

// stubMovedCam stubs the incident above: stranger on the stale lease, camera
// findable at its new address.
func stubMovedCam(t *testing.T) {
	t.Helper()
	stubARP(t, map[string]string{staleIP: strangerMAC, camIP: camMAC})
}

func TestNormalizeMAC(t *testing.T) {
	tests := []struct{ in, want string }{
		// macOS arp(8) strips leading zeros per octet — the case that makes a
		// raw string compare against Wyze's MAC wrongly report a mismatch.
		{"f0:9:d:cb:4e:ce", strangerMAC},
		{"f0:09:0d:cb:4e:ce", strangerMAC},
		{"F0-09-0D-CB-4E-CE", strangerMAC},
		{camMAC, camMAC},
		{"80482caa9f2f", camMAC},
		{"  80482CAA9F2F  ", camMAC},
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

// The 0x0-flag row is the unresolved-entry guard: its all-zero MAC would
// otherwise parse as a real address and match nothing usefully.
func TestParseProcNetARP(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.220    0x1         0x2         f0:09:0d:cb:4e:ce     *        eth0
192.168.1.222    0x1         0x2         80:48:2c:aa:9f:2f     *        eth0
192.168.1.5      0x1         0x0         00:00:00:00:00:00     *        eth0
`
	got := parseProcNetARP(table)
	if got[staleIP] != strangerMAC {
		t.Errorf("%s = %q, want %q", staleIP, got[staleIP], strangerMAC)
	}
	if got[camIP] != camMAC {
		t.Errorf("%s = %q, want %q", camIP, got[camIP], camMAC)
	}
	if _, ok := got["192.168.1.5"]; ok {
		t.Error("unresolved entry should be dropped")
	}
}

func TestParseARPCommand(t *testing.T) {
	const out = `? (192.168.1.220) at f0:9:d:cb:4e:ce on en0 ifscope [ethernet]
? (192.168.1.222) at 80:48:2c:aa:9f:2f on en0 ifscope [ethernet]
? (192.168.1.9) at (incomplete) on en0 ifscope [ethernet]
`
	got := parseARPCommand(out)
	if got[staleIP] != strangerMAC {
		t.Errorf("%s = %q, want %q", staleIP, got[staleIP], strangerMAC)
	}
	if got[camIP] != camMAC {
		t.Errorf("%s = %q, want %q", camIP, got[camIP], camMAC)
	}
	if _, ok := got["192.168.1.9"]; ok {
		t.Error("incomplete entry should be dropped")
	}
}

func TestLANIPForMAC(t *testing.T) {
	t.Run("resolves, in either MAC notation", func(t *testing.T) {
		stubMovedCam(t)
		if ip, ok := lanIPForMAC(neighbours(), "80:48:2c:aa:9f:2f"); !ok || ip != camIP {
			t.Errorf("lanIPForMAC = (%q, %v), want (%s, true)", ip, ok, camIP)
		}
		if _, ok := lanIPForMAC(neighbours(), "AABBCCDDEEFF"); ok {
			t.Error("absent MAC should not resolve")
		}
		if _, ok := lanIPForMAC(neighbours(), "not-a-mac"); ok {
			t.Error("malformed MAC should not resolve")
		}
	})

	// A MAC visible at two addresses is a table caught mid-move; picking one
	// would be a coin flip, so it must decline to answer.
	t.Run("ambiguous MAC declines", func(t *testing.T) {
		stubARP(t, map[string]string{camIP: camMAC, "192.168.1.223": camMAC})
		if ip, ok := lanIPForMAC(neighbours(), camMAC); ok {
			t.Errorf("ambiguous MAC resolved to %q, want no answer", ip)
		}
	})
}

func TestEffectiveLANIP(t *testing.T) {
	t.Run("routes around the stale lease", func(t *testing.T) {
		stubMovedCam(t)
		if ip, rec := effectiveLANIP(staleIP, camMAC); ip != camIP || !rec {
			t.Errorf("effectiveLANIP = (%q, %v), want (%s, true)", ip, rec, camIP)
		}
	})

	t.Run("leaves good values alone", func(t *testing.T) {
		stubMovedCam(t)
		// Cloud agrees with the wire — no substitution.
		if ip, rec := effectiveLANIP(camIP, camMAC); ip != camIP || rec {
			t.Errorf("matching IP = (%q, %v), want (%s, false)", ip, rec, camIP)
		}
		// MAC nowhere in the table: we must not invent an address, even though
		// the cloud IP is demonstrably someone else's.
		if ip, rec := effectiveLANIP(staleIP, "AABBCCDDEEFF"); ip != staleIP || rec {
			t.Errorf("unlocatable MAC = (%q, %v), want the cloud value unchanged", ip, rec)
		}
		// Missing inputs fall through untouched.
		if ip, rec := effectiveLANIP("", camMAC); ip != "" || rec {
			t.Errorf("empty IP = (%q, %v)", ip, rec)
		}
		if ip, rec := effectiveLANIP(staleIP, ""); ip != staleIP || rec {
			t.Errorf("empty MAC = (%q, %v)", ip, rec)
		}
	})

	// An empty table (no /proc/net/arp, arp(8) missing, a routed segment) must
	// be inert: every camera keeps dialing exactly what discovery gave it.
	t.Run("empty table is inert", func(t *testing.T) {
		stubARP(t, nil)
		if ip, rec := effectiveLANIP(staleIP, camMAC); ip != staleIP || rec {
			t.Errorf("empty table = (%q, %v), want the cloud value unchanged", ip, rec)
		}
		if got := lanIPConflict(staleIP, camMAC); got != "" {
			t.Errorf("empty table conflict = %q, want \"\"", got)
		}
	})
}

func TestLANIPConflict(t *testing.T) {
	// A stranger holds the cloud IP and the camera is nowhere on the segment:
	// nothing to route around, so the operator needs telling.
	t.Run("unrecoverable squatter is reported", func(t *testing.T) {
		stubARP(t, map[string]string{staleIP: strangerMAC})
		got := lanIPConflict(staleIP, camMAC)
		if got == "" {
			t.Fatal("want a conflict for an IP held by a different MAC")
		}
		if !strings.Contains(got, strangerMAC) || !strings.Contains(got, staleIP) {
			t.Errorf("conflict should name both the IP and the squatter, got %q", got)
		}

		// An IP with no neighbour entry proves nothing — must stay silent.
		if got := lanIPConflict("192.168.1.199", camMAC); got != "" {
			t.Errorf("absent ARP entry must not report a conflict, got %q", got)
		}
		// Missing inputs.
		if got := lanIPConflict("", camMAC); got != "" {
			t.Errorf("empty IP = %q, want \"\"", got)
		}
		if got := lanIPConflict(staleIP, ""); got != "" {
			t.Errorf("empty MAC = %q, want \"\"", got)
		}
	})

	t.Run("camera at its own address is not a conflict", func(t *testing.T) {
		stubARP(t, map[string]string{camIP: camMAC})
		if got := lanIPConflict(camIP, "80:48:2c:aa:9f:2f"); got != "" {
			t.Errorf("matching MAC should not conflict, got %q", got)
		}
	})

	// When the camera is findable elsewhere, effectiveLANIP already dials the
	// recovered address, so reporting the stale lease as the reason for a
	// failure points at a problem that was handled.
	t.Run("silent when recoverable", func(t *testing.T) {
		stubMovedCam(t)
		if got := lanIPConflict(staleIP, camMAC); got != "" {
			t.Errorf("recoverable conflict should stay silent, got %q", got)
		}
	})

	// A MAC at two addresses is not recoverable (lanIPForMAC declines), so the
	// conflict is real and must still be reported.
	t.Run("ambiguous MAC still reports", func(t *testing.T) {
		stubARP(t, map[string]string{staleIP: strangerMAC, camIP: camMAC, "192.168.1.223": camMAC})
		if got := lanIPConflict(staleIP, camMAC); got == "" {
			t.Error("ambiguous MAC is unrecoverable; conflict should be reported")
		}
	})
}

// End to end: the go2rtc source URL a moved camera gets must carry the address
// the neighbour table found, not the stale one Wyze reported.
func TestStreamSourceFor_dialsRecoveredIP(t *testing.T) {
	stubMovedCam(t)

	m := NewManager(&config.Config{Quality: "hd", CamOverrides: map[string]config.CamOverride{}},
		nil, nil, zerolog.Nop())
	info := wyzeapi.CameraInfo{
		Name: "litterbox", Model: "HL_CAM4",
		MAC: camMAC, LanIP: staleIP,
		P2PID: "UID01234567890123456", ENR: "enr",
	}
	cam := NewCamera(info, "hd", true, false)
	m.InjectCamera("litterbox", cam)

	url, protocol := m.streamSourceFor(cam)
	if protocol != "tutk" {
		t.Fatalf("protocol = %q, want tutk", protocol)
	}
	if !strings.Contains(url, "wyze://"+camIP+"?") {
		t.Errorf("source = %q, want it to dial %s", url, camIP)
	}
	if strings.Contains(url, staleIP) {
		t.Errorf("source = %q, must not dial the stale %s", url, staleIP)
	}
	if got := m.DialIP("litterbox"); got != camIP {
		t.Errorf("DialIP = %q, want %s", got, camIP)
	}

	// Discovery later overwrites Info with the same stale cloud IP; the
	// substitution is re-derived per dial, so it must survive that.
	cam.UpdateInfo(info)
	if url, _ := m.streamSourceFor(cam); !strings.Contains(url, camIP) {
		t.Errorf("after re-discovery source = %q, want it still dialing %s", url, camIP)
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
		effectiveLANIP(staleIP, camMAC)
		lanIPConflict(staleIP, camMAC)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("arpSource called %d times for 6 lookups, want 1 (nil result not cached)", got)
	}
	if n := neighbours(); n == nil {
		t.Error("neighbours() returned nil; callers range over it")
	}
}
