package engine

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/e6qu/fun-with-sandboxes/internal/dockerfile"
	"github.com/e6qu/fun-with-sandboxes/internal/egress"
	"github.com/e6qu/fun-with-sandboxes/internal/identity"
	"github.com/e6qu/fun-with-sandboxes/internal/image"
	"github.com/e6qu/fun-with-sandboxes/internal/procs"
	"github.com/e6qu/fun-with-sandboxes/internal/rootfs"
	"github.com/e6qu/fun-with-sandboxes/internal/runtime"
	"github.com/e6qu/fun-with-sandboxes/internal/seatbelt"
)

type BuildOptions struct {
	Dockerfile string
	Tags       []string
	BuildArgs  map[string]*string
	Labels     map[string]string
	Target     string
	// NetworkMode "none" gives RUN steps no internet.
	NetworkMode string
	// Registries holds the credentials the CLI sent for pulling base images.
	Registries image.Credentials
	// NoCache builds every step afresh, as --no-cache does; the steps still
	// enter the cache.
	NoCache bool
	// Remote is a context to fetch instead of the uploaded one: a Git
	// repository, a tarball or a Dockerfile at a URL.
	Remote string
}

type stage struct {
	name string
	root string
	// base is the image the stage builds on, empty for scratch; layers are
	// the uncompressed layer tars the stage's steps added on top of it.
	base   string
	layers []string
	// configOnly marks a stage that edits a config alone, as docker commit
	// and import changes do: WORKDIR makes no directory.
	configOnly bool
	// key chains the stage's instructions from its base, keying the build
	// cache.
	key     string
	cfg     image.Config
	args    map[string]string
	shell   []string
	workdir string
}

func (s *stage) env() []string { return s.cfg.Config.Env }

func (s *stage) lookup(k string) (string, bool) {
	for i := len(s.cfg.Config.Env) - 1; i >= 0; i-- {
		if n, v, _ := strings.Cut(s.cfg.Config.Env[i], "="); n == k {
			return v, true
		}
	}
	v, ok := s.args[k]
	return v, ok
}

func (s *stage) setEnv(k, v string) {
	for i, kv := range s.cfg.Config.Env {
		if n, _, _ := strings.Cut(kv, "="); n == k {
			s.cfg.Config.Env[i] = k + "=" + v
			return
		}
	}
	s.cfg.Config.Env = append(s.cfg.Config.Env, k+"="+v)
}

type builder struct {
	e        *Engine
	opts     BuildOptions
	dir      string
	context  string
	stages   []*stage
	progress func(string)
	stack    string
	proxy    int
	steps    int
	step     int
	nlayers  int
	// cache is nil where steps edit a config alone.
	cache *buildCache
}

// Build runs a Dockerfile against the uploaded context, as the classic
// builder does: each stage starts from a clone of its base image, RUN steps
// execute under the stage's Seatbelt profile, each RUN, COPY and ADD adds a
// layer, and the last stage becomes the image.
func (e *Engine) Build(ctx context.Context, opts BuildOptions, buildContext io.Reader, progress func(string)) (image.Image, error) {
	if opts.Dockerfile == "" {
		opts.Dockerfile = "Dockerfile"
	}
	var tags []string
	for _, t := range opts.Tags {
		n, err := image.Normalize(t)
		if err != nil {
			return image.Image{}, invalid("invalid tag %q: %v", t, err)
		}
		tags = append(tags, n)
	}
	if err := os.MkdirAll(e.images.BuildDir(), 0o700); err != nil {
		return image.Image{}, err
	}
	dir, err := os.MkdirTemp(e.images.BuildDir(), "b")
	if err != nil {
		return image.Image{}, err
	}
	defer os.RemoveAll(dir)
	// A prune waits for builds that may still read cached layers.
	e.buildMu.RLock()
	defer e.buildMu.RUnlock()
	b := &builder{e: e, opts: opts, dir: dir, context: filepath.Join(dir, "context"), progress: progress, cache: e.buildCache()}
	if err := os.MkdirAll(b.context, 0o700); err != nil {
		return image.Image{}, err
	}
	if opts.Remote != "" {
		if err := fetchRemoteContext(ctx, opts.Remote, b.context, &b.opts); err != nil {
			return image.Image{}, err
		}
	} else if err := image.Extract(b.context, buildContext, false); err != nil {
		return image.Image{}, invalid("read build context: %v", err)
	}
	dfPath, err := rootfs.Join(b.context, opts.Dockerfile)
	if err != nil {
		return image.Image{}, err
	}
	df, err := os.Open(dfPath)
	if err != nil {
		return image.Image{}, invalid("Cannot locate specified Dockerfile: %s", opts.Dockerfile)
	}
	ins, err := dockerfile.Parse(df)
	df.Close()
	if err != nil {
		return image.Image{}, invalid("dockerfile parse error: %v", err)
	}
	defer b.releaseStack()
	last, err := b.run(ctx, ins)
	if err != nil {
		return image.Image{}, err
	}
	for k, v := range opts.Labels {
		if last.cfg.Config.Labels == nil {
			last.cfg.Config.Labels = map[string]string{}
		}
		last.cfg.Config.Labels[k] = v
	}
	// A build whose every step came from the cache makes the same image.
	last.cfg.Created = v1.Time{Time: time.Now().UTC()}
	if h := last.cfg.History; len(h) > 0 {
		last.cfg.Created = h[len(h)-1].Created
	}
	img, err := e.images.Extend(last.base, last.layers, last.root, last.cfg, tags)
	if err != nil {
		return image.Image{}, err
	}
	short := strings.TrimPrefix(img.ID, "sha256:")[:12]
	progress(fmt.Sprintf("Successfully built %s\n", short))
	for _, t := range tags {
		progress(fmt.Sprintf("Successfully tagged %s\n", t))
	}
	return img, nil
}

func (b *builder) run(ctx context.Context, ins []dockerfile.Instruction) (*stage, error) {
	globals := map[string]string{}
	i := 0
	for ; i < len(ins) && ins[i].Cmd != "FROM"; i++ {
		if ins[i].Cmd != "ARG" {
			return nil, invalid("line %d: %s before the first FROM; only ARG may come first", ins[i].Line, ins[i].Cmd)
		}
		b.progress(stepLine(i+1, len(ins), ins[i]))
		k, v, err := b.arg(ins[i], func(string) (string, bool) { return "", false })
		if err != nil {
			return nil, err
		}
		globals[k] = v
	}
	if i == len(ins) {
		return nil, invalid("the Dockerfile has no FROM instruction")
	}
	// Count the steps of the stages the target needs, for "Step i/N".
	end := len(ins)
	if b.opts.Target != "" {
		found := false
		for j := i; j < len(ins); j++ {
			if ins[j].Cmd != "FROM" {
				continue
			}
			if found {
				end = j
				break
			}
			if _, name := fromParts(ins[j]); strings.EqualFold(name, b.opts.Target) {
				found = true
			}
		}
		if !found {
			return nil, invalid("failed to reach build target %s in Dockerfile", b.opts.Target)
		}
	}
	b.steps = end
	b.step = i
	var cur *stage
	for ; i < end; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		in := ins[i]
		b.step++
		b.progress(stepLine(b.step, b.steps, in))
		if in.Cmd == "FROM" {
			s, err := b.from(ctx, in, globals)
			if err != nil {
				return nil, err
			}
			cur = s
			b.stages = append(b.stages, s)
			if err := b.triggers(ctx, s, globals); err != nil {
				return nil, err
			}
			continue
		}
		if err := b.apply(ctx, cur, in, globals); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// stepLine is the classic builder's "Step i/N : INSTRUCTION args" line.
func stepLine(i, n int, in dockerfile.Instruction) string {
	keys := make([]string, 0, len(in.Flags))
	for k := range in.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var flags strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&flags, "--%s=%s ", k, in.Flags[k])
	}
	return fmt.Sprintf("Step %d/%d : %s %s%s\n", i, n, in.Cmd, flags.String(), in.Raw)
}

func fromParts(in dockerfile.Instruction) (ref, name string) {
	w, _ := dockerfile.Words(in.Raw)
	if len(w) == 3 && strings.EqualFold(w[1], "as") {
		return w[0], w[2]
	}
	if len(w) > 0 {
		return w[0], ""
	}
	return "", ""
}

func (b *builder) arg(in dockerfile.Instruction, lookup func(string) (string, bool)) (string, string, error) {
	if len(in.Args) == 0 {
		return "", "", invalid("line %d: ARG requires a name", in.Line)
	}
	raw, err := dockerfile.Expand(in.Raw, lookup)
	if err != nil {
		return "", "", invalid("line %d: %v", in.Line, err)
	}
	name, def, hasDef := strings.Cut(strings.TrimSpace(raw), "=")
	if v, ok := b.opts.BuildArgs[name]; ok && v != nil {
		return name, *v, nil
	}
	if !hasDef {
		if v, ok := lookup(name); ok {
			return name, v, nil
		}
	}
	return name, strings.Trim(def, `"`), nil
}

func (b *builder) from(ctx context.Context, in dockerfile.Instruction, globals map[string]string) (*stage, error) {
	if p := in.Flags["platform"]; p != "" && p != image.Platform.OS+"/"+image.Platform.Architecture {
		return nil, unsupported("FROM --platform=%s: the engine builds %s/%s images only", p, image.Platform.OS, image.Platform.Architecture)
	}
	ref, name := fromParts(in)
	ref, err := dockerfile.Expand(ref, func(k string) (string, bool) { v, ok := globals[k]; return v, ok })
	if err != nil || ref == "" {
		return nil, invalid("line %d: FROM requires an image", in.Line)
	}
	s := &stage{name: name, root: filepath.Join(b.dir, "s"+strconv.Itoa(len(b.stages))), args: map[string]string{}, shell: []string{"/bin/sh", "-c"}}
	switch {
	case ref == "scratch":
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return nil, err
		}
		s.key = cacheKey("scratch")
	case b.stageNamed(ref) != nil:
		prev := b.stageNamed(ref)
		if err := image.CloneDir(prev.root, s.root); err != nil {
			return nil, err
		}
		s.cfg = image.Config{ConfigFile: *prev.cfg.DeepCopy(), HealthStartInterval: prev.cfg.HealthStartInterval}
		s.base, s.layers = prev.base, append([]string(nil), prev.layers...)
		s.key = prev.key
		s.shell = append([]string(nil), prev.shell...)
		s.workdir = prev.workdir
	default:
		img, err := b.baseImage(ctx, ref)
		if err != nil {
			return nil, err
		}
		if err := b.e.images.Clone(img.ID, s.root); err != nil {
			return nil, err
		}
		s.cfg = image.Config{ConfigFile: *img.Config.DeepCopy(), HealthStartInterval: img.HealthStartInterval}
		s.base = img.ID
		s.key = cacheKey("image", img.ID)
		s.workdir = s.cfg.Config.WorkingDir
		if len(s.cfg.Config.Shell) > 0 {
			s.shell = append([]string(nil), s.cfg.Config.Shell...)
		}
		b.progress(fmt.Sprintf(" ---> %s\n", strings.TrimPrefix(img.ID, "sha256:")[:12]))
	}
	return s, nil
}

// triggers runs the ONBUILD instructions the stage's base image holds, as
// the first steps of the stage, and keeps them out of the stage's image.
func (b *builder) triggers(ctx context.Context, s *stage, globals map[string]string) error {
	triggers := s.cfg.Config.OnBuild
	if len(triggers) == 0 {
		return nil
	}
	s.cfg.Config.OnBuild = nil
	plural := "s"
	if len(triggers) == 1 {
		plural = ""
	}
	b.progress(fmt.Sprintf("# Executing %d build trigger%s\n", len(triggers), plural))
	for _, t := range triggers {
		ins, err := dockerfile.Parse(strings.NewReader(t))
		if err != nil {
			return invalid("ONBUILD trigger %q: %v", t, err)
		}
		for _, in := range ins {
			if err := b.apply(ctx, s, in, globals); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *builder) stageNamed(ref string) *stage {
	for i, s := range b.stages {
		if (s.name != "" && strings.EqualFold(s.name, ref)) || ref == strconv.Itoa(i) {
			return s
		}
	}
	return nil
}

func (b *builder) baseImage(ctx context.Context, ref string) (image.Image, error) {
	img, err := b.e.images.Get(ref)
	if err == nil {
		return img, nil
	}
	b.progress(fmt.Sprintf("Pulling %s\n", ref))
	pulled, err := b.e.images.Pull(ctx, ref, b.opts.Registries.For(ref), func(id, status string) { b.progress(id + ": " + status + "\n") })
	if err != nil {
		return image.Image{}, notFound("pull access denied or image not found for %s: %v", ref, err)
	}
	return pulled.Image, nil
}

func (b *builder) apply(ctx context.Context, s *stage, in dockerfile.Instruction, globals map[string]string) error {
	expand := func(v string) (string, error) {
		out, err := dockerfile.Expand(v, s.lookup)
		if err != nil {
			return "", invalid("line %d: %v", in.Line, err)
		}
		return out, nil
	}
	emptyLayer := true
	// key names the stage after this instruction; a step that changes files
	// folds what it changed into it.
	key := cacheKey(s.key, in.Cmd, flagsKey(in.Flags), in.Raw)
	record := func() error {
		created := time.Now().UTC()
		if b.cache != nil {
			e, ok := b.cached(key)
			if !ok && emptyLayer {
				var err error
				if e, err = b.cache.put(key, created, ""); err != nil {
					return err
				}
			}
			created = e.Created
		}
		s.key = key
		s.cfg.History = append(s.cfg.History, v1.History{Created: v1.Time{Time: created}, CreatedBy: in.Cmd + " " + in.Raw, EmptyLayer: emptyLayer})
		return nil
	}
	cfg := &s.cfg.Config
	switch in.Cmd {
	case "ARG":
		k, v, err := b.arg(in, func(k string) (string, bool) {
			if v, ok := s.args[k]; ok {
				return v, ok
			}
			v, ok := globals[k]
			return v, ok
		})
		if err != nil {
			return err
		}
		s.args[k] = v
		s.key = cacheKey(key, k, v)
		return nil
	case "ENV":
		raw, err := expand(in.Raw)
		if err != nil {
			return err
		}
		kvs, err := dockerfile.KeyValues(raw, true)
		if err != nil {
			return invalid("line %d: ENV %v", in.Line, err)
		}
		for _, kv := range kvs {
			s.setEnv(kv[0], kv[1])
		}
	case "LABEL":
		raw, err := expand(in.Raw)
		if err != nil {
			return err
		}
		kvs, err := dockerfile.KeyValues(raw, false)
		if err != nil {
			return invalid("line %d: LABEL %v", in.Line, err)
		}
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, kv := range kvs {
			cfg.Labels[kv[0]] = kv[1]
		}
	case "MAINTAINER":
		s.cfg.Author = in.Raw
	case "WORKDIR":
		wd, err := expand(in.Raw)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(wd, "/") {
			wd = path.Join("/", s.workdir, wd)
		}
		s.workdir = path.Clean(wd)
		cfg.WorkingDir = s.workdir
		if s.configOnly {
			break
		}
		host, err := rootfs.Join(s.root, s.workdir)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(host); errors.Is(err, os.ErrNotExist) {
			emptyLayer = false
			if key, err = b.contentStep(ctx, s, key, func() error { return os.MkdirAll(host, 0o755) }); err != nil {
				return err
			}
		}
	case "USER":
		u, err := expand(in.Raw)
		if err != nil {
			return err
		}
		if err := b.e.checkUser(u); err != nil {
			return err
		}
		cfg.User = u
	case "EXPOSE":
		raw, err := expand(in.Raw)
		if err != nil {
			return err
		}
		words, err := dockerfile.Words(raw)
		if err != nil {
			return invalid("line %d: %v", in.Line, err)
		}
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, w := range words {
			if !strings.Contains(w, "/") {
				w += "/tcp"
			}
			cfg.ExposedPorts[w] = struct{}{}
		}
	case "VOLUME":
		vols := in.Args
		if !in.JSON {
			raw, err := expand(in.Raw)
			if err != nil {
				return err
			}
			if vols, err = dockerfile.Words(raw); err != nil {
				return invalid("line %d: %v", in.Line, err)
			}
		}
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]struct{}{}
		}
		for _, v := range vols {
			cfg.Volumes[v] = struct{}{}
		}
	case "STOPSIGNAL":
		sig, err := expand(in.Raw)
		if err != nil {
			return err
		}
		if _, err := parseSignal(sig); err != nil {
			return err
		}
		cfg.StopSignal = sig
	case "SHELL":
		if !in.JSON || len(in.Args) == 0 {
			return invalid("line %d: SHELL requires the arguments to be in JSON form", in.Line)
		}
		s.shell = append([]string(nil), in.Args...)
		cfg.Shell = s.shell
	case "CMD":
		cfg.Cmd = b.command(s, in)
	case "ENTRYPOINT":
		cfg.Entrypoint = b.command(s, in)
		// Docker drops an inherited CMD when a stage sets ENTRYPOINT.
		cfg.Cmd = nil
	case "HEALTHCHECK":
		hc, start, err := healthcheck(s, in)
		if err != nil {
			return err
		}
		cfg.Healthcheck = hc
		s.cfg.HealthStartInterval = start
	case "COPY", "ADD":
		emptyLayer = false
		var err error
		if key, err = b.contentStep(ctx, s, key, func() error { return b.copy(ctx, s, in, expand) }); err != nil {
			return err
		}
	case "RUN":
		emptyLayer = false
		key = cacheKey(key, argsKey(s.args))
		if err := b.cachedRun(ctx, s, key, in); err != nil {
			return err
		}
	case "ONBUILD":
		trigger, err := dockerfile.Parse(strings.NewReader(in.Raw))
		if err != nil || len(trigger) != 1 {
			return invalid("line %d: ONBUILD needs one instruction", in.Line)
		}
		switch trigger[0].Cmd {
		case "ONBUILD", "FROM", "MAINTAINER":
			return invalid("line %d: %s isn't allowed as an ONBUILD trigger", in.Line, trigger[0].Cmd)
		}
		cfg.OnBuild = append(cfg.OnBuild, in.Raw)
	default:
		return invalid("line %d: unknown instruction: %s", in.Line, in.Cmd)
	}
	return record()
}

func (b *builder) command(s *stage, in dockerfile.Instruction) []string {
	if in.JSON {
		return append([]string(nil), in.Args...)
	}
	if len(in.Args) == 0 {
		return nil
	}
	return append(append([]string(nil), s.shell...), in.Raw)
}

func healthcheck(s *stage, in dockerfile.Instruction) (*v1.HealthConfig, time.Duration, error) {
	hc := &v1.HealthConfig{}
	var start time.Duration
	for k, v := range in.Flags {
		switch k {
		case "retries":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, 0, invalid("line %d: --retries: %v", in.Line, err)
			}
			hc.Retries = n
		case "interval", "timeout", "start-period", "start-interval":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, 0, invalid("line %d: --%s: %v", in.Line, k, err)
			}
			switch k {
			case "interval":
				hc.Interval = d
			case "timeout":
				hc.Timeout = d
			case "start-interval":
				start = d
			default:
				hc.StartPeriod = d
			}
		default:
			return nil, 0, invalid("line %d: unknown HEALTHCHECK flag --%s", in.Line, k)
		}
	}
	kind, rest, _ := strings.Cut(in.Raw, " ")
	switch strings.ToUpper(kind) {
	case "NONE":
		hc.Test = []string{"NONE"}
	case "CMD":
		rest = strings.TrimSpace(rest)
		var arr []string
		if strings.HasPrefix(rest, "[") {
			if ins, err := dockerfile.Parse(strings.NewReader("CMD " + rest)); err == nil && ins[0].JSON {
				arr = ins[0].Args
			}
		}
		if arr != nil {
			hc.Test = append([]string{"CMD"}, arr...)
		} else {
			hc.Test = []string{"CMD-SHELL", rest}
		}
	default:
		return nil, 0, invalid("line %d: HEALTHCHECK must be NONE or CMD", in.Line)
	}
	return hc, start, nil
}

func (b *builder) copy(ctx context.Context, s *stage, in dockerfile.Instruction, expand func(string) (string, error)) error {
	if ch := in.Flags["chown"]; ch != "" {
		user, _, _ := strings.Cut(ch, ":")
		if err := b.e.checkUser(user); err != nil {
			return err
		}
	}
	var mode os.FileMode
	if m := in.Flags["chmod"]; m != "" {
		n, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			return invalid("line %d: --chmod=%s: %v", in.Line, m, err)
		}
		mode = os.FileMode(n)
	}
	args := in.Args
	if !in.JSON {
		raw, err := expand(in.Raw)
		if err != nil {
			return err
		}
		if args, err = dockerfile.Words(raw); err != nil {
			return invalid("line %d: %v", in.Line, err)
		}
	}
	if len(args) < 2 {
		return invalid("line %d: %s requires at least two arguments", in.Line, in.Cmd)
	}
	srcs, dest := args[:len(args)-1], args[len(args)-1]
	if !strings.HasPrefix(dest, "/") {
		dest = path.Join("/", s.workdir, dest)
		if strings.HasSuffix(args[len(args)-1], "/") {
			dest += "/"
		}
	}
	destIsDir := strings.HasSuffix(dest, "/") || len(srcs) > 1
	destHost, err := rootfs.Join(s.root, dest)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(destHost); err == nil && fi.IsDir() {
		destIsDir = true
	}

	srcRoot := b.context
	if from := in.Flags["from"]; from != "" {
		if in.Cmd == "ADD" {
			return invalid("line %d: ADD does not take --from", in.Line)
		}
		if st := b.stageNamed(from); st != nil {
			srcRoot = st.root
		} else {
			img, err := b.baseImage(ctx, from)
			if err != nil {
				return err
			}
			srcRoot = b.e.images.Root(img.ID)
		}
	}
	for _, src := range srcs {
		if in.Cmd == "ADD" && (strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")) {
			if err := b.download(ctx, src, destHost, destIsDir); err != nil {
				return invalid("line %d: ADD %s: %v", in.Line, src, err)
			}
			continue
		}
		pattern, err := rootfs.Join(srcRoot, src)
		if err != nil {
			return err
		}
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			return notFound("%s failed: file not found in build context or excluded by .dockerignore: stat %s: file does not exist", in.Cmd, src)
		}
		for _, m := range matches {
			if !within(m, srcRoot) {
				return invalid("%s failed: %s is outside the build context", in.Cmd, src)
			}
			if in.Cmd == "ADD" && isArchive(m) {
				if err := os.MkdirAll(destHost, 0o755); err != nil {
					return err
				}
				f, err := os.Open(m)
				if err != nil {
					return err
				}
				err = image.Extract(destHost, f, false)
				f.Close()
				if err != nil {
					return invalid("line %d: ADD %s: %v", in.Line, src, err)
				}
				continue
			}
			target := destHost
			fi, err := os.Lstat(m)
			if err != nil {
				return err
			}
			if destIsDir && !fi.IsDir() {
				target = filepath.Join(destHost, filepath.Base(m))
			}
			if err := copyInto(m, target, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// cached returns the cache's entry for key unless the build skips the cache.
func (b *builder) cached(key string) (cacheEntry, bool) {
	if b.opts.NoCache {
		return cacheEntry{}, false
	}
	return b.cache.get(key)
}

// change runs a step that changes the stage's files and returns an APFS
// clone of the root from before the step and the layer it made: the
// difference between that clone and the root after it.
func (b *builder) change(ctx context.Context, s *stage, step func() error) (before, layer string, err error) {
	n := b.nlayers
	b.nlayers++
	before = filepath.Join(b.dir, "before"+strconv.Itoa(n))
	if err := image.CloneDir(s.root, before); err != nil {
		return "", "", err
	}
	if err := step(); err != nil {
		os.RemoveAll(before)
		return "", "", err
	}
	layer = filepath.Join(b.dir, "layer"+strconv.Itoa(n)+".tar")
	f, err := os.Create(layer)
	if err != nil {
		os.RemoveAll(before)
		return "", "", err
	}
	err = image.Diff(ctx, before, s.root, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.RemoveAll(before)
		return "", "", err
	}
	return before, layer, nil
}

// contentStep runs a cheap step, such as COPY, and keys it by what it
// changed. When the cache holds that change, the stage takes the cached
// layer, byte for byte, so the image comes out the same; it returns the
// step's key.
func (b *builder) contentStep(ctx context.Context, s *stage, key string, step func() error) (string, error) {
	before, layer, err := b.change(ctx, s, step)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(before)
	ck, err := contentKey(layer)
	if err != nil {
		return "", err
	}
	key = cacheKey(key, ck)
	if e, ok := b.cached(key); ok && e.Layer != "" {
		if err := os.RemoveAll(s.root); err != nil {
			return "", err
		}
		if err := os.Rename(before, s.root); err != nil {
			return "", err
		}
		if err := b.applyCached(s, e); err != nil {
			return "", err
		}
		os.Remove(layer)
		return key, nil
	}
	e, err := b.cache.put(key, time.Now().UTC(), layer)
	if err != nil {
		return "", err
	}
	s.layers = append(s.layers, b.cache.layerPath(e))
	return key, nil
}

// cachedRun runs a RUN step unless the cache holds its key, whose layer then
// takes its place.
func (b *builder) cachedRun(ctx context.Context, s *stage, key string, in dockerfile.Instruction) error {
	if e, ok := b.cached(key); ok && e.Layer != "" {
		return b.applyCached(s, e)
	}
	before, layer, err := b.change(ctx, s, func() error { return b.runStep(ctx, s, in) })
	if err != nil {
		return err
	}
	os.RemoveAll(before)
	e, err := b.cache.put(key, time.Now().UTC(), layer)
	if err != nil {
		return err
	}
	s.layers = append(s.layers, b.cache.layerPath(e))
	return nil
}

// applyCached puts a cached layer on the stage's root and in its layers.
func (b *builder) applyCached(s *stage, e cacheEntry) error {
	f, err := os.Open(b.cache.layerPath(e))
	if err != nil {
		return err
	}
	err = image.ApplyLayer(s.root, f)
	f.Close()
	if err != nil {
		return err
	}
	s.layers = append(s.layers, b.cache.layerPath(e))
	b.progress(" ---> Using cache\n")
	return nil
}

// copyInto copies a file, symlink or directory's contents to dst, applying
// mode when it is non-zero.
func copyInto(src, dst string, mode os.FileMode) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		perm := fi.Mode().Perm()
		if mode != 0 {
			perm = mode
		}
		switch {
		case fi.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return os.Chmod(target, perm|0o700)
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(l, target)
		case fi.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			os.Remove(target)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			return os.Chmod(target, perm)
		}
		return nil
	})
}

// isArchive reports whether ADD unpacks the file: a tar, plain or gzipped.
func isArchive(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return false
		}
		defer gz.Close()
		r = gz
	}
	_, err = tar.NewReader(r).Next()
	return err == nil
}

// download fetches an ADD URL from the engine, as dockerd fetches it, into
// a file with mode 0600.
func (b *builder) download(ctx context.Context, src, dest string, destIsDir bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("%s", res.Status)
	}
	if destIsDir {
		u, _ := url.Parse(src)
		name := path.Base(u.Path)
		if name == "/" || name == "." || name == "" {
			return errors.New("cannot determine a file name for the download")
		}
		dest = filepath.Join(dest, name)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ensureStack gives the build a stack of its own, so RUN steps reach the
// internet through an egress proxy no other stack shares.
func (b *builder) ensureStack() (int, error) {
	if b.opts.NetworkMode == "none" || !b.e.cfg.Egress {
		return 0, nil
	}
	if b.stack == "" {
		b.stack = "build-" + strings.ToLower(filepath.Base(b.dir))
		if _, err := b.e.alloc.Stack(b.stack); err != nil {
			return 0, err
		}
		port, err := b.e.ensureEgress(b.stack)
		if err != nil {
			return 0, err
		}
		b.proxy = port
	}
	return b.proxy, nil
}

func (b *builder) releaseStack() {
	if b.stack != "" {
		b.e.closeEgress(b.stack)
		b.e.alloc.Release(b.stack)
	}
}

func (b *builder) runStep(ctx context.Context, s *stage, in dockerfile.Instruction) error {
	argv := in.Args
	if !in.JSON {
		argv = append(append([]string(nil), s.shell...), in.Raw)
	}
	if len(argv) == 0 {
		return invalid("line %d: RUN requires a command", in.Line)
	}
	proxy, err := b.ensureStack()
	if err != nil {
		return err
	}
	id, err := identity.Resolve(s.root, s.cfg.Config.User)
	if err != nil {
		return invalid("line %d: %v", in.Line, err)
	}
	lib, err := b.e.Library()
	if err != nil {
		return err
	}
	env := append(b.runEnv(s, proxy), id.Env()...)
	spec := seatbelt.Spec{Root: s.root, StateRoot: b.e.cfg.Root, IPCPrefix: seatbelt.IPCPrefixFor(s.root), Libraries: []string{lib}}
	if proxy != 0 {
		spec.ConnectPorts = []int{proxy}
	}
	w := progressWriter{progress: b.progress}
	proc, err := runtime.Start(runtime.Spec{
		Root: s.root, Argv: argv, Env: env, WorkDir: s.workdir,
		Profile: spec, ProfilePath: filepath.Join(b.dir, "run-"+strconv.Itoa(b.step)+".sb"),
		Stdout: w, Stderr: w, Library: lib,
	})
	if err != nil {
		return invalid("line %d: RUN %s: %v", in.Line, in.Raw, err)
	}
	select {
	case <-proc.Done():
	case <-ctx.Done():
		proc.KillAll()
		<-proc.Done()
	}
	proc.KillAll()
	if err := procs.KillTagged("SANDBOX_ROOT=" + s.root); err != nil {
		return err
	}
	proc.DrainIO(outputDrain)
	if err := ctx.Err(); err != nil {
		return err
	}
	if code := proc.ExitCode(); code != 0 {
		return invalid("The command '%s' returned a non-zero code: %d", strings.Join(argv, " "), code)
	}
	return nil
}

// runEnv is a RUN step's environment: the stage's variables and build
// arguments, PATH mapped into the root, and the SANDBOX_* variables.
func (b *builder) runEnv(s *stage, proxy int) []string {
	vars := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	for k, v := range s.args {
		set(k, v)
	}
	for _, kv := range s.env() {
		k, v, _ := strings.Cut(kv, "=")
		set(k, v)
	}
	var mapped []string
	for _, d := range filepath.SplitList(vars["PATH"]) {
		if strings.HasPrefix(d, "/") {
			mapped = append(mapped, filepath.Join(s.root, d))
		}
	}
	vars["PATH"] = strings.Join(mapped, ":")
	home := filepath.Join(s.root, "root")
	os.MkdirAll(home, 0o755)
	set("HOME", home)
	set("SANDBOX_ROOT", s.root)
	set("SANDBOX_IPC_PREFIX", seatbelt.IPCPrefixFor(s.root))
	set("SANDBOX_HOST", "127.0.0.1")
	if proxy != 0 {
		for _, kv := range egress.Variables(proxy) {
			if _, ok := vars[kv[0]]; !ok {
				set(kv[0], kv[1])
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vars[k])
	}
	return out
}

type progressWriter struct{ progress func(string) }

func (w progressWriter) Write(p []byte) (int, error) {
	w.progress(string(p))
	return len(p), nil
}
