// Package miniship reads MINISHIP.md, the fork's record of the upstream tag
// the miniship line sits on and of every patch the line carries.
//
// The record is written for people and read by two programs: this package's
// tests, which hold the branch to it, and miniship-cloud's scheduled check,
// which reads the fork point to decide which upstream releases and advisories
// are newer than it. Both read the same two table rows, so their shape is part
// of the record's contract:
//
//	| Upstream tag | `v2.4.2` |
//	| Upstream commit | `9070bda7e9ab6b6484e04a0983b8afd61c8315e6` |
package miniship

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ForkPoint is the upstream release the miniship line is rebased onto.
type ForkPoint struct {
	// Tag is upstream's release tag, such as v2.4.2.
	Tag string
	// Commit is the forty-character commit that tag names.
	Commit string
}

// Patch is one numbered entry under "## The patches".
type Patch struct {
	Number int
	// Title is the entry's bold name.
	Title string
	// Why is the entry's prose after the dash, as one paragraph.
	Why string
	// Paths are the entry's Paths line: repository paths, and directories
	// spelled with a trailing slash, that the patch changes.
	Paths []string
}

// Record is what MINISHIP.md states.
type Record struct {
	ForkPoint ForkPoint
	// Patches are in the order the record lists them, oldest first.
	Patches []Patch
}

// Version is what a binary built from this record reports: the upstream tag,
// without its v, on the miniship line. 2.4.2+miniship is v2.4.2 with the
// patches this record lists.
func (r Record) Version() string {
	return strings.TrimPrefix(r.ForkPoint.Tag, "v") + "+miniship"
}

// Claiming returns every patch whose Paths line names path.
func (r Record) Claiming(path string) []Patch {
	var out []Patch
	for _, patch := range r.Patches {
		for _, claim := range patch.Paths {
			if Claims(claim, path) {
				out = append(out, patch)
				break
			}
		}
	}
	return out
}

// Claims reports whether one entry of a Paths line names path: the same path,
// or a directory, spelled with a trailing slash, that holds it.
func Claims(claim, path string) bool {
	if strings.HasSuffix(claim, "/") {
		return strings.HasPrefix(path, claim)
	}
	return claim == path
}

var (
	tagRow    = regexp.MustCompile("(?m)^\\| Upstream tag \\| `([^`]*)` \\|\\s*$")
	commitRow = regexp.MustCompile("(?m)^\\| Upstream commit \\| `([^`]*)` \\|\\s*$")

	// "3. **Name** — why", the first line of an entry.
	entry = regexp.MustCompile(`^(\d+)\. \*\*(.+?)\*\*(?: — (.*))?$`)
	// "   Paths: `a/b.go`, `c/`", an entry's indented Paths line.
	pathsLine = regexp.MustCompile(`^\s+Paths:(.*)$`)
	quoted    = regexp.MustCompile("`([^`]+)`")
)

// Parse reads a record. It refuses one that states its fork point other than
// exactly once, because a second statement is a second answer.
func Parse(text string) (Record, error) {
	tag, err := theOne(tagRow, "Upstream tag", text)
	if err != nil {
		return Record{}, err
	}
	commit, err := theOne(commitRow, "Upstream commit", text)
	if err != nil {
		return Record{}, err
	}
	patches, err := parsePatches(text)
	if err != nil {
		return Record{}, err
	}
	return Record{ForkPoint: ForkPoint{Tag: tag, Commit: commit}, Patches: patches}, nil
}

// parsePatches reads the numbered entries between "## The patches" and the
// next heading of the same level. An entry is its first line and the indented
// lines under it; anything else in the section is prose about the list.
func parsePatches(text string) ([]Patch, error) {
	var (
		patches []Patch
		why     []string
		inList  bool
	)
	finish := func() error {
		if len(patches) == 0 {
			return nil
		}
		last := &patches[len(patches)-1]
		last.Why = strings.Join(why, " ")
		why = nil
		if last.Why == "" {
			return fmt.Errorf("patch %d, %q, says no why after its dash", last.Number, last.Title)
		}
		if len(last.Paths) == 0 {
			return fmt.Errorf("patch %d, %q, has no Paths line naming what it changes", last.Number, last.Title)
		}
		return nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			if inList {
				break
			}
			inList = strings.TrimSpace(line) == "## The patches"
			continue
		}
		if !inList {
			continue
		}
		if m := entry.FindStringSubmatch(line); m != nil {
			if err := finish(); err != nil {
				return nil, err
			}
			number, _ := strconv.Atoi(m[1])
			patches = append(patches, Patch{Number: number, Title: m[2]})
			if rest := strings.TrimSpace(m[3]); rest != "" {
				why = append(why, rest)
			}
			continue
		}
		if len(patches) == 0 || !strings.HasPrefix(line, " ") {
			continue
		}
		last := &patches[len(patches)-1]
		if m := pathsLine.FindStringSubmatch(line); m != nil {
			line = m[1]
		} else if len(last.Paths) == 0 {
			why = append(why, strings.TrimSpace(line))
			continue
		}
		// The Paths line, or a line it wrapped onto.
		for _, q := range quoted.FindAllStringSubmatch(line, -1) {
			last.Paths = append(last.Paths, q[1])
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return patches, nil
}

func theOne(row *regexp.Regexp, name, text string) (string, error) {
	found := row.FindAllStringSubmatch(text, -1)
	if len(found) != 1 {
		return "", fmt.Errorf("the record states %q %d times; it must state it once, as a table row", name, len(found))
	}
	return found[0][1], nil
}
