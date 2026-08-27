package project

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Identity struct {
	Root   string
	Key    string
	Remote string
	Name   string
}

func Resolve(path string) (Identity, error) {
	if path == "" {
		path = "."
	}
	rootOutput, err := git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return Identity{}, fmt.Errorf("%s is not inside a Git repository", path)
	}
	root, err := filepath.Abs(strings.TrimSpace(rootOutput))
	if err != nil {
		return Identity{}, fmt.Errorf("resolve Git root: %w", err)
	}
	root = filepath.Clean(root)
	name := filepath.Base(root)
	remoteOutput, remoteErr := git(root, "remote", "get-url", "origin")
	remote := ""
	if remoteErr == nil {
		remote = NormalizeRemote(strings.TrimSpace(remoteOutput))
	}
	key := remote
	if key == "" {
		// A repository name remains stable when a repository without an origin is
		// copied to another device. It is less specific than a normalized remote,
		// but it avoids making an absolute device-local path the canonical key.
		key = "local/" + name
	}
	return Identity{Root: root, Key: key, Remote: remote, Name: name}, nil
}

var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):/?(.+)$`)

func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if match := scpRemote.FindStringSubmatch(raw); match != nil && !strings.Contains(raw, "://") {
		return normalizeHostPath(match[1], match[2])
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	switch parsed.Scheme {
	case "http", "https", "ssh", "git":
		return normalizeHostPath(parsed.Hostname(), parsed.Path)
	default:
		return ""
	}
}

func normalizeHostPath(host, path string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + path
}

func git(path string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", path}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	return string(output), err
}
