package actionmanager

import (
	"context"
	"log"

	clientset "github.com/kyma-project/kyma/components/kyma-operator/pkg/client/clientset/versioned"
	listers "github.com/kyma-project/kyma/components/kyma-operator/pkg/client/listers/installer/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const actionLabelName = "action"

// ActionManager .
type ActionManager interface {
	// RemoveActionLabel .
	RemoveActionLabel(name string, namespace string) error
}

//KymaActionManager .
type KymaActionManager struct {
	installationLister listers.InstallationLister
	internalClientset  *clientset.Clientset
}

// NewKymaActionManager .
func NewKymaActionManager(internalClientset *clientset.Clientset, installationLister listers.InstallationLister) *KymaActionManager {
	return &KymaActionManager{
		installationLister: installationLister,
		internalClientset:  internalClientset,
	}
}

//RemoveActionLabel removes "action" label from Installation CR
func (am *KymaActionManager) RemoveActionLabel(name string, namespace string) error {

	retryErr := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return true // retry on every kind of error
	}, func() error {
		instObj, getErr := am.installationLister.Installations(namespace).Get(name)
		if getErr != nil {
			log.Println("Error on getting installation object")
			log.Println(getErr)
			return getErr
		}

		installationCopy := instObj.DeepCopy()
		labels := installationCopy.GetLabels()
		delete(labels, actionLabelName)
		installationCopy.SetLabels(labels)

		_, updateErr := am.internalClientset.InstallerV1alpha1().Installations(namespace).Update(context.TODO(), installationCopy, v1.UpdateOptions{})
		return updateErr
	})

	if retryErr != nil {
		log.Println("Error on removing installation action")
		log.Println(retryErr)
		return retryErr
	}

	return nil
}
