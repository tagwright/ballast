// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package check

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
)

// fakeEngine drives the outcome mapping: it returns checkErr from Check and
// records the readData it was called with.
type fakeEngine struct {
	checkErr error
	version  string
	gotRead  bool
	called   bool
}

func (f *fakeEngine) Check(_ context.Context, _ engine.Repo, readData bool) error {
	f.called = true
	f.gotRead = readData
	return f.checkErr
}

func (f *fakeEngine) Name() string { return "restic" }

func (f *fakeEngine) Version(context.Context) string { return f.version }

func testSpec() *discovery.BackupSpec {
	return &discovery.BackupSpec{
		Service:       "librespeed-ts",
		Destination:   "local",
		RepoPath:      "librespeed-ts",
		ContainerName: "librespeed-ts",
		ContainerID:   "deadbeef",
	}
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	step := 0
	return func() time.Time {
		// started, then finished 1500ms later.
		out := t.Add(time.Duration(step) * 1500 * time.Millisecond)
		step++
		return out
	}
}

func TestRun_PassMetadata(t *testing.T) {
	eng := &fakeEngine{checkErr: nil, version: "0.17.0"}
	c := Run(context.Background(), eng, engine.Repo{}, Params{
		Spec:        testSpec(),
		HostID:      "h_abc",
		RuntimeName: "docker",
		Version:     "1.2.3",
		Trigger:     "schedule",
		ReadData:    false,
		Now:         fixedClock(),
	})

	if !eng.called || eng.gotRead {
		t.Fatalf("expected Check called with readData=false, called=%v read=%v", eng.called, eng.gotRead)
	}
	if c.Record != record.CheckRecordType {
		t.Errorf("record = %q, want %q", c.Record, record.CheckRecordType)
	}
	if c.Result != "pass" {
		t.Errorf("result = %q, want pass", c.Result)
	}
	if c.Reason != nil || c.ReasonCode != nil {
		t.Errorf("pass must carry null reason/reason_code, got %v / %v", c.Reason, c.ReasonCode)
	}
	if c.Method != record.CheckMethodMetadata {
		t.Errorf("method = %q, want %q", c.Method, record.CheckMethodMetadata)
	}
	if c.DurationMs != 1500 {
		t.Errorf("duration_ms = %d, want 1500", c.DurationMs)
	}
	if c.Engine.Version != "0.17.0" {
		t.Errorf("engine version = %q, want 0.17.0", c.Engine.Version)
	}
	if c.RepoID != "local:librespeed-ts" {
		t.Errorf("repo_id = %q, want local:librespeed-ts", c.RepoID)
	}
}

func TestRun_ReadDataMethod(t *testing.T) {
	eng := &fakeEngine{}
	c := Run(context.Background(), eng, engine.Repo{}, Params{
		Spec: testSpec(), ReadData: true, Now: fixedClock(),
	})
	if !eng.gotRead {
		t.Errorf("expected Check called with readData=true")
	}
	if c.Method != record.CheckMethodReadData {
		t.Errorf("method = %q, want %q", c.Method, record.CheckMethodReadData)
	}
}

func TestRun_FailCheckErrors(t *testing.T) {
	eng := &fakeEngine{checkErr: errors.New("engine: check: restic exited 1: pack 0abc is damaged")}
	c := Run(context.Background(), eng, engine.Repo{}, Params{Spec: testSpec(), Now: fixedClock()})

	if c.Result != "fail" {
		t.Fatalf("result = %q, want fail", c.Result)
	}
	if c.ReasonCode == nil || *c.ReasonCode != "check_errors" {
		t.Errorf("reason_code = %v, want check_errors", c.ReasonCode)
	}
	if c.Reason == nil || !strings.Contains(*c.Reason, "damaged") {
		t.Errorf("reason = %v, want the restic error text", c.Reason)
	}
}

func TestRun_CancelledInconclusive(t *testing.T) {
	eng := &fakeEngine{checkErr: fmt.Errorf("engine: check: %w", context.Canceled)}
	c := Run(context.Background(), eng, engine.Repo{}, Params{Spec: testSpec(), Now: fixedClock()})

	if c.Result != "inconclusive" {
		t.Fatalf("result = %q, want inconclusive", c.Result)
	}
	if c.ReasonCode == nil || *c.ReasonCode != "cancelled" {
		t.Errorf("reason_code = %v, want cancelled", c.ReasonCode)
	}
}

func TestBounded_TruncatesAtUTF8Boundary(t *testing.T) {
	// A long run of a multi-byte rune, so a naive byte cut could split one.
	long := strings.Repeat("é", 5000) // 2 bytes each => 10000 bytes
	got := bounded(long)
	if len(got) > reasonMaxBytes {
		t.Errorf("bounded length = %d, want <= %d", len(got), reasonMaxBytes)
	}
	if !utf8ValidString(got) {
		t.Errorf("bounded output is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("bounded output should note truncation, got tail %q", tail(got))
	}
	// A short reason is returned unchanged.
	if bounded("short") != "short" {
		t.Errorf("bounded mangled a short reason")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			// range yields U+FFFD for an invalid sequence; a literal replacement
			// char in the input would also trip this, which the test never uses.
			return false
		}
	}
	return true
}

func tail(s string) string {
	if len(s) < 20 {
		return s
	}
	return s[len(s)-20:]
}
