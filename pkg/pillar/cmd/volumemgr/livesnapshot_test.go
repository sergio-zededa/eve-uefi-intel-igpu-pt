// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package volumemgr

import (
	"testing"

	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/sirupsen/logrus"
)

func TestCheckLiveSnapshotPossible(t *testing.T) {
	if err := checkLiveSnapshotPossible(&types.VolumesSnapshotConfig{SnapshotID: "snap"}); err == nil {
		t.Error("live snapshot without a domain name must be refused")
	}
	// No such domain is running on the test host, so its QMP socket is absent.
	cfg := &types.VolumesSnapshotConfig{SnapshotID: "snap", Live: true,
		DomainName: "no-such-domain-for-live-snapshot-test"}
	if err := checkLiveSnapshotPossible(cfg); err == nil {
		t.Error("live snapshot of a domain without a QMP socket must be refused")
	}
}

// TestFreezeGuestWithoutAgentIsHarmless: a domain whose guest agent socket
// does not exist must degrade to a crash-consistent snapshot, i.e. freezeGuest
// neither fails nor returns a thaw function that does anything harmful.
func TestFreezeGuestWithoutAgentIsHarmless(t *testing.T) {
	logger := logrus.StandardLogger()
	log = base.NewSourceLogObject(logger, "test-volumemgr", 0)
	cfg := &types.VolumesSnapshotConfig{SnapshotID: "snap", Live: true,
		DomainName: "no-such-domain-for-live-snapshot-test"}
	thaw := freezeGuest(cfg)
	if thaw == nil {
		t.Fatal("freezeGuest returned a nil thaw function")
	}
	thaw()
}
