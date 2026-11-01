package engine

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
)

// gitPath matches a URL path naming a Git repository, as dockerd's does:
// one ending in .git.
var gitPath = regexp.MustCompile(`\.git(?:#.+)?$`)

// isGitURL reports a remote dockerd treats as a Git repository.
func isGitURL(remote string) bool {
	if strings.HasPrefix(remote, "http://") || strings.HasPrefix(remote, "https://") {
		return gitPath.MatchString(remote)
	}
	for _, p := range []string{"git://", "github.com/", "git@"} {
		if strings.HasPrefix(remote, p) {
			return true
		}
	}
	return false
}

// fetchRemoteContext fills dir with the build context remote names, as
// dockerd fetches one for docker build URL: a Git repository at a ref and
// subdirectory, a tarball, plain or compressed, or a plain Dockerfile.
func fetchRemoteContext(ctx context.Context, remote, dir string, opts *BuildOptions) error {
	if isGitURL(remote) {
		return gitContext(ctx, remote, dir)
	}
	if !strings.HasPrefix(remote, "http://") && !strings.HasPrefix(remote, "https://") {
		return invalid("unable to prepare context: %q is neither a Git repository nor an http(s) URL", remote)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remote, nil)
	if err != nil {
		return invalid("%v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return invalid("unable to prepare context: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return invalid("unable to prepare context: unable to download remote context %s: %s", remote, res.Status)
	}
	br := bufio.NewReaderSize(res.Body, 1024)
	head, _ := br.Peek(512)
	if !isArchiveHeader(head) {
		// A plain-text Dockerfile is the whole context.
		opts.Dockerfile = "Dockerfile"
		f, err := os.Create(filepath.Join(dir, "Dockerfile"))
		if err != nil {
			return err
		}
		_, err = br.WriteTo(f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	tarPath := filepath.Join(filepath.Dir(dir), "remote.tar")
	if err := decompressTo(br, tarPath); err != nil {
		return invalid("unable to prepare context: %v", err)
	}
	defer os.Remove(tarPath)
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := image.Extract(dir, f, false); err != nil {
		return invalid("unable to prepare context: %v", err)
	}
	return nil
}

// isArchiveHeader reports a stream that starts as a tar, or as gzip,
// bzip2, xz or zstd data.
func isArchiveHeader(b []byte) bool {
	for _, magic := range [][]byte{{0x1f, 0x8b}, []byte("BZh"), {0xfd, '7', 'z', 'X', 'Z', 0}, {0x28, 0xb5, 0x2f, 0xfd}} {
		if len(b) >= len(magic) && string(b[:len(magic)]) == string(magic) {
			return true
		}
	}
	return len(b) >= 262 && string(b[257:262]) == "ustar"
}

// gitContext checks out a repository as dockerd does: the fragment after #
// names a ref and, after a colon, a subdirectory, and the fetch is shallow,
// with submodules.
func gitContext(ctx context.Context, remote, dir string) error {
	repo, fragment, _ := strings.Cut(remote, "#")
	ref, sub, _ := strings.Cut(fragment, ":")
	if strings.HasPrefix(repo, "github.com/") {
		repo = "https://" + repo
	}
	if u, err := url.Parse(repo); err == nil && u.Scheme == "file" {
		return invalid("unable to prepare context: file:// Git repositories are not supported")
	}
	checkout := filepath.Join(filepath.Dir(dir), "git")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return err
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return invalid("unable to prepare context: git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	fetch := []string{"fetch", "--depth", "1", "origin"}
	if ref != "" {
		fetch = append(fetch, ref)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repo},
		fetch,
		{"checkout", "--quiet", "FETCH_HEAD"},
		{"submodule", "update", "--init", "--recursive", "--depth", "1"},
	} {
		if err := git(args...); err != nil {
			return err
		}
	}
	src := checkout
	if sub != "" {
		p, err := rootfs.Join(checkout, sub)
		if err != nil {
			return invalid("unable to prepare context: %v", err)
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return invalid("unable to prepare context: subdirectory %s of %s does not exist", sub, repo)
		}
		src = p
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	if err := os.Rename(src, dir); err != nil {
		return fmt.Errorf("unable to prepare context: %w", err)
	}
	return nil
}
