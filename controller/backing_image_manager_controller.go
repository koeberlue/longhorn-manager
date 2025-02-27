package controller

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/kubernetes/pkg/controller"

	bimapi "github.com/longhorn/backing-image-manager/api"
	bimtypes "github.com/longhorn/backing-image-manager/pkg/types"

	"github.com/longhorn/longhorn-manager/constant"
	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/engineapi"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

const (
	BackingImageManagerPodContainerName = "backing-image-manager"
)

type BackingImageManagerController struct {
	*baseController

	namespace      string
	controllerID   string
	serviceAccount string
	bimImageName   string

	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	ds *datastore.DataStore

	cacheSyncs []cache.InformerSynced

	lock       *sync.RWMutex
	monitorMap map[string]chan struct{}
	backoffMap sync.Map

	versionUpdater func(*longhorn.BackingImageManager) error
}

type BackingImageManagerMonitor struct {
	Name         string
	controllerID string

	ds                 *datastore.DataStore
	log                logrus.FieldLogger
	backoff            *flowcontrol.Backoff
	lock               *sync.Mutex
	updateNotification bool
	// Receive stop signals from main sync loop
	stopCh chan struct{}
	// The monitor should voluntarily exit if the streaming doesn't work,
	// or the ownership of the related manager is taken over by others.
	monitorVoluntaryStopCh chan struct{}
	done                   bool

	client *engineapi.BackingImageManagerClient
	stream *bimapi.BackingImageStream
}

func updateBackingImageManagerVersion(bim *longhorn.BackingImageManager) error {
	cli, err := engineapi.NewBackingImageManagerClient(bim)
	if err != nil {
		return err
	}
	apiMinVersion, apiVersion, err := cli.VersionGet()
	if err != nil {
		return err
	}
	bim.Status.APIMinVersion = apiMinVersion
	bim.Status.APIVersion = apiVersion
	return nil
}

func NewBackingImageManagerController(
	logger logrus.FieldLogger,
	ds *datastore.DataStore,
	scheme *runtime.Scheme,
	kubeClient clientset.Interface,
	namespace, controllerID, serviceAccount, backingImageManagerImage string) *BackingImageManagerController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	c := &BackingImageManagerController{
		baseController: newBaseController("longhorn-backing-image-manager", logger),

		namespace:      namespace,
		controllerID:   controllerID,
		serviceAccount: serviceAccount,
		bimImageName:   backingImageManagerImage,

		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme, v1.EventSource{Component: "longhorn-backing-image-manager-controller"}),

		ds: ds,

		backoffMap: sync.Map{},

		lock:       &sync.RWMutex{},
		monitorMap: map[string]chan struct{}{},

		versionUpdater: updateBackingImageManagerVersion,
	}

	ds.BackingImageManagerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueBackingImageManager,
		UpdateFunc: func(old, cur interface{}) { c.enqueueBackingImageManager(cur) },
		DeleteFunc: c.enqueueBackingImageManager,
	})
	c.cacheSyncs = append(c.cacheSyncs, ds.BackingImageManagerInformer.HasSynced)

	ds.BackingImageInformer.AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueForBackingImage,
		UpdateFunc: func(old, cur interface{}) { c.enqueueForBackingImage(cur) },
		DeleteFunc: c.enqueueForBackingImage,
	}, 0)
	c.cacheSyncs = append(c.cacheSyncs, ds.BackingImageInformer.HasSynced)

	ds.NodeInformer.AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, cur interface{}) { c.enqueueForLonghornNode(cur) },
		DeleteFunc: c.enqueueForLonghornNode,
	}, 0)
	c.cacheSyncs = append(c.cacheSyncs, ds.NodeInformer.HasSynced)

	ds.PodInformer.AddEventHandlerWithResyncPeriod(cache.FilteringResourceEventHandler{
		FilterFunc: isBackingImageManagerPod,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueForBackingImageManagerPod,
			UpdateFunc: func(old, cur interface{}) { c.enqueueForBackingImageManagerPod(cur) },
			DeleteFunc: c.enqueueForBackingImageManagerPod,
		},
	}, 0)
	c.cacheSyncs = append(c.cacheSyncs, ds.PodInformer.HasSynced)

	return c
}

func (c *BackingImageManagerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	logrus.Info("Starting Longhorn backing image manager controller")
	defer logrus.Info("Shut down Longhorn backing image manager controller")

	if !cache.WaitForNamedCacheSync("longhorn backing image manager", stopCh, c.cacheSyncs...) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *BackingImageManagerController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *BackingImageManagerController) processNextWorkItem() bool {
	key, quit := c.queue.Get()

	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncBackingImageManager(key.(string))
	c.handleErr(err, key)

	return true
}

func (c *BackingImageManagerController) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < maxRetries {
		logrus.WithError(err).Errorf("Failed to sync Longhorn backing image manager %v", key)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logrus.WithError(err).Errorf("Dropping Longhorn backing image manager %v out of the queue", key)
	c.queue.Forget(key)
}

func getLoggerForBackingImageManager(logger logrus.FieldLogger, bim *longhorn.BackingImageManager) *logrus.Entry {
	return logger.WithFields(
		logrus.Fields{
			"backingImageManager": bim.Name,
			"nodeID":              bim.Spec.NodeID,
			"diskUUID":            bim.Spec.DiskUUID,
		},
	)
}

func (c *BackingImageManagerController) syncBackingImageManager(key string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "BackingImageManagerController failed to sync %v", key)
	}()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if namespace != c.namespace {
		return nil
	}

	bim, err := c.ds.GetBackingImageManager(name)
	if err != nil {
		if datastore.ErrorIsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "failed to get backing image manager")
	}

	log := getLoggerForBackingImageManager(c.logger, bim)

	if !c.isResponsibleFor(bim) {
		return nil
	}
	if bim.Status.OwnerID != c.controllerID {
		bim.Status.OwnerID = c.controllerID
		bim, err = c.ds.UpdateBackingImageManagerStatus(bim)
		if err != nil {
			// we don't mind others coming first
			if apierrors.IsConflict(errors.Cause(err)) {
				return nil
			}
			return err
		}
		log.Infof("Backing image manager got new owner %v", c.controllerID)
	}

	if bim.DeletionTimestamp != nil {
		if err := c.cleanupBackingImageManager(bim); err != nil {
			return err
		}
		return c.ds.RemoveFinalizerForBackingImageManager(bim)
	}

	existingBIM := bim.DeepCopy()
	defer func() {
		if err == nil && !reflect.DeepEqual(existingBIM.Status, bim.Status) {
			_, err = c.ds.UpdateBackingImageManagerStatus(bim)
		}
		if apierrors.IsConflict(errors.Cause(err)) {
			logrus.WithError(err).Debugf("Requeue %v due to conflict", key)
			c.enqueueBackingImageManager(bim)
			err = nil
		}
	}()

	if bim.Status.BackingImageFileMap == nil {
		bim.Status.BackingImageFileMap = map[string]longhorn.BackingImageFileInfo{}
	}

	node, diskName, err := c.ds.GetReadyDiskNode(bim.Spec.DiskUUID)
	if err != nil && !types.ErrorIsNotFound(err) {
		return err
	}
	noReadyDisk := node == nil
	diskMigrated := node != nil && (node.Name != bim.Spec.NodeID || node.Spec.Disks[diskName].Path != bim.Spec.DiskPath)
	if noReadyDisk || diskMigrated {
		if bim.Status.CurrentState != longhorn.BackingImageManagerStateUnknown {
			if noReadyDisk {
				log.Warnf("Node or disk is not ready, will update state from %v to %v then return", bim.Status.CurrentState, longhorn.BackingImageManagerStateUnknown)
				c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUnknown, "Node or disk is not ready, will update state from %v to %v then return", bim.Status.CurrentState, longhorn.BackingImageManagerStateUnknown)
			}
			if diskMigrated {
				log.Warnf("Disk %v(%v) is migrated to path %v on node %v; will update state from %v to %v then return", diskName, bim.Spec.DiskUUID, node.Spec.Disks[diskName].Path, node.Name, bim.Status.CurrentState, longhorn.BackingImageManagerStateUnknown)
				c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUnknown, "Disk %v(%v) is migrated to path %v on node %v; will update state from %v to %v, do cleanup, then wait for spec update", diskName, bim.Spec.DiskUUID, node.Spec.Disks[diskName].Path, node.Name, bim.Status.CurrentState, longhorn.BackingImageManagerStateError)
			}
			bim.Status.CurrentState = longhorn.BackingImageManagerStateUnknown
			c.updateForUnknownBackingImageManager(bim)
		}
		return nil
	}

	backoffValue, _ := c.backoffMap.Load(bim.Name)
	backoff, ok := backoffValue.(*flowcontrol.Backoff)
	if !ok {
		backoff = flowcontrol.NewBackOff(time.Minute, time.Minute*5)
		c.backoffMap.Store(bim.Name, backoff)
	}

	if err := c.syncBackingImageManagerPod(bim, backoff); err != nil {
		return err
	}

	if err := c.handleBackingImageFiles(bim, backoff); err != nil {
		return err
	}

	return nil
}

func (c *BackingImageManagerController) cleanupBackingImageManager(bim *longhorn.BackingImageManager) (err error) {
	log := getLoggerForBackingImageManager(c.logger, bim)

	if bim.Spec.Image == c.bimImageName && bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning && bim.Status.IP != "" {
		cli, err := engineapi.NewBackingImageManagerClient(bim)
		if err != nil {
			log.WithError(err).Warnf("Failed to launch a gRPC client during cleanup, will skip deleting all files")
		} else {
			log.Info("Deleting all backing image files during cleanup")
			for biName, biFileInfo := range bim.Status.BackingImageFileMap {
				if err := cli.Delete(biName, biFileInfo.UUID); err != nil {
					log.WithError(err).Warnf("Failed to launch a gRPC client during cleanup, will skip deleting the file for backing image %v(%v)", biName, biFileInfo.UUID)
					continue
				}
			}
		}
	}
	if c.isMonitoring(bim.Name) {
		c.stopMonitoring(bim.Name)
	}
	c.backoffMap.Delete(bim.Name)
	if err := c.ds.DeletePod(bim.Name); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func (c *BackingImageManagerController) updateForUnknownBackingImageManager(bim *longhorn.BackingImageManager) {
	if bim.Status.CurrentState != longhorn.BackingImageManagerStateUnknown {
		return
	}

	if c.isMonitoring(bim.Name) {
		c.stopMonitoring(bim.Name)
	}
	c.backoffMap.Delete(bim.Name)

	log := getLoggerForBackingImageManager(c.logger, bim)
	for biName, info := range bim.Status.BackingImageFileMap {
		if info.State == longhorn.BackingImageStateFailed {
			continue
		}
		info.State = longhorn.BackingImageStateUnknown
		bim.Status.BackingImageFileMap[biName] = info
	}
	for biName := range bim.Spec.BackingImages {
		if _, ok := bim.Status.BackingImageFileMap[biName]; ok {
			continue
		}
		bi, err := c.ds.GetBackingImage(biName)
		if err != nil {
			log.Warnf("Failed to get backing image %v before marking the empty file record in an unavailable disk as unknown", biName)
			continue
		}
		info := longhorn.BackingImageFileInfo{
			Name:  bi.Name,
			UUID:  bi.Status.UUID,
			State: longhorn.BackingImageStateUnknown,
		}
		bim.Status.BackingImageFileMap[biName] = info
	}

}

func (c *BackingImageManagerController) syncBackingImageManagerPod(bim *longhorn.BackingImageManager, backoff *flowcontrol.Backoff) (err error) {
	defer func() {
		err = errors.Wrap(err, "failed to sync backing image manager pod")
	}()

	log := getLoggerForBackingImageManager(c.logger, bim)

	pod, err := c.ds.GetPod(bim.Name)
	if err != nil {
		return errors.Wrapf(err, "failed to get pod for backing image manager %v", bim.Name)
	}

	// Sync backing image manager status with related pod
	if pod == nil {
		isNewBackingImageManager := bim.Status.CurrentState == "" || bim.Status.CurrentState == longhorn.BackingImageManagerStateStopped
		if isNewBackingImageManager {
			bim.Status.CurrentState = longhorn.BackingImageManagerStateStopped
		} else {
			log.Errorf("No pod for backing image manager with state %v, will update to state %v", bim.Status.CurrentState, longhorn.BackingImageManagerStateError)
			c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "No pod for backing image manager with state %v, will update to state %v", bim.Status.CurrentState, longhorn.BackingImageManagerStateError)
			bim.Status.CurrentState = longhorn.BackingImageManagerStateError
		}
	} else if pod.Spec.NodeName != bim.Spec.NodeID {
		if bim.Status.CurrentState != longhorn.BackingImageManagerStateError {
			log.Errorf("Pod node name %v doesn't match backing image manager node ID %v, will update to state %v", pod.Spec.NodeName, bim.Spec.NodeID, longhorn.BackingImageManagerStateError)
			c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "Pod node name %v doesn't match backing image manager node ID %v, will update to state %v", pod.Spec.NodeName, bim.Spec.NodeID, longhorn.BackingImageManagerStateError)
			bim.Status.CurrentState = longhorn.BackingImageManagerStateError
		}
	} else if pod.DeletionTimestamp != nil {
		if bim.Status.CurrentState != longhorn.BackingImageManagerStateError {
			log.Errorf("Pod deletion timestamp is set for backing image manager with state %v, will update to state %v", bim.Status.CurrentState, longhorn.BackingImageManagerStateError)
			c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "Pod deletion timestamp is set for backing image manager with state %v, will update to state %v", bim.Status.CurrentState, longhorn.BackingImageManagerStateError)
			bim.Status.CurrentState = longhorn.BackingImageManagerStateError
		}
	} else {
		switch pod.Status.Phase {
		case v1.PodPending:
			if bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning {
				log.Errorf("Backing image manager is state %v but the related pod is pending", longhorn.BackingImageManagerStateRunning)
				c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "Backing image manager is state %v but the related pod is pending", longhorn.BackingImageManagerStateRunning)
				bim.Status.CurrentState = longhorn.BackingImageManagerStateError
			} else {
				bim.Status.CurrentState = longhorn.BackingImageManagerStateStarting
			}
		case v1.PodRunning:
			// Make sure readiness probe has passed.
			isReady := true
			for _, st := range pod.Status.ContainerStatuses {
				if !st.Ready {
					isReady = false
					break
				}
			}
			if !isReady && bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning {
				log.Errorf("Backing image manager is state %v but the related pod container not ready, will update to state %v", longhorn.BackingImageManagerStateRunning, longhorn.BackingImageManagerStateError)
				c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "Backing image manager is state %v but the related pod container not ready, will update to state %v", longhorn.BackingImageManagerStateRunning, longhorn.BackingImageManagerStateError)
				bim.Status.CurrentState = longhorn.BackingImageManagerStateError
			} else if isReady && bim.Status.CurrentState != longhorn.BackingImageManagerStateRunning {
				log.Infof("Backing image manager becomes state %v", longhorn.BackingImageManagerStateRunning)
				c.eventRecorder.Eventf(bim, v1.EventTypeNormal, constant.EventReasonUpdate, "Backing image manager becomes state %v", longhorn.BackingImageManagerStateRunning)
				bim.Status.CurrentState = longhorn.BackingImageManagerStateRunning
			}

			if bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning {
				storageIP := c.ds.GetStorageIPFromPod(pod)
				if bim.Status.StorageIP != storageIP {
					bim.Status.StorageIP = storageIP
					logrus.Warnf("Inconsistent storage IP from pod %v, update backing image status storage IP %v", pod.Name, bim.Status.StorageIP)
				}

				bim.Status.IP = pod.Status.PodIP
			}
		default:
			log.Errorf("Unexpected pod phase %v, will update backing image manager to state %v", pod.Status.Phase, longhorn.BackingImageManagerStateError)
			c.eventRecorder.Eventf(bim, v1.EventTypeWarning, constant.EventReasonUpdate, "Unexpected pod phase %v, will update backing image manager to state %v", pod.Status.Phase, longhorn.BackingImageManagerStateError)
			bim.Status.CurrentState = longhorn.BackingImageManagerStateError
		}
	}

	if bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning {
		if bim.Status.APIVersion == engineapi.UnknownBackingImageManagerAPIVersion {
			if err := c.versionUpdater(bim); err != nil {
				return err
			}
		}
	} else {
		bim.Status.APIVersion = engineapi.UnknownBackingImageManagerAPIVersion
		bim.Status.APIMinVersion = engineapi.UnknownBackingImageManagerAPIVersion
	}

	// It's meaningless to start or monitor a pod for an old manager
	// since it will cleaned up immediately.
	if bim.Spec.Image != c.bimImageName {
		return nil
	}

	if bim.Status.CurrentState == longhorn.BackingImageManagerStateRunning && !c.isMonitoring(bim.Name) {
		c.startMonitoring(bim, backoff)
	} else if bim.Status.CurrentState != longhorn.BackingImageManagerStateRunning && c.isMonitoring(bim.Name) {
		c.stopMonitoring(bim.Name)
	}

	// Delete and restart backing image manager pod.
	if bim.Status.CurrentState == longhorn.BackingImageManagerStateError || bim.Status.CurrentState == longhorn.BackingImageManagerStateStopped {
		for name, file := range bim.Status.BackingImageFileMap {
			if file.State == longhorn.BackingImageStateFailed {
				continue
			}
			file.State = longhorn.BackingImageStateUnknown
			file.Message = "Backing image manager pod is not running"
			bim.Status.BackingImageFileMap[name] = file
		}

		pod, err := c.ds.GetPod(bim.Name)
		if err != nil {
			return err
		}
		if pod != nil && pod.DeletionTimestamp == nil {
			log.Info("Deleting pod before recreation")
			if err := c.ds.DeletePod(pod.Name); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		} else if pod == nil {
			// Similar to InstanceManagerController.
			// Longhorn shouldn't create the pod when users set taints with NoExecute effect on a node the bim is preferred.
			if c.controllerID == bim.Spec.NodeID {
				log.Info("Creating backing image manager pod")
				if err := c.createBackingImageManagerPod(bim); err != nil {
					return err
				}
				bim.Status.CurrentState = longhorn.BackingImageManagerStateStarting
				c.eventRecorder.Eventf(bim, v1.EventTypeNormal, constant.EventReasonCreate, "Creating backing image manager pod %v for disk %v on node %v. Backing image manager state will become %v", bim.Name, bim.Spec.DiskUUID, bim.Spec.NodeID, longhorn.BackingImageManagerStateStarting)
			}
		}
	}

	return nil
}

func (c *BackingImageManagerController) handleBackingImageFiles(bim *longhorn.BackingImageManager, backoff *flowcontrol.Backoff) (err error) {
	log := getLoggerForBackingImageManager(c.logger, bim)

	if bim.Status.CurrentState != longhorn.BackingImageManagerStateRunning {
		return nil
	}

	if err := engineapi.CheckBackingImageManagerCompatibility(bim.Status.APIMinVersion, bim.Status.APIVersion); err != nil {
		log.WithError(err).Warn("Skipping handling files for incompatible backing image manager")
		return nil
	}

	if bim.Spec.Image != c.bimImageName {
		return nil
	}

	cli, err := engineapi.NewBackingImageManagerClient(bim)
	if err != nil {
		return err
	}

	if err := c.deleteInvalidBackingImages(bim, cli, log, backoff); err != nil {
		return err
	}

	if err := c.prepareBackingImageFiles(bim, cli, log, backoff); err != nil {
		return err
	}

	return nil
}

func (c *BackingImageManagerController) deleteInvalidBackingImages(bim *longhorn.BackingImageManager, cli *engineapi.BackingImageManagerClient, log logrus.FieldLogger, backoff *flowcontrol.Backoff) (err error) {
	defer func() {
		err = errors.Wrap(err, "failed to do cleanup for invalid backing images")
	}()

	for biName, biFileInfo := range bim.Status.BackingImageFileMap {
		deleteRequired := false

		bi, err := c.ds.GetBackingImage(biName)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			deleteRequired = true
			log.Warnf("Failed to find backing image %v during invalid backing image cleanup, will skip it", biName)
		}
		if bi != nil && bi.Status.UUID == "" {
			continue
		}

		// Delete the file from a backing image manager when:
		//   1. The spec record is removed
		//      or does not match the current backing image.
		//   2. The status record does not match the current backing image.
		//   3. The file state recorded in the current backing image is failed
		//      and there are available files in other backing image managers.
		deleteRequired = deleteRequired || (bi != nil && bim.Spec.BackingImages[biName] != bi.Status.UUID)
		deleteRequired = deleteRequired || (bi != nil && biFileInfo.UUID != "" && biFileInfo.UUID != bi.Status.UUID)
		if !deleteRequired && bi != nil {
			// Prefer to check the file state in BackingImage.Status,
			// which is synced from BackingImageManager.Status with some
			// adjustments.
			fileState := biFileInfo.State
			if bi.Status.DiskFileStatusMap[bim.Spec.DiskUUID] != nil {
				fileState = bi.Status.DiskFileStatusMap[bim.Spec.DiskUUID].State
			}
			if fileState == longhorn.BackingImageStateFailed {
				for _, biFileInfo := range bi.Status.DiskFileStatusMap {
					if biFileInfo.State == longhorn.BackingImageStateFailed {
						continue
					}
					deleteRequired = true
					break
				}
			}
		}
		if !deleteRequired {
			continue
		}

		log.Infof("Deleting the file for invalid backing image %v, in backing image manager spec UUID %v, backing image correct UUID %v", biName, bim.Spec.BackingImages[biName], biFileInfo.UUID)
		if err := cli.Delete(biName, biFileInfo.UUID); err != nil && !types.ErrorIsNotFound(err) {
			return err
		}
		delete(bim.Status.BackingImageFileMap, biName)
		backoff.DeleteEntry(biName)
		c.eventRecorder.Eventf(bim, v1.EventTypeNormal, constant.EventReasonDelete, "Deleted backing image %v in disk %v on node %v", biName, bim.Spec.DiskUUID, bim.Spec.NodeID)
	}

	return nil
}

func (c *BackingImageManagerController) prepareBackingImageFiles(currentBIM *longhorn.BackingImageManager, cli *engineapi.BackingImageManagerClient, bimLog logrus.FieldLogger, backoff *flowcontrol.Backoff) (err error) {
	defer func() {
		err = errors.Wrap(err, "failed to prepare backing image files")
	}()

	bims, err := c.ds.ListBackingImageManagers()
	if err != nil {
		return err
	}
	for biName := range currentBIM.Spec.BackingImages {
		log := bimLog.WithFields(logrus.Fields{"backingImage": biName})

		bi, err := c.ds.GetBackingImage(biName)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.WithError(err).Warn("Failed to get backing image before preparing files, will skip handling this backing image")
			}
			continue
		}

		bids, err := c.ds.GetBackingImageDataSource(biName)
		if err != nil {
			log.WithError(err).Warn("Failed to get backing image data source before preparing files, will skip handling this backing image")
			continue
		}

		currentInfo, exists := currentBIM.Status.BackingImageFileMap[biName]
		requireFile := !exists || currentInfo.State == longhorn.BackingImageStateFailed
		// Ensure the bids can be deleted instead of being stuck in the `BackingImageStateFailed` state.
		// ref: https://github.com/longhorn/longhorn/issues/6086#issuecomment-1590662594
		requireFile = requireFile || bids.Status.CurrentState == longhorn.BackingImageStateFailed
		if !requireFile {
			continue
		}

		// Manager waits and fetches the 1st available file from BackingImageDataSource
		if !bids.Spec.FileTransferred {

			// If bids is failed and not transferred, orphan tmp file might be left on the host.
			// Clean up and set the state to failed-and-cleanup
			if bids.Status.CurrentState == longhorn.BackingImageStateFailed {
				if err := cli.Delete(bi.Name, bi.Status.UUID); err != nil {
					return err
				}
				bids.Status.CurrentState = longhorn.BackingImageStateFailedAndCleanUp
				if _, err = c.ds.UpdateBackingImageDataSourceStatus(bids); err != nil {
					return err
				}
				continue
			}

			if bids.Status.CurrentState != longhorn.BackingImageStateReadyForTransfer {
				continue
			}
			if bids.Spec.DiskUUID != currentBIM.Spec.DiskUUID {
				continue
			}
			if bids.Status.StorageIP == "" {
				log.Warnf("Failed to get backing image data source %v storage IP, cannot start transfer the file to the backing image manager", bids.Name)
				continue
			}
			log.Infof("Starting to fetch the data source file from the backing image data source work directory %v", bimtypes.DataSourceDirectoryName)
			if _, err := cli.Fetch(bi.Name, bi.Status.UUID, bids.Status.Checksum, fmt.Sprintf("%s:%d", bids.Status.StorageIP, engineapi.BackingImageDataSourceDefaultPort), bids.Status.Size); err != nil {
				if types.ErrorAlreadyExists(err) {
					continue
				}
				return err
			}
			// No backoff when fetching the 1st file.
			c.eventRecorder.Eventf(currentBIM, v1.EventTypeNormal, constant.EventReasonFetching, "Fetched the first file for backing image %v in disk %v on node %v", bi.Name, currentBIM.Spec.DiskUUID, currentBIM.Spec.NodeID)
			continue
		}

		if backoff.IsInBackOffSinceUpdate(bi.Name, time.Now()) {
			log.Debugf("Failed to re-fetch or re-sync backing image file %v immediately since it is still in the backoff window", bi.Name)
			continue
		}

		noReadyFile := true
		var senderCandidate *longhorn.BackingImageManager
		for _, bim := range bims {
			if bim.Status.CurrentState != longhorn.BackingImageManagerStateRunning || bim.Spec.Image != c.bimImageName {
				continue
			}
			info, exists := bim.Status.BackingImageFileMap[biName]
			if !exists {
				continue
			}
			if info.State != longhorn.BackingImageStateReady {
				continue
			}
			noReadyFile = false
			if info.SendingReference >= bimtypes.SendingLimit {
				continue
			}
			senderCandidate = bim
			break
		}

		// Due to cases like upgrade, there is no ready record among all default backing image manager.
		// Then Longhorn will ask managers to check then reuse existing files.
		if noReadyFile {
			size := bi.Status.Size
			if size == 0 {
				size = bids.Status.Size
			}
			// Empty source file name means trying to find and reuse the file in the work directory.
			if _, err := cli.Fetch(bi.Name, bi.Status.UUID, bi.Status.Checksum, "", size); err != nil {
				if types.ErrorAlreadyExists(err) {
					log.Warn("Backing image already exists, no need to check and reuse file")
					continue
				}
				backoff.Next(bi.Name, time.Now())
				return err
			}
			backoff.Next(bi.Name, time.Now())
			log.Info("Reusing the existing file in the work directory")
			c.eventRecorder.Eventf(currentBIM, v1.EventTypeNormal, constant.EventReasonFetching, "Reuse the existing file for backing image %v in disk %v on node %v", bi.Name, currentBIM.Spec.DiskUUID, currentBIM.Spec.NodeID)
			continue
		}

		if senderCandidate != nil {
			log.WithFields(logrus.Fields{"fromHost": senderCandidate.Status.StorageIP, "size": bi.Status.Size}).Info("Requesting syncing backing image")
			if _, err := cli.Sync(biName, bi.Status.UUID, bi.Status.Checksum, senderCandidate.Status.StorageIP, bi.Status.Size); err != nil {
				if types.ErrorAlreadyExists(err) {
					log.WithFields(logrus.Fields{"fromHost": senderCandidate.Status.StorageIP, "size": bi.Status.Size}).Warn("Backing image already exists, no need to sync from others")
					continue
				}
				backoff.Next(bi.Name, time.Now())
				return err
			}
			backoff.Next(bi.Name, time.Now())
			c.eventRecorder.Eventf(currentBIM, v1.EventTypeNormal, constant.EventReasonSyncing, "Syncing backing image %v in disk %v on node %v from %v(%v)", bi.Name, currentBIM.Spec.DiskUUID, currentBIM.Spec.NodeID, senderCandidate.Name, senderCandidate.Status.StorageIP)
			continue
		}
	}

	return nil
}

func (c *BackingImageManagerController) createBackingImageManagerPod(bim *longhorn.BackingImageManager) (err error) {
	defer func() {
		err = errors.Wrap(err, "failed to create backing image manager pod")
	}()

	tolerations, err := c.ds.GetSettingTaintToleration()
	if err != nil {
		return err
	}
	nodeSelector, err := c.ds.GetSettingSystemManagedComponentsNodeSelector()
	if err != nil {
		return err
	}
	registrySecretSetting, err := c.ds.GetSetting(types.SettingNameRegistrySecret)
	if err != nil {
		return err
	}
	registrySecret := registrySecretSetting.Value

	podManifest, err := c.generateBackingImageManagerPodManifest(bim, tolerations, registrySecret, nodeSelector)
	if err != nil {
		return err
	}
	if _, err := c.ds.CreatePod(podManifest); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func (c *BackingImageManagerController) generateBackingImageManagerPodManifest(bim *longhorn.BackingImageManager, tolerations []v1.Toleration, registrySecret string, nodeSelector map[string]string) (*v1.Pod, error) {
	tolerationsByte, err := json.Marshal(tolerations)
	if err != nil {
		return nil, err
	}

	priorityClass, err := c.ds.GetSetting(types.SettingNamePriorityClass)
	if err != nil {
		return nil, err
	}

	imagePullPolicy, err := c.ds.GetSettingImagePullPolicy()
	if err != nil {
		return nil, err
	}

	privileged := true
	podSpec := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            bim.Name,
			Namespace:       c.namespace,
			OwnerReferences: datastore.GetOwnerReferencesForBackingImageManager(bim),
			Labels:          types.GetBackingImageManagerLabels(bim.Spec.NodeID, bim.Spec.DiskUUID),
			Annotations:     map[string]string{types.GetLonghornLabelKey(types.LastAppliedTolerationAnnotationKeySuffix): string(tolerationsByte)},
		},
		Spec: v1.PodSpec{
			ServiceAccountName: c.serviceAccount,
			Tolerations:        util.GetDistinctTolerations(tolerations),
			NodeSelector:       nodeSelector,
			PriorityClassName:  priorityClass.Value,
			Containers: []v1.Container{
				{
					Name:            BackingImageManagerPodContainerName,
					Image:           bim.Spec.Image,
					ImagePullPolicy: imagePullPolicy,
					Command: []string{
						"backing-image-manager", "--debug",
						"daemon",
						"--listen", fmt.Sprintf("%s:%d", "0.0.0.0", engineapi.BackingImageManagerDefaultPort),
						"--sync-listen", fmt.Sprintf("%s:%d", "0.0.0.0", engineapi.BackingImageSyncServerDefaultPort),
					},
					ReadinessProbe: &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							TCPSocket: &v1.TCPSocketAction{
								Port: intstr.FromInt(engineapi.BackingImageManagerDefaultPort),
							},
						},
						InitialDelaySeconds: datastore.PodProbeInitialDelay,
						TimeoutSeconds:      datastore.PodProbeTimeoutSeconds,
						PeriodSeconds:       datastore.PodProbePeriodSeconds,
						FailureThreshold:    datastore.PodLivenessProbeFailureThreshold,
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "disk-path",
							MountPath: bimtypes.DiskPathInContainer,
						},
					},
					Env: []v1.EnvVar{
						{
							Name: types.EnvPodIP,
							ValueFrom: &v1.EnvVarSource{
								FieldRef: &v1.ObjectFieldSelector{
									FieldPath: "status.podIP",
								},
							},
						},
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: &privileged,
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "disk-path",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: bim.Spec.DiskPath,
						},
					},
				},
			},
			NodeName:      bim.Spec.NodeID,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	if registrySecret != "" {
		podSpec.Spec.ImagePullSecrets = []v1.LocalObjectReference{
			{
				Name: registrySecret,
			},
		}
	}

	storageNetwork, err := c.ds.GetSetting(types.SettingNameStorageNetwork)
	if err != nil {
		return nil, err
	}

	nadAnnot := string(types.CNIAnnotationNetworks)
	if storageNetwork.Value != types.CniNetworkNone {
		podSpec.Annotations[nadAnnot] = types.CreateCniAnnotationFromSetting(storageNetwork)
	}

	return podSpec, nil
}

func (c *BackingImageManagerController) enqueueBackingImageManager(backingImageManager interface{}) {
	key, err := controller.KeyFunc(backingImageManager)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to get key for object %#v: %v", backingImageManager, err))
		return
	}

	c.queue.Add(key)
}

func isBackingImageManagerPod(obj interface{}) bool {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return false
		}

		// use the last known state, to enqueue, dependent objects
		pod, ok = deletedState.Obj.(*v1.Pod)
		if !ok {
			return false
		}
	}

	if pod.Labels[types.GetLonghornLabelComponentKey()] == types.LonghornLabelBackingImageManager {
		return true
	}
	return false
}

func (c *BackingImageManagerController) enqueueForBackingImage(obj interface{}) {
	backingImage, ok := obj.(*longhorn.BackingImage)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("received unexpected obj: %#v", obj))
			return
		}

		// use the last known state, to enqueue, dependent objects
		backingImage, ok = deletedState.Obj.(*longhorn.BackingImage)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("DeletedFinalStateUnknown contained invalid object: %#v", deletedState.Obj))
			return
		}
	}

	backingImage, err := c.ds.GetBackingImage(backingImage.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		utilruntime.HandleError(fmt.Errorf("failed to get backing image %v: %v ", backingImage.Name, err))
		return
	}

	bims, err := c.ds.ListBackingImageManagers()
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.WithField("backingImage", backingImage.Name).Warn("Failed to list backing image managers for a backing image, may be deleted")
			return
		}
		utilruntime.HandleError(fmt.Errorf("failed to list backing image manager: %v", err))
		return
	}

	for _, bim := range bims {
		if _, exists := bim.Spec.BackingImages[backingImage.Name]; exists {
			c.enqueueBackingImageManager(bim)
		}
	}
}

func (c *BackingImageManagerController) enqueueForLonghornNode(obj interface{}) {
	node, ok := obj.(*longhorn.Node)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("received unexpected obj: %#v", obj))
			return
		}

		// use the last known state, to enqueue, dependent objects
		node, ok = deletedState.Obj.(*longhorn.Node)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("DeletedFinalStateUnknown contained invalid object: %#v", deletedState.Obj))
			return
		}
	}

	node, err := c.ds.GetNode(node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// there is no Longhorn node created for the Kubernetes
			// node (e.g. controller/etcd node). Skip it
			return
		}
		utilruntime.HandleError(fmt.Errorf("failed to get node %v: %v ", node.Name, err))
		return
	}

	bims, err := c.ds.ListBackingImageManagersByNode(node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.WithField("node", node.Name).Warn("Failed to list backing image managers for a node, may be deleted")
			return
		}
		utilruntime.HandleError(fmt.Errorf("failed to get backing image manager: %v", err))
		return
	}

	for _, bim := range bims {
		c.enqueueBackingImageManager(bim)
	}
}

func (c *BackingImageManagerController) enqueueForBackingImageManagerPod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("received unexpected obj: %#v", obj))
			return
		}

		// use the last known state, to enqueue, dependent objects
		pod, ok = deletedState.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("DeletedFinalStateUnknown contained invalid object: %#v", deletedState.Obj))
			return
		}
	}

	bim, err := c.ds.GetBackingImageManager(pod.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.WithField("pod", pod.Name).Warn("Failed to find backing image manager for pod, may be deleted")
			return
		}
		utilruntime.HandleError(fmt.Errorf("couldn't get backing image manager: %v", err))
		return
	}
	c.enqueueBackingImageManager(bim)
}

func (c *BackingImageManagerController) startMonitoring(bim *longhorn.BackingImageManager, backoff *flowcontrol.Backoff) {
	log := getLoggerForBackingImageManager(c.logger, bim)

	c.lock.Lock()
	defer c.lock.Unlock()

	if _, ok := c.monitorMap[bim.Name]; ok {
		return
	}

	client, err := engineapi.NewBackingImageManagerClient(bim)
	if err != nil {
		log.Error("Failed to launch gRPC client for backing image manager before monitoring")
		return
	}
	stream, err := client.Watch()
	if err != nil {
		log.Error("Failed to launch gRPC watching stream for backing image manager before monitoring")
		return
	}

	stopCh := make(chan struct{}, 1)
	monitorVoluntaryStopCh := make(chan struct{})
	monitor := &BackingImageManagerMonitor{
		Name:         bim.Name,
		controllerID: c.controllerID,

		ds:                     c.ds,
		log:                    log,
		backoff:                backoff,
		lock:                   &sync.Mutex{},
		stopCh:                 stopCh,
		monitorVoluntaryStopCh: monitorVoluntaryStopCh,
		done:                   false,
		updateNotification:     true,

		client: client,
		stream: stream,
	}
	c.monitorMap[bim.Name] = stopCh

	log.Info("Starting monitoring")
	go monitor.Run()
	go func() {
		<-monitorVoluntaryStopCh
		c.stopMonitoring(bim.Name)
	}()
}

func (c *BackingImageManagerController) stopMonitoring(bimName string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	log := c.logger.WithField("backingImageManager", bimName)

	log.Info("Stopping monitoring")
	stopCh, ok := c.monitorMap[bimName]
	if !ok {
		return
	}
	select {
	case <-stopCh:
		// channel is already closed
	default:
		close(stopCh)
	}
	delete(c.monitorMap, bimName)
}

func (c *BackingImageManagerController) isMonitoring(bimName string) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()

	_, ok := c.monitorMap[bimName]
	return ok
}

func (m *BackingImageManagerMonitor) Run() {
	defer func() {
		if err := m.stream.Close(); err != nil {
			m.log.Error("Failed to close streaming when stopping monitoring")
		}
		close(m.monitorVoluntaryStopCh)
	}()

	go func() {
		continuousFailureCount := 0
		for {
			if continuousFailureCount >= engineapi.MaxMonitorRetryCount {
				m.done = true
			}

			if m.done {
				return
			}

			if err := m.stream.Recv(); err != nil {
				m.log.WithError(err).Error("Error receiving next item")
				continuousFailureCount++
				time.Sleep(engineapi.MinPollCount * engineapi.PollInterval)
			} else {
				continuousFailureCount = 0
				m.lock.Lock()
				m.updateNotification = true
				m.lock.Unlock()
			}
		}
	}()

	needUpdate := false
	timer := 0
	ticker := time.NewTicker(engineapi.MinPollCount * engineapi.PollInterval)
	defer ticker.Stop()
	tick := ticker.C
	for {
		select {
		case <-tick:
			if m.done {
				return
			}

			m.lock.Lock()
			needUpdate = false
			timer++
			if timer >= engineapi.MaxPollCount || m.updateNotification {
				needUpdate = true
				m.updateNotification = false
				timer = 0
			}
			m.lock.Unlock()

			if needUpdate {
				if needStop := m.pollAndUpdateBackingImageFileMap(); needStop {
					m.done = true
					return
				}
			}
		case <-m.stopCh:
			m.done = true
			return
		}
	}
}

func (m *BackingImageManagerMonitor) pollAndUpdateBackingImageFileMap() (needStop bool) {
	var monitorErr error
	defer func() {
		if monitorErr != nil {
			m.log.WithError(monitorErr).Error("Failed to poll and update backing image file map in monitor goroutine")
		}
	}()
	bim, err := m.ds.GetBackingImageManager(m.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.log.Warn("Stopping monitoring because the backing image manager no longer exists")
			return true
		}
		monitorErr = err
		return false
	}

	if bim.Status.OwnerID != m.controllerID {
		m.log.Warnf("Stopping monitoring because the backing image manager owner ID becomes %v", bim.Status.OwnerID)
		return true
	}

	resp, err := m.client.List()
	if err != nil {
		monitorErr = err
		return false
	}

	if reflect.DeepEqual(bim.Status.BackingImageFileMap, resp) {
		return false
	}

	bim.Status.BackingImageFileMap = resp
	if _, err := m.ds.UpdateBackingImageManagerStatus(bim); err != nil {
		monitorErr = err
		return false
	}
	for biName, fileInfo := range bim.Status.BackingImageFileMap {
		if fileInfo.State == longhorn.BackingImageStateReady {
			m.backoff.DeleteEntry(biName)
		}
	}

	return false
}

func (c *BackingImageManagerController) isResponsibleFor(bim *longhorn.BackingImageManager) bool {
	return isControllerResponsibleFor(c.controllerID, c.ds, bim.Name, bim.Spec.NodeID, bim.Status.OwnerID)
}
