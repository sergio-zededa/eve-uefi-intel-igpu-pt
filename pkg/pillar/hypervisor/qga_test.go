// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeGuestAgent emulates qemu-ga behind a unix socket: it answers
// guest-sync-delimited with the delimiter plus the echoed id and every other
// command from the replies table. Commands are recorded for assertions.
type fakeGuestAgent struct {
	t        *testing.T
	listener net.Listener
	replies  map[string]string // command -> raw JSON reply (without newline)
	commands chan string
	// leadingGarbage is written before the first reply to emulate stale
	// bytes from a previous session in the agent's output buffer.
	leadingGarbage string
}

func newFakeGuestAgent(t *testing.T, replies map[string]string, garbage string) *fakeGuestAgent {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qga")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeGuestAgent{t: t, listener: l, replies: replies,
		commands: make(chan string, 16), leadingGarbage: garbage}
	go f.serve()
	t.Cleanup(func() { l.Close() })
	return f
}

func (f *fakeGuestAgent) socket() string { return f.listener.Addr().String() }

func (f *fakeGuestAgent) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeGuestAgent) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	if f.leadingGarbage != "" {
		conn.Write([]byte(f.leadingGarbage))
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		// The sync request is prefixed by the 0xFF delimiter.
		if len(line) > 0 && line[0] == qgaSyncDelimiter {
			line = line[1:]
		}
		var cmd struct {
			Execute   string `json:"execute"`
			Arguments struct {
				ID int64 `json:"id"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(line, &cmd); err != nil {
			f.t.Errorf("fake agent got malformed command %q: %v", line, err)
			return
		}
		f.commands <- cmd.Execute
		if cmd.Execute == "guest-sync-delimited" {
			conn.Write([]byte{qgaSyncDelimiter})
			reply, _ := json.Marshal(map[string]int64{"return": cmd.Arguments.ID})
			conn.Write(append(reply, '\n'))
			continue
		}
		reply, ok := f.replies[cmd.Execute]
		if !ok {
			reply = `{"error": {"class": "CommandNotFound", "desc": "The command ` + cmd.Execute + ` has not been found"}}`
		}
		conn.Write([]byte(reply + "\n"))
	}
}

func (f *fakeGuestAgent) expectCommands(want ...string) {
	f.t.Helper()
	for _, w := range want {
		select {
		case got := <-f.commands:
			if got != w {
				f.t.Errorf("agent received %q, want %q", got, w)
			}
		case <-time.After(2 * time.Second):
			f.t.Errorf("agent did not receive %q", w)
		}
	}
}

func TestQgaFreezeThaw(t *testing.T) {
	agent := newFakeGuestAgent(t, map[string]string{
		"guest-fsfreeze-status": `{"return": "thawed"}`,
		"guest-fsfreeze-freeze": `{"return": 3}`,
		"guest-fsfreeze-thaw":   `{"return": 3}`,
	}, "")

	status, err := QgaFsFreezeStatus(agent.socket())
	if err != nil {
		t.Fatalf("QgaFsFreezeStatus: %v", err)
	}
	if status != QgaFsFreezeStatusThawed {
		t.Errorf("status = %q, want thawed", status)
	}
	agent.expectCommands("guest-sync-delimited", "guest-fsfreeze-status")

	frozen, err := QgaFsFreeze(agent.socket())
	if err != nil {
		t.Fatalf("QgaFsFreeze: %v", err)
	}
	if frozen != 3 {
		t.Errorf("frozen = %d, want 3", frozen)
	}
	agent.expectCommands("guest-sync-delimited", "guest-fsfreeze-freeze")

	thawed, err := QgaFsThaw(agent.socket())
	if err != nil {
		t.Fatalf("QgaFsThaw: %v", err)
	}
	if thawed != 3 {
		t.Errorf("thawed = %d, want 3", thawed)
	}
	agent.expectCommands("guest-sync-delimited", "guest-fsfreeze-thaw")
}

// TestQgaSyncSkipsStaleOutput checks that leftovers from a previous session in
// the agent's output do not confuse the client: the sync exchange must skip
// them and the following command must still get its own reply.
func TestQgaSyncSkipsStaleOutput(t *testing.T) {
	agent := newFakeGuestAgent(t, map[string]string{
		"guest-ping": `{"return": {}}`,
	}, "{\"return\": 42}\nnot even json\n")
	if err := QgaPing(agent.socket()); err != nil {
		t.Fatalf("QgaPing: %v", err)
	}
	agent.expectCommands("guest-sync-delimited", "guest-ping")
}

func TestQgaAgentError(t *testing.T) {
	agent := newFakeGuestAgent(t, map[string]string{
		"guest-fsfreeze-freeze": `{"error": {"class": "GenericError", "desc": "VSS failed"}}`,
	}, "")
	_, err := QgaFsFreeze(agent.socket())
	if err == nil {
		t.Fatal("QgaFsFreeze succeeded, want error from agent")
	}
	if errors.Is(err, ErrQgaNotResponding) {
		t.Errorf("agent error misreported as not responding: %v", err)
	}
}

// TestQgaNotResponding emulates a guest without qemu-ga: qemu accepts the
// connection on the host side but nothing ever answers. The client must give
// up after its sync timeout and report ErrQgaNotResponding.
func TestQgaNotResponding(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qga")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Swallow input, never reply.
			go func() {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()

	q := &qgaConn{}
	conn, err := net.DialTimeout("unix", sock, qgaDialTimeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	q.conn, q.reader, q.socket = conn, bufio.NewReader(conn), sock
	err = q.sync(200 * time.Millisecond)
	if !errors.Is(err, ErrQgaNotResponding) {
		t.Fatalf("sync error = %v, want ErrQgaNotResponding", err)
	}
}

func TestQgaNoSocket(t *testing.T) {
	if err := QgaPing(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("QgaPing on a missing socket succeeded")
	}
}

func TestGetQgaSocket(t *testing.T) {
	if got, want := GetQgaSocket("dom1"), filepath.Join(kvmStateDir, "dom1", "qga"); got != want {
		t.Errorf("GetQgaSocket = %q, want %q", got, want)
	}
}
