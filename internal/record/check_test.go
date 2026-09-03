// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullCheck returns a fully-populated pass-case Check with every optional
// member left at its explicit-null zero value (nil Reason/ReasonCode/
// RequestedBy), so the null-rendering assertions have something to check.
func fullCheck() *Check {
	return &Check{
		Record:         CheckRecordType,
		CheckID:        "01J000000000000000000CHECK",
		HostID:         "h_abc123",
		Runtime:        "docker",
		RuntimeRef:     map[string]string{"container_name": "librespeed-ts", "container_id": "deadbeef"},
		Service:        "librespeed-ts",
		RepoID:         "local:librespeed-ts",
		Trigger:        "schedule",
		RequestedBy:    nil,
		Method:         CheckMethodMetadata,
		StartedAt:      "2026-08-27T12:00:00Z",
		FinishedAt:     "2026-08-27T12:00:03Z",
		DurationMs:     3000,
		Result:         "pass",
		Reason:         nil,
		ReasonCode:     nil,
		BallastVersion: "1.2.3",
		Engine:         Engine{Name: "restic", Version: "0.17.0"},
	}
}

func TestMarshalCheck_DiscriminatorAndMethod(t *testing.T) {
	data, err := MarshalCheck(fullCheck())
	if err != nil {
		t.Fatalf("MarshalCheck: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := m["record"]; got != "ballast.check.v1" {
		t.Errorf("record = %v, want ballast.check.v1", got)
	}
	if got := m["record"]; got != CheckRecordType {
		t.Errorf("record = %v, want %v", got, CheckRecordType)
	}
	if got := m["method"]; got != CheckMethodMetadata {
		t.Errorf("method = %v, want %v", got, CheckMethodMetadata)
	}
	if CheckMethodMetadata != "metadata" || CheckMethodReadData != "read-data" {
		t.Errorf("method constants drifted: metadata=%q read-data=%q", CheckMethodMetadata, CheckMethodReadData)
	}

	if !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("marshaled record must end with a newline")
	}
}

func TestMarshalCheck_OptionalsAreExplicitNull(t *testing.T) {
	data, err := MarshalCheck(fullCheck())
	if err != nil {
		t.Fatalf("MarshalCheck: %v", err)
	}
	s := string(data)

	// Every optional (pointer) member must render as an explicit null, never be
	// omitted, so a reader can tell not-known from not-reported.
	for _, want := range []string{
		`"requested_by": null`,
		`"reason": null`,
		`"reason_code": null`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled record missing explicit null %q\n%s", want, s)
		}
	}

	// And a round trip preserves the type discriminator.
	var back Check
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.Record != CheckRecordType {
		t.Errorf("round-trip record = %q, want %q", back.Record, CheckRecordType)
	}
	if back.RequestedBy != nil || back.Reason != nil || back.ReasonCode != nil {
		t.Errorf("round-trip: optional members should stay nil, got requested_by=%v reason=%v reason_code=%v",
			back.RequestedBy, back.Reason, back.ReasonCode)
	}
}

func TestWriteCheck_Path(t *testing.T) {
	dir := t.TempDir()
	c := fullCheck()

	path, err := WriteCheck(dir, c)
	if err != nil {
		t.Fatalf("WriteCheck: %v", err)
	}

	want := filepath.Join(dir, "checks", c.Service, c.CheckID+".json")
	if path != want {
		t.Errorf("WriteCheck path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %v, want 0644", info.Mode().Perm())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	var back Check
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal written file: %v", err)
	}
	if back.CheckID != c.CheckID || back.Record != CheckRecordType {
		t.Errorf("written record = %+v, want check_id %q record %q", back, c.CheckID, CheckRecordType)
	}
}
