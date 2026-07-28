package viammod

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// loadCredsFile reads a dotenv-format credentials file from disk and builds
// the Wyze credentials from it. The file lives on the machine (mode 0600,
// provisioned out-of-band) so the secrets never enter the Viam cloud config.
//
// Unlike the headless loadEnvFile, this does NOT mutate the process
// environment — it parses straight into a Credentials value. It errors if any
// of the four required keys (WYZE_EMAIL, WYZE_PASSWORD, WYZE_API_ID,
// WYZE_API_KEY) are missing, naming the absent keys without echoing any value.
func loadCredsFile(path string) (wyzeapi.Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return wyzeapi.Credentials{}, fmt.Errorf("open creds_file %q: %w", path, err)
	}
	defer f.Close()

	kv, err := parseDotenv(f)
	if err != nil {
		return wyzeapi.Credentials{}, fmt.Errorf("parse creds_file %q: %w", path, err)
	}

	creds := wyzeapi.Credentials{
		Email:    kv["WYZE_EMAIL"],
		Password: kv["WYZE_PASSWORD"],
		APIID:    kv["WYZE_API_ID"],
		APIKey:   kv["WYZE_API_KEY"],
		TOTPKey:  kv["WYZE_TOTP_KEY"], // optional (MFA)
	}

	if missing := missingRequiredKeys(kv); len(missing) > 0 {
		// Name the absent keys only — never include any value in the error.
		return wyzeapi.Credentials{}, fmt.Errorf("creds_file %q missing required keys: %s", path, strings.Join(missing, ", "))
	}

	return creds, nil
}

// requiredCredKeys are the dotenv keys that must be present and non-empty.
var requiredCredKeys = []string{"WYZE_EMAIL", "WYZE_PASSWORD", "WYZE_API_ID", "WYZE_API_KEY"}

// missingRequiredKeys returns the required keys absent or blank in kv, in a
// stable order for deterministic error messages.
func missingRequiredKeys(kv map[string]string) []string {
	var missing []string
	for _, k := range requiredCredKeys {
		if strings.TrimSpace(kv[k]) == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// parseDotenv reads KEY=VALUE lines into a map. Blank lines and `#` comments
// are skipped, an optional leading `export ` is stripped, and surrounding
// single or double quotes are trimmed from values. Lines without `=` are
// ignored. Matches the headless loadEnvFile parsing, minus the env mutation.
func parseDotenv(r io.Reader) (map[string]string, error) {
	kv := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		kv[key] = val
	}
	return kv, scanner.Err()
}
