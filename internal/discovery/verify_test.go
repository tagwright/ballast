// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// TestDiscoverVerifyConfiguredDetection proves that the presence (or absence)
// of any verify.* label sets BackupSpec.VerifyConfigured, the opt-in trigger
// for the backup-time manifest, without the label needing to be interpreted.
func TestDiscoverVerifyConfiguredDetection(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "no verify labels",
			labels: map[string]string{"ballast.enable": "true"},
			want:   false,
		},
		{
			name: "verify.mode present",
			labels: map[string]string{
				"ballast.enable":      "true",
				"ballast.verify.mode": "files",
			},
			want: true,
		},
		{
			name: "bare verify present",
			labels: map[string]string{
				"ballast.enable": "true",
				"ballast.verify": "true",
			},
			want: true,
		},
		{
			name: "verify under tagwright prefix",
			labels: map[string]string{
				"tagwright.backup.enable":       "true",
				"tagwright.backup.verify.probe": "SELECT 1",
			},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, _, err := Discover(runtime.Container{ID: "c", Name: "svc", Labels: c.labels}, cfg)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if spec == nil {
				t.Fatal("Discover: nil spec")
			}
			if spec.VerifyConfigured != c.want {
				t.Errorf("VerifyConfigured = %v, want %v", spec.VerifyConfigured, c.want)
			}
		})
	}
}
