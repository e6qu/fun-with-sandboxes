package cri

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/image"
)

// criImage is img in CRI's shape. The sandbox image is pinned: the
// kubelet's image garbage collection leaves it.
func (s *Service) criImage(img image.Image) *runtimeapi.Image {
	out := &runtimeapi.Image{
		Id:          img.ID,
		RepoTags:    nonNil(img.RepoTags),
		RepoDigests: nonNil(img.RepoDigests),
		Size:        uint64(img.Size),
		Spec:        &runtimeapi.ImageSpec{Image: img.ID},
	}
	user, _, _ := strings.Cut(img.Config.Config.User, ":")
	if uid, err := strconv.ParseInt(user, 10, 64); err == nil {
		out.Uid = &runtimeapi.Int64Value{Value: uid}
	} else {
		out.Username = user
	}
	if pinned, err := s.e.Images().Get(s.sandboxImage); err == nil && pinned.ID == img.ID {
		out.Pinned = true
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Service) ListImages(ctx context.Context, req *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	want := ""
	if spec := req.GetFilter().GetImage(); spec != nil {
		want = spec.Image
	}
	var wantID string
	if want != "" {
		img, err := s.e.Images().Get(want)
		if errors.Is(err, image.ErrNotFound) {
			return &runtimeapi.ListImagesResponse{Images: []*runtimeapi.Image{}}, nil
		} else if err != nil {
			return nil, grpcError(err)
		}
		wantID = img.ID
	}
	out := []*runtimeapi.Image{}
	for _, img := range s.e.Images().List() {
		if wantID != "" && img.ID != wantID {
			continue
		}
		out = append(out, s.criImage(img))
	}
	return &runtimeapi.ListImagesResponse{Images: out}, nil
}

// ImageStatus describes an image, and answers no image, not an error,
// for one the runtime does not have.
func (s *Service) ImageStatus(ctx context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	ref := req.GetImage().GetImage()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "an image is required")
	}
	img, err := s.e.Images().Get(ref)
	if errors.Is(err, image.ErrNotFound) {
		return &runtimeapi.ImageStatusResponse{}, nil
	} else if err != nil {
		return nil, grpcError(err)
	}
	res := &runtimeapi.ImageStatusResponse{Image: s.criImage(img)}
	if req.Verbose {
		res.Info = map[string]string{"created": img.Created.Format(time.RFC3339Nano)}
	}
	return res, nil
}

func authenticator(a *runtimeapi.AuthConfig) authn.Authenticator {
	if a == nil || (a.Username == "" && a.Password == "" && a.Auth == "" && a.IdentityToken == "" && a.RegistryToken == "") {
		return authn.Anonymous
	}
	return authn.FromConfig(authn.AuthConfig{
		Username: a.Username, Password: a.Password, Auth: a.Auth,
		IdentityToken: a.IdentityToken, RegistryToken: a.RegistryToken,
	})
}

func (s *Service) PullImage(ctx context.Context, req *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	ref := req.GetImage().GetImage()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "an image is required")
	}
	pulled, err := s.e.Images().Pull(ctx, ref, authenticator(req.Auth), func(string, string) {})
	if err != nil {
		return nil, pullError(ref, err)
	}
	return &runtimeapi.PullImageResponse{ImageRef: pulled.Image.ID}, nil
}

func pullError(ref string, err error) error {
	if errors.Is(err, image.ErrNotFound) {
		return status.Errorf(codes.NotFound, "pull %s: %v", ref, err)
	}
	return status.Errorf(codes.Unknown, "pull %s: %v", ref, err)
}

// ensureImage is the ID of the image ref names, pulled if the runtime
// does not have it.
func (s *Service) ensureImage(ctx context.Context, ref string, auth *runtimeapi.AuthConfig) (string, error) {
	img, err := s.e.Images().Get(ref)
	if err == nil {
		return img.ID, nil
	}
	if !errors.Is(err, image.ErrNotFound) {
		return "", grpcError(err)
	}
	pulled, err := s.e.Images().Pull(ctx, ref, authenticator(auth), func(string, string) {})
	if err != nil {
		return "", pullError(ref, err)
	}
	return pulled.Image.ID, nil
}

// RemoveImage removes an image, and succeeds for one the runtime does not
// have.
func (s *Service) RemoveImage(ctx context.Context, req *runtimeapi.RemoveImageRequest) (*runtimeapi.RemoveImageResponse, error) {
	ref := req.GetImage().GetImage()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "an image is required")
	}
	img, err := s.e.Images().Get(ref)
	if errors.Is(err, image.ErrNotFound) {
		return &runtimeapi.RemoveImageResponse{}, nil
	} else if err != nil {
		return nil, grpcError(err)
	}
	// CRI removes the image, whatever references name it.
	if _, err := s.e.ImageRemove(img.ID, true); err != nil && !isNotFound(err) {
		return nil, grpcError(err)
	}
	return &runtimeapi.RemoveImageResponse{}, nil
}

// diskUsage is what files take: their bytes on disk and their inodes.
type diskUsage struct{ bytes, inodes uint64 }

func (u *diskUsage) add(v diskUsage) {
	u.bytes += v.bytes
	u.inodes += v.inodes
}

// usage is what the files under dir take, dir included.
func usage(dir string) diskUsage {
	var u diskUsage
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		u.inodes++
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				u.bytes += uint64(st.Blocks) * 512
			}
		}
		return nil
	})
	return u
}

// imageUsage is what the image store takes. It walks each unpacked root
// once, and every other file each time.
func (s *Service) imageUsage() diskUsage {
	dir := filepath.Join(s.e.Root(), "images")
	u := diskUsage{inodes: 1}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "roots" {
			u.add(usage(filepath.Join(dir, e.Name())))
		}
	}
	roots := filepath.Join(dir, "roots")
	entries, _ = os.ReadDir(roots)
	u.inodes++
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// A root the store is still unpacking is staged under a dot name.
		if strings.HasPrefix(name, ".") {
			u.add(usage(filepath.Join(roots, name)))
			continue
		}
		present[name] = true
		ru, ok := s.rootUsage[name]
		if !ok {
			ru = usage(filepath.Join(roots, name))
			s.rootUsage[name] = ru
		}
		u.add(ru)
	}
	for name := range s.rootUsage {
		if !present[name] {
			delete(s.rootUsage, name)
		}
	}
	return u
}

// ImageFsInfo reports the filesystems the runtime keeps its images and
// its containers' files on: the directories of its store, and what the
// files in each take.
func (s *Service) ImageFsInfo(ctx context.Context, req *runtimeapi.ImageFsInfoRequest) (*runtimeapi.ImageFsInfoResponse, error) {
	now := time.Now().UnixNano()
	fsUsage := func(dir string, u diskUsage) *runtimeapi.FilesystemUsage {
		return &runtimeapi.FilesystemUsage{
			Timestamp:  now,
			FsId:       &runtimeapi.FilesystemIdentifier{Mountpoint: dir},
			UsedBytes:  &runtimeapi.UInt64Value{Value: u.bytes},
			InodesUsed: &runtimeapi.UInt64Value{Value: u.inodes},
		}
	}
	stacks := filepath.Join(s.e.Root(), "stacks")
	return &runtimeapi.ImageFsInfoResponse{
		ImageFilesystems:     []*runtimeapi.FilesystemUsage{fsUsage(filepath.Join(s.e.Root(), "images"), s.imageUsage())},
		ContainerFilesystems: []*runtimeapi.FilesystemUsage{fsUsage(stacks, usage(stacks))},
	}, nil
}
