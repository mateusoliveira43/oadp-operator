package lib

import (
	"context"
	"log"
	"time"

	oadpv1alpha1 "github.com/openshift/oadp-operator/api/v1alpha1"
	"github.com/openshift/oadp-operator/pkg/common"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func HasCorrectNumResticPods(namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		client, err := setUpClient()
		if err != nil {
			return false, err
		}
		resticOptions := metav1.ListOptions{
			FieldSelector: "metadata.name=" + common.NodeAgent,
		}
		resticDaemeonSet, err := client.AppsV1().DaemonSets(namespace).List(context.TODO(), resticOptions)
		if err != nil {
			return false, err
		}
		var numScheduled int32
		var numDesired int32

		for _, daemonSetInfo := range (*resticDaemeonSet).Items {
			numScheduled = daemonSetInfo.Status.CurrentNumberScheduled
			numDesired = daemonSetInfo.Status.DesiredNumberScheduled
		}
		// check correct num of Restic pods are initialized
		if numScheduled != 0 && numDesired != 0 {
			if numScheduled == numDesired {
				return true, nil
			}
		}
		if numDesired == 0 {
			return true, nil
		}
		return false, err
	}
}

func waitForDesiredResticPods(namespace string) error {
	return wait.PollImmediate(time.Second*5, time.Minute*2, HasCorrectNumResticPods(namespace))
}

func AreNodeAgentPodsRunning(namespace string) wait.ConditionFunc {
	log.Printf("Checking for correct number of running Node Agent pods...")
	return func() (bool, error) {
		err := waitForDesiredResticPods(namespace)
		if err != nil {
			return false, err
		}
		client, err := setUpClient()
		if err != nil {
			return false, err
		}
		resticPodOptions := metav1.ListOptions{
			LabelSelector: "name=" + common.NodeAgent,
		}
		// get pods in the oadp-operator-e2e namespace with label selector
		podList, err := client.CoreV1().Pods(namespace).List(context.TODO(), resticPodOptions)
		if err != nil {
			return false, nil
		}
		// loop until pod status is 'Running' or timeout
		for _, podInfo := range (*podList).Items {
			if podInfo.Status.Phase != "Running" {
				return false, err
			}
		}
		return true, err
	}
}

// keep for now
func IsResticDaemonsetDeleted(namespace string) wait.ConditionFunc {
	log.Printf("Checking if Restic daemonset has been deleted...")
	return func() (bool, error) {
		client, err := setUpClient()
		if err != nil {
			return false, nil
		}
		// Check for daemonSet
		_, err = client.AppsV1().DaemonSets(namespace).Get(context.Background(), common.NodeAgent, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
}

func (v *DpaCustomResource) DisableRestic(namespace string, instanceName string) error {
	err := v.SetClient()
	if err != nil {
		return err
	}
	dpa := &oadpv1alpha1.DataProtectionApplication{}
	err = v.Client.Get(context.Background(), client.ObjectKey{
		Namespace: v.Namespace,
		Name:      v.Name,
	}, dpa)
	if err != nil {
		return err
	}
	dpa.Spec.Configuration.Restic.Enable = pointer.Bool(false)

	err = v.Client.Update(context.Background(), dpa)
	if err != nil {
		return err
	}
	log.Printf("spec 'enable_restic' has been updated to false")
	return nil
}

func (v *DpaCustomResource) EnableResticNodeSelector(namespace string, s3Bucket string, credSecretRef string, instanceName string) error {
	err := v.SetClient()
	if err != nil {
		return err
	}
	dpa := &oadpv1alpha1.DataProtectionApplication{}
	err = v.Client.Get(context.Background(), client.ObjectKey{
		Namespace: v.Namespace,
		Name:      v.Name,
	}, dpa)
	if err != nil {
		return err
	}
	nodeSelector := map[string]string{"foo": "bar"}
	dpa.Spec.Configuration.Restic.PodConfig.NodeSelector = nodeSelector

	err = v.Client.Update(context.Background(), dpa)
	if err != nil {
		return err
	}
	log.Printf("spec 'restic_node_selector' has been updated")
	return nil
}

func ResticDaemonSetHasNodeSelector(namespace, key, value string) wait.ConditionFunc {
	return func() (bool, error) {
		client, err := setUpClient()
		if err != nil {
			return false, nil
		}
		ds, err := client.AppsV1().DaemonSets(namespace).Get(context.TODO(), common.NodeAgent, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		// verify daemonset has nodeSelector "foo": "bar"
		selector := ds.Spec.Template.Spec.NodeSelector[key]

		if selector == value {
			return true, nil
		}
		return false, err
	}
}

func GetResticDaemonsetList(namespace string) (*appsv1.DaemonSetList, error) {
	client, err := setUpClient()
	if err != nil {
		return nil, err
	}
	registryListOptions := metav1.ListOptions{
		LabelSelector: "component=velero",
	}
	// get pods in the oadp-operator-e2e namespace with label selector
	deploymentList, err := client.AppsV1().DaemonSets(namespace).List(context.TODO(), registryListOptions)
	if err != nil {
		return nil, err
	}
	return deploymentList, nil
}
