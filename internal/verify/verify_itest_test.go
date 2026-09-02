// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

//go:build integration

// Live end-to-end verify tests. Excluded from the normal "go test ./..." run
// (the //go:build integration tag). They require a real Docker socket, a real
// restic binary on PATH, and outbound network to pull postgres and nginx
// images, so they are run only by test/integration and skip cleanly when the
// socket or restic is absent.
//
// Every throwaway object these tests create is prefixed "ballast-verify-itest"
// and torn down by the verify code under test; nothing here ever touches a
// container, volume, or network outside that prefix, so a running stack is
// never disturbed.
package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/core/runtime"
)

const itestSocket = "/var/run/docker.sock"

func requireLive(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(itestSocket); err != nil {
		t.Skipf("no Docker socket at %s: %v", itestSocket, err)
	}
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH")
	}
}

// seedRepo initializes a throwaway restic repository in a temp dir and returns
// the engine and repo bound to it.
func seedRepo(t *testing.T) (*engine.Restic, engine.Repo) {
	t.Helper()
	dir := t.TempDir()
	pw := "ballast-verify-itest-password"
	repo := engine.Repo{URL: dir, Password: func() (string, error) { return pw, nil }}
	eng := engine.NewRestic("")
	if err := eng.EnsureRepo(context.Background(), repo); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	return eng, repo
}

func itestDeps(eng verifyEngine, rt runtime.Runtime, repo engine.Repo) Deps {
	return Deps{
		Runtime:     rt,
		Engine:      eng,
		Repo:        repo,
		StateDir:    "",
		HostID:      "h_9c1a2b7e8d4c5f00",
		Version:     "00.01.00b1",
		RuntimeName: "docker",
		Trigger:     "manual",
		NamePrefix:  "ballast-verify-itest",
	}
}

// verifyEngine is the local alias for the engine capability verify needs, so
// the concrete *engine.Restic can be handed in.
type verifyEngine = Engine

// TestLiveStreamRestorePostgres backs up a tiny SQL dump as a stream snapshot,
// then verifies it restores into a throwaway postgres and a row-count probe
// passes.
func TestLiveStreamRestorePostgres(t *testing.T) {
	requireLive(t)
	eng, repo := seedRepo(t)

	dump := "CREATE TABLE users (id int);\nINSERT INTO users VALUES (1),(2),(3);\n"
	if _, err := eng.Backup(context.Background(), engine.BackupRequest{
		Repo:          repo,
		Host:          "app-db",
		Tags:          []string{"ballast", "stream=db"},
		Stdin:         strings.NewReader(dump),
		StdinFilename: "db.sql",
	}); err != nil {
		t.Fatalf("seed stream backup: %v", err)
	}

	rt := runtime.NewDocker(itestSocket)
	defer rt.Close()

	spec := &discovery.BackupSpec{
		Service: "app-db", ContainerID: "n/a", ContainerName: "app-db",
		Destination: "local", RepoPath: "app-db",
		Verify: discovery.VerifySpec{
			Configured: true,
			Mode:       discovery.VerifyModeStreamRestore,
			Image:      "postgres:16-alpine",
			DataEngine: "postgres",
			Restore:    "psql -v ON_ERROR_STOP=1 -U postgres",
			Ready:      "pg_isready -U postgres",
			Probe:      "psql -U postgres -tAc 'select count(*) from users'",
			Expect:     "^[1-9][0-9]*$",
			User:       "postgres",
			Env: map[string]string{
				"POSTGRES_PASSWORD":         "verify",
				"POSTGRES_HOST_AUTH_METHOD": "trust",
			},
			Timeout: 5 * time.Minute,
		},
	}

	v, err := Run(context.Background(), spec, runtime.Container{}, "latest", itestDeps(eng, rt, repo))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("stream-restore result=%s reason=%v rows=%d id=%s", v.Result, v.Reason, v.Checked["rows"], v.VerifyID)
	if v.Result != "pass" {
		t.Fatalf("stream-restore did not pass: result=%s reason=%v", v.Result, deref(v.Reason))
	}
	if v.Checked["rows"] != 3 {
		t.Errorf("checked.rows = %d, want 3", v.Checked["rows"])
	}
	if !v.ScratchDestroyed {
		t.Errorf("scratch not destroyed: %v", deref(v.ScratchDestroyErr))
	}
	if !v.Environment.NetworkIsolated {
		t.Errorf("throwaway network was not recorded isolated")
	}
	assertNoItestLeftovers(t, rt)
}

// TestLiveContainerModeNginx backs up a directory of static files, then
// verifies it restores into a fresh scratch volume mounted into a throwaway
// nginx, and a probe inside the container reads the restored file.
func TestLiveContainerModeNginx(t *testing.T) {
	requireLive(t)
	eng, repo := seedRepo(t)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "marker"), []byte("hello-verify"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Backup(context.Background(), engine.BackupRequest{
		Repo: repo, Host: "web", Tags: []string{"ballast", "fs"}, Paths: []string{src},
	}); err != nil {
		t.Fatalf("seed fs backup: %v", err)
	}

	rt := runtime.NewDocker(itestSocket)
	defer rt.Close()

	container := runtime.Container{
		ID: "web", Name: "web",
		Mounts: []runtime.Mount{
			{Type: runtime.MountVolume, Name: "webdata", Source: src, Destination: "/usr/share/nginx/html"},
		},
	}
	spec := &discovery.BackupSpec{
		Service: "web", ContainerID: "web", ContainerName: "web",
		Destination: "local", RepoPath: "web",
		Paths: []string{src},
		Verify: discovery.VerifySpec{
			Configured: true,
			Mode:       discovery.VerifyModeContainer,
			Image:      "nginx:alpine",
			DataEngine: "files",
			Ready:      "test -f /usr/share/nginx/html/marker",
			Probe:      "cat /usr/share/nginx/html/marker",
			Expect:     "hello-verify",
			Timeout:    5 * time.Minute,
		},
	}

	v, err := Run(context.Background(), spec, container, "latest", itestDeps(eng, rt, repo))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("container result=%s reason=%v id=%s", v.Result, v.Reason, v.VerifyID)
	if v.Result != "pass" {
		t.Fatalf("container mode did not pass: result=%s reason=%v", v.Result, deref(v.Reason))
	}
	if !v.ScratchDestroyed {
		t.Errorf("scratch not destroyed: %v", deref(v.ScratchDestroyErr))
	}
	assertNoItestLeftovers(t, rt)
}

// assertNoItestLeftovers fails if any ballast-verify-itest container or network
// survived the teardown, the leak check that a copy of restored data was not
// left behind. It queries the runtime directly rather than shelling out.
func assertNoItestLeftovers(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	ctx := context.Background()
	if conts, err := rt.List(ctx); err == nil {
		for _, c := range conts {
			if strings.Contains(c.Name, "ballast-verify-itest") {
				t.Errorf("leftover throwaway container: %s (%s)", c.Name, c.State)
			}
		}
	} else {
		t.Logf("container leftover check: %v", err)
	}
	if insp, ok := rt.(runtime.NetworkInspector); ok {
		if nets, err := insp.ListNetworks(ctx); err == nil {
			for _, n := range nets {
				if strings.Contains(n.Name, "ballast-verify-itest") {
					t.Errorf("leftover throwaway network: %s", n.Name)
				}
			}
		}
	}
}
