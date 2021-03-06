package controller

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/scheduler"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta1"
	lhinformers "github.com/longhorn/longhorn-manager/k8s/pkg/client/informers/externalversions/longhorn/v1beta1"
)

var (
	ownerKindNode = longhorn.SchemeGroupVersion.WithKind("Node").String()
)

type NodeController struct {
	// which namespace controller is running with
	namespace    string
	controllerID string

	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	ds *datastore.DataStore

	nStoreSynced  cache.InformerSynced
	pStoreSynced  cache.InformerSynced
	sStoreSynced  cache.InformerSynced
	rStoreSynced  cache.InformerSynced
	knStoreSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface

	getDiskInfoHandler    GetDiskInfoHandler
	topologyLabelsChecker TopologyLabelsChecker
	getDiskConfig         GetDiskConfig
	generateDiskConfig    GenerateDiskConfig

	scheduler *scheduler.ReplicaScheduler
}

type GetDiskInfoHandler func(string) (*util.DiskInfo, error)
type TopologyLabelsChecker func(kubeClient clientset.Interface, vers string) (bool, error)

type GetDiskConfig func(string) (*util.DiskConfig, error)
type GenerateDiskConfig func(string) (*util.DiskConfig, error)

func NewNodeController(
	ds *datastore.DataStore,
	scheme *runtime.Scheme,
	nodeInformer lhinformers.NodeInformer,
	settingInformer lhinformers.SettingInformer,
	podInformer coreinformers.PodInformer,
	replicaInformer lhinformers.ReplicaInformer,
	kubeNodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	namespace, controllerID string) *NodeController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	nc := &NodeController{
		namespace:    namespace,
		controllerID: controllerID,

		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme, v1.EventSource{Component: "longhorn-node-controller"}),

		ds: ds,

		nStoreSynced:  nodeInformer.Informer().HasSynced,
		pStoreSynced:  podInformer.Informer().HasSynced,
		sStoreSynced:  settingInformer.Informer().HasSynced,
		rStoreSynced:  replicaInformer.Informer().HasSynced,
		knStoreSynced: kubeNodeInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(EnhancedDefaultControllerRateLimiter(), "longhorn-node"),

		getDiskInfoHandler:    util.GetDiskInfo,
		topologyLabelsChecker: util.IsKubernetesVersionAtLeast,
		getDiskConfig:         util.GetDiskConfig,
		generateDiskConfig:    util.GenerateDiskConfig,
	}

	nc.scheduler = scheduler.NewReplicaScheduler(ds)

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			n := obj.(*longhorn.Node)
			nc.enqueueNode(n)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cur := newObj.(*longhorn.Node)
			nc.enqueueNode(cur)
		},
		DeleteFunc: func(obj interface{}) {
			n := obj.(*longhorn.Node)
			nc.enqueueNode(n)
		},
	})

	settingInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *longhorn.Setting:
					return nc.filterSettings(t)
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", nc, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					s := obj.(*longhorn.Setting)
					nc.enqueueSetting(s)
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					cur := newObj.(*longhorn.Setting)
					nc.enqueueSetting(cur)
				},
			},
		},
	)

	replicaInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *longhorn.Replica:
					return nc.filterReplica(t)
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", nc, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					r := obj.(*longhorn.Replica)
					nc.enqueueReplica(r)
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					cur := newObj.(*longhorn.Replica)
					nc.enqueueReplica(cur)
				},
				DeleteFunc: func(obj interface{}) {
					r := obj.(*longhorn.Replica)
					nc.enqueueReplica(r)
				},
			},
		},
	)

	podInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return nc.filterManagerPod(t)
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", nc, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					nc.enqueueManagerPod(pod)
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					cur := newObj.(*v1.Pod)
					nc.enqueueManagerPod(cur)
				},
				DeleteFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					nc.enqueueManagerPod(pod)
				},
			},
		},
	)

	kubeNodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			cur := newObj.(*v1.Node)
			nc.enqueueKubernetesNode(cur)
		},
		DeleteFunc: func(obj interface{}) {
			n := obj.(*v1.Node)
			nc.enqueueKubernetesNode(n)
		},
	})

	return nc
}

func (nc *NodeController) filterSettings(s *longhorn.Setting) bool {
	// filter that only StorageMinimalAvailablePercentage will impact disk status
	if types.SettingName(s.Name) == types.SettingNameStorageMinimalAvailablePercentage {
		return true
	}
	return false
}

func (nc *NodeController) filterReplica(r *longhorn.Replica) bool {
	// only sync replica running on current node
	if r.Spec.NodeID == nc.controllerID {
		return true
	}
	return false
}

func (nc *NodeController) filterManagerPod(obj *v1.Pod) bool {
	// only filter pod that control by manager
	controlByManager := false
	podContainers := obj.Spec.Containers
	for _, con := range podContainers {
		if con.Name == "longhorn-manager" {
			controlByManager = true
			break
		}
	}

	return controlByManager
}

func (nc *NodeController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer nc.queue.ShutDown()

	logrus.Infof("Start Longhorn node controller")
	defer logrus.Infof("Shutting down Longhorn node controller")

	if !controller.WaitForCacheSync("longhorn node", stopCh,
		nc.nStoreSynced, nc.pStoreSynced, nc.sStoreSynced, nc.rStoreSynced, nc.knStoreSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(nc.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (nc *NodeController) worker() {
	for nc.processNextWorkItem() {
	}
}

func (nc *NodeController) processNextWorkItem() bool {
	key, quit := nc.queue.Get()

	if quit {
		return false
	}
	defer nc.queue.Done(key)

	err := nc.syncNode(key.(string))
	nc.handleErr(err, key)

	return true
}

func (nc *NodeController) handleErr(err error, key interface{}) {
	if err == nil {
		nc.queue.Forget(key)
		return
	}

	if nc.queue.NumRequeues(key) < maxRetries {
		logrus.Warnf("Error syncing Longhorn node %v: %v", key, err)
		nc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logrus.Warnf("Dropping Longhorn node %v out of the queue: %v", key, err)
	nc.queue.Forget(key)
}

func (nc *NodeController) syncNode(key string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "fail to sync node for %v", key)
	}()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if namespace != nc.namespace {
		// Not ours, don't do anything
		return nil
	}

	node, err := nc.ds.GetNode(name)
	if err != nil {
		if datastore.ErrorIsNotFound(err) {
			logrus.Errorf("Longhorn node %v has been deleted", key)
			return nil
		}
		return err
	}

	if node.DeletionTimestamp != nil {
		nc.eventRecorder.Eventf(node, v1.EventTypeWarning, EventReasonDelete, "Deleting node %v", node.Name)
		return nc.ds.RemoveFinalizerForNode(node)
	}

	existingNode := node.DeepCopy()
	defer func() {
		// we're going to update volume assume things changes
		if err == nil && !reflect.DeepEqual(existingNode.Status, node.Status) {
			_, err = nc.ds.UpdateNodeStatus(node)
		}
		// requeue if it's conflict
		if apierrors.IsConflict(errors.Cause(err)) {
			logrus.Debugf("Requeue %v due to conflict: %v", key, err)
			nc.enqueueNode(node)
			err = nil
		}
	}()

	// sync node state by manager pod
	managerPods, err := nc.ds.ListManagerPods()
	if err != nil {
		return err
	}
	nodeManagerFound := false
	for _, pod := range managerPods {
		if pod.Spec.NodeName == node.Name {
			nodeManagerFound = true
			podConditions := pod.Status.Conditions
			for _, podCondition := range podConditions {
				if podCondition.Type == v1.PodReady {
					if podCondition.Status == v1.ConditionTrue && pod.Status.Phase == v1.PodRunning {
						node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
							types.NodeConditionTypeReady, types.ConditionStatusTrue,
							"", fmt.Sprintf("Node %v is ready", node.Name),
							nc.eventRecorder, node, v1.EventTypeNormal)
					} else {
						node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
							types.NodeConditionTypeReady, types.ConditionStatusFalse,
							string(types.NodeConditionReasonManagerPodDown),
							fmt.Sprintf("Node %v is down: the manager pod %v is not running", node.Name, pod.Name),
							nc.eventRecorder, node, v1.EventTypeWarning)
					}
					break
				}
			}
			break
		}
	}

	if !nodeManagerFound {
		node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
			types.NodeConditionTypeReady, types.ConditionStatusFalse,
			string(types.NodeConditionReasonManagerPodMissing),
			fmt.Sprintf("manager pod missing: node %v has no manager pod running on it", node.Name),
			nc.eventRecorder, node, v1.EventTypeWarning)
	}

	// sync node state with kuberentes node status
	kubeNode, err := nc.ds.GetKubernetesNode(name)
	if err != nil {
		// if kubernetes node has been removed from cluster
		if apierrors.IsNotFound(err) {
			node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
				types.NodeConditionTypeReady, types.ConditionStatusFalse,
				string(types.NodeConditionReasonKubernetesNodeGone),
				fmt.Sprintf("Kubernetes node missing: node %v has been removed from the cluster and there is no manager pod running on it", node.Name),
				nc.eventRecorder, node, v1.EventTypeWarning)
		} else {
			return err
		}
	} else {
		kubeConditions := kubeNode.Status.Conditions
		for _, con := range kubeConditions {
			switch con.Type {
			case v1.NodeReady:
				if con.Status != v1.ConditionTrue {
					node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
						types.NodeConditionTypeReady, types.ConditionStatusFalse,
						string(types.NodeConditionReasonKubernetesNodeNotReady),
						fmt.Sprintf("Kubernetes node %v not ready: %v", node.Name, con.Reason),
						nc.eventRecorder, node, v1.EventTypeWarning)
					break
				}
			case v1.NodeOutOfDisk,
				v1.NodeDiskPressure,
				v1.NodePIDPressure,
				v1.NodeMemoryPressure,
				v1.NodeNetworkUnavailable:
				if con.Status == v1.ConditionTrue {
					node.Status.Conditions = types.SetConditionAndRecord(node.Status.Conditions,
						types.NodeConditionTypeReady, types.ConditionStatusFalse,
						string(types.NodeConditionReasonKubernetesNodePressure),
						fmt.Sprintf("Kubernetes node %v has pressure: %v, %v", node.Name, con.Reason, con.Message),
						nc.eventRecorder, node, v1.EventTypeWarning)

					break
				}
			default:
				if con.Status == v1.ConditionTrue {
					nc.eventRecorder.Eventf(node, v1.EventTypeWarning, types.NodeConditionReasonUnknownNodeConditionTrue, "Unknown condition true of kubernetes node %v: condition type is %v, reason is %v, message is %v", node.Name, con.Type, con.Reason, con.Message)
				}
				break
			}
		}

		DisableSchedulingOnCordonedNode, err :=
			nc.ds.GetSettingAsBool(types.SettingNameDisableSchedulingOnCordonedNode)
		if err != nil {
			logrus.Errorf("error getting disable scheduling on cordoned node setting: %v", err)
			return err
		}

		// Update node condition based on
		// DisableSchedulingOnCordonedNode setting and
		// k8s node status
		kubeSpec := kubeNode.Spec
		if DisableSchedulingOnCordonedNode &&
			kubeSpec.Unschedulable == true {
			node.Status.Conditions =
				types.SetConditionAndRecord(node.Status.Conditions,
					types.NodeConditionTypeSchedulable,
					types.ConditionStatusFalse,
					string(types.NodeConditionReasonKubernetesNodeCordoned),
					fmt.Sprintf("Node %v is cordoned", node.Name),
					nc.eventRecorder, node,
					v1.EventTypeNormal)
		} else {
			node.Status.Conditions =
				types.SetConditionAndRecord(node.Status.Conditions,
					types.NodeConditionTypeSchedulable,
					types.ConditionStatusTrue,
					"",
					"",
					nc.eventRecorder, node,
					v1.EventTypeNormal)
		}

		isUsingTopologyLabels, err := nc.topologyLabelsChecker(nc.kubeClient, types.KubernetesTopologyLabelsVersion)
		if err != nil {
			return err
		}
		node.Status.Region, node.Status.Zone = types.GetRegionAndZone(kubeNode.Labels, isUsingTopologyLabels)

	}

	if nc.controllerID != node.Name {
		return nil
	}

	// sync disks status on current node
	if err := nc.syncDiskStatus(node); err != nil {
		return err
	}
	// sync mount propagation status on current node
	for _, pod := range managerPods {
		if pod.Spec.NodeName == node.Name {
			if err := nc.syncNodeStatus(pod, node); err != nil {
				return err
			}
		}
	}

	if err := nc.syncInstanceManagers(node); err != nil {
		return err
	}

	return nil
}

func (nc *NodeController) enqueueNode(node *longhorn.Node) {
	key, err := controller.KeyFunc(node)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", node, err))
		return
	}

	nc.queue.AddRateLimited(key)
}

func (nc *NodeController) enqueueSetting(setting *longhorn.Setting) {
	nodeList, err := nc.ds.ListNodes()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get all nodes: %v ", err))
		return
	}

	for _, node := range nodeList {
		nc.enqueueNode(node)
	}
}

func (nc *NodeController) enqueueReplica(replica *longhorn.Replica) {
	node, err := nc.ds.GetNode(replica.Spec.NodeID)
	if err != nil {
		// no replica would be scheduled to the node if the node is not
		// available. If the node was removed after being scheduled to,
		// the replica should be removed before that.
		utilruntime.HandleError(fmt.Errorf("Couldn't get node %v for replica %v: %v ",
			replica.Spec.NodeID, replica.Name, err))
		return
	}
	nc.enqueueNode(node)
}

func (nc *NodeController) enqueueManagerPod(pod *v1.Pod) {
	nodeList, err := nc.ds.ListNodes()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get all nodes: %v ", err))
		return
	}
	for _, node := range nodeList {
		nc.enqueueNode(node)
	}
}

func (nc *NodeController) enqueueKubernetesNode(n *v1.Node) {
	node, err := nc.ds.GetNode(n.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// there is no Longhorn node created for the Kubernetes
			// node (e.g. controller/etcd node). Skip it
			return
		}
		utilruntime.HandleError(fmt.Errorf("Couldn't get node %v: %v ", n.Name, err))
		return
	}
	nc.enqueueNode(node)
}

type diskInfo struct {
	entry *util.DiskInfo
	err   error
}

func (nc *NodeController) getDiskInfoMap(node *longhorn.Node) map[string]*diskInfo {
	result := map[string]*diskInfo{}
	for id, disk := range node.Spec.Disks {
		info, err := nc.getDiskInfoHandler(disk.Path)
		result[id] = &diskInfo{
			entry: info,
			err:   err,
		}
	}
	return result
}

// Check all disks in the same filesystem ID are in ready status
func (nc *NodeController) isFSIDDuplicatedWithExistingReadyDisk(name string, disks []string, diskStatusMap map[string]*types.DiskStatus) bool {
	if len(disks) > 1 {
		for _, otherName := range disks {
			diskReady :=
				types.GetCondition(
					diskStatusMap[otherName].Conditions,
					types.DiskConditionTypeReady)

			if (otherName != name) && (diskReady.Status ==
				types.ConditionStatusTrue) {
				return true
			}
		}
	}

	return false
}

func (nc *NodeController) syncDiskStatus(node *longhorn.Node) error {
	// sync the disks between node.Spec.Disks and node.Status.DiskStatus
	if node.Status.DiskStatus == nil {
		node.Status.DiskStatus = map[string]*types.DiskStatus{}
	}
	for id := range node.Spec.Disks {
		if node.Status.DiskStatus[id] == nil {
			node.Status.DiskStatus[id] = &types.DiskStatus{}
		}
		diskStatus := node.Status.DiskStatus[id]
		if diskStatus.Conditions == nil {
			diskStatus.Conditions = map[string]types.Condition{}
		}
		if diskStatus.ScheduledReplica == nil {
			diskStatus.ScheduledReplica = map[string]int64{}
		}
		// when condition are not ready, the old storage data should be cleaned
		diskStatus.StorageMaximum = 0
		diskStatus.StorageAvailable = 0
		node.Status.DiskStatus[id] = diskStatus
	}
	for id := range node.Status.DiskStatus {
		if _, exists := node.Spec.Disks[id]; !exists {
			delete(node.Status.DiskStatus, id)
		}
	}

	diskStatusMap := node.Status.DiskStatus
	diskInfoMap := nc.getDiskInfoMap(node)

	// update Ready condition
	fsid2Disks := map[string][]string{}
	for id, info := range diskInfoMap {
		if info.err != nil {
			diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
				types.DiskConditionTypeReady, types.ConditionStatusFalse,
				string(types.DiskConditionReasonNoDiskInfo),
				fmt.Sprintf("Disk %v(%v) on node %v is not ready: Get disk information error: %v", id, node.Spec.Disks[id].Path, node.Name, info.err),
				nc.eventRecorder, node, v1.EventTypeWarning)
		} else {
			if fsid2Disks[info.entry.Fsid] == nil {
				fsid2Disks[info.entry.Fsid] = []string{}
			}
			fsid2Disks[info.entry.Fsid] = append(fsid2Disks[info.entry.Fsid], id)
		}
	}

	for fsid, disks := range fsid2Disks {
		for _, id := range disks {
			diskStatus := diskStatusMap[id]
			disk := node.Spec.Disks[id]
			diskUUID := ""
			diskConfig, err := nc.getDiskConfig(node.Spec.Disks[id].Path)
			if err != nil {
				if !types.ErrorIsNotFound(err) {
					diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
						types.DiskConditionTypeReady, types.ConditionStatusFalse,
						string(types.DiskConditionReasonNoDiskInfo),
						fmt.Sprintf("Disk %v(%v) on node %v is not ready: failed to get disk config: error: %v", id, disk.Path, node.Name, err),
						nc.eventRecorder, node, v1.EventTypeWarning)
					continue
				}
			} else {
				diskUUID = diskConfig.DiskUUID
			}

			if diskStatusMap[id].DiskUUID == "" {
				// Check disks in the same filesystem
				if nc.isFSIDDuplicatedWithExistingReadyDisk(
					id, disks, diskStatusMap) {
					// Found multiple disks in the same Fsid
					diskStatusMap[id].Conditions =
						types.SetConditionAndRecord(
							diskStatusMap[id].Conditions,
							types.DiskConditionTypeReady,
							types.ConditionStatusFalse,
							string(types.DiskConditionReasonDiskFilesystemChanged),
							fmt.Sprintf("Disk %v(%v) on node %v is not ready: disk has same file system ID %v as other disks %+v", id, disk.Path, node.Name, fsid, disks),
							nc.eventRecorder, node,
							v1.EventTypeWarning)
					continue

				}

				if diskUUID == "" {
					diskConfig, err := nc.generateDiskConfig(node.Spec.Disks[id].Path)
					if err != nil {
						diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
							types.DiskConditionTypeReady, types.ConditionStatusFalse,
							string(types.DiskConditionReasonNoDiskInfo),
							fmt.Sprintf("Disk %v(%v) on node %v is not ready: failed to generate disk config: error: %v", id, disk.Path, node.Name, err),
							nc.eventRecorder, node, v1.EventTypeWarning)
						continue
					}
					diskUUID = diskConfig.DiskUUID
				}
				diskStatus.DiskUUID = diskUUID
			} else { // diskStatusMap[id].DiskUUID != ""
				if diskUUID == "" {
					diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
						types.DiskConditionTypeReady, types.ConditionStatusFalse,
						string(types.DiskConditionReasonDiskFilesystemChanged),
						fmt.Sprintf("Disk %v(%v) on node %v is not ready: cannot find disk config file, maybe due to a mount error", id, disk.Path, node.Name),
						nc.eventRecorder, node, v1.EventTypeWarning)
				} else if diskStatusMap[id].DiskUUID != diskUUID {
					diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
						types.DiskConditionTypeReady, types.ConditionStatusFalse,
						string(types.DiskConditionReasonDiskFilesystemChanged),
						fmt.Sprintf("Disk %v(%v) on node %v is not ready: record diskUUID doesn't match the one on the disk ", id, disk.Path, node.Name),
						nc.eventRecorder, node, v1.EventTypeWarning)
				}
			}

			if diskStatus.DiskUUID == diskUUID {
				diskStatus.StorageMaximum = diskInfoMap[id].entry.StorageMaximum
				diskStatus.StorageAvailable = diskInfoMap[id].entry.StorageAvailable
				diskStatusMap[id].Conditions = types.SetConditionAndRecord(diskStatusMap[id].Conditions,
					types.DiskConditionTypeReady, types.ConditionStatusTrue,
					"", fmt.Sprintf("Disk %v(%v) on node %v is ready", id, disk.Path, node.Name),
					nc.eventRecorder, node, v1.EventTypeNormal)
			}
			diskStatusMap[id] = diskStatus
		}

	}

	// update Schedulable condition
	replicaDiskMap, err := nc.ds.ListReplicasByNode(node.Name)
	if err != nil {
		return err
	}
	minimalAvailablePercentage, err := nc.ds.GetSettingAsInt(types.SettingNameStorageMinimalAvailablePercentage)
	if err != nil {
		return err
	}

	for id, disk := range node.Spec.Disks {
		diskStatus := diskStatusMap[id]

		// calculate storage scheduled
		scheduledReplica := map[string]int64{}
		storageScheduled := int64(0)
		for _, replica := range replicaDiskMap[id] {
			storageScheduled += replica.Spec.VolumeSize
			scheduledReplica[replica.Name] = replica.Spec.VolumeSize
		}
		diskStatus.StorageScheduled = storageScheduled
		diskStatus.ScheduledReplica = scheduledReplica
		delete(replicaDiskMap, id)

		// check disk pressure
		info, err := nc.scheduler.GetDiskSchedulingInfo(disk, diskStatus)
		if err != nil {
			return err
		}
		if !nc.scheduler.IsSchedulableToDisk(0, 0, info) {
			diskStatus.Conditions = types.SetConditionAndRecord(diskStatus.Conditions,
				types.DiskConditionTypeSchedulable, types.ConditionStatusFalse,
				string(types.DiskConditionReasonDiskPressure),
				fmt.Sprintf("the disk %v(%v) on the node %v has %v available, but requires reserved %v, minimal %v%s to schedule more replicas",
					id, disk.Path, node.Name, diskStatus.StorageAvailable, disk.StorageReserved, minimalAvailablePercentage, "%"),
				nc.eventRecorder, node, v1.EventTypeWarning)

		} else {
			diskStatus.Conditions = types.SetConditionAndRecord(diskStatus.Conditions,
				types.DiskConditionTypeSchedulable, types.ConditionStatusTrue,
				"", fmt.Sprintf("Disk %v(%v) on node %v is schedulable", id, disk.Path, node.Name),
				nc.eventRecorder, node, v1.EventTypeNormal)
		}
		diskStatusMap[id] = diskStatus
	}

	// if there's some replicas scheduled to wrong disks, write them to log
	if len(replicaDiskMap) > 0 {
		eReplicas := []string{}
		for _, replicas := range replicaDiskMap {
			for _, replica := range replicas {
				eReplicas = append(eReplicas, replica.Name)
			}
		}
		logrus.Errorf("Warning: These replicas have been assigned to a disk no longer exist: %v", strings.Join(eReplicas, ", "))
	}

	return nil
}

func (nc *NodeController) syncNodeStatus(pod *v1.Pod, node *longhorn.Node) error {
	// sync bidirectional mount propagation for node status to check whether the node could deploy CSI driver
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == types.LonghornSystemKey {
			mountPropagationStr := ""
			if mount.MountPropagation == nil {
				mountPropagationStr = "nil"
			} else {
				mountPropagationStr = string(*mount.MountPropagation)
			}
			if mount.MountPropagation == nil || *mount.MountPropagation != v1.MountPropagationBidirectional {
				node.Status.Conditions = types.SetCondition(node.Status.Conditions, types.NodeConditionTypeMountPropagation, types.ConditionStatusFalse,
					string(types.NodeConditionReasonNoMountPropagationSupport),
					fmt.Sprintf("The MountPropagation value %s is not detected from pod %s, node %s", mountPropagationStr, pod.Name, pod.Spec.NodeName))
			} else {
				node.Status.Conditions = types.SetCondition(node.Status.Conditions, types.NodeConditionTypeMountPropagation, types.ConditionStatusTrue, "", "")
			}
			break
		}
	}

	return nil
}

func (nc *NodeController) syncInstanceManagers(node *longhorn.Node) error {
	defaultInstanceManagerImage, err := nc.ds.GetSettingValueExisted(types.SettingNameDefaultInstanceManagerImage)
	if err != nil {
		return err
	}

	imTypes := []types.InstanceManagerType{types.InstanceManagerTypeEngine}

	// Clean up all replica managers if there is no disk on the node
	if len(node.Spec.Disks) == 0 {
		rmMap, err := nc.ds.ListInstanceManagersByNode(node.Name, types.InstanceManagerTypeReplica)
		if err != nil {
			return err
		}
		for _, rm := range rmMap {
			logrus.Debugf("Prepare to clean up the replica manager %v since there is no available disk on node %v", rm.Name, node.Name)
			if err := nc.ds.DeleteInstanceManager(rm.Name); err != nil {
				return err
			}
		}
	} else {
		imTypes = append(imTypes, types.InstanceManagerTypeReplica)
	}

	for _, imType := range imTypes {
		defaultInstanceManagerCreated := false
		imMap, err := nc.ds.ListInstanceManagersByNode(node.Name, imType)
		if err != nil {
			return err
		}
		for _, im := range imMap {
			if im.Labels[types.GetLonghornLabelKey(types.LonghornLabelNode)] != im.Spec.NodeID {
				return fmt.Errorf("BUG: Instance manager %v NodeID %v is not consistent with the label %v=%v",
					im.Name, im.Spec.NodeID, types.GetLonghornLabelKey(types.LonghornLabelNode), im.Labels[types.GetLonghornLabelKey(types.LonghornLabelNode)])
			}
			cleanupRequired := true
			if im.Spec.Image == defaultInstanceManagerImage {
				// Create default instance manager if needed.
				defaultInstanceManagerCreated = true
				cleanupRequired = false
			} else {
				// Clean up old instance managers if there is no running instance.
				if im.Status.CurrentState == types.InstanceManagerStateRunning && im.DeletionTimestamp == nil {
					for _, instance := range im.Status.Instances {
						if instance.Status.State == types.InstanceStateRunning || instance.Status.State == types.InstanceStateStarting {
							cleanupRequired = false
							break
						}
					}
				}
			}
			if cleanupRequired {
				logrus.Debugf("Prepare to clean up the redundant instance manager %v when there is no running/starting instance", im.Name)
				if err := nc.ds.DeleteInstanceManager(im.Name); err != nil {
					return err
				}
			}
		}
		if !defaultInstanceManagerCreated {
			imName, err := types.GetInstanceManagerName(imType)
			if err != nil {
				return err
			}
			logrus.Debugf("Prepare to create default instance manager %v, node: %v, default instance manager image: %v, type: %v",
				imName, node.Name, defaultInstanceManagerImage, imType)
			if _, err := nc.createInstanceManager(node, imName, defaultInstanceManagerImage, imType); err != nil {
				return err
			}
		}
	}
	return nil
}

func (nc *NodeController) createInstanceManager(node *longhorn.Node, imName, image string, imType types.InstanceManagerType) (*longhorn.InstanceManager, error) {
	instanceManager := &longhorn.InstanceManager{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          types.GetInstanceManagerLabels(node.Name, image, imType),
			Name:            imName,
			OwnerReferences: datastore.GetOwnerReferencesForNode(node),
		},
		Spec: types.InstanceManagerSpec{
			Image:  image,
			NodeID: node.Name,
			Type:   imType,
		},
	}

	return nc.ds.CreateInstanceManager(instanceManager)
}
