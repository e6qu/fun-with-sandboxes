package image

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func testRegistry(t *testing.T) string {
	t.Helper()
	reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(reg.Close)
	return strings.TrimPrefix(reg.URL, "http://")
}

func push(t *testing.T, ref string, img v1.Image) v1.Hash {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	dg, _ := img.Digest()
	return dg
}

type progressLog []string

func (p *progressLog) record(id, status string) { *p = append(*p, status) }

func (p progressLog) count(status string) int {
	n := 0
	for _, s := range p {
		if s == status {
			n++
		}
	}
	return n
}

// A pull records the tag and the repository@digest it resolved, downloads
// only the layers the store lacks, and knows when it changed nothing.
func TestPullRecordsDigestsAndReusesLayers(t *testing.T) {
	host := testRegistry(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := darwinImage(t, 2)
	extra, _ := random.Layer(128, types.OCILayer)
	child, err := mutate.AppendLayers(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	baseDigest := push(t, host+"/r/base:1", base)
	childDigest := push(t, host+"/r/child:1", child)

	var p progressLog
	got, err := s.Pull(context.Background(), host+"/r/base:1", p.record)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := host + "/r/base@" + baseDigest.String()
	if got.UpToDate || got.Digest != baseDigest.String() || got.Ref != host+"/r/base:1" ||
		len(got.Image.RepoDigests) != 1 || got.Image.RepoDigests[0] != wantDigest {
		t.Fatalf("pulled %+v, want digest reference %s", got, wantDigest)
	}
	if p.count("Pulling fs layer") != 2 || p.count("Pull complete") != 2 {
		t.Fatalf("progress %v", p)
	}
	if byDigest, err := s.Get(wantDigest); err != nil || byDigest.ID != got.Image.ID {
		t.Fatalf("Get by digest reference: %+v, %v", byDigest, err)
	}

	p = nil
	again, err := s.Pull(context.Background(), host+"/r/base:1", p.record)
	if err != nil || !again.UpToDate || len(p) != 0 {
		t.Fatalf("pulling again: %+v %v, progress %v", again, err, p)
	}

	p = nil
	if _, err := s.Pull(context.Background(), host+"/r/child:1", p.record); err != nil {
		t.Fatal(err)
	}
	if p.count("Already exists") != 2 || p.count("Pulling fs layer") != 1 {
		t.Fatalf("pulling the child: progress %v, want 2 layers reused and 1 pulled", p)
	}

	byDigest, err := s.Pull(context.Background(), host+"/r/child@"+childDigest.String(), func(string, string) {})
	if err != nil || !byDigest.UpToDate || len(byDigest.Image.RepoTags) != 1 {
		t.Fatalf("pulling by digest: %+v, %v", byDigest, err)
	}
}

// Pulling an index stores its image for the engine's platform under the
// index's digest.
func TestPullChoosesThePlatformFromAnIndex(t *testing.T) {
	host := testRegistry(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	darwin := darwinImage(t, 1)
	linux, _ := random.Image(64, 1)
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: linux, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: Platform.Architecture}}},
		mutate.IndexAddendum{Add: darwin, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: Platform.OS, Architecture: Platform.Architecture}}},
	)
	r, _ := name.ParseReference(host + "/r/multi:1")
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatal(err)
	}
	idxDigest, _ := idx.Digest()
	got, err := s.Pull(context.Background(), r.String(), func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := darwin.ConfigName()
	if got.Image.ID != wantID.String() || got.Image.RepoDigests[0] != host+"/r/multi@"+idxDigest.String() {
		t.Fatalf("pulled %+v, want image %s under index digest %s", got.Image, wantID, idxDigest)
	}
}

// docker rmi drops one reference while others remain, and a repository's
// digest references go with its last tag.
func TestRemoveDigestReferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg v1.ConfigFile
	img, err := extendRoot(t, s, "", t.TempDir(), cfg, []string{"a:1", "b:1"})
	if err != nil {
		t.Fatal(err)
	}
	const digest = "a@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	if err := s.AddReference(img.ID, digest); err != nil {
		t.Fatal(err)
	}
	unused := func(string) bool { return false }
	r, err := s.Remove("a:1", false, unused)
	if err != nil || r.Deleted || strings.Join(r.Untagged, " ") != "a:1 "+digest {
		t.Fatalf("removing a:1: %+v, %v", r, err)
	}
	if err := s.Tag("b:1", "a:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReference(img.ID, digest); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Remove(digest, false, unused); err != nil || r.Deleted || len(r.Untagged) != 1 {
		t.Fatalf("removing the digest reference: %+v, %v", r, err)
	}
	if _, err := s.Remove("b:1", false, unused); err != nil {
		t.Fatal(err)
	}
	r, err = s.Remove("a:1", false, unused)
	if err != nil || !r.Deleted {
		t.Fatalf("removing the last reference: %+v, %v", r, err)
	}
}

func TestNormalizeDigestReferences(t *testing.T) {
	const d = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	for in, want := range map[string]string{
		"postgres@" + d:                   "postgres@" + d,
		"docker.io/library/postgres@" + d: "postgres@" + d,
		"127.0.0.1:5000/a/b@" + d:         "127.0.0.1:5000/a/b@" + d,
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
