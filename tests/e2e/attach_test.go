package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// attachWS opens a websocket attach to a container, as the API's websocket
// clients do.
func attachWS(t *testing.T, version, name string, q url.Values) *websocket.Conn {
	t.Helper()
	cfg, err := websocket.NewConfig("ws://engine/"+version+"/containers/"+name+"/attach/ws?"+q.Encode(), "http://engine")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", strings.TrimPrefix(dockerHost, "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// receiveUntil reads the websocket's messages until their text holds want,
// and returns the frame type of the last.
func receiveUntil(t *testing.T, ws *websocket.Conn, want string) string {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(time.Minute))
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		var msg []byte
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			t.Fatalf("waiting for %q in %q: %v", want, got.String(), err)
		}
		got.Write(msg)
	}
	return got.String()
}

// receiveEnd reads the websocket until the engine closes it.
func receiveEnd(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(time.Minute))
	var got strings.Builder
	for {
		var msg []byte
		err := websocket.Message.Receive(ws, &msg)
		got.Write(msg)
		if err == io.EOF {
			return got.String()
		}
		if err != nil {
			t.Fatalf("waiting for the attach to end after %q: %v", got.String(), err)
		}
	}
}

func running(t *testing.T, name string) bool {
	t.Helper()
	return strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Running}}", name)) == "true"
}

// eventsSince lists the container's events of one action since a time.
func eventsSince(t *testing.T, name, action string, since time.Time) string {
	t.Helper()
	return mustDocker(t, "events", "--since", since.Format(time.RFC3339Nano), "--until", time.Now().Add(time.Second).Format(time.RFC3339Nano),
		"--filter", "container="+name, "--filter", "event="+action, "--format", "{{.Action}}")
}

// A websocket attach carries the container's input and its unframed
// output, and ends when the container does.
func TestWebsocketAttach(t *testing.T) {
	const name = "e2e-ws-attach"
	docker(t, "rm", "-f", name)
	defer docker(t, "rm", "-f", name)
	mustDocker(t, "run", "-d", "-i", "--name", name, "fws/python:3.14", "python3", "-c",
		`import sys; line = sys.stdin.readline(); print("got", line.strip(), flush=True); print("on stderr", file=sys.stderr, flush=True)`)
	ws := attachWS(t, "v1.47", name, url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}})
	if _, err := ws.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	out := receiveEnd(t, ws)
	if !strings.Contains(out, "got hello\n") || !strings.Contains(out, "on stderr\n") {
		t.Fatalf("the websocket carried %q", out)
	}
	if strings.ContainsRune(out, 1) || strings.ContainsRune(out, 2) {
		t.Fatalf("the websocket carried multiplexed frames: %q", out)
	}
	if code := strings.TrimSpace(mustDocker(t, "wait", name)); code != "0" {
		t.Fatalf("the container exited with %s", code)
	}
}

// From API 1.42 a websocket attach takes only the streams it names.
func TestWebsocketAttachStreams(t *testing.T) {
	const name = "e2e-ws-streams"
	docker(t, "rm", "-f", name)
	defer docker(t, "rm", "-f", name)
	mustDocker(t, "run", "-d", "-i", "--name", name, "fws/python:3.14", "python3", "-c",
		`import sys; sys.stdin.readline(); print("to stderr", file=sys.stderr, flush=True); print("to stdout", flush=True)`)
	ws := attachWS(t, "v1.47", name, url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}})
	ws.Write([]byte("go\n"))
	if out := receiveEnd(t, ws); out != "to stdout\n" {
		t.Fatalf("a websocket attach to stdout carried %q", out)
	}
}

// The detach keys end an attach to a container's terminal and leave it
// running.
func TestDetachKeys(t *testing.T) {
	const name = "e2e-detach"
	docker(t, "rm", "-f", name)
	defer docker(t, "rm", "-f", name)
	mustDocker(t, "run", "-d", "-i", "-t", "--name", name, "fws/python:3.14", "python3", "-c",
		`import sys
for line in sys.stdin:
    print("echo:", line.strip(), flush=True)`)

	res, err := engineClient().Get("http://engine/v1.47/containers/" + name + "/attach/ws?stream=1&stdin=1&stdout=1&detachKeys=ctrl-A,a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Invalid detach keys (ctrl-A,a) provided") {
		t.Fatalf("malformed detach keys: %d %s", res.StatusCode, body)
	}

	since := time.Now()
	ws := attachWS(t, "v1.47", name, url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "detachKeys": {"ctrl-x,x"}})
	ws.Write([]byte("one\r"))
	receiveUntil(t, ws, "echo: one")
	// A detach key followed by another key reaches the container.
	ws.Write([]byte{24})
	ws.Write([]byte("y\r"))
	receiveUntil(t, ws, "echo: \x18y")
	ws.Write([]byte{24})
	ws.Write([]byte("x"))
	receiveEnd(t, ws)
	if !running(t, name) {
		t.Fatal("the container stopped when its client detached")
	}

	// Without keys of its own, a client detaches with ctrl-p ctrl-q, sent
	// here in one write.
	ws = attachWS(t, "v1.47", name, url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}})
	ws.Write([]byte("two\r"))
	receiveUntil(t, ws, "echo: two")
	ws.Write([]byte{16, 17})
	receiveEnd(t, ws)
	if !running(t, name) {
		t.Fatal("the container stopped when its client detached")
	}
	if got := strings.Fields(eventsSince(t, name, "detach", since)); len(got) != 2 {
		t.Fatalf("detach events %q, want two", got)
	}
}

// An exec's detach keys end its client's attach and leave it running.
func TestExecDetachKeys(t *testing.T) {
	const name = "e2e-exec-detach"
	docker(t, "rm", "-f", name)
	defer docker(t, "rm", "-f", name)
	mustDocker(t, "run", "-d", "--name", name, "fws/python:3.14", "python3", "-c", "import time; time.sleep(3600)")

	post := func(path string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		res, err := engineClient().Post("http://engine/v1.47"+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post("/containers/"+name+"/exec", map[string]any{"Cmd": []string{"true"}, "DetachKeys": "ctrl-A,a"})
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "Invalid escape keys (ctrl-A,a) provided") {
		t.Fatalf("malformed escape keys: %d %s", res.StatusCode, body)
	}

	res = post("/containers/"+name+"/exec", map[string]any{
		"AttachStdin": true, "AttachStdout": true, "Tty": true, "DetachKeys": "ctrl-x,x",
		"Cmd": []string{"python3", "-c", "import sys\nfor line in sys.stdin:\n    print('exec:', line.strip(), flush=True)"},
	})
	var created struct{ Id string }
	json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if created.Id == "" {
		t.Fatalf("exec create: %d", res.StatusCode)
	}
	var inspect struct {
		Running    bool
		DetachKeys []byte
	}
	execInspect := func() {
		t.Helper()
		res, err := engineClient().Get("http://engine/v1.47/exec/" + created.Id + "/json")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(&inspect)
	}
	execInspect()
	if !bytes.Equal(inspect.DetachKeys, []byte{24, 'x'}) {
		t.Fatalf("exec inspect's detach keys %v", inspect.DetachKeys)
	}

	since := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(dockerHost, "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Minute))
	io.WriteString(conn, "POST /v1.47/exec/"+created.Id+"/start HTTP/1.1\r\nHost: engine\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 13\r\n\r\n{\"Tty\": true}")
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("exec start: %s", resp.Status)
	}
	io.WriteString(conn, "hi\r")
	var out []byte
	for !bytes.Contains(out, []byte("exec: hi")) {
		b := make([]byte, 4096)
		n, err := rd.Read(b)
		out = append(out, b[:n]...)
		if err != nil {
			t.Fatalf("waiting for the exec's echo in %q: %v", out, err)
		}
	}
	conn.Write([]byte{24, 'x'})
	if rest, err := io.ReadAll(rd); err != nil {
		t.Fatalf("after the detach keys, %q: %v", rest, err)
	}
	execInspect()
	if !inspect.Running {
		t.Fatal("the exec stopped when its client detached")
	}
	if got := strings.TrimSpace(eventsSince(t, name, "exec_detach", since)); got != "exec_detach" {
		t.Fatalf("exec_detach events %q", got)
	}
}
