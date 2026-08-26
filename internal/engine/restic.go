// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Restic drives the restic CLI. It is the first and default engine.
//
// Every method shells out to the restic binary with --json where restic
// supports it, and parses the result. The repository password is never
// placed on the child process's argv and never logged: it is written to a
// 0600 temporary file for the lifetime of a single restic invocation and
// handed to the child as RESTIC_PASSWORD_FILE, then removed.
type Restic struct {
	// binary is the path to the restic executable.
	binary string
}

// NewRestic returns a restic driver using the given binary path (default
// "restic" if empty).
func NewRestic(binary string) *Restic {
	if binary == "" {
		binary = "restic"
	}
	return &Restic{binary: binary}
}

// compile-time assertion that the driver satisfies the interface.
var _ Engine = (*Restic)(nil)

func (r *Restic) Name() string { return "restic" }

func (r *Restic) EnsureRepo(ctx context.Context, repo Repo) error {
	_, err := r.run(ctx, repo, nil, "cat", "config")
	if err == nil {
		return nil
	}

	var rerr *resticError
	if !errors.As(err, &rerr) || !looksUninitialized(rerr.stderr) {
		return fmt.Errorf("engine: check repository: %w", err)
	}

	if _, err := r.run(ctx, repo, nil, "init"); err != nil {
		return fmt.Errorf("engine: init repository: %w", err)
	}
	return nil
}

// looksUninitialized reports whether stderr from `restic cat config`
// indicates the repository has simply never been initialized, as opposed to
// some other failure (wrong password, unreachable backend, corrupt repo)
// that EnsureRepo must surface rather than paper over with a fresh init.
func looksUninitialized(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "does not exist") ||
		strings.Contains(s, "unable to open config file") ||
		strings.Contains(s, "no repository found") ||
		strings.Contains(s, "repository not found")
}

func (r *Restic) Backup(ctx context.Context, req BackupRequest) (BackupResult, error) {
	if (len(req.Paths) == 0) == (req.Stdin == nil) {
		return BackupResult{}, errors.New("engine: backup request must set exactly one of Paths or Stdin")
	}

	args := []string{"backup", "--json"}
	if req.Host != "" {
		args = append(args, "--host", req.Host)
	}
	for _, tag := range req.Tags {
		args = append(args, "--tag", tag)
	}
	for _, pattern := range req.Excludes {
		args = append(args, "--exclude", pattern)
	}
	if req.ExcludeCaches {
		args = append(args, "--exclude-caches")
	}

	var stdin io.Reader
	if req.Stdin != nil {
		args = append(args, "--stdin")
		if req.StdinFilename != "" {
			args = append(args, "--stdin-filename", req.StdinFilename)
		}
		stdin = req.Stdin
	} else {
		args = append(args, req.Paths...)
	}

	res, runErr := r.run(ctx, req.Repo, stdin, args...)

	// Parse stdout for a summary message even when runErr is set: for a
	// --stdin backup, restic reads its stdin through an OS pipe, which has
	// no way to signal "the writer errored" to the reading end, only a
	// plain close indistinguishable from a clean end of input (see
	// runStreamBackup's doc comment in the orchestrator package for the
	// full mechanics). So restic itself can complete and exit 0, printing a
	// real "summary" message with a real snapshot ID, while cmd.Wait()
	// still surfaces a non-nil error here because Go's own stdin-copy
	// goroutine failed. Returning that summary's SnapshotID alongside the
	// error (instead of discarding it as BackupResult{}) is what lets a
	// caller find and delete the snapshot restic wrote despite the failure,
	// rather than losing track of it.
	result, parseErr := parseBackupSummary(res.Stdout)
	if parseErr != nil {
		// No summary in stdout: nothing was written, or runErr already
		// explains why there's no output to parse. Report whichever error
		// is more informative, preferring runErr since a parse failure with
		// no runErr (a summary-shaped restic invocation produced no summary
		// line) still needs to surface as a backup failure.
		if runErr != nil {
			return BackupResult{}, fmt.Errorf("engine: backup: %w", runErr)
		}
		return BackupResult{}, fmt.Errorf("engine: backup: %w", parseErr)
	}
	if runErr != nil {
		return result, fmt.Errorf("engine: backup: %w", runErr)
	}
	return result, nil
}

// resticBackupMessage is the shape of one line of `restic backup --json`
// output that matters to Ballast: the final "summary" message. restic also
// emits "status", "verbose_status", and "error" messages on the way there,
// which are parsed and discarded.
type resticBackupMessage struct {
	MessageType   string  `json:"message_type"`
	SnapshotID    string  `json:"snapshot_id"`
	DataAdded     uint64  `json:"data_added"`
	FilesNew      uint64  `json:"files_new"`
	TotalDuration float64 `json:"total_duration"`
}

// parseBackupSummary scans the newline-delimited JSON restic backup --json
// writes to stdout and extracts the "summary" message.
func parseBackupSummary(stdout []byte) (BackupResult, error) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg resticBackupMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// Not every line is guaranteed to unmarshal into this shape;
			// skip anything that doesn't rather than fail the whole backup
			// on an informational line we don't care about.
			continue
		}
		if msg.MessageType != "summary" {
			continue
		}

		return BackupResult{
			SnapshotID: msg.SnapshotID,
			BytesAdded: msg.DataAdded,
			FilesNew:   msg.FilesNew,
			Duration:   time.Duration(msg.TotalDuration * float64(time.Second)),
		}, nil
	}
	if err := scanner.Err(); err != nil {
		return BackupResult{}, fmt.Errorf("read backup output: %w", err)
	}
	return BackupResult{}, errors.New("no summary message in restic backup output")
}

func (r *Restic) Forget(ctx context.Context, repo Repo, policy RetentionPolicy) error {
	args := []string{"forget", "--json"}

	if policy.Last > 0 {
		args = append(args, "--keep-last", strconv.Itoa(policy.Last))
	}
	if policy.Hourly > 0 {
		args = append(args, "--keep-hourly", strconv.Itoa(policy.Hourly))
	}
	if policy.Daily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(policy.Daily))
	}
	if policy.Weekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(policy.Weekly))
	}
	if policy.Monthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(policy.Monthly))
	}
	if policy.Yearly > 0 {
		args = append(args, "--keep-yearly", strconv.Itoa(policy.Yearly))
	}
	if policy.Within != "" {
		args = append(args, "--keep-within", policy.Within)
	}
	for _, tag := range policy.KeepTags {
		args = append(args, "--keep-tag", tag)
	}

	// No --prune here: Prune is a separate, explicit step Ballast calls on
	// its own schedule, never bundled into a forget.
	if _, err := r.run(ctx, repo, nil, args...); err != nil {
		return fmt.Errorf("engine: forget: %w", err)
	}
	return nil
}

// DeleteSnapshot removes a single snapshot by ID via `restic forget <id>`,
// independent of any retention policy. No --prune here either, matching
// Forget: reclaiming the now-unreferenced data is a separate, explicit step.
func (r *Restic) DeleteSnapshot(ctx context.Context, repo Repo, id string) error {
	if id == "" {
		return errors.New("engine: delete snapshot: empty snapshot ID")
	}
	if _, err := r.run(ctx, repo, nil, "forget", id); err != nil {
		return fmt.Errorf("engine: delete snapshot %s: %w", id, err)
	}
	return nil
}

func (r *Restic) Prune(ctx context.Context, repo Repo) error {
	if _, err := r.run(ctx, repo, nil, "prune"); err != nil {
		return fmt.Errorf("engine: prune: %w", err)
	}
	return nil
}

func (r *Restic) Check(ctx context.Context, repo Repo, readData bool) error {
	args := []string{"check"}
	if readData {
		args = append(args, "--read-data")
	}

	if _, err := r.run(ctx, repo, nil, args...); err != nil {
		return fmt.Errorf("engine: check: %w", err)
	}
	return nil
}

// resticSnapshot is the shape of one entry in `restic snapshots --json`
// output.
type resticSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

func (r *Restic) Snapshots(ctx context.Context, repo Repo) ([]Snapshot, error) {
	res, err := r.run(ctx, repo, nil, "snapshots", "--json")
	if err != nil {
		return nil, fmt.Errorf("engine: snapshots: %w", err)
	}

	var raw []resticSnapshot
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("engine: snapshots: parse output: %w", err)
	}

	snapshots := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		// restic's JSON only carries "hostname"; Snapshot exposes both Host
		// and Hostname, so both are populated from it.
		snapshots = append(snapshots, Snapshot{
			ID:       s.ID,
			Time:     s.Time,
			Host:     s.Hostname,
			Hostname: s.Hostname,
			Tags:     s.Tags,
			Paths:    s.Paths,
		})
	}
	return snapshots, nil
}

func (r *Restic) Restore(ctx context.Context, req RestoreRequest) error {
	snapshotID := req.SnapshotID
	if snapshotID == "" {
		snapshotID = "latest"
	}

	args := []string{"restore", snapshotID, "--target", req.Target}
	for _, pattern := range req.Include {
		args = append(args, "--include", pattern)
	}

	if _, err := r.run(ctx, req.Repo, nil, args...); err != nil {
		return fmt.Errorf("engine: restore: %w", err)
	}
	return nil
}

// maxStderrInError caps how much of a failed command's stderr is folded into
// the returned error, so a runaway restic invocation can't produce an
// unbounded error message.
const maxStderrInError = 4096

// runResult is the captured output of one restic invocation.
type runResult struct {
	Stdout []byte
	Stderr []byte
}

// resticError describes a restic invocation that exited non-zero. Its
// message includes the restic subcommand, its exit code, and a truncated
// tail of stderr; it never includes the repository password, which restic
// only ever receives via a 0600 temp file, not argv and not a log line.
type resticError struct {
	command  string
	exitCode int
	stderr   string
	cause    error
}

func (e *resticError) Error() string {
	if e.stderr == "" {
		return fmt.Sprintf("restic %s: exit %d: %v", e.command, e.exitCode, e.cause)
	}
	return fmt.Sprintf("restic %s: exit %d: %s", e.command, e.exitCode, e.stderr)
}

func (e *resticError) Unwrap() error { return e.cause }

// run executes `restic <args...>` against repo. stdin, if non-nil, is piped
// to the child's standard input (used for streamed backups). Stdout and
// stderr are captured separately; a non-zero exit produces a *resticError
// carrying the exit code and a truncated tail of stderr.
func (r *Restic) run(ctx context.Context, repo Repo, stdin io.Reader, args ...string) (runResult, error) {
	env, cleanup, err := r.childEnv(repo)
	if err != nil {
		return runResult{}, err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	res := runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runErr == nil {
		return res, nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}

	return res, &resticError{
		command:  strings.Join(args, " "),
		exitCode: exitCode,
		stderr:   truncateOutput(res.Stderr, maxStderrInError),
		cause:    runErr,
	}
}

// childEnv builds the environment for one restic child process: the
// repository location, the password (via a 0600 temp file, so it never
// appears on argv or in a log line), and any backend credentials from
// repo.Env, layered over the calling process's own environment. The
// returned cleanup func removes the password file and must be called once
// the child process has exited.
func (r *Restic) childEnv(repo Repo) (env []string, cleanup func(), err error) {
	password, err := repo.Password()
	if err != nil {
		return nil, func() {}, fmt.Errorf("engine: resolve repository password: %w", err)
	}

	passwordFile, err := writePasswordFile(password)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup = func() { _ = os.Remove(passwordFile) }

	overrides := make(map[string]string, len(repo.Env)+2)
	overrides["RESTIC_REPOSITORY"] = repo.URL
	overrides["RESTIC_PASSWORD_FILE"] = passwordFile
	for k, v := range repo.Env {
		overrides[k] = v
	}

	// Strip any password restic would otherwise also accept, so our temp
	// file is the only source of it regardless of what the calling
	// process's own environment happens to carry.
	strip := []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND"}

	return mergeEnv(os.Environ(), overrides, strip), cleanup, nil
}

// writePasswordFile writes password to a new 0600 temporary file and
// returns its path. The caller is responsible for removing it.
func writePasswordFile(password string) (string, error) {
	f, err := os.CreateTemp("", "ballast-restic-password-*")
	if err != nil {
		return "", fmt.Errorf("engine: create password file: %w", err)
	}
	path := f.Name()

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("engine: chmod password file: %w", err)
	}
	if _, err := f.WriteString(password); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("engine: write password file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("engine: close password file: %w", err)
	}
	return path, nil
}

// mergeEnv returns base with each key in strip or overrides removed, then
// overrides appended (sorted, for a deterministic result). Removing
// conflicting keys first means an override always wins, regardless of how
// the OS environment lists duplicate keys.
func mergeEnv(base []string, overrides map[string]string, strip []string) []string {
	drop := make(map[string]bool, len(overrides)+len(strip))
	for k := range overrides {
		drop[k] = true
	}
	for _, k := range strip {
		drop[k] = true
	}

	merged := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		merged = append(merged, kv)
	}

	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		merged = append(merged, k+"="+overrides[k])
	}
	return merged
}

// truncateOutput trims surrounding whitespace from b and caps it at max
// bytes, so a large stderr blob can't blow up an error message.
func truncateOutput(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "... (truncated)"
	}
	return s
}
