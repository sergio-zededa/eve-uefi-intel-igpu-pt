// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package volumemgr

import (
	"errors"
	"fmt"
	"os"

	"github.com/lf-edge/eve/pkg/pillar/hypervisor"
	"github.com/lf-edge/eve/pkg/pillar/types"
)

// Live snapshots: taking a snapshot of an app instance's volumes while the
// app instance keeps running.
//
// The volume handlers know how to snapshot a single volume that is in use
// (see VolumeHandler.CreateLiveSnapshot). What belongs here is the part that
// spans all the volumes of one app instance: making sure the domain is
// actually reachable, and quiescing the guest once around the whole batch so
// that every volume is captured at the same, filesystem-consistent point.
//
// Quiescing relies on the QEMU guest agent (qemu-ga) running inside the guest
// (for Windows guests it is part of the virtio-win drivers package and hooks
// into VSS). It is strictly best effort: a guest without an agent still gets
// a crash-consistent snapshot, which is what an offline snapshot of a
// suddenly powered-off VM would give as well.

// checkLiveSnapshotPossible verifies the prerequisites of a live snapshot of
// the volumes described by config: the domain must be named and must be a
// running KVM domain, which is the only hypervisor exposing the QMP control
// socket the volume handlers rely on.
func checkLiveSnapshotPossible(config *types.VolumesSnapshotConfig) error {
	if config.DomainName == "" {
		return fmt.Errorf("live snapshot %s requested without a domain name", config.SnapshotID)
	}
	qmpSocket := hypervisor.GetQmpExecutorSocket(config.DomainName)
	if _, err := os.Stat(qmpSocket); err != nil {
		return fmt.Errorf("live snapshot %s requested but domain %s is not running under KVM (%s: %v)",
			config.SnapshotID, config.DomainName, qmpSocket, err)
	}
	return nil
}

// freezeGuest asks the guest agent of the domain to freeze the guest
// filesystems and returns the function that thaws them again. The returned
// function is always safe to call; when no freeze took place it is a no-op.
// Failures are logged and never abort the snapshot: they only downgrade it
// from application-consistent to crash-consistent.
func freezeGuest(config *types.VolumesSnapshotConfig) (thaw func()) {
	thaw = func() {}
	qgaSocket := hypervisor.GetQgaSocket(config.DomainName)
	status, err := hypervisor.QgaFsFreezeStatus(qgaSocket)
	if err != nil {
		if errors.Is(err, hypervisor.ErrQgaNotResponding) {
			log.Warnf("freezeGuest(%s): no guest agent in domain %s, taking a crash-consistent snapshot",
				config.SnapshotID, config.DomainName)
		} else {
			log.Warnf("freezeGuest(%s): guest agent of domain %s unusable (%v), taking a crash-consistent snapshot",
				config.SnapshotID, config.DomainName, err)
		}
		return thaw
	}
	if status == hypervisor.QgaFsFreezeStatusFrozen {
		// Somebody else froze the guest; it is consistent already and it is
		// not ours to thaw.
		log.Warnf("freezeGuest(%s): guest filesystems of domain %s are already frozen, leaving them alone",
			config.SnapshotID, config.DomainName)
		return thaw
	}
	frozen, err := hypervisor.QgaFsFreeze(qgaSocket)
	if err != nil {
		log.Warnf("freezeGuest(%s): guest-fsfreeze-freeze failed in domain %s (%v), taking a crash-consistent snapshot",
			config.SnapshotID, config.DomainName, err)
		// The agent may have frozen some filesystems before failing; make
		// sure the guest is not left blocked.
		thawGuest(config, qgaSocket)
		return thaw
	}
	log.Noticef("freezeGuest(%s): froze %d guest filesystems in domain %s",
		config.SnapshotID, frozen, config.DomainName)
	return func() { thawGuest(config, qgaSocket) }
}

// thawGuest asks the guest agent to thaw the guest filesystems. A failure here
// is serious (the guest stays blocked on writes), so it is logged as an error;
// the agent itself thaws after its own timeout as a last resort.
func thawGuest(config *types.VolumesSnapshotConfig, qgaSocket string) {
	thawed, err := hypervisor.QgaFsThaw(qgaSocket)
	if err != nil {
		log.Errorf("thawGuest(%s): guest-fsfreeze-thaw failed in domain %s: %v",
			config.SnapshotID, config.DomainName, err)
		return
	}
	log.Noticef("thawGuest(%s): thawed %d guest filesystems in domain %s",
		config.SnapshotID, thawed, config.DomainName)
}
