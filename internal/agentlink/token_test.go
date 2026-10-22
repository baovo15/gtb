package agentlink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadToken(t *testing.T) {
	root := t.TempDir()
	if err := SaveToken(root, "https://gtb.example.com", "abc123", "tok-secret"); err != nil {
		t.Fatal(err)
	}
	tokPath := filepath.Join(root, ".runtime", "machine-token")
	mjPath := filepath.Join(root, ".runtime", "agentlink", "machine.json")
	for _, p := range []string{tokPath, mjPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
	for _, d := range []string{filepath.Join(root, ".runtime"), filepath.Join(root, ".runtime", "agentlink")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v, want 0700", d, fi.Mode().Perm())
		}
	}
	var mj map[string]string
	b, _ := os.ReadFile(mjPath)
	if err := json.Unmarshal(b, &mj); err != nil || mj["server_url"] != "https://gtb.example.com" || mj["machine_id"] != "abc123" {
		t.Fatalf("machine.json = %s (%v)", b, err)
	}
	if strings.Contains(string(b), "tok-secret") {
		t.Fatal("token written into machine.json")
	}
	u, id, tok, err := LoadToken(root)
	if err != nil || u != "https://gtb.example.com" || id != "abc123" || tok != "tok-secret" {
		t.Fatalf("LoadToken = %q %q %q %v", u, id, tok, err)
	}
	// Overwrite (join --force) works and leaves no temp files behind.
	if err := SaveToken(root, "https://gtb.example.com", "def456", "tok-two"); err != nil {
		t.Fatal(err)
	}
	if _, id, tok, err := LoadToken(root); err != nil || id != "def456" || tok != "tok-two" {
		t.Fatalf("after overwrite: %q %q %v", id, tok, err)
	}
	for _, d := range []string{filepath.Join(root, ".runtime"), filepath.Join(root, ".runtime", "agentlink")} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp") {
				t.Fatalf("temp file left: %s", e.Name())
			}
		}
	}
}

func TestLoadToken_TrimsWhitespace(t *testing.T) {
	root := t.TempDir()
	if err := SaveToken(root, "https://gtb.example.com", "m1", "tok"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, ".runtime", "machine-token")
	if err := os.WriteFile(p, []byte("  tok\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, tok, err := LoadToken(root); err != nil || tok != "tok" {
		t.Fatalf("token %q %v", tok, err)
	}
}

func TestLoadToken_RejectsWorldReadable(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		root := t.TempDir()
		if err := SaveToken(root, "https://gtb.example.com", "m1", "tok"); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, ".runtime", "machine-token")
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, _, tok, err := LoadToken(root)
		if err == nil {
			t.Fatalf("mode %o accepted", mode)
		}
		if tok != "" {
			t.Fatalf("mode %o: token returned with error", mode)
		}
		if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "chmod 600 "+p) {
			t.Fatalf("mode %o: error does not name file and fix: %v", mode, err)
		}
	}
}

func TestLoadToken_MissingMachineJSON(t *testing.T) {
	root := t.TempDir()
	if err := SaveToken(root, "https://gtb.example.com", "m1", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".runtime", "agentlink", "machine.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadToken(root); err == nil {
		t.Fatal("missing machine.json accepted")
	}
}

func TestLoadToken_NotJoined(t *testing.T) {
	if _, _, _, err := LoadToken(t.TempDir()); err == nil {
		t.Fatal("no token accepted")
	}
}

func TestValidateServerURL(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"https://gtb.nhaboc.com", true},
		{"https://gtb.nhaboc.com/", true},
		{"https://gtb.nhaboc.com:8443/base", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.1", true},
		{"http://localhost:3000", true},
		{"http://LOCALHOST", true},
		{"http://[::1]:8080", true},
		{"http://[::1]", true},
		{"http://gtb.nhaboc.com", false},
		{"http://10.0.0.1", false},
		{"http://127.0.0.1.evil.com", false},
		{"http://localhost.evil.com", false},
		{"https://user:pass@gtb.nhaboc.com", false},
		{"https://user@gtb.nhaboc.com", false},
		{"https://gtb.nhaboc.com?x=1", false},
		{"https://gtb.nhaboc.com/?", false},
		{"https://gtb.nhaboc.com#frag", false},
		{"ftp://gtb.nhaboc.com", false},
		{"gtb.nhaboc.com", false},
		{"https://", false},
		{"", false},
	}
	for _, c := range cases {
		err := ValidateServerURL(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ValidateServerURL(%q) err=%v, want ok=%v", c.in, err, c.ok)
		}
	}
}

func TestNormalizeServerURL_StripsTrailingSlash(t *testing.T) {
	for in, want := range map[string]string{
		"https://gtb.nhaboc.com/":      "https://gtb.nhaboc.com",
		"https://gtb.nhaboc.com/base/": "https://gtb.nhaboc.com/base",
		"http://[::1]:8080":            "http://[::1]:8080",
	} {
		got, err := NormalizeServerURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
