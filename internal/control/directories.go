package control

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Directory browsing is confined to the daemon user's home, including symlinks.
// os.Root enforces the boundary during filesystem operations, not just preflight.
type ListDirectoriesQuery struct {
	Path       string `json:"path,omitempty"`
	After      string `json:"after,omitempty"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
}

type DirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryPage struct {
	Home      string           `json:"home"`
	Path      string           `json:"path"`
	Parent    string           `json:"parent,omitempty"`
	Entries   []DirectoryEntry `json:"entries"`
	NextAfter string           `json:"next_after,omitempty"`
}

func (q ListDirectoriesQuery) Validate() error {
	if len(q.Path) > 8192 || strings.ContainsAny(q.Path, "\x00\r\n") ||
		(q.Path != "" && !filepath.IsAbs(q.Path)) {
		return invalid("path", "path must be empty or an absolute path inside the home directory")
	}
	if len(q.After) > 1024 || strings.ContainsAny(q.After, "/\\\x00\r\n") {
		return invalid("after", "invalid directory cursor")
	}
	return nil
}

func handleDirectoriesQuery(ctx context.Context, src Source, q ListDirectoriesQuery) QueryResult {
	result := QueryResult{Type: QueryDirectoriesList}
	if src.Kind != SourceLocal && (src.Kind != SourceRemote || src.RemoteID == "") {
		result.Error = &ControlError{Code: "unauthorized", Message: "an authenticated source is required"}
		return result
	}
	home, err := os.UserHomeDir()
	if err == nil {
		result.Directories, err = listHomeDirectories(ctx, home, q)
	}
	if err != nil {
		code := "directory_unavailable"
		message := "Cannot read this directory. It must be accessible and inside the home directory."
		if errors.Is(err, os.ErrNotExist) {
			code, message = "not_found", "This directory no longer exists."
		}
		result.Error = &ControlError{Code: code, Message: message}
	}
	return result
}

func listHomeDirectories(ctx context.Context, home string, q ListDirectoriesQuery) (*DirectoryPage, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	path := q.Path
	if path == "" {
		path = home
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(home, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, os.ErrPermission
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	page := &DirectoryPage{Home: home, Path: path, Entries: []DirectoryEntry{}}
	if rel != "." {
		page.Parent = filepath.Dir(path)
	}
	// Retain only the first page plus one lookahead entry, even for huge folders.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, readErr := dir.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := entry.Name()
			if name <= q.After || (!q.ShowHidden && strings.HasPrefix(name, ".")) || strings.ContainsAny(name, "\r\n") {
				continue
			}
			if len(page.Entries) == 101 && name >= page.Entries[100].Name {
				continue
			}
			isDir := entry.IsDir()
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := root.Stat(filepath.Join(rel, name))
				isDir = err == nil && info.IsDir()
			}
			if !isDir {
				continue
			}
			index := sort.Search(len(page.Entries), func(i int) bool { return page.Entries[i].Name >= name })
			page.Entries = append(page.Entries, DirectoryEntry{})
			copy(page.Entries[index+1:], page.Entries[index:])
			page.Entries[index] = DirectoryEntry{Name: name, Path: filepath.Join(path, name)}
			if len(page.Entries) > 101 {
				page.Entries = page.Entries[:101]
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if len(page.Entries) > 100 {
		page.Entries = page.Entries[:100]
		page.NextAfter = page.Entries[99].Name
	}
	return page, nil
}
