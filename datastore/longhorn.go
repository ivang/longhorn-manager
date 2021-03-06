package datastore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta1"
)

const (
	// NameMaximumLength restricted the length due to Kubernetes name limitation
	NameMaximumLength = 40
)

var (
	longhornFinalizerKey = longhorn.SchemeGroupVersion.Group

	VerificationRetryInterval = 100 * time.Millisecond
	VerificationRetryCounts   = 20
)

func (s *DataStore) InitSettings() error {
	for _, sName := range types.SettingNameList {
		definition, ok := types.SettingDefinitions[sName]
		if !ok {
			return fmt.Errorf("BUG: setting %v is not defined", sName)
		}
		if _, err := s.sLister.Settings(s.namespace).Get(string(sName)); err != nil {
			if ErrorIsNotFound(err) {
				setting := &longhorn.Setting{
					ObjectMeta: metav1.ObjectMeta{
						Name: string(sName),
					},
					Setting: types.Setting{
						Value: definition.Default,
					},
				}
				if _, err := s.CreateSetting(setting); err != nil {
					return err
				}
			} else {
				return err
			}
		}
	}
	return nil
}

func (s *DataStore) CreateSetting(setting *longhorn.Setting) (*longhorn.Setting, error) {
	// GetSetting automatically create default entry, so no need to double check
	return s.lhClient.LonghornV1beta1().Settings(s.namespace).Create(setting)
}

func (s *DataStore) UpdateSetting(setting *longhorn.Setting) (*longhorn.Setting, error) {
	obj, err := s.lhClient.LonghornV1beta1().Settings(s.namespace).Update(setting)
	if err != nil {
		return nil, err
	}
	verifyUpdate(setting.Name, obj, func(name string) (runtime.Object, error) {
		return s.getSettingRO(name)
	})
	return obj, nil
}

func (s *DataStore) ValidateSetting(name, value string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "fail to set settings with invalid %v", name)
	}()
	sName := types.SettingName(name)

	if err := types.ValidateInitSetting(name, value); err != nil {
		return err
	}

	switch sName {
	case types.SettingNameBackupTarget:
		vs, err := s.ListStandbyVolumesRO()
		if err != nil {
			return errors.Wrapf(err, "failed to list standby volume when modifying BackupTarget")
		}
		if len(vs) != 0 {
			standbyVolumeNames := make([]string, len(vs))
			for k := range vs {
				standbyVolumeNames = append(standbyVolumeNames, k)
			}
			return fmt.Errorf("cannot modify BackupTarget since there are existing standby volumes: %v", standbyVolumeNames)
		}
	case types.SettingNameTaintToleration:
		list, err := s.ListVolumesRO()
		if err != nil {
			return errors.Wrapf(err, "failed to list volumes before modifying toleration setting")
		}
		for _, v := range list {
			if v.Status.State != types.VolumeStateDetached {
				return fmt.Errorf("cannot modify toleration setting before all volumes are detached")
			}
		}
	case types.SettingNamePriorityClass:
		if value != "" {
			if _, err := s.GetPriorityClass(value); err != nil {
				return errors.Wrapf(err, "failed to get priority class %v before modifying priority class setting", value)
			}
		}
		list, err := s.ListVolumesRO()
		if err != nil {
			return errors.Wrapf(err, "failed to list volumes before modifying priority class setting")
		}
		for _, v := range list {
			if v.Status.State != types.VolumeStateDetached {
				return fmt.Errorf("cannot modify priority class setting before all volumes are detached")
			}
		}
	}
	return nil
}

func (s *DataStore) getSettingRO(name string) (*longhorn.Setting, error) {
	return s.sLister.Settings(s.namespace).Get(name)
}

// GetSetting will automatically fill the non-existing setting if it's a valid
// setting name.
// The function will not return nil for *longhorn.Setting when error is nil
func (s *DataStore) GetSetting(sName types.SettingName) (*longhorn.Setting, error) {
	definition, ok := types.SettingDefinitions[sName]
	if !ok {
		return nil, fmt.Errorf("setting %v is not supported", sName)
	}
	resultRO, err := s.getSettingRO(string(sName))
	if err != nil {
		if !ErrorIsNotFound(err) {
			return nil, err
		}
		resultRO = &longhorn.Setting{
			ObjectMeta: metav1.ObjectMeta{
				Name: string(sName),
			},
			Setting: types.Setting{
				Value: definition.Default,
			},
		}
	}
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) GetSettingValueExisted(sName types.SettingName) (string, error) {
	setting, err := s.GetSetting(sName)
	if err != nil {
		return "", err
	}
	if setting.Value == "" {
		return "", fmt.Errorf("setting %v is empty", sName)
	}
	return setting.Value, nil
}

func (s *DataStore) ListSettings() (map[types.SettingName]*longhorn.Setting, error) {
	itemMap := make(map[types.SettingName]*longhorn.Setting)

	list, err := s.sLister.Settings(s.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, itemRO := range list {
		// Cannot use cached object from lister
		settingField := types.SettingName(itemRO.Name)
		// Ignore the items that we don't recongize
		if _, ok := types.SettingDefinitions[settingField]; ok {
			itemMap[settingField] = itemRO.DeepCopy()
		}
	}
	// fill up the missing entries
	for sName, definition := range types.SettingDefinitions {
		if _, ok := itemMap[sName]; !ok {
			itemMap[sName] = &longhorn.Setting{
				ObjectMeta: metav1.ObjectMeta{
					Name: string(sName),
				},
				Setting: types.Setting{
					Value: definition.Default,
				},
			}
		}
	}
	return itemMap, nil
}

func (s *DataStore) GetCredentialFromSecret(secretName string) (map[string]string, error) {
	secret, err := s.kubeClient.CoreV1().Secrets(s.namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	credentialSecret := make(map[string]string)
	if secret.Data != nil {
		credentialSecret[types.AWSAccessKey] = string(secret.Data[types.AWSAccessKey])
		credentialSecret[types.AWSSecretKey] = string(secret.Data[types.AWSSecretKey])
		credentialSecret[types.AWSEndPoint] = string(secret.Data[types.AWSEndPoint])
		credentialSecret[types.AWSCert] = string(secret.Data[types.AWSCert])
		credentialSecret[types.HTTPSProxy] = string(secret.Data[types.HTTPSProxy])
		credentialSecret[types.HTTPProxy] = string(secret.Data[types.HTTPProxy])
		credentialSecret[types.NOProxy] = string(secret.Data[types.NOProxy])
	}
	return credentialSecret, nil
}

func checkVolume(v *longhorn.Volume) error {
	size, err := util.ConvertSize(v.Spec.Size)
	if err != nil {
		return err
	}
	if v.Name == "" || size == 0 || v.Spec.NumberOfReplicas == 0 {
		return fmt.Errorf("BUG: missing required field %+v", v)
	}
	errs := validation.IsDNS1123Label(v.Name)
	if len(errs) != 0 {
		return fmt.Errorf("Invalid volume name: %+v", errs)
	}
	if len(v.Name) > NameMaximumLength {
		return fmt.Errorf("Volume name is too long %v, must be less than %v characters",
			v.Name, NameMaximumLength)
	}
	return nil
}

func tagVolumeLabel(volumeName string, obj runtime.Object) error {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	labels := metadata.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	for k, v := range types.GetVolumeLabels(volumeName) {
		labels[k] = v
	}
	metadata.SetLabels(labels)
	return nil
}

func fixupMetadata(volumeName string, obj runtime.Object) error {
	if err := tagVolumeLabel(volumeName, obj); err != nil {
		return err
	}
	if err := util.AddFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	return nil
}

func getVolumeSelector(volumeName string) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: types.GetVolumeLabels(volumeName),
	})
}

func GetOwnerReferencesForVolume(v *longhorn.Volume) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion: longhorn.SchemeGroupVersion.String(),
			Kind:       types.LonghornKindVolume,
			UID:        v.UID,
			Name:       v.Name,
		},
	}
}

func (s *DataStore) CreateVolume(v *longhorn.Volume) (*longhorn.Volume, error) {
	if err := checkVolume(v); err != nil {
		return nil, err
	}
	if err := fixupMetadata(v.Name, v); err != nil {
		return nil, err
	}
	ret, err := s.lhClient.LonghornV1beta1().Volumes(s.namespace).Create(v)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(v.Name, "volume", func(name string) (runtime.Object, error) {
		return s.getVolumeRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.Volume)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for volume")
	}
	return ret, nil
}

func (s *DataStore) UpdateVolume(v *longhorn.Volume) (*longhorn.Volume, error) {
	if err := checkVolume(v); err != nil {
		return nil, err
	}
	if err := fixupMetadata(v.Name, v); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().Volumes(s.namespace).Update(v)
	if err != nil {
		return nil, err
	}
	verifyUpdate(v.Name, obj, func(name string) (runtime.Object, error) {
		return s.getVolumeRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateVolumeStatus(v *longhorn.Volume) (*longhorn.Volume, error) {
	obj, err := s.lhClient.LonghornV1beta1().Volumes(s.namespace).UpdateStatus(v)
	if err != nil {
		return nil, err
	}
	verifyUpdate(v.Name, obj, func(name string) (runtime.Object, error) {
		return s.getVolumeRO(name)
	})
	return obj, nil
}

// DeleteVolume won't result in immediately deletion since finalizer was set by default
func (s *DataStore) DeleteVolume(name string) error {
	return s.lhClient.LonghornV1beta1().Volumes(s.namespace).Delete(name, &metav1.DeleteOptions{})
}

// RemoveFinalizerForVolume will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForVolume(obj *longhorn.Volume) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().Volumes(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for volume %v", obj.Name)
	}
	return nil
}

func (s *DataStore) getVolumeRO(name string) (*longhorn.Volume, error) {
	return s.vLister.Volumes(s.namespace).Get(name)
}

func (s *DataStore) GetVolume(name string) (*longhorn.Volume, error) {
	resultRO, err := s.vLister.Volumes(s.namespace).Get(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) ListVolumesRO() ([]*longhorn.Volume, error) {
	return s.vLister.Volumes(s.namespace).List(labels.Everything())
}

func (s *DataStore) ListVolumes() (map[string]*longhorn.Volume, error) {
	itemMap := make(map[string]*longhorn.Volume)

	list, err := s.ListVolumesRO()
	if err != nil {
		return nil, err
	}

	for _, itemRO := range list {
		// Cannot use cached object from lister
		itemMap[itemRO.Name] = itemRO.DeepCopy()
	}
	return itemMap, nil
}

func (s *DataStore) ListStandbyVolumesRO() (map[string]*longhorn.Volume, error) {
	itemMap := make(map[string]*longhorn.Volume)

	list, err := s.ListVolumesRO()
	if err != nil {
		return nil, err
	}

	for _, itemRO := range list {
		if itemRO.Spec.Standby {
			itemMap[itemRO.Name] = itemRO
		}
	}
	return itemMap, nil
}

func checkEngine(engine *longhorn.Engine) error {
	if engine.Name == "" || engine.Spec.VolumeName == "" {
		return fmt.Errorf("BUG: missing required field %+v", engine)
	}
	return nil
}

func (s *DataStore) CreateEngine(e *longhorn.Engine) (*longhorn.Engine, error) {
	if err := checkEngine(e); err != nil {
		return nil, err
	}
	if err := fixupMetadata(e.Spec.VolumeName, e); err != nil {
		return nil, err
	}
	if err := tagNodeLabel(e.Spec.NodeID, e); err != nil {
		return nil, err
	}

	ret, err := s.lhClient.LonghornV1beta1().Engines(s.namespace).Create(e)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(e.Name, "engine", func(name string) (runtime.Object, error) {
		return s.getEngineRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.Engine)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for engine")
	}

	return ret, nil
}

func (s *DataStore) UpdateEngine(e *longhorn.Engine) (*longhorn.Engine, error) {
	if err := checkEngine(e); err != nil {
		return nil, err
	}
	if err := fixupMetadata(e.Spec.VolumeName, e); err != nil {
		return nil, err
	}
	if err := tagNodeLabel(e.Spec.NodeID, e); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().Engines(s.namespace).Update(e)
	if err != nil {
		return nil, err
	}
	verifyUpdate(e.Name, obj, func(name string) (runtime.Object, error) {
		return s.getEngineRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateEngineStatus(e *longhorn.Engine) (*longhorn.Engine, error) {
	obj, err := s.lhClient.LonghornV1beta1().Engines(s.namespace).UpdateStatus(e)
	if err != nil {
		return nil, err
	}
	verifyUpdate(e.Name, obj, func(name string) (runtime.Object, error) {
		return s.getEngineRO(name)
	})
	return obj, nil
}

// DeleteEngine won't result in immediately deletion since finalizer was set by default
func (s *DataStore) DeleteEngine(name string) error {
	return s.lhClient.LonghornV1beta1().Engines(s.namespace).Delete(name, &metav1.DeleteOptions{})
}

// RemoveFinalizerForEngine will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForEngine(obj *longhorn.Engine) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().Engines(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for engine %v", obj.Name)
	}
	return nil
}

func (s *DataStore) getEngineRO(name string) (*longhorn.Engine, error) {
	return s.eLister.Engines(s.namespace).Get(name)
}

func (s *DataStore) getEngine(name string) (*longhorn.Engine, error) {
	resultRO, err := s.getEngineRO(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) GetEngine(name string) (*longhorn.Engine, error) {
	return s.eLister.Engines(s.namespace).Get(name)
}

func (s *DataStore) listEngines(selector labels.Selector) (map[string]*longhorn.Engine, error) {
	list, err := s.eLister.Engines(s.namespace).List(selector)
	if err != nil {
		return nil, err
	}
	engines := map[string]*longhorn.Engine{}
	for _, e := range list {
		// Cannot use cached object from lister
		engines[e.Name] = e.DeepCopy()
	}
	return engines, nil
}

func (s *DataStore) ListEngines() (map[string]*longhorn.Engine, error) {
	return s.listEngines(labels.Everything())
}

func (s *DataStore) ListEnginesRO() ([]*longhorn.Engine, error) {
	return s.eLister.Engines(s.namespace).List(labels.Everything())
}

func (s *DataStore) ListVolumeEngines(volumeName string) (map[string]*longhorn.Engine, error) {
	selector, err := getVolumeSelector(volumeName)
	if err != nil {
		return nil, err
	}
	return s.listEngines(selector)
}

func checkReplica(r *longhorn.Replica) error {
	if r.Name == "" || r.Spec.VolumeName == "" {
		return fmt.Errorf("BUG: missing required field %+v", r)
	}
	if (r.Status.CurrentState == types.InstanceStateRunning) != (r.Status.IP != "") {
		return fmt.Errorf("BUG: instance state and IP wasn't in sync %+v", r)
	}
	return nil
}

func (s *DataStore) CreateReplica(r *longhorn.Replica) (*longhorn.Replica, error) {
	if err := checkReplica(r); err != nil {
		return nil, err
	}
	if err := fixupMetadata(r.Spec.VolumeName, r); err != nil {
		return nil, err
	}
	if err := tagNodeLabel(r.Spec.NodeID, r); err != nil {
		return nil, err
	}

	ret, err := s.lhClient.LonghornV1beta1().Replicas(s.namespace).Create(r)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(r.Name, "replica", func(name string) (runtime.Object, error) {
		return s.getReplicaRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.Replica)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for replica")
	}

	return ret, nil
}

func (s *DataStore) UpdateReplica(r *longhorn.Replica) (*longhorn.Replica, error) {
	if err := checkReplica(r); err != nil {
		return nil, err
	}
	if err := fixupMetadata(r.Spec.VolumeName, r); err != nil {
		return nil, err
	}
	if err := tagNodeLabel(r.Spec.NodeID, r); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().Replicas(s.namespace).Update(r)
	if err != nil {
		return nil, err
	}
	verifyUpdate(r.Name, obj, func(name string) (runtime.Object, error) {
		return s.getReplicaRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateReplicaStatus(r *longhorn.Replica) (*longhorn.Replica, error) {
	if err := checkReplica(r); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().Replicas(s.namespace).UpdateStatus(r)
	if err != nil {
		return nil, err
	}
	verifyUpdate(r.Name, obj, func(name string) (runtime.Object, error) {
		return s.getReplicaRO(name)
	})
	return obj, nil
}

// DeleteReplica won't result in immediately deletion since finalizer was set by default
func (s *DataStore) DeleteReplica(name string) error {
	return s.lhClient.LonghornV1beta1().Replicas(s.namespace).Delete(name, &metav1.DeleteOptions{})
}

// RemoveFinalizerForReplica will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForReplica(obj *longhorn.Replica) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().Replicas(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for replica %v", obj.Name)
	}
	return nil
}

func (s *DataStore) GetReplica(name string) (*longhorn.Replica, error) {
	result, err := s.getReplica(name)
	if err != nil {
		return nil, err
	}
	return s.fixupReplica(result)
}

func (s *DataStore) getReplicaRO(name string) (*longhorn.Replica, error) {
	return s.rLister.Replicas(s.namespace).Get(name)
}

func (s *DataStore) getReplica(name string) (*longhorn.Replica, error) {
	resultRO, err := s.rLister.Replicas(s.namespace).Get(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) listReplicas(selector labels.Selector) (map[string]*longhorn.Replica, error) {
	list, err := s.rLister.Replicas(s.namespace).List(selector)
	if err != nil {
		return nil, err
	}

	itemMap := map[string]*longhorn.Replica{}
	for _, itemRO := range list {
		// Cannot use cached object from lister
		itemMap[itemRO.Name], err = s.fixupReplica(itemRO.DeepCopy())
		if err != nil {
			return nil, err
		}
	}
	return itemMap, nil
}

func (s *DataStore) ListReplicas() (map[string]*longhorn.Replica, error) {
	return s.listReplicas(labels.Everything())
}

func (s *DataStore) ListVolumeReplicas(volumeName string) (map[string]*longhorn.Replica, error) {
	selector, err := getVolumeSelector(volumeName)
	if err != nil {
		return nil, err
	}
	return s.listReplicas(selector)
}

func (s *DataStore) fixupReplica(replica *longhorn.Replica) (*longhorn.Replica, error) {
	return replica, nil
}

// ReplicaAddressToReplicaName will directly return the address if the format is invalid or the replica is not found.
func ReplicaAddressToReplicaName(address string, rs []*longhorn.Replica) string {
	addressComponents := strings.Split(strings.TrimPrefix(address, "tcp://"), ":")
	// The address format should be `<IP>:<Port>` after removing the prefix "tcp://".
	if len(addressComponents) != 2 {
		return address
	}
	for _, r := range rs {
		if addressComponents[0] == r.Status.IP && addressComponents[1] == strconv.Itoa(r.Status.Port) {
			return r.Name
		}
	}
	// Cannot find matching replica by the address, replica may be removed already. Use address instead.
	return address
}

func GetOwnerReferencesForEngineImage(ei *longhorn.EngineImage) []metav1.OwnerReference {
	blockOwnerDeletion := true
	return []metav1.OwnerReference{
		{
			APIVersion:         longhorn.SchemeGroupVersion.String(),
			Kind:               types.LonghornKindEngineImage,
			Name:               ei.Name,
			UID:                ei.UID,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}
}

func (s *DataStore) CreateEngineImage(img *longhorn.EngineImage) (*longhorn.EngineImage, error) {
	if err := util.AddFinalizer(longhornFinalizerKey, img); err != nil {
		return nil, err
	}
	ret, err := s.lhClient.LonghornV1beta1().EngineImages(s.namespace).Create(img)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(img.Name, "engine image", func(name string) (runtime.Object, error) {
		return s.getEngineImageRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.EngineImage)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for engine image")
	}

	return ret, nil
}

func (s *DataStore) UpdateEngineImage(img *longhorn.EngineImage) (*longhorn.EngineImage, error) {
	if err := util.AddFinalizer(longhornFinalizerKey, img); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().EngineImages(s.namespace).Update(img)
	if err != nil {
		return nil, err
	}
	verifyUpdate(img.Name, obj, func(name string) (runtime.Object, error) {
		return s.getEngineImageRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateEngineImageStatus(img *longhorn.EngineImage) (*longhorn.EngineImage, error) {
	obj, err := s.lhClient.LonghornV1beta1().EngineImages(s.namespace).UpdateStatus(img)
	if err != nil {
		return nil, err
	}
	verifyUpdate(img.Name, obj, func(name string) (runtime.Object, error) {
		return s.getEngineImageRO(name)
	})
	return obj, nil
}

// DeleteEngineImage won't result in immediately deletion since finalizer was set by default
func (s *DataStore) DeleteEngineImage(name string) error {
	propagation := metav1.DeletePropagationForeground
	return s.lhClient.LonghornV1beta1().EngineImages(s.namespace).Delete(name, &metav1.DeleteOptions{PropagationPolicy: &propagation})
}

// RemoveFinalizerForEngineImage will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForEngineImage(obj *longhorn.EngineImage) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().EngineImages(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for engine image %v", obj.Name)
	}
	return nil
}

func (s *DataStore) getEngineImageRO(name string) (*longhorn.EngineImage, error) {
	return s.iLister.EngineImages(s.namespace).Get(name)
}

func (s *DataStore) getEngineImage(name string) (*longhorn.EngineImage, error) {
	resultRO, err := s.getEngineImageRO(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) GetEngineImage(name string) (*longhorn.EngineImage, error) {
	result, err := s.getEngineImage(name)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *DataStore) ListEngineImages() (map[string]*longhorn.EngineImage, error) {
	itemMap := map[string]*longhorn.EngineImage{}

	list, err := s.iLister.EngineImages(s.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, itemRO := range list {
		// Cannot use cached object from lister
		itemMap[itemRO.Name] = itemRO.DeepCopy()
	}
	return itemMap, nil
}

func (s *DataStore) CreateNode(node *longhorn.Node) (*longhorn.Node, error) {
	if err := util.AddFinalizer(longhornFinalizerKey, node); err != nil {
		return nil, err
	}
	ret, err := s.lhClient.LonghornV1beta1().Nodes(s.namespace).Create(node)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(node.Name, "node", func(name string) (runtime.Object, error) {
		return s.getNodeRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.Node)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for node")
	}

	return ret, nil
}

// CreateDefaultNode will create the default Disk at the value of the DefaultDataPath Setting only if Create Default
// Disk on Labeled Nodes has been disabled.
func (s *DataStore) CreateDefaultNode(name string) (*longhorn.Node, error) {
	requireLabel, err := s.GetSettingAsBool(types.SettingNameCreateDefaultDiskLabeledNodes)
	if err != nil {
		return nil, err
	}
	node := &longhorn.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: types.NodeSpec{
			Name:            name,
			AllowScheduling: true,
			Tags:            []string{},
		},
	}

	// For newly added node, the customized default disks will be applied only if the setting is enabled.
	if !requireLabel {
		// Note: this part wasn't moved to the controller is because
		// this will be done only once.
		// If user remove all the disks on the node, the default disk
		// will not be recreated automatically
		dataPath, err := s.GetSettingValueExisted(types.SettingNameDefaultDataPath)
		if err != nil {
			return nil, err
		}
		disks, err := types.CreateDefaultDisk(dataPath)
		if err != nil {
			return nil, err
		}
		node.Spec.Disks = disks
	}

	return s.CreateNode(node)
}

func (s *DataStore) getNodeRO(name string) (*longhorn.Node, error) {
	return s.nLister.Nodes(s.namespace).Get(name)
}

func (s *DataStore) GetNode(name string) (*longhorn.Node, error) {
	resultRO, err := s.getNodeRO(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) UpdateNode(node *longhorn.Node) (*longhorn.Node, error) {
	obj, err := s.lhClient.LonghornV1beta1().Nodes(s.namespace).Update(node)
	if err != nil {
		return nil, err
	}
	verifyUpdate(node.Name, obj, func(name string) (runtime.Object, error) {
		return s.getNodeRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateNodeStatus(node *longhorn.Node) (*longhorn.Node, error) {
	obj, err := s.lhClient.LonghornV1beta1().Nodes(s.namespace).UpdateStatus(node)
	if err != nil {
		return nil, err
	}
	verifyUpdate(node.Name, obj, func(name string) (runtime.Object, error) {
		return s.getNodeRO(name)
	})
	return obj, nil
}

func (s *DataStore) ListNodes() (map[string]*longhorn.Node, error) {
	itemMap := make(map[string]*longhorn.Node)

	nodeList, err := s.nLister.Nodes(s.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, node := range nodeList {
		// Cannot use cached object from lister
		itemMap[node.Name] = node.DeepCopy()
	}
	return itemMap, nil
}

func (s *DataStore) GetRandomReadyNode() (*longhorn.Node, error) {
	logrus.Debugf("Prepare to find a random ready node")
	nodeList, err := s.ListNodes()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get random ready node")
	}
	var usableNode *longhorn.Node
	for name := range nodeList {
		node := nodeList[name]
		readyCondition := types.GetCondition(node.Status.Conditions, types.NodeConditionTypeReady)
		if readyCondition.Status == types.ConditionStatusTrue && node.Spec.AllowScheduling == true {
			usableNode = node
			break
		}
	}
	if usableNode == nil {
		return nil, fmt.Errorf("unable to get a ready node")
	}
	return usableNode, nil
}

// RemoveFinalizerForNode will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForNode(obj *longhorn.Node) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().Nodes(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for node %v", obj.Name)
	}
	return nil
}

func (s *DataStore) IsNodeDownOrDeleted(name string) (bool, error) {
	if name == "" {
		return false, errors.New("no node name provided to check node down or deleted")
	}
	node, err := s.getNodeRO(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	cond := types.GetCondition(node.Status.Conditions, types.NodeConditionTypeReady)
	if cond.Status == types.ConditionStatusFalse &&
		(cond.Reason == string(types.NodeConditionReasonKubernetesNodeGone) ||
			cond.Reason == string(types.NodeConditionReasonKubernetesNodeNotReady)) {
		return true, nil
	}
	return false, nil
}

func getNodeSelector(nodeName string) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			types.LonghornNodeKey: nodeName,
		},
	})
}

func (s *DataStore) ListReplicasByNode(name string) (map[string][]*longhorn.Replica, error) {
	nodeSelector, err := getNodeSelector(name)
	if err != nil {
		return nil, err
	}
	replicaList, err := s.rLister.Replicas(s.namespace).List(nodeSelector)
	if err != nil {
		return nil, err
	}

	replicaDiskMap := map[string][]*longhorn.Replica{}
	for _, replica := range replicaList {
		if _, ok := replicaDiskMap[replica.Spec.DiskID]; !ok {
			replicaDiskMap[replica.Spec.DiskID] = []*longhorn.Replica{}
		}
		replicaDiskMap[replica.Spec.DiskID] = append(replicaDiskMap[replica.Spec.DiskID], replica.DeepCopy())
	}
	return replicaDiskMap, nil
}

func tagNodeLabel(nodeID string, obj runtime.Object) error {
	// fix longhornnode label for object
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	labels := metadata.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[types.LonghornNodeKey] = nodeID
	metadata.SetLabels(labels)
	return nil
}

func GetOwnerReferencesForNode(node *longhorn.Node) []metav1.OwnerReference {
	blockOwnerDeletion := true
	return []metav1.OwnerReference{
		{
			APIVersion:         longhorn.SchemeGroupVersion.String(),
			Kind:               types.LonghornKindNode,
			Name:               node.Name,
			UID:                node.UID,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}
}

func (s *DataStore) GetSettingAsInt(settingName types.SettingName) (int64, error) {
	definition, ok := types.SettingDefinitions[settingName]
	if !ok {
		return -1, fmt.Errorf("setting %v is not supported", settingName)
	}
	settings, err := s.GetSetting(settingName)
	if err != nil {
		return -1, err
	}
	value := settings.Value

	if definition.Type == types.SettingTypeInt {
		result, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return -1, err
		}
		return result, nil
	}

	return -1, fmt.Errorf("The %v setting value couldn't change to integer, value is %v ", string(settingName), value)
}

func (s *DataStore) GetSettingAsBool(settingName types.SettingName) (bool, error) {
	definition, ok := types.SettingDefinitions[settingName]
	if !ok {
		return false, fmt.Errorf("setting %v is not supported", settingName)
	}
	settings, err := s.GetSetting(settingName)
	if err != nil {
		return false, err
	}
	value := settings.Value

	if definition.Type == types.SettingTypeBool {
		result, err := strconv.ParseBool(value)
		if err != nil {
			return false, err
		}
		return result, nil
	}

	return false, fmt.Errorf("The %v setting value couldn't be converted to bool, value is %v ", string(settingName), value)
}

func (s *DataStore) ResetMonitoringEngineStatus(e *longhorn.Engine) (*longhorn.Engine, error) {
	e.Status.Endpoint = ""
	e.Status.LastRestoredBackup = ""
	e.Status.ReplicaModeMap = nil
	e.Status.BackupStatus = nil
	e.Status.RestoreStatus = nil
	e.Status.PurgeStatus = nil
	e.Status.RebuildStatus = nil
	ret, err := s.UpdateEngineStatus(e)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to reset engine status for %v", e.Name)
	}
	return ret, nil
}

func (s *DataStore) DeleteNode(name string) error {
	return s.lhClient.LonghornV1beta1().Nodes(s.namespace).Delete(name, &metav1.DeleteOptions{})
}

func (s *DataStore) ListEnginesByNode(name string) ([]*longhorn.Engine, error) {
	nodeSelector, err := getNodeSelector(name)
	engineList, err := s.eLister.Engines(s.namespace).List(nodeSelector)
	if err != nil {
		return nil, err
	}
	return engineList, nil
}

func GetOwnerReferencesForInstanceManager(im *longhorn.InstanceManager) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion: longhorn.SchemeGroupVersion.String(),
			Kind:       types.LonghornKindInstanceManager,
			Name:       im.Name,
			UID:        im.UID,
		},
	}
}

func (s *DataStore) CreateInstanceManager(im *longhorn.InstanceManager) (*longhorn.InstanceManager, error) {
	if err := util.AddFinalizer(longhornFinalizerKey, im); err != nil {
		return nil, err
	}
	ret, err := s.lhClient.LonghornV1beta1().InstanceManagers(s.namespace).Create(im)
	if err != nil {
		return nil, err
	}
	if SkipListerCheck {
		return ret, nil
	}

	obj, err := verifyCreation(im.Name, "instance manager", func(name string) (runtime.Object, error) {
		return s.getInstanceManagerRO(name)
	})
	if err != nil {
		return nil, err
	}
	ret, ok := obj.(*longhorn.InstanceManager)
	if !ok {
		return nil, fmt.Errorf("BUG: datastore: verifyCreation returned wrong type for instance manager")
	}

	return ret, nil
}

// DeleteInstanceManager won't result in immediately deletion since finalizer was set by default
func (s *DataStore) DeleteInstanceManager(name string) error {
	return s.lhClient.LonghornV1beta1().InstanceManagers(s.namespace).Delete(name, &metav1.DeleteOptions{})
}

func (s *DataStore) getInstanceManagerRO(name string) (*longhorn.InstanceManager, error) {
	return s.imLister.InstanceManagers(s.namespace).Get(name)
}

func (s *DataStore) getInstanceManager(name string) (*longhorn.InstanceManager, error) {
	resultRO, err := s.getInstanceManagerRO(name)
	if err != nil {
		return nil, err
	}
	// Cannot use cached object from lister
	return resultRO.DeepCopy(), nil
}

func (s *DataStore) GetInstanceManager(name string) (*longhorn.InstanceManager, error) {
	result, err := s.getInstanceManager(name)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func CheckInstanceManagerType(im *longhorn.InstanceManager) (types.InstanceManagerType, error) {
	imTypeLabelkey := types.GetLonghornLabelKey(types.LonghornLabelInstanceManagerType)
	imType, exist := im.Labels[imTypeLabelkey]
	if !exist {
		return types.InstanceManagerType(""), fmt.Errorf("no label %v in instance manager %v", imTypeLabelkey, im.Name)
	}

	switch imType {
	case string(types.InstanceManagerTypeEngine):
		return types.InstanceManagerTypeEngine, nil
	case string(types.InstanceManagerTypeReplica):
		return types.InstanceManagerTypeReplica, nil
	}

	return types.InstanceManagerType(""), fmt.Errorf("unknown type %v for instance manager %v", imType, im.Name)
}

func (s *DataStore) ListInstanceManagersBySelector(node, instanceManagerImage string, managerType types.InstanceManagerType) (map[string]*longhorn.InstanceManager, error) {
	itemMap := map[string]*longhorn.InstanceManager{}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: types.GetInstanceManagerLabels(node, instanceManagerImage, managerType),
	})
	if err != nil {
		return nil, err
	}

	listRO, err := s.imLister.InstanceManagers(s.namespace).List(selector)
	if err != nil {
		return nil, err
	}
	for _, itemRO := range listRO {
		// Cannot use cached object from lister
		itemMap[itemRO.Name] = itemRO.DeepCopy()
	}
	return itemMap, nil
}

func (s *DataStore) GetInstanceManagerByInstance(obj interface{}) (*longhorn.InstanceManager, error) {
	var (
		name, nodeID string
		imType       types.InstanceManagerType
	)

	image, err := s.GetSettingValueExisted(types.SettingNameDefaultInstanceManagerImage)
	if err != nil {
		return nil, err
	}

	switch obj.(type) {
	case *longhorn.Engine:
		engine := obj.(*longhorn.Engine)
		name = engine.Name
		nodeID = engine.Spec.NodeID
		imType = types.InstanceManagerTypeEngine
	case *longhorn.Replica:
		replica := obj.(*longhorn.Replica)
		name = replica.Name
		nodeID = replica.Spec.NodeID
		imType = types.InstanceManagerTypeReplica
	default:
		return nil, fmt.Errorf("unknown type for GetInstanceManagerByInstance, %+v", obj)
	}
	if nodeID == "" {
		return nil, fmt.Errorf("invalid request for GetInstanceManagerByInstance: no NodeID specified for instance %v", name)
	}

	imMap, err := s.ListInstanceManagersBySelector(nodeID, image, imType)
	if err != nil {
		return nil, err
	}
	if len(imMap) == 1 {
		for _, im := range imMap {
			return im, nil
		}

	}
	return nil, fmt.Errorf("can not find the only available instance manager for instance %v, node %v, instance manager image %v, type %v", name, nodeID, image, imType)
}

func (s *DataStore) ListInstanceManagersByNode(node string, imType types.InstanceManagerType) (map[string]*longhorn.InstanceManager, error) {
	return s.ListInstanceManagersBySelector(node, "", imType)
}

func (s *DataStore) ListInstanceManagers() (map[string]*longhorn.InstanceManager, error) {
	itemMap := map[string]*longhorn.InstanceManager{}

	list, err := s.imLister.InstanceManagers(s.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, itemRO := range list {
		// Cannot use cached object from lister
		itemMap[itemRO.Name] = itemRO.DeepCopy()
	}
	return itemMap, nil
}

// RemoveFinalizerForInstanceManager will result in deletion if DeletionTimestamp was set
func (s *DataStore) RemoveFinalizerForInstanceManager(obj *longhorn.InstanceManager) error {
	if !util.FinalizerExists(longhornFinalizerKey, obj) {
		// finalizer already removed
		return nil
	}
	if err := util.RemoveFinalizer(longhornFinalizerKey, obj); err != nil {
		return err
	}
	_, err := s.lhClient.LonghornV1beta1().InstanceManagers(s.namespace).Update(obj)
	if err != nil {
		// workaround `StorageError: invalid object, Code: 4` due to empty object
		if obj.DeletionTimestamp != nil {
			return nil
		}
		return errors.Wrapf(err, "unable to remove finalizer for instance manager %v", obj.Name)
	}
	return nil
}

func (s *DataStore) UpdateInstanceManager(im *longhorn.InstanceManager) (*longhorn.InstanceManager, error) {
	if err := util.AddFinalizer(longhornFinalizerKey, im); err != nil {
		return nil, err
	}

	obj, err := s.lhClient.LonghornV1beta1().InstanceManagers(s.namespace).Update(im)
	if err != nil {
		return nil, err
	}
	verifyUpdate(im.Name, obj, func(name string) (runtime.Object, error) {
		return s.getInstanceManagerRO(name)
	})
	return obj, nil
}

func (s *DataStore) UpdateInstanceManagerStatus(im *longhorn.InstanceManager) (*longhorn.InstanceManager, error) {
	obj, err := s.lhClient.LonghornV1beta1().InstanceManagers(s.namespace).UpdateStatus(im)
	if err != nil {
		return nil, err
	}
	verifyUpdate(im.Name, obj, func(name string) (runtime.Object, error) {
		return s.getInstanceManagerRO(name)
	})
	return obj, nil
}

func verifyCreation(name, kind string, getMethod func(name string) (runtime.Object, error)) (runtime.Object, error) {
	// WORKAROUND: The immedidate read after object's creation can fail.
	// See https://github.com/longhorn/longhorn/issues/133
	var (
		ret runtime.Object
		err error
	)
	for i := 0; i < VerificationRetryCounts; i++ {
		if ret, err = getMethod(name); err == nil {
			break
		}
		if !ErrorIsNotFound(err) {
			break
		}
		time.Sleep(VerificationRetryInterval)
	}
	if err != nil {
		return nil, fmt.Errorf("Unable to verify the existance of newly created %s %s: %v", kind, name, err)
	}
	return ret, nil
}

func verifyUpdate(name string, obj runtime.Object, getMethod func(name string) (runtime.Object, error)) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		logrus.Errorf("BUG: datastore: cannot verify update for %v (%+v) because cannot get accessor: %v", name, obj, err)
		return
	}
	minimalResourceVersion := accessor.GetResourceVersion()
	verified := false
	for i := 0; i < VerificationRetryCounts; i++ {
		ret, err := getMethod(name)
		if err != nil {
			logrus.Errorf("datastore: failed to get updated object %v", name)
			return
		}
		accessor, err := meta.Accessor(ret)
		if err != nil {
			logrus.Errorf("BUG: datastore: cannot verify update for %v because cannot get accessor for updated object: %v", name, err)
			return
		}
		if resourceVersionAtLeast(accessor.GetResourceVersion(), minimalResourceVersion) {
			verified = true
			break
		}
		time.Sleep(VerificationRetryInterval)
	}
	if !verified {
		logrus.Errorf("Unable to verify the update of %s", name)
	}
}

// resourceVersionAtLeast depends on the Kubernetes internal resource version implmentation
// See https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#concurrency-control-and-consistency
func resourceVersionAtLeast(curr, min string) bool {
	// skip unit testing code
	if curr == "" || min == "" {
		return true
	}
	currVersion, err := strconv.ParseInt(curr, 10, 64)
	if err != nil {
		logrus.Errorf("datastore: failed to parse current resource version %v: %v", curr, err)
		return false
	}
	minVersion, err := strconv.ParseInt(min, 10, 64)
	if err != nil {
		logrus.Errorf("datastore: failed to parse minimal resource version %v: %v", min, err)
		return false
	}
	return currVersion >= minVersion
}

func (s *DataStore) IsEngineImageCLIAPIVersionOne(imageName string) (bool, error) {
	if imageName == "" {
		return false, fmt.Errorf("cannot check the CLI API Version based on empty image name")
	}

	ei, err := s.GetEngineImage(types.GetEngineImageChecksumName(imageName))
	if err != nil {
		return false, errors.Wrapf(err, "failed to get engine image object based on image name %v", imageName)
	}

	if ei.Status.CLIAPIVersion == 1 {
		logrus.Debugf("Found engine image object %v whose CLIAPIVersion was 1", ei.Name)
		return true, nil
	}
	return false, nil
}

func (s *DataStore) IsEngineImageCLIAPIVersionLessThanThree(imageName string) (bool, error) {
	if imageName == "" {
		return false, fmt.Errorf("cannot check the CLI API Version based on empty image name")
	}

	ei, err := s.GetEngineImage(types.GetEngineImageChecksumName(imageName))
	if err != nil {
		return false, errors.Wrapf(err, "failed to get engine image object based on image name %v", imageName)
	}

	if ei.Status.CLIAPIVersion < 3 {
		logrus.Debugf("Found engine image object %v whose CLIAPIVersion was less than 3", ei.Name)
		return true, nil
	}
	return false, nil
}
