// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedmanager

import (
	"testing"
	"time"

	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/hypervisor"
	"github.com/lf-edge/eve/pkg/pillar/pubsub"
	"github.com/lf-edge/eve/pkg/pillar/types"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestCanTakeLiveSnapshot(t *testing.T) {
	kvm := hypervisor.KVMHypervisorName
	other := hypervisor.NullHypervisorName
	running := &types.DomainStatus{Activated: true, DomainName: "dom1"}
	halted := &types.DomainStatus{Activated: false, DomainName: "dom1"}
	unnamed := &types.DomainStatus{Activated: true}
	vm := types.AppInstanceConfig{FixedResources: types.VmConfig{VirtualizationMode: types.HVM}}
	container := types.AppInstanceConfig{FixedResources: types.VmConfig{VirtualizationMode: types.NOHYPER}}

	cases := []struct {
		name   string
		hv     *string
		config types.AppInstanceConfig
		domain *types.DomainStatus
		want   bool
	}{
		{"running kvm vm", &kvm, vm, running, true},
		{"no hypervisor selected", nil, vm, running, false},
		{"other hypervisor", &other, vm, running, false},
		{"plain container", &kvm, container, running, false},
		{"no domain status", &kvm, vm, nil, false},
		{"halted domain", &kvm, vm, halted, false},
		{"domain without name", &kvm, vm, unnamed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &zedmanagerContext{hypervisorPtr: tc.hv}
			if got := canTakeLiveSnapshot(ctx, tc.config, tc.domain); got != tc.want {
				t.Errorf("canTakeLiveSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPrepareVolumesSnapshotConfigsSkipsTriggered: once a request has been
// handed over to volumemgr it must not be prepared (and hence re-published)
// again on the next handleModify.
func TestPrepareVolumesSnapshotConfigsSkipsTriggered(t *testing.T) {
	logger := logrus.StandardLogger()
	log = base.NewSourceLogObject(logger, agentName, 0)

	appUUID, _ := uuid.NewV4()
	vol1, _ := uuid.NewV4()
	vol2, _ := uuid.NewV4()
	config := types.AppInstanceConfig{
		UUIDandVersion: types.UUIDandVersion{UUID: appUUID, Version: "1"},
		VolumeRefConfigList: []types.VolumeRefConfig{
			{VolumeID: vol1}, {VolumeID: vol2},
		},
	}
	status := &types.AppInstanceStatus{UUIDandVersion: config.UUIDandVersion}
	status.SnapStatus.RequestedSnapshots = []types.SnapshotInstanceStatus{
		{Snapshot: types.SnapshotDesc{SnapshotID: "in-flight", SnapshotType: types.SnapshotTypeImmediate},
			TimeTriggered: time.Now()},
		{Snapshot: types.SnapshotDesc{SnapshotID: "pending", SnapshotType: types.SnapshotTypeImmediate}},
		{Snapshot: types.SnapshotDesc{SnapshotID: "unspecified", SnapshotType: types.SnapshotTypeUnspecified}},
	}

	prepared := prepareVolumesSnapshotConfigs(nil, config, status)
	if len(prepared) != 1 {
		t.Fatalf("prepared %d configs, want 1: %+v", len(prepared), prepared)
	}
	assert.Equal(t, "pending", prepared[0].SnapshotID)
	assert.Equal(t, types.VolumesSnapshotCreate, prepared[0].Action)
	assert.Equal(t, appUUID, prepared[0].AppUUID)
	assert.ElementsMatch(t, []uuid.UUID{vol1, vol2}, prepared[0].VolumeIDs)
	assert.False(t, prepared[0].Live, "live is decided by handleModify, not here")
}

// TestTriggerSnapshotsPublishesLiveConfigAndStampsRequest checks the handover
// to volumemgr: the prepared config is published as-is (including the live
// flag and domain name), removed from the prepared list, and the matching
// request gets its TimeTriggered stamped regardless of the snapshot type.
func TestTriggerSnapshotsPublishesLiveConfigAndStampsRequest(t *testing.T) {
	logger := logrus.StandardLogger()
	log = base.NewSourceLogObject(logger, agentName, 0)
	ps := pubsub.New(pubsub.NewMemoryDriver(), logger, log)

	pubSnapConfig, err := ps.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.VolumesSnapshotConfig{},
	})
	assert.NoError(t, err)
	pubAppInstanceStatus, err := ps.NewPublication(pubsub.PublicationOptions{
		AgentName: agentName,
		TopicType: types.AppInstanceStatus{},
	})
	assert.NoError(t, err)
	ctx := &zedmanagerContext{
		pubVolumesSnapConfig: pubSnapConfig,
		pubAppInstanceStatus: pubAppInstanceStatus,
	}

	appUUID, _ := uuid.NewV4()
	vol, _ := uuid.NewV4()
	status := &types.AppInstanceStatus{UUIDandVersion: types.UUIDandVersion{UUID: appUUID, Version: "1"}}
	status.SnapStatus.RequestedSnapshots = []types.SnapshotInstanceStatus{
		{Snapshot: types.SnapshotDesc{SnapshotID: "snap-live", SnapshotType: types.SnapshotTypeImmediate}},
		{Snapshot: types.SnapshotDesc{SnapshotID: "snap-other", SnapshotType: types.SnapshotTypeImmediate}},
	}
	status.SnapStatus.PreparedVolumesSnapshotConfigs = []types.VolumesSnapshotConfig{{
		SnapshotID: "snap-live",
		Action:     types.VolumesSnapshotCreate,
		VolumeIDs:  []uuid.UUID{vol},
		AppUUID:    appUUID,
		Live:       true,
		DomainName: "dom1",
	}}

	triggerSnapshots(ctx, status)

	published := lookupVolumesSnapshotConfig(ctx, "snap-live")
	if published == nil {
		t.Fatal("VolumesSnapshotConfig was not published")
	}
	assert.True(t, published.Live)
	assert.Equal(t, "dom1", published.DomainName)
	assert.Equal(t, []uuid.UUID{vol}, published.VolumeIDs)
	assert.Empty(t, status.SnapStatus.PreparedVolumesSnapshotConfigs)
	assert.False(t, status.SnapStatus.RequestedSnapshots[0].TimeTriggered.IsZero(),
		"triggered request must be stamped")
	assert.True(t, status.SnapStatus.RequestedSnapshots[1].TimeTriggered.IsZero(),
		"request that was not triggered must stay untouched")
}
