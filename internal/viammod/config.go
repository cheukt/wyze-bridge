package viammod

import (
	"fmt"
	"strings"
)

// Config is the (non-secret) JSON configuration for the
// cheukt:wyze-bridge:manager generic service. The Wyze credentials are NOT
// here — they live in an on-machine dotenv file referenced by CredsFile, so
// secrets never enter the Viam cloud config. See loadCredsFile.
type Config struct {
	// CredsFile is the path on the machine to the dotenv credentials file
	// (mode 0600). Required, non-blank.
	CredsFile string `json:"creds_file"`

	// BridgeIP is the host IP advertised in WebRTC ICE candidates. Optional.
	BridgeIP string `json:"bridge_ip,omitempty"`

	// StateDir is where go2rtc.yaml and Wyze state are written. Optional;
	// the constructor defaults it to $VIAM_MODULE_DATA, falling back to
	// ./local/config when that env is unset.
	StateDir string `json:"state_dir,omitempty"`

	// RTSPPort is the go2rtc RTSP listen port. Optional; defaults to 8554.
	RTSPPort int `json:"rtsp_port,omitempty"`

	// LogLevel sets the Viam logger level (trace/debug/info/warn/error).
	// Optional; defaults to info.
	LogLevel string `json:"log_level,omitempty"`

	// ForceIOTCDetail enables verbose go2rtc + bridge logging. Optional.
	ForceIOTCDetail bool `json:"force_iotc_detail,omitempty"`

	// STUNServer overrides the WebRTC STUN server. Optional; defaults to
	// stun:stun.l.google.com:19302.
	STUNServer string `json:"stun_server,omitempty"`

	// FilterNames restricts which cameras are exposed as streams, by camera
	// nickname (case-insensitive). Optional; empty means expose all. Combine
	// with FilterModels / FilterMACs — a camera matching any of them is
	// selected. With FilterBlock=true the sense inverts: matched cameras are
	// excluded. Mirrors the FILTER_NAMES env var.
	FilterNames []string `json:"filter_names,omitempty"`

	// FilterModels restricts exposed cameras by model code or human-readable
	// model name (case-insensitive). Optional. Mirrors FILTER_MODELS.
	FilterModels []string `json:"filter_models,omitempty"`

	// FilterMACs restricts exposed cameras by MAC address (case-insensitive).
	// Optional. Mirrors FILTER_MACS.
	FilterMACs []string `json:"filter_macs,omitempty"`

	// FilterBlock inverts the filter: when true, cameras matching the filters
	// are excluded rather than the only ones included. Optional; defaults to
	// false (allow-list). Mirrors FILTER_BLOCKS.
	FilterBlock bool `json:"filter_block,omitempty"`
}

// validate checks the config and returns the resource dependencies the
// service needs (none — it is self-contained). It enforces only that
// creds_file is set; the file's contents are validated at construction time
// (validate may run before the file is present on the host). The rdk
// resource.Config Validate method wraps this.
func (c *Config) validate(path string) ([]string, error) {
	if strings.TrimSpace(c.CredsFile) == "" {
		return nil, fmt.Errorf(`%s: "creds_file" is required`, path)
	}
	return nil, nil
}
