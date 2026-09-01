// SPDX-License-Identifier: GPL-3.0-or-later

package hostid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateIsValidAndUnique(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !Valid(a) || !Valid(b) {
		t.Fatalf("generated ids not valid: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("two generated ids collided: %q", a)
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"h_3f9c1a2b7e8d4c5f", true},
		{"h_dead", true},
		{"h_dea", false},               // fewer than 4 hex
		{"h_DEADBEEF", false},          // uppercase not allowed
		{"3f9c1a2b", false},            // missing prefix
		{"h_", false},                  // no hex
		{"h_" + longHex(65), false},    // more than 64 hex
		{"h_" + longHex(64), true},     // exactly 64 hex
	}
	for _, c := range cases {
		if got := Valid(c.id); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func longHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestLoadOrCreatePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	if !Valid(first) {
		t.Fatalf("first id not valid: %q", first)
	}

	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if second != first {
		t.Fatalf("identity changed across reload: %q != %q", second, first)
	}

	// The file exists at the documented path.
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("host_id file missing: %v", err)
	}
}

func TestLoadOrCreateCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate on missing dir: %v", err)
	}
	if !Valid(id) {
		t.Fatalf("id not valid: %q", id)
	}
}

func TestLoadOrCreateRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("not-a-host-id"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatalf("expected error on corrupt host_id file, got nil")
	}
}
