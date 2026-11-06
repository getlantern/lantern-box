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

--user-id "an operator" --no-port-mapping
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
	assert.Equal(t, "an operator", user, "a quoted value keeps its space")
	assert.True(t, noMap)
	assert.Equal(t, 30*time.Minute, rotation)
}

// Comments are cut where a shell would cut them, and Windows line endings
// don't end up in values.
func TestFlagsFileCommentsAndLineEndings(t *testing.T) {
	path := writeFlagsFile(t, "--max-peers 5  # five peers\r\n"+
		"--user-id \"tag #1\"\r\n"+
		"--bandwidth-limit 10mbit#not-a-comment\r\n"+
		"--api-url a\\ #b\r\n")
	fs := serveFlagsWith(t, "--register")
	require.NoError(t, applyFlagsFile(fs, path))

	peers, _ := fs.GetInt("max-peers")
	user, _ := fs.GetString("user-id")
	limit, _ := fs.GetString("bandwidth-limit")
	api, _ := fs.GetString("api-url")
	assert.Equal(t, 5, peers, "a trailing comment and a CRLF are dropped")
	assert.Equal(t, "tag #1", user, "a # inside quotes is kept")
	assert.Equal(t, "10mbit#not-a-comment", limit, "a # inside a word is kept")
	assert.Equal(t, "a #b", api, "an escaped space keeps the # in the word")
}

// A line ending in a backslash continues onto the next, as in a shell, and a
// comment on that next line is still a comment.
func TestFlagsFileContinuation(t *testing.T) {
	path := writeFlagsFile(t, "--max-peers \\\n5 \\\n# five peers\n--user-id x\n")
	fs := serveFlagsWith(t, "--register")
	require.NoError(t, applyFlagsFile(fs, path))
	peers, _ := fs.GetInt("max-peers")
	user, _ := fs.GetString("user-id")
	assert.Equal(t, 5, peers)
	assert.Equal(t, "x", user)
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
		"unterminated quote": `--user-id "unfinished`,
		"bad value":          "--max-peers many",
		"nested flags file":  "--flags-file /elsewhere",
	} {
		t.Run(name, func(t *testing.T) {
			err := applyFlagsFile(serveFlagsWith(t), writeFlagsFile(t, contents))
			assert.Error(t, err)
		})
	}

	t.Run("dangling symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lamplighter.args")
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), path))
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
