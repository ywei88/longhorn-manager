package scheduler

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"
	"github.com/sirupsen/logrus"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1alpha1"
)

type ReplicaScheduler struct {
	ds *datastore.DataStore
}

type Disk struct {
	types.DiskSpec
	NodeID string
}

type DiskSchedulingInfo struct {
	StorageAvailable           int64
	StorageMaximum             int64
	StorageReserved            int64
	StorageScheduled           int64
	OverProvisioningPercentage int64
	MinimalAvailablePercentage int64
}

func NewReplicaScheduler(ds *datastore.DataStore) *ReplicaScheduler {
	rcScheduler := &ReplicaScheduler{
		ds: ds,
	}
	return rcScheduler
}

// ScheduleReplica will return (nil, nil) for unschedulable replica
func (rcs *ReplicaScheduler) ScheduleReplica(replica *longhorn.Replica, replicas map[string]*longhorn.Replica, volume *longhorn.Volume) (*longhorn.Replica, error) {
	// only called when replica is starting for the first time
	if replica.Spec.NodeID != "" {
		return nil, fmt.Errorf("BUG: Replica %v has been scheduled to node %v", replica.Name, replica.Spec.NodeID)
	}

	// get all hosts
	nodeInfo, err := rcs.getNodeInfo()
	if err != nil {
		return nil, err
	}
	if len(nodeInfo) == 0 {
		logrus.Errorf("There's no available node for replica %v, size %v", replica.ObjectMeta.Name, replica.Spec.VolumeSize)
		return nil, nil
	}

	// find proper node and disk
	diskCandidates := rcs.chooseDiskCandidates(nodeInfo, replicas, replica, volume)

	// there's no disk that fit for current replica
	if len(diskCandidates) == 0 {
		logrus.Errorf("There's no available disk for replica %v, size %v", replica.ObjectMeta.Name, replica.Spec.VolumeSize)
		return nil, nil
	}

	// schedule replica to disk
	rcs.scheduleReplicaToDisk(replica, diskCandidates)

	return replica, nil
}

func (rcs *ReplicaScheduler) chooseDiskCandidates(nodeInfo map[string]*longhorn.Node, replicas map[string]*longhorn.Replica, replica *longhorn.Replica, volume *longhorn.Volume) map[string]*Disk {
	diskCandidates := map[string]*Disk{}
	filterdNode := []*longhorn.Node{}
	for nodeName, node := range nodeInfo {
		// Filter Nodes first. If the Nodes don't match the tags, don't bother checking their Disks as candidates.
		isFilterd := false
		if !rcs.checkTagsAreFulfilled(node.Spec.Tags, volume.Spec.NodeSelector) {
			isFilterd = true
		}
		for _, r := range replicas {
			// filter replica in deleting process
			if r.Spec.NodeID != "" && r.Spec.NodeID == nodeName && r.DeletionTimestamp == nil && r.Spec.FailedAt == "" {
				filterdNode = append(filterdNode, node)
				isFilterd = true
				break
			}
		}
		if !isFilterd {
			diskCandidates = rcs.filterNodeDisksForReplica(node, replica, replicas, volume)
			if len(diskCandidates) > 0 {
				return diskCandidates
			}
		}
	}
	// If there's no disk fit for replica on other nodes,
	// try to schedule to node that has been scheduled replicas.
	// Avoid this if Replica Hard Anti-Affinity is enabled.
	softAntiAffinity, err := rcs.ds.GetSettingAsBool(types.SettingNameReplicaSoftAntiAffinity)
	if err != nil {
		logrus.Errorf("error getting replica hard anti-affinity setting: %v", err)
	}
	// Defaulting to soft anti-affinity if we can't get the hard anti-affinity setting.
	if err != nil || softAntiAffinity {
		for _, node := range filterdNode {
			diskCandidates = rcs.filterNodeDisksForReplica(node, replica, replicas, volume)
		}
	}

	return diskCandidates
}

func (rcs *ReplicaScheduler) filterNodeDisksForReplica(node *longhorn.Node, replica *longhorn.Replica, replicas map[string]*longhorn.Replica, volume *longhorn.Volume) map[string]*Disk {
	preferredDisk := map[string]*Disk{}
	// find disk that fit for current replica
	disks := node.Spec.Disks
	diskStatus := node.Status.DiskStatus
	for fsid, disk := range disks {
		status := diskStatus[fsid]
		info, err := rcs.GetDiskSchedulingInfo(disk, status)
		if err != nil {
			logrus.Errorf("Fail to get settings when scheduling replica: %v", err)
			return preferredDisk
		}
		scheduledReplica := status.ScheduledReplica
		// check other replicas for the same volume has been accounted on current node
		var storageScheduled int64
		for rName, r := range replicas {
			if _, ok := scheduledReplica[rName]; !ok && r.Spec.NodeID != "" && r.Spec.NodeID == node.Name {
				storageScheduled += r.Spec.VolumeSize
			}
		}
		if storageScheduled > 0 {
			info.StorageScheduled += storageScheduled
		}
		if !disk.AllowScheduling ||
			!rcs.IsSchedulableToDisk(replica.Spec.VolumeSize, info) {
			continue
		}
		// Check if the Disk's Tags are valid.
		if !rcs.checkTagsAreFulfilled(disk.Tags, volume.Spec.DiskSelector) {
			continue
		}
		suggestDisk := &Disk{
			DiskSpec: disk,
			NodeID:   node.Name,
		}
		preferredDisk[fsid] = suggestDisk
	}

	return preferredDisk
}

func (rcs *ReplicaScheduler) checkTagsAreFulfilled(itemTags, volumeTags []string) bool {
	if !sort.StringsAreSorted(itemTags) {
		logrus.Warnf("BUG: Tags are not sorted, sort now")
		sort.Strings(itemTags)
	}

	for _, tag := range volumeTags {
		if index := sort.SearchStrings(itemTags, tag); index >= len(itemTags) || itemTags[index] != tag {
			return false
		}
	}

	return true
}

func (rcs *ReplicaScheduler) getNodeInfo() (map[string]*longhorn.Node, error) {
	nodeInfo, err := rcs.ds.ListNodes()
	if err != nil {
		return nil, err
	}
	scheduledNode := map[string]*longhorn.Node{}
	for _, node := range nodeInfo {
		nodeReadyCondition := types.GetNodeConditionFromStatus(node.Status, types.NodeConditionTypeReady)
		if node != nil && node.DeletionTimestamp == nil && nodeReadyCondition.Status == types.ConditionStatusTrue && node.Spec.AllowScheduling {
			scheduledNode[node.Name] = node
		}
	}
	return scheduledNode, nil
}

func (rcs *ReplicaScheduler) scheduleReplicaToDisk(replica *longhorn.Replica, diskCandidates map[string]*Disk) {
	// get a random disk from diskCandidates
	var fsid string
	var disk *Disk
	for fsid, disk = range diskCandidates {
		break
	}
	replica.Spec.NodeID = disk.NodeID
	replica.Spec.DiskID = fsid
	replica.Spec.DataPath = filepath.Join(disk.Path, "replicas", replica.Spec.VolumeName+"-"+util.RandomID())
	logrus.Debugf("Schedule replica %v to node %v, disk %v, datapath %v",
		replica.Name, replica.Spec.NodeID, replica.Spec.DiskID, replica.Spec.DataPath)
}

func (rcs *ReplicaScheduler) IsSchedulableToDisk(size int64, info *DiskSchedulingInfo) bool {
	// StorageReserved = the space is already used by 3rd party + the space will be used by 3rd party.
	// StorageAvailable = the space can be used by 3rd party or Longhorn system.
	// There is no (direct) relationship between StorageReserved and StorageAvailable.
	return info.StorageMaximum > 0 && info.StorageAvailable > 0 &&
		(size+info.StorageScheduled) <= (info.StorageMaximum-info.StorageReserved)*(info.OverProvisioningPercentage/100) &&
		info.StorageAvailable > info.StorageMaximum*info.MinimalAvailablePercentage/100
}

func (rcs *ReplicaScheduler) GetDiskSchedulingInfo(disk types.DiskSpec, diskStatus types.DiskStatus) (*DiskSchedulingInfo, error) {
	// get StorageOverProvisioningPercentage and StorageMinimalAvailablePercentage settings
	overProvisioningPercentage, err := rcs.ds.GetSettingAsInt(types.SettingNameStorageOverProvisioningPercentage)
	if err != nil {
		return nil, err
	}
	minimalAvailablePercentage, err := rcs.ds.GetSettingAsInt(types.SettingNameStorageMinimalAvailablePercentage)
	if err != nil {
		return nil, err
	}
	info := &DiskSchedulingInfo{
		StorageAvailable:           diskStatus.StorageAvailable,
		StorageScheduled:           diskStatus.StorageScheduled,
		StorageReserved:            disk.StorageReserved,
		StorageMaximum:             diskStatus.StorageMaximum,
		OverProvisioningPercentage: overProvisioningPercentage,
		MinimalAvailablePercentage: minimalAvailablePercentage,
	}
	return info, nil
}
