package viammod

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig_validate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "ok with creds_file",
			cfg:     Config{CredsFile: "/etc/wyze-bridge/wyze.env"},
			wantErr: false,
		},
		{
			name:      "blank creds_file rejected",
			cfg:       Config{CredsFile: ""},
			wantErr:   true,
			errSubstr: "creds_file",
		},
		{
			name:      "whitespace-only creds_file rejected",
			cfg:       Config{CredsFile: "   "},
			wantErr:   true,
			errSubstr: "creds_file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, err := tt.cfg.validate("services.wyze")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validate() = nil error, want error")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("validate() error = %q, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() unexpected error: %v", err)
			}
			if len(deps) != 0 {
				t.Errorf("validate() deps = %v, want none (self-contained)", deps)
			}
		})
	}
}

func TestLoadCredsFile(t *testing.T) {
	const (
		email = "user@example.com"
		pass  = "s3cr3t-pa$$word"
		apiID = "api-id-123"
		apiKy = "api-key-456"
		totp  = "JBSWY3DPEHPK3PXP"
	)

	t.Run("full creds with optional totp and quoting", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wyze.env")
		writeFile(t, path, strings.Join([]string{
			"# Wyze creds",
			"export WYZE_EMAIL=" + email,
			`WYZE_PASSWORD="` + pass + `"`,
			"WYZE_API_ID = " + apiID, // spaces around =
			"WYZE_API_KEY='" + apiKy + "'",
			"WYZE_TOTP_KEY=" + totp,
			"",
		}, "\n"))

		creds, err := loadCredsFile(path)
		if err != nil {
			t.Fatalf("loadCredsFile() error: %v", err)
		}
		if creds.Email != email {
			t.Errorf("Email = %q, want %q", creds.Email, email)
		}
		if creds.Password != pass {
			t.Errorf("Password = %q, want %q", creds.Password, pass)
		}
		if creds.APIID != apiID {
			t.Errorf("APIID = %q, want %q", creds.APIID, apiID)
		}
		if creds.APIKey != apiKy {
			t.Errorf("APIKey = %q, want %q", creds.APIKey, apiKy)
		}
		if creds.TOTPKey != totp {
			t.Errorf("TOTPKey = %q, want %q", creds.TOTPKey, totp)
		}
		if !creds.IsSet() {
			t.Errorf("IsSet() = false, want true")
		}
	})

	t.Run("totp optional", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wyze.env")
		writeFile(t, path, strings.Join([]string{
			"WYZE_EMAIL=" + email,
			"WYZE_PASSWORD=" + pass,
			"WYZE_API_ID=" + apiID,
			"WYZE_API_KEY=" + apiKy,
		}, "\n"))

		creds, err := loadCredsFile(path)
		if err != nil {
			t.Fatalf("loadCredsFile() error: %v", err)
		}
		if creds.TOTPKey != "" {
			t.Errorf("TOTPKey = %q, want empty", creds.TOTPKey)
		}
		if !creds.IsSet() {
			t.Errorf("IsSet() = false, want true")
		}
	})

	t.Run("missing required keys are named, no value leaked", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wyze.env")
		// Only email + api_id present; password + api_key missing.
		writeFile(t, path, strings.Join([]string{
			"WYZE_EMAIL=" + email,
			"WYZE_API_ID=" + apiID,
		}, "\n"))

		_, err := loadCredsFile(path)
		if err == nil {
			t.Fatalf("loadCredsFile() = nil error, want error")
		}
		msg := err.Error()
		for _, want := range []string{"WYZE_PASSWORD", "WYZE_API_KEY"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q missing key name %q", msg, want)
			}
		}
		for _, present := range []string{"WYZE_EMAIL", "WYZE_API_ID"} {
			if strings.Contains(msg, present) {
				t.Errorf("error %q names present key %q (should not)", msg, present)
			}
		}
		// The error must never echo a secret value.
		if strings.Contains(msg, email) || strings.Contains(msg, apiID) {
			t.Errorf("error %q leaked a credential value", msg)
		}
	})

	t.Run("blank value counts as missing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wyze.env")
		writeFile(t, path, strings.Join([]string{
			"WYZE_EMAIL=" + email,
			"WYZE_PASSWORD=",
			"WYZE_API_ID=" + apiID,
			"WYZE_API_KEY=" + apiKy,
		}, "\n"))

		_, err := loadCredsFile(path)
		if err == nil {
			t.Fatalf("loadCredsFile() = nil error, want error")
		}
		if !strings.Contains(err.Error(), "WYZE_PASSWORD") {
			t.Errorf("error %q should name WYZE_PASSWORD", err)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := loadCredsFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
		if err == nil {
			t.Fatalf("loadCredsFile() = nil error, want error for missing file")
		}
		if !strings.Contains(err.Error(), "open creds_file") {
			t.Errorf("error %q, want open creds_file context", err)
		}
	})
}

func TestNormalizeFilter(t *testing.T) {
	got := normalizeFilter([]string{"  Front Door ", "", "garage", "  "})
	want := []string{"FRONT DOOR", "GARAGE"}
	if len(got) != len(want) {
		t.Fatalf("normalizeFilter() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalizeFilter()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if normalizeFilter(nil) != nil {
		t.Errorf("normalizeFilter(nil) should stay nil (means expose all)")
	}
}

func TestBuildBridgeConfig_filters(t *testing.T) {
	cfg := &Config{
		CredsFile:    "/x",
		FilterNames:  []string{"Front Door"},
		FilterModels: []string{"wyzec1"},
		FilterMACs:   []string{"aabbcc"},
		FilterBlock:  true,
	}
	bc := buildBridgeConfig(cfg)
	if len(bc.FilterNames) != 1 || bc.FilterNames[0] != "FRONT DOOR" {
		t.Errorf("FilterNames = %v, want [FRONT DOOR]", bc.FilterNames)
	}
	if len(bc.FilterModels) != 1 || bc.FilterModels[0] != "WYZEC1" {
		t.Errorf("FilterModels = %v, want [WYZEC1]", bc.FilterModels)
	}
	if len(bc.FilterMACs) != 1 || bc.FilterMACs[0] != "AABBCC" {
		t.Errorf("FilterMACs = %v, want [AABBCC]", bc.FilterMACs)
	}
	if !bc.FilterBlocks {
		t.Errorf("FilterBlocks = false, want true")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
