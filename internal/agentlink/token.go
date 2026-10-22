// This file holds the machine's local credentials: the enrollment token and
// the server it belongs to (spec §7), plus server URL validation.
package agentlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// TokenPath returns <townRoot>/.runtime/machine-token.
func TokenPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "machine-token")
}

// MachineFilePath returns <townRoot>/.runtime/agentlink/machine.json.
func MachineFilePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "agentlink", "machine.json")
}

// MachineFile is the content of machine.json. It never holds the token.
type MachineFile struct {
	ServerURL string `json:"server_url"`
	MachineID string `json:"machine_id"`
}

// writeFileAtomic writes data to path via a 0600 temp file in the same
// directory and a rename, creating the directory with 0700.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone; tighten ours.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*") // CreateTemp uses 0600
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	ok = true
	return nil
}

// SaveToken stores the machine's credentials under townRoot: the token in
// .runtime/machine-token and {server_url, machine_id} in
// .runtime/agentlink/machine.json, both mode 0600 and written atomically.
func SaveToken(townRoot, serverURL, machineID, token string) error {
	if token == "" || machineID == "" || serverURL == "" {
		return errors.New("agentlink: save token: server url, machine id and token are required")
	}
	mj, err := json.MarshalIndent(MachineFile{ServerURL: serverURL, MachineID: machineID}, "", "  ")
	if err != nil {
		return fmt.Errorf("agentlink: encode machine.json: %w", err)
	}
	if err := writeFileAtomic(MachineFilePath(townRoot), append(mj, '\n')); err != nil {
		return fmt.Errorf("agentlink: save machine.json: %w", err)
	}
	if err := writeFileAtomic(TokenPath(townRoot), []byte(token+"\n")); err != nil {
		return fmt.Errorf("agentlink: save token: %w", err)
	}
	return nil
}

// HasToken reports whether a token file exists under townRoot.
func HasToken(townRoot string) bool {
	_, err := os.Lstat(TokenPath(townRoot))
	return err == nil
}

// LoadToken reads the machine's credentials. It refuses a token file whose
// mode grants any group or other permission, and refuses when machine.json
// is missing or incomplete.
func LoadToken(townRoot string) (serverURL, machineID, token string, err error) {
	p := TokenPath(townRoot)
	fi, err := os.Stat(p)
	if err != nil {
		return "", "", "", fmt.Errorf("agentlink: read token %s: %w (run 'gtb join')", p, err)
	}
	if !fi.Mode().IsRegular() {
		return "", "", "", fmt.Errorf("agentlink: token %s is not a regular file", p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", "", "", fmt.Errorf("agentlink: token file %s has mode %#o, which allows group or other access; fix with: chmod 600 %s", p, fi.Mode().Perm(), p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", "", fmt.Errorf("agentlink: read token %s: %w", p, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", "", "", fmt.Errorf("agentlink: token file %s is empty (run 'gtb join')", p)
	}
	mp := MachineFilePath(townRoot)
	mb, err := os.ReadFile(mp)
	if err != nil {
		return "", "", "", fmt.Errorf("agentlink: read %s: %w (run 'gtb join --force')", mp, err)
	}
	var mf MachineFile
	if err := json.Unmarshal(mb, &mf); err != nil {
		return "", "", "", fmt.Errorf("agentlink: parse %s: %w", mp, err)
	}
	if mf.ServerURL == "" || mf.MachineID == "" {
		return "", "", "", fmt.Errorf("agentlink: %s lacks server_url or machine_id (run 'gtb join --force')", mp)
	}
	return mf.ServerURL, mf.MachineID, tok, nil
}

// ValidateServerURL checks that u is an acceptable gtb server URL: https to
// any host, or http only to a loopback host (127.0.0.1, localhost, ::1),
// with no user-info, query or fragment.
func ValidateServerURL(u string) error {
	_, err := NormalizeServerURL(u)
	return err
}

// NormalizeServerURL validates u as ValidateServerURL does and returns it
// with any trailing slash removed.
func NormalizeServerURL(u string) (string, error) {
	p, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("invalid server url: %w", err)
	}
	if p.Opaque != "" || p.Host == "" || p.Hostname() == "" {
		return "", fmt.Errorf("invalid server url %q: want https://host[:port]", u)
	}
	if p.User != nil {
		return "", errors.New("invalid server url: must not contain user info")
	}
	if p.RawQuery != "" || p.ForceQuery || strings.Contains(u, "?") {
		return "", errors.New("invalid server url: must not contain a query")
	}
	if p.Fragment != "" || strings.Contains(u, "#") {
		return "", errors.New("invalid server url: must not contain a fragment")
	}
	switch strings.ToLower(p.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(p.Hostname()) {
			return "", fmt.Errorf("invalid server url %q: http is only allowed for 127.0.0.1, localhost or ::1; use https", u)
		}
	default:
		return "", fmt.Errorf("invalid server url %q: scheme must be https", u)
	}
	return strings.TrimRight(p.String(), "/"), nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}
