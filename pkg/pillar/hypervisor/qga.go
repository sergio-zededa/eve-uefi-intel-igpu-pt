// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/sirupsen/logrus"
)

// This file implements the small subset of the QEMU Guest Agent protocol
// (https://qemu-project.gitlab.io/qemu/interop/qemu-ga-ref.html) that EVE
// needs to take application-consistent live snapshots: freezing and thawing
// the guest's filesystems. The agent (qemu-ga) runs inside the guest and is
// reached through the "org.qemu.guest_agent.0" virtio-serial port declared in
// the qemu config template; on the host side that port is a unix socket, see
// GetQgaSocket.
//
// The wire format is QMP-like (one JSON object per line) but without the
// capabilities handshake. Since the channel is a plain serial port, the agent
// may not be running at all (nothing ever answers) or may still have stale
// bytes from a previous client in its buffer. Every session therefore starts
// with guest-sync-delimited, which makes the agent discard whatever precedes
// the 0xFF marker and lets us skip any stale replies on our side.

const (
	// qgaDialTimeout bounds connecting to the host-side socket. qemu accepts
	// the connection even when no agent runs in the guest, so this only
	// fails when the domain is gone.
	qgaDialTimeout = 3 * time.Second
	// qgaSyncTimeout bounds the initial guest-sync-delimited exchange and is
	// what effectively detects "no agent running in this guest".
	qgaSyncTimeout = 5 * time.Second
	// qgaFreezeTimeout bounds guest-fsfreeze-freeze. On Windows the agent
	// goes through VSS, which can take a while when many writers are
	// registered; the agent itself gives up after 60s.
	qgaFreezeTimeout = 90 * time.Second
	// qgaThawTimeout bounds guest-fsfreeze-thaw.
	qgaThawTimeout = 60 * time.Second
	// qgaStatusTimeout bounds guest-fsfreeze-status.
	qgaStatusTimeout = 10 * time.Second

	// qgaSyncDelimiter precedes the guest-sync-delimited request and reply.
	qgaSyncDelimiter = 0xff

	// QgaFsFreezeStatusFrozen is the guest-fsfreeze-status value reported
	// while the guest filesystems are frozen.
	QgaFsFreezeStatusFrozen = "frozen"
	// QgaFsFreezeStatusThawed is the guest-fsfreeze-status value reported
	// while the guest filesystems are operating normally.
	QgaFsFreezeStatusThawed = "thawed"
)

// ErrQgaNotResponding is returned when a connection to the guest agent socket
// could be established but the agent inside the guest never answered, i.e.
// no qemu-ga is running (or it is not installed at all).
var ErrQgaNotResponding = errors.New("qemu guest agent is not responding")

// qgaConn is a single session with the guest agent of one domain.
type qgaConn struct {
	conn   net.Conn
	reader *bufio.Reader
	socket string
}

// qgaCommand mirrors the wire format of a guest agent command.
type qgaCommand struct {
	Execute   string      `json:"execute"`
	Arguments interface{} `json:"arguments,omitempty"`
}

// qgaResponse mirrors the wire format of a guest agent reply.
type qgaResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

// qgaDial connects to the guest agent socket and synchronizes with the agent.
// It returns ErrQgaNotResponding (wrapped) when nothing answers the sync.
func qgaDial(socket string) (*qgaConn, error) {
	conn, err := net.DialTimeout("unix", socket, qgaDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to guest agent socket %s: %w", socket, err)
	}
	q := &qgaConn{conn: conn, reader: bufio.NewReader(conn), socket: socket}
	if err := q.sync(qgaSyncTimeout); err != nil {
		q.close()
		return nil, err
	}
	return q, nil
}

func (q *qgaConn) close() {
	_ = q.conn.Close()
}

// sync issues guest-sync-delimited and discards everything the agent sends
// until the matching reply arrives.
func (q *qgaConn) sync(timeout time.Duration) error {
	id := rand.Int63() //nolint:gosec // not security sensitive: only needs to be unlikely to repeat
	cmd, err := json.Marshal(qgaCommand{Execute: "guest-sync-delimited",
		Arguments: map[string]int64{"id": id}})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	if err := q.conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := q.conn.Write(append([]byte{qgaSyncDelimiter}, append(cmd, '\n')...)); err != nil {
		return fmt.Errorf("failed to write to guest agent socket %s: %w", q.socket, err)
	}
	// Skip everything up to and including the delimiter that starts the
	// agent's reply, then read replies until one carries our id.
	if _, err := q.reader.ReadBytes(qgaSyncDelimiter); err != nil {
		return q.notResponding(err)
	}
	for {
		line, err := q.reader.ReadBytes('\n')
		if err != nil {
			return q.notResponding(err)
		}
		var resp qgaResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Garbage left over from a previous session; keep reading.
			continue
		}
		var got int64
		if resp.Error == nil && json.Unmarshal(resp.Return, &got) == nil && got == id {
			return nil
		}
	}
}

func (q *qgaConn) notResponding(cause error) error {
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w on %s", ErrQgaNotResponding, q.socket)
	}
	return fmt.Errorf("guest agent session on %s failed: %w", q.socket, cause)
}

// execute sends one command and returns the "return" payload of its reply.
// Errors reported by the agent are returned as Go errors.
func (q *qgaConn) execute(execute string, arguments interface{}, timeout time.Duration) (json.RawMessage, error) {
	cmd, err := json.Marshal(qgaCommand{Execute: execute, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal guest agent command %q: %w", execute, err)
	}
	logrus.Debugf("executing guest agent command on %s: %s", q.socket, cmd)
	if err := q.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := q.conn.Write(append(cmd, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write guest agent command %q to %s: %w", execute, q.socket, err)
	}
	for {
		line, err := q.reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("no reply to guest agent command %q from %s: %w", execute, q.socket, err)
		}
		var resp qgaResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Not a reply (e.g. a stray partial line); keep reading.
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("guest agent command %q failed: %s (%s)", execute, resp.Error.Desc, resp.Error.Class)
		}
		return resp.Return, nil
	}
}

// QgaPing checks whether a guest agent is running and responsive in the given
// domain. It returns nil on success and an error wrapping ErrQgaNotResponding
// when the guest does not answer.
func QgaPing(socket string) error {
	q, err := qgaDial(socket)
	if err != nil {
		return err
	}
	defer q.close()
	_, err = q.execute("guest-ping", nil, qgaSyncTimeout)
	return err
}

// QgaFsFreezeStatus returns the guest-fsfreeze-status of the domain, one of
// QgaFsFreezeStatusFrozen or QgaFsFreezeStatusThawed.
func QgaFsFreezeStatus(socket string) (string, error) {
	q, err := qgaDial(socket)
	if err != nil {
		return "", err
	}
	defer q.close()
	raw, err := q.execute("guest-fsfreeze-status", nil, qgaStatusTimeout)
	if err != nil {
		return "", err
	}
	var status string
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", fmt.Errorf("unexpected guest-fsfreeze-status reply %s: %w", raw, err)
	}
	return status, nil
}

// QgaFsFreeze asks the guest agent to sync and freeze all guest filesystems
// (on Windows this goes through VSS, so registered applications get to
// quiesce as well). It returns the number of filesystems frozen. The caller
// must thaw with QgaFsThaw as soon as the snapshot is taken: while frozen the
// guest blocks on every write.
func QgaFsFreeze(socket string) (int, error) {
	q, err := qgaDial(socket)
	if err != nil {
		return 0, err
	}
	defer q.close()
	raw, err := q.execute("guest-fsfreeze-freeze", nil, qgaFreezeTimeout)
	if err != nil {
		return 0, err
	}
	var frozen int
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return 0, fmt.Errorf("unexpected guest-fsfreeze-freeze reply %s: %w", raw, err)
	}
	return frozen, nil
}

// QgaFsThaw asks the guest agent to thaw all guest filesystems previously
// frozen by QgaFsFreeze. It returns the number of filesystems thawed.
func QgaFsThaw(socket string) (int, error) {
	q, err := qgaDial(socket)
	if err != nil {
		return 0, err
	}
	defer q.close()
	raw, err := q.execute("guest-fsfreeze-thaw", nil, qgaThawTimeout)
	if err != nil {
		return 0, err
	}
	var thawed int
	if err := json.Unmarshal(raw, &thawed); err != nil {
		return 0, fmt.Errorf("unexpected guest-fsfreeze-thaw reply %s: %w", raw, err)
	}
	return thawed, nil
}
