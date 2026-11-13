package runtime

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// A container's execs run in its sandbox, as docker exec runs in the
// container's namespaces: Seatbelt lets a process signal and inspect only
// processes of its own sandbox, the ones that inherited it, and a process
// that entered the container's profile by itself would be a sandbox of its
// own. So the spawn helper of the container's main process, once in the
// sandbox, starts an anchor: a process of the helper's binary, outside the
// main process's tree, that starts each exec on the engine's request.
const (
	// anchorFD is the listening socket the anchor accepts requests on, in
	// the spawn helper and in the anchor.
	anchorFD = 4
	// initVar holds the main process's host pid, which the container
	// library presents as pid 1.
	initVar = "FWS_INIT"
)

type anchorRequest struct {
	Path string
	Argv []string
	Env  []string
	Dir  string
	TTY  bool
}

type anchorReply struct {
	Pid   int    `json:",omitempty"`
	Code  *int   `json:",omitempty"`
	Error string `json:",omitempty"`
}

func init() {
	switch os.Getenv(RoleVar) {
	case anchorLaunchRole:
		os.Exit(launchAnchor())
	case anchorRole:
		os.Exit(serveAnchor())
	}
}

// startAnchor starts the anchor from the sandboxed spawn helper, through a
// launcher that exits at once, so the anchor is no child of the main
// process the helper becomes: a shell's wait would wait for it. The
// launcher is the engine's C program where it built one, which starts in
// a millisecond, or else this binary, whose Go runtime takes several.
func startAnchor(launcher string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	// The C launcher, where the engine built one, starts the anchor itself;
	// otherwise this binary does, in the launcher's role.
	path, argv, role := self, []string{"fws-anchor", strconv.Itoa(os.Getpid())}, anchorLaunchRole
	if launcher != "" {
		path, argv, role = launcher, []string{"fws-launch", self, strconv.Itoa(os.Getpid())}, anchorRole
	}
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Env:   []string{RoleVar + "=" + role},
		Files: []uintptr{null.Fd(), null.Fd(), null.Fd(), anchorFD},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return fmt.Errorf("start the exec anchor: %w", err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("start the exec anchor: %w", err)
	}
	if !ws.Exited() || ws.ExitStatus() != 0 {
		return errors.New("start the exec anchor: its launcher failed")
	}
	return syscall.Close(anchorFD)
}

func launchAnchor() int {
	self, err := os.Executable()
	if err != nil || len(os.Args) < 2 {
		return 1
	}
	_, err = syscall.ForkExec(self, []string{"fws-anchor", os.Args[1]}, &syscall.ProcAttr{
		Env:   []string{RoleVar + "=" + anchorRole},
		Files: []uintptr{0, 1, 2, 3},
	})
	if err != nil {
		return 1
	}
	return 0
}

// serveAnchor starts the execs the engine asks for until the container's
// main process, whose pid it was started with, exits.
func serveAnchor() int {
	main, err := strconv.Atoi(os.Args[len(os.Args)-1])
	if err != nil {
		return 1
	}
	l, err := net.FileListener(os.NewFile(3, "anchor"))
	if err != nil {
		return 1
	}
	go func() {
		awaitExit(main)
		os.Exit(0)
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			return 0
		}
		go serveExec(c.(*net.UnixConn), main)
	}
}

func awaitExit(pid int) {
	kq, err := unix.Kqueue()
	if err != nil {
		return
	}
	ev := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		return
	}
	out := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, out, nil)
		if n > 0 || (err != nil && !errors.Is(err, syscall.EINTR)) {
			return
		}
	}
}

// A request is an 8-byte length, sent with the process's standard streams
// as rights, then that many bytes of JSON.
func serveExec(c *net.UnixConn, main int) {
	defer c.Close()
	enc := json.NewEncoder(c)
	hdr := make([]byte, 8)
	oob := make([]byte, unix.CmsgSpace(3*4))
	n, oobn, _, _, err := c.ReadMsgUnix(hdr, oob)
	if err != nil {
		return
	}
	var files []*os.File
	if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
		for _, m := range msgs {
			fds, err := unix.ParseUnixRights(&m)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "stdio"))
			}
		}
	}
	defer closeAll(files)
	if _, err := io.ReadFull(c, hdr[n:]); err != nil {
		return
	}
	body := make([]byte, binary.BigEndian.Uint64(hdr))
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	var req anchorRequest
	if err := json.Unmarshal(body, &req); err != nil || len(files) != 3 {
		enc.Encode(anchorReply{Error: "exec anchor: a malformed request"})
		return
	}
	env := append(withoutVars(req.Env, initVar), initVar+"="+strconv.Itoa(main))
	p, err := os.StartProcess(req.Path, req.Argv, &os.ProcAttr{
		Dir: req.Dir, Env: env, Files: files,
		// A terminal's process leads a session with the terminal as its
		// controlling one; any other leads its own process group, as the
		// spawn helper's processes do.
		Sys: &syscall.SysProcAttr{Setsid: req.TTY, Setctty: req.TTY, Setpgid: !req.TTY},
	})
	closeAll(files)
	files = nil
	if err != nil {
		enc.Encode(anchorReply{Error: err.Error()})
		return
	}
	if err := enc.Encode(anchorReply{Pid: p.Pid}); err != nil {
		p.Kill()
	}
	code := 255
	if st, err := p.Wait(); err == nil {
		code = exitCode(st.Sys().(syscall.WaitStatus))
	}
	enc.Encode(anchorReply{Code: &code})
}

// startInAnchor asks the anchor at sock to start the process with stdio as
// its standard streams, and returns its pid and the connection its exit
// code arrives on.
func startInAnchor(sock string, req anchorRequest, stdio [3]*os.File) (int, *json.Decoder, io.Closer, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("reach the container's exec anchor: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		c.Close()
		return 0, nil, nil, err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint64(hdr, uint64(len(body)))
	rights := unix.UnixRights(int(stdio[0].Fd()), int(stdio[1].Fd()), int(stdio[2].Fd()))
	if _, _, err := c.WriteMsgUnix(hdr, rights, nil); err != nil {
		c.Close()
		return 0, nil, nil, err
	}
	if _, err := c.Write(body); err != nil {
		c.Close()
		return 0, nil, nil, err
	}
	dec := json.NewDecoder(c)
	var r anchorReply
	if err := dec.Decode(&r); err != nil {
		c.Close()
		return 0, nil, nil, fmt.Errorf("the container's exec anchor: %w", err)
	}
	if r.Error != "" {
		c.Close()
		return 0, nil, nil, errors.New(r.Error)
	}
	return r.Pid, dec, c, nil
}
