package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/kballard/go-shellquote"
	"github.com/spf13/pflag"
)

// applyFlagsFile sets serve's flags from the file at path, for each one the
// command line didn't set. The file holds flags as a shell would quote them,
// on one line or several, with blank lines and lines starting with # ignored.
// Nothing in it is expanded or run.
//
// A missing file means no flags. One that exists but can't be read, a
// dangling symlink included, is an error, so the node never runs without the
// flags its operator set.
func applyFlagsFile(cmdFlags *pflag.FlagSet, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
				return nil
			}
		}
		return fmt.Errorf("read flags file: %w", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			lines = append(lines, line)
		}
	}
	words, err := shellquote.Split(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("flags file %s: %w", path, err)
	}

	// A separate set with the same flags, so the file's values don't
	// overwrite the command line's before we know which were set there.
	fileFlags := pflag.NewFlagSet(path, pflag.ContinueOnError)
	fileFlags.SetOutput(io.Discard)
	defineServeFlags(fileFlags)
	if err := fileFlags.Parse(words); err != nil {
		return fmt.Errorf("flags file %s: %w", path, err)
	}
	if extra := fileFlags.Args(); len(extra) > 0 {
		return fmt.Errorf("flags file %s: %q isn't a flag", path, extra[0])
	}
	var setErr error
	fileFlags.Visit(func(f *pflag.Flag) {
		if setErr != nil || cmdFlags.Changed(f.Name) {
			return
		}
		if err := cmdFlags.Set(f.Name, f.Value.String()); err != nil {
			setErr = fmt.Errorf("flags file %s: --%s: %w", path, f.Name, err)
		}
	})
	return setErr
}
