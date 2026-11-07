package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

// flagsFileNotices is where applyFlagsFile reports flags it ignores. A
// variable so tests can read it.
var flagsFileNotices io.Writer = os.Stderr

// applyFlagsFile sets serve's flags from the file at path, for each one the
// command line didn't set. The file holds flags separated by whitespace, on
// one line or several, and a word starting with # begins a comment that runs
// to the end of its line. There's no quoting, so no value can contain a space.
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
	var words []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "#") {
				break
			}
			words = append(words, word)
		}
	}

	// Parsed into a separate set with the same flags, which checks each value,
	// keeping the raw values to set on the command line's flags afterwards.
	fileFlags := pflag.NewFlagSet(path, pflag.ContinueOnError)
	fileFlags.SetOutput(io.Discard)
	defineServeFlags(fileFlags)
	type setting struct{ name, value string }
	var settings []setting
	err = fileFlags.ParseAll(words, func(f *pflag.Flag, value string) error {
		if err := fileFlags.Set(f.Name, value); err != nil {
			return err
		}
		settings = append(settings, setting{f.Name, value})
		return nil
	})
	if err != nil {
		return fmt.Errorf("flags file %s: %w", path, err)
	}
	if extra := fileFlags.Args(); len(extra) > 0 {
		return fmt.Errorf("flags file %s: %q isn't a flag", path, extra[0])
	}
	// Which flags the command line set, taken before any file value lands, so
	// a flag repeated in the file keeps its last value, as on a command line.
	fromCommandLine := map[string]bool{}
	cmdFlags.Visit(func(f *pflag.Flag) { fromCommandLine[f.Name] = true })
	noted := map[string]bool{}
	for _, s := range settings {
		if fromCommandLine[s.name] {
			if !noted[s.name] {
				noted[s.name] = true
				fmt.Fprintf(flagsFileNotices, "flags file %s: ignoring --%s, which the command line sets\n", path, s.name)
			}
			continue
		}
		if err := cmdFlags.Set(s.name, s.value); err != nil {
			return fmt.Errorf("flags file %s: --%s: %w", path, s.name, err)
		}
	}
	return nil
}
