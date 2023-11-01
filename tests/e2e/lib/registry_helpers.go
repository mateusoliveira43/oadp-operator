package lib

import (
	"context"
	"fmt"
	"log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

func AreRegistryDeploymentsAvailable(c *kubernetes.Clientset, namespace string) wait.ConditionFunc {
	log.Printf("Checking for available registry deployments")
	return func() (bool, error) {
		// get pods in the oadp-operator-e2e namespace with label selector
		deploymentList, err := GetRegistryDeploymentList(c, namespace)
		if err != nil {
			return false, err
		}
		if len(deploymentList.Items) == 0 {
			return false, fmt.Errorf("registry deployment is not yet created")
		}
		// loop until deployment status is 'Running' or timeout
		for _, deploymentInfo := range deploymentList.Items {
			for _, conditions := range deploymentInfo.Status.Conditions {
				if conditions.Type == appsv1.DeploymentAvailable && conditions.Status != corev1.ConditionTrue {
					return false, fmt.Errorf("registry deployment is not yet available.\nconditions: %v", deploymentInfo.Status.Conditions)
				}
			}
		}
		return true, nil
	}
}

func GetRegistryDeploymentList(c *kubernetes.Clientset, namespace string) (*appsv1.DeploymentList, error) {
	registryListOptions := metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=Registry",
	}
	// get pods in the oadp-operator-e2e namespace with label selector
	deploymentList, err := c.AppsV1().Deployments(namespace).List(context.Background(), registryListOptions)
	if err != nil {
		return nil, err
	}
	return deploymentList, nil
}

func AreRegistryDeploymentsNotAvailable(c *kubernetes.Clientset, namespace string) wait.ConditionFunc {
	log.Printf("Checking for unavailable registry deployments")
	return func() (bool, error) {
		// get pods in the oadp-operator-e2e namespace with label selector
		deploymentList, err := GetRegistryDeploymentList(c, namespace)
		if err != nil {
			return false, err
		}
		// loop until deployment status is 'Running' or timeout
		for _, deploymentInfo := range deploymentList.Items {
			for _, conditions := range deploymentInfo.Status.Conditions {
				if conditions.Type == appsv1.DeploymentAvailable && conditions.Status == corev1.ConditionTrue {
					return false, fmt.Errorf("registry deployment is still available.\nconditions: %v", deploymentInfo.Status.Conditions)
				}
			}
		}
		return true, nil
	}
}
