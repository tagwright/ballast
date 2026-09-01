// SPDX-License-Identifier: GPL-3.0-or-later

package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var sha256Prefixed = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func writeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.txt":       "hello",
		"sub/b.txt":   "world!!",
		"sub/c.empty": "",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildCountsAndDigest(t *testing.T) {
	root := writeTree(t)
	loc := filepath.Join(t.TempDir(), "m", "run.json")

	h, err := Build([]string{root}, loc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if h.Entries != 3 {
		t.Fatalf("entries = %d, want 3", h.Entries)
	}
	if h.Bytes != uint64(len("hello")+len("world!!")+0) {
		t.Fatalf("bytes = %d", h.Bytes)
	}
	if !sha256Prefixed.MatchString(h.Digest) {
		t.Fatalf("digest %q not sha256-prefixed", h.Digest)
	}
	if h.Location != loc {
		t.Fatalf("location = %q, want %q", h.Location, loc)
	}

	// The digest is the SHA-256 of the exact file bytes on disk.
	body, err := os.ReadFile(loc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	sum := sha256.Sum256(body)
	if want := "sha256:" + hex.EncodeToString(sum[:]); want != h.Digest {
		t.Fatalf("digest %q does not match file bytes %q", h.Digest, want)
	}

	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("manifest is not valid json: %v", err)
	}
	if doc.Record != Record {
		t.Fatalf("record = %q", doc.Record)
	}
	for _, e := range doc.Entries {
		if !sha256Prefixed.MatchString(e.SHA256) {
			t.Fatalf("entry sha %q not prefixed", e.SHA256)
		}
	}
}

func TestBuildDeterministic(t *testing.T) {
	root := writeTree(t)
	h1, err := Build([]string{root}, filepath.Join(t.TempDir(), "1.json"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Build([]string{root}, filepath.Join(t.TempDir(), "2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if h1.Digest != h2.Digest {
		t.Fatalf("digest not deterministic: %q != %q", h1.Digest, h2.Digest)
	}
}
