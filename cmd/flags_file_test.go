package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveFlagsWith returns serve's flags parsed from args, as cobra would.
func serveFlagsWith(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	defineServeFlags(fs)
	fs.String("flags-file", "", "")
	require.NoError(t, fs.Parse(args))
	return fs
}

func writeFlagsFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lamplighter.args")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestFlagsFileSetsFlags(t *testing.T) {
	path := writeFlagsFile(t, `# Lamplighter flags
--bandwidth-limit 10mbit --max-peers 5

--user-id operator --no-port-mapping
--rotation-interval=30m
`)
	fs := serveFlagsWith(t, "--register")
	require.NoError(t, applyFlagsFile(fs, path))

	limit, _ := fs.GetString("bandwidth-limit")
	peers, _ := fs.GetInt("max-peers")
	user, _ := fs.GetString("user-id")
	noMap, _ := fs.GetBool("no-port-mapping")
	rotation, _ := fs.GetDuration("rotation-interval")
	assert.Equal(t, "10mbit", limit)
	assert.Equal(t, 5, peers)
	assert.Equal(t, "operator", user)
	assert.True(t, noMap)
	assert.Equal(t, 30*time.Minute, rotation)
}

// A word starting with # begins a comment to the end of its line, and
// Windows line endings don't end up in values.
func TestFlagsFileCommentsAndLineEndings(t *testing.T) {
	path := writeFlagsFile(t, "\uFEFF# Lamplighter\r\n"+
		"--max-peers 5  # five peers\r\n"+
		"--bandwidth-limit 10mbit#not-a-comment\r\n"+
		"--user-id operator\r\n")
	fs := serveFlagsWith(t, "--register")
	require.NoError(t, applyFlagsFile(fs, path))

	peers, _ := fs.GetInt("max-peers")
	limit, _ := fs.GetString("bandwidth-limit")
	user, _ := fs.GetString("user-id")
	assert.Equal(t, 5, peers, "a trailing comment is dropped")
	assert.Equal(t, "10mbit#not-a-comment", limit, "a # inside a word is kept")
	assert.Equal(t, "operator", user, "a CRLF doesn't end up in a value")
	// The byte-order mark at the start didn't stop the first line being a
	// comment, or the require above would have failed.
}

func TestFlagsFileLosesToCommandLine(t *testing.T) {
	path := writeFlagsFile(t, "--max-peers 5 --state-dir /from/file --state-dir /again")
	fs := serveFlagsWith(t, "--register", "--max-peers", "3", "--state-dir", "/from/cli")
	var notices strings.Builder
	flagsFileNotices = &notices
	t.Cleanup(func() { flagsFileNotices = os.Stderr })
	require.NoError(t, applyFlagsFile(fs, path))
	assert.Equal(t, 1, strings.Count(notices.String(), "ignoring --state-dir"), "noted once: %q", notices.String())
	assert.Equal(t, 1, strings.Count(notices.String(), "ignoring --max-peers"))

	peers, _ := fs.GetInt("max-peers")
	dir, _ := fs.GetString("state-dir")
	assert.Equal(t, 3, peers)
	assert.Equal(t, "/from/cli", dir)
}

// A flag repeated in the file keeps its last value, as on a command line, and
// isn't mistaken for one the command line set.
func TestFlagsFileRepeatedFlag(t *testing.T) {
	path := writeFlagsFile(t, "--max-peers 5 --max-peers 3")
	fs := serveFlagsWith(t, "--register")
	var notices strings.Builder
	flagsFileNotices = &notices
	t.Cleanup(func() { flagsFileNotices = os.Stderr })
	require.NoError(t, applyFlagsFile(fs, path))
	peers, _ := fs.GetInt("max-peers")
	assert.Equal(t, 3, peers)
	assert.Empty(t, notices.String())
}

func TestFlagsFileMissingMeansNone(t *testing.T) {
	fs := serveFlagsWith(t, "--register")
	require.NoError(t, applyFlagsFile(fs, filepath.Join(t.TempDir(), "absent")))
	peers, _ := fs.GetInt("max-peers")
	assert.Zero(t, peers)
}

// A file that's there but can't be used stops the node rather than letting it
// run without the flags its operator set.
func TestFlagsFileErrors(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown flag":       "--no-such-flag 1",
		"positional word":    "--max-peers 5 stray",
		"bad value":          "--max-peers many",
		"nested flags file":  "--flags-file /elsewhere",
		"value on next line": "--user-id\n--no-port-mapping",
	} {
		t.Run(name, func(t *testing.T) {
			err := applyFlagsFile(serveFlagsWith(t), writeFlagsFile(t, contents))
			assert.Error(t, err)
		})
	}

	t.Run("dangling symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lamplighter.args")
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), path); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("can't create a symlink without the privilege for it: %v", err)
			}
			require.NoError(t, err)
		}
		assert.Error(t, applyFlagsFile(serveFlagsWith(t), path))
	})

	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permissions don't stop root, and Windows has no mode bits")
		}
		path := writeFlagsFile(t, "--max-peers 5")
		require.NoError(t, os.Chmod(path, 0))
		assert.Error(t, applyFlagsFile(serveFlagsWith(t), path))
	})
}
