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
// with # comments, on one line or several. Nothing in it is expanded or run.
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
	words, err := shellquote.Split(stripComments(strings.ReplaceAll(string(data), "\r", "")))
	if err != nil {
		return fmt.Errorf("flags file %s: %w", path, err)
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
	for _, s := range settings {
		if cmdFlags.Changed(s.name) {
			fmt.Fprintf(os.Stderr, "flags file %s: ignoring --%s, which the command line sets\n", path, s.name)
			continue
		}
		if err := cmdFlags.Set(s.name, s.value); err != nil {
			return fmt.Errorf("flags file %s: --%s: %w", path, s.name, err)
		}
	}
	return nil
}

// stripComments removes # comments from s, as a shell does: a # starting an
// unquoted word begins a comment that runs to the end of its line, and a #
// anywhere else, inside quotes or a word, is kept.
func stripComments(s string) string {
	var b strings.Builder
	var quote byte
	wordStart := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case quote == '"':
			if c == '\\' && i+1 < len(s) {
				b.WriteByte(c)
				i++
				c = s[i]
			} else if c == '"' {
				quote = 0
			}
		case c == '#' && wordStart:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
			wordStart = true
			continue
		case c == '\\' && i+1 < len(s):
			// An escaped character, even a space, is part of a word.
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
			wordStart = false
			continue
		case c == '\'' || c == '"':
			quote = c
		}
		b.WriteByte(c)
		wordStart = quote == 0 && (c == ' ' || c == '\t' || c == '\n')
	}
	return b.String()
}
