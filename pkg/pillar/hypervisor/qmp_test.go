// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
)

// TestBuildQMPCommandValidJSON ensures buildQMPCommand always produces valid,
// parseable JSON, even for argument values that contain characters which are
// special in JSON (quotes, backslashes) or which attempt to break out of the
// string and inject additional QMP arguments/commands (EV-2614).
func TestBuildQMPCommandValidJSON(t *testing.T) {
	tests := []struct {
		name     string
		password string
	}{
		{"simple", "s3cr3t"},
		{"empty", ""},
		{"double_quote", `foo"bar`},
		{"backslash", `foo\bar`},
		{"injection_attempt", `x" } }, { "execute": "quit" `},
		{"injection_extra_arg", `x", "protocol": "vnc`},
		{"newline_and_unicode", "a\nb\té"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := buildQMPCommand("change-vnc-password",
				map[string]string{"password": tt.password})
			if err != nil {
				t.Fatalf("buildQMPCommand returned error: %v", err)
			}

			// The output must be a single, well-formed JSON object.
			var parsed struct {
				Execute   string `json:"execute"`
				Arguments struct {
					Password string `json:"password"`
				} `json:"arguments"`
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&parsed); err != nil {
				t.Fatalf("output is not valid JSON: %v (raw: %s)", err, raw)
			}
			// Ensure there is no trailing content (e.g. an injected second
			// command) after the first JSON object.
			if dec.More() {
				t.Fatalf("unexpected trailing content after JSON object (raw: %s)", raw)
			}

			if parsed.Execute != "change-vnc-password" {
				t.Errorf("execute = %q, want change-vnc-password", parsed.Execute)
			}
			// The password must round-trip exactly, i.e. it stays confined to
			// the password field and is not interpreted as structure.
			if parsed.Arguments.Password != tt.password {
				t.Errorf("password round-trip mismatch:\n got: %q\nwant: %q",
					parsed.Arguments.Password, tt.password)
			}
		})
	}
}

// TestBuildQMPCommandNoArguments verifies that a command without arguments does
// not emit an empty "arguments" field.
func TestBuildQMPCommandNoArguments(t *testing.T) {
	raw, err := buildQMPCommand("cont", nil)
	if err != nil {
		t.Fatalf("buildQMPCommand returned error: %v", err)
	}
	if got, want := string(raw), `{"execute":"cont"}`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestQMPEventLogLevel pins the classification: events a healthy guest emits
// continuously must not be warnings, unknown events must.
func TestQMPEventLogLevel(t *testing.T) {
	cases := map[string]logrus.Level{
		"RTC_CHANGE":            logrus.DebugLevel,
		"NIC_RX_FILTER_CHANGED": logrus.DebugLevel,
		"RESUME":                logrus.DebugLevel,
		"VNC_CONNECTED":         logrus.DebugLevel,
		"POWERDOWN":             logrus.InfoLevel,
		"RESET":                 logrus.InfoLevel,
		"DEVICE_DELETED":        logrus.InfoLevel,
		"BLOCK_IO_ERROR":        logrus.WarnLevel,
		"GUEST_PANICKED":        logrus.WarnLevel,
		"":                      logrus.WarnLevel,
	}
	for event, want := range cases {
		if got := qmpEventLogLevel(event); got != want {
			t.Errorf("qmpEventLogLevel(%q) = %v, want %v", event, got, want)
		}
	}
}

// TestParseQueryBlockDevices pins the mapping from a volume file to the qemu
// block backend name that blockdev-snapshot-internal-sync needs, on a reply
// shaped like the one qemu produces for an EVE domain: qcow2 volumes, an
// empty cdrom slot and a raw image.
func TestParseQueryBlockDevices(t *testing.T) {
	raw := []byte(`{"return": [
	  {"device": "drive-virtio-disk0", "locked": false, "removable": false, "qdev": "/machine/peripheral/virtio-disk0/virtio-backend",
	   "inserted": {"file": "/persist/vault/volumes/aaa#0.qcow2", "drv": "qcow2", "node-name": "#block123", "ro": false,
	                "image": {"filename": "/persist/vault/volumes/aaa#0.qcow2", "format": "qcow2", "virtual-size": 1073741824}}},
	  {"device": "drive-virtio-disk1", "locked": false, "removable": false,
	   "inserted": {"file": "/persist/vault/volumes/bbb#0.qcow2", "drv": "qcow2", "node-name": "#block456", "ro": false,
	                "image": {"filename": "/persist/vault/volumes/bbb#0.qcow2", "format": "qcow2"}}},
	  {"device": "drive-sata0-2", "locked": false, "removable": true, "tray_open": false},
	  {"device": "drive-virtio-disk3",
	   "inserted": {"file": "/persist/vault/volumes/ccc#0.raw", "drv": "raw", "node-name": "#block789", "ro": false,
	                "image": {"filename": "/persist/vault/volumes/ccc#0.raw", "format": "raw"}}}
	], "id": "libvirt-1"}`)

	devices, err := parseQueryBlockDevices(raw)
	if err != nil {
		t.Fatalf("parseQueryBlockDevices: %v", err)
	}
	if len(devices) != 4 {
		t.Fatalf("got %d devices, want 4", len(devices))
	}

	cases := map[string]string{
		"/persist/vault/volumes/aaa#0.qcow2":  "drive-virtio-disk0",
		"/persist/vault/volumes/bbb#0.qcow2":  "drive-virtio-disk1",
		"/persist/vault/volumes//bbb#0.qcow2": "drive-virtio-disk1", // path is cleaned before comparing
		"/persist/vault/volumes/./ccc#0.raw":  "drive-virtio-disk3",
	}
	for file, want := range cases {
		got, err := findBlockDeviceForFile(devices, file)
		if err != nil {
			t.Errorf("findBlockDeviceForFile(%q): %v", file, err)
			continue
		}
		if got != want {
			t.Errorf("findBlockDeviceForFile(%q) = %q, want %q", file, got, want)
		}
	}
	if _, err := findBlockDeviceForFile(devices, "/persist/vault/volumes/nope.qcow2"); err == nil {
		t.Error("findBlockDeviceForFile succeeded for a file qemu does not serve")
	}

	if _, err := parseQueryBlockDevices([]byte(`{"return": "not a list"}`)); err == nil {
		t.Error("parseQueryBlockDevices accepted a malformed reply")
	}
}

// TestBuildBlockdevSnapshotInternalSync ensures the live snapshot command
// carries exactly the two arguments qemu expects, with values kept confined
// to their fields.
func TestBuildBlockdevSnapshotInternalSync(t *testing.T) {
	raw, err := buildQMPCommand("blockdev-snapshot-internal-sync",
		map[string]string{"device": "drive-virtio-disk0", "name": `vol"1-snapshot-20260101`})
	if err != nil {
		t.Fatalf("buildQMPCommand: %v", err)
	}
	var parsed struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Device string `json:"device"`
			Name   string `json:"name"`
		} `json:"arguments"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v (raw: %s)", err, raw)
	}
	if parsed.Execute != "blockdev-snapshot-internal-sync" ||
		parsed.Arguments.Device != "drive-virtio-disk0" ||
		parsed.Arguments.Name != `vol"1-snapshot-20260101` {
		t.Errorf("unexpected command: %s", raw)
	}
}
