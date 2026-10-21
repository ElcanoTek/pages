// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// File-backed templates: register designs from FILES, without an agent (the
// `pages template` verb). A template is a DIRECTORY containing template.html,
// so it can carry its metadata (and later, assets) beside the document. The
// directory name is the template name unless template.json overrides it,
// which keeps `sync` predictable: what you see in the tree is what gets
// registered.

// TemplateFile and MetadataFile are the file-backed convention's names.
const (
	TemplateFile = "template.html"
	MetadataFile = "template.json"
)

// FileEntry is one discovered template directory. Title, Description and
// Note come from template.json (nil when absent).
type FileEntry struct {
	Name        string
	Path        string // the template.html path inside the FS
	HTML        string
	ReadErr     error // template.html could not be read
	Title       *string
	Description *string
	Note        *string
}

// collationKey approximates String#localeCompare (ICU root collation) for the
// characters template directories use: '_' < '-' < digits < letters, case
// ignored at the first level.
func collationKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '_':
			b.WriteByte(1)
		case r == '-':
			b.WriteByte(2)
		case r >= '0' && r <= '9':
			b.WriteByte(byte(10 + r - '0'))
		case r >= 'a' && r <= 'z':
			b.WriteByte(byte(30 + r - 'a'))
		case r >= 'A' && r <= 'Z':
			b.WriteByte(byte(30 + r - 'A'))
		default:
			b.WriteString(string(r))
		}
	}
	return b.String()
}

// Discover lists fsys's template directories in name order. root labels
// error messages (the directory fsys was opened from). A missing directory is
// empty, not an error: sync runs on deployments that ship no templates. An
// invalid template.json is an error, so a typo fails at sync rather than
// silently dropping the title a human meant to set.
func Discover(fsys fs.FS, root string) ([]FileEntry, error) {
	dirs, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return []FileEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(dirs, func(i, j int) bool {
		a, b := collationKey(dirs[i].Name()), collationKey(dirs[j].Name())
		if a != b {
			return a < b
		}
		return dirs[i].Name() > dirs[j].Name() // lowercase first on a case-only tie
	})
	found := []FileEntry{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		htmlPath := path.Join(d.Name(), TemplateFile)
		if _, err := fs.Stat(fsys, htmlPath); err != nil {
			continue
		}
		e := FileEntry{Name: d.Name(), Path: htmlPath}
		if err := readMetadata(fsys, root, d.Name(), &e); err != nil {
			return nil, err
		}
		b, err := fs.ReadFile(fsys, htmlPath)
		if err != nil {
			e.ReadErr = err
		} else {
			e.HTML = string(b)
		}
		found = append(found, e)
	}
	return found, nil
}

func readMetadata(fsys fs.FS, root, dir string, e *FileEntry) error {
	rel := path.Join(dir, MetadataFile)
	label := rel
	if root != "" {
		label = path.Join(root, rel)
	}
	b, err := fs.ReadFile(fsys, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	v, err := jsonv.Parse(b)
	if err != nil {
		return fmt.Errorf("%s: invalid JSON (%w)", label, err)
	}
	obj, ok := v.(*jsonv.Object)
	if !ok {
		return fmt.Errorf("%s: must contain a JSON object", label)
	}
	var ferr error
	obj.Range(func(key string, value any) bool {
		var dst **string
		switch key {
		case "name":
			s, ok := value.(string)
			if !ok {
				ferr = fmt.Errorf("%s: %q must be a string", label, key)
				return false
			}
			e.Name = s
			return true
		case "title":
			dst = &e.Title
		case "description":
			dst = &e.Description
		case "note":
			dst = &e.Note
		default:
			ferr = fmt.Errorf("%s: unknown key %s", label, jsonv.Quote(key))
			return false
		}
		if value == nil {
			return true
		}
		s, ok := value.(string)
		if !ok {
			ferr = fmt.Errorf("%s: %q must be a string", label, key)
			return false
		}
		*dst = &s
		return true
	})
	return ferr
}
