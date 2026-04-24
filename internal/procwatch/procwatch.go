// Package procwatch is a tiny Linux-/proc/ helper used to:
//
//   - Resolve the stable start-time of a PID so we can detect PID reuse.
//   - Check whether a PID is still alive.
//   - Walk the parent-process chain for the CLI's Claude-PID discovery.
//   - Read a process's environment to find the hook's owning Claude process.
//
// Pure Go, no cgo, Linux-only.
package procwatch

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// StartTime returns field 22 ("starttime") of /proc/<pid>/stat as an opaque
// string. Two reads of the same (pid, starttime) identify the same process
// instance — guards against PID reuse.
func StartTime(pid int) (string, error) {
	f, err := readStat(pid)
	if err != nil {
		return "", err
	}
	// After the `pid (comm)` header, field 3 is state; starttime is field 22,
	// i.e. offset 19 in the post-comm fields (22 - 3).
	if len(f) < 20 {
		return "", fmt.Errorf("procwatch: short stat for pid %d", pid)
	}
	return f[19], nil
}

// Alive reports whether pid is still the process identified by startToken.
// Any read error (ESRCH included) is treated as "not alive".
func Alive(pid int, startToken string) bool {
	got, err := StartTime(pid)
	if err != nil {
		return false
	}
	return got == startToken
}

// PPid returns the parent PID of pid, or 0 if unknown.
func PPid(pid int) int {
	f, err := readStat(pid)
	if err != nil || len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// HasEnv reports whether /proc/<pid>/environ contains an exact kv entry
// like "CLAUDECODE=1". Returns false on any read error (permission, ESRCH,
// etc.) so callers can treat missing info as "no match".
func HasEnv(pid int, kv string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	want := []byte(kv)
	for entry := range bytes.SplitSeq(data, []byte{0}) {
		if bytes.Equal(entry, want) {
			return true
		}
	}
	return false
}

// readStat returns the post-`(comm)` fields of /proc/<pid>/stat split on
// whitespace. The `comm` field itself can contain arbitrary characters
// including spaces and parens, so we split off anything up to the last ')'
// before tokenizing the rest.
func readStat(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return nil, fmt.Errorf("procwatch: malformed stat for pid %d", pid)
	}
	return strings.Fields(s[i+2:]), nil
}
