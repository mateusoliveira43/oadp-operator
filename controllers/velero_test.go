package controllers

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/operator-framework/operator-lib/proxy"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oadpv1alpha1 "github.com/openshift/oadp-operator/api/v1alpha1"
	oadpclient "github.com/openshift/oadp-operator/pkg/client"
	"github.com/openshift/oadp-operator/pkg/common"
	"github.com/openshift/oadp-operator/pkg/velero/server"
)

const (
	proxyEnvKey                            = "HTTP_PROXY"
	proxyEnvValue                          = "http://proxy.example.com:8080"
	argsMetricsPortTest              int32 = 69420
	defaultFileSystemBackupTimeout         = "--fs-backup-timeout=4h"
	defaultRestoreResourcePriorities       = "--restore-resource-priorities=securitycontextconstraints,customresourcedefinitions,klusterletconfigs.config.open-cluster-management.io,managedcluster.cluster.open-cluster-management.io,namespaces,roles,rolebindings,clusterrolebindings,klusterletaddonconfig.agent.open-cluster-management.io,managedclusteraddon.addon.open-cluster-management.io,storageclasses,volumesnapshotclass.snapshot.storage.k8s.io,volumesnapshotcontents.snapshot.storage.k8s.io,volumesnapshots.snapshot.storage.k8s.io,datauploads.velero.io,persistentvolumes,persistentvolumeclaims,serviceaccounts,secrets,configmaps,limitranges,pods,replicasets.apps,clusterclasses.cluster.x-k8s.io,endpoints,services,-,clusterbootstraps.run.tanzu.vmware.com,clusters.cluster.x-k8s.io,clusterresourcesets.addons.cluster.x-k8s.io"
	defaultDisableInformerCache            = "--disable-informer-cache=false"
)

var (
	veleroDeploymentLabel = map[string]string{
		"app.kubernetes.io/name":       common.Velero,
		"app.kubernetes.io/instance":   "test-Velero-CR",
		"app.kubernetes.io/managed-by": common.OADPOperator,
		"app.kubernetes.io/component":  Server,
		"component":                    "velero",
		oadpv1alpha1.OadpOperatorLabel: "True",
	}
	veleroPodLabelAppend        = map[string]string{"deploy": "velero"}
	veleroDeploymentMatchLabels = common.AppendTTMapAsCopy(veleroDeploymentLabel, veleroPodLabelAppend)
	veleroPodAnnotations        = map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "8085",
		"prometheus.io/path":   "/metrics",
	}
	veleroPodObjectMeta = metav1.ObjectMeta{
		Labels:      veleroDeploymentMatchLabels,
		Annotations: veleroPodAnnotations,
	}
	baseEnvVars = []corev1.EnvVar{
		{Name: common.VeleroScratchDirEnvKey, Value: "/scratch"},
		{
			Name: common.VeleroNamespaceEnvKey,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
		{Name: common.LDLibraryPathEnvKey, Value: "/plugins"},
		{Name: "OPENSHIFT_IMAGESTREAM_BACKUP", Value: "true"},
	}

	baseVolumeMounts = []corev1.VolumeMount{
		{Name: "plugins", MountPath: "/plugins"},
		{Name: "scratch", MountPath: "/scratch"},
		{Name: "certs", MountPath: "/etc/ssl/certs"},
	}

	baseVolumes = []corev1.Volume{
		{
			Name:         "plugins",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         "certs",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	baseContainer = corev1.Container{
		Image:           common.AWSPluginImage,
		Name:            common.VeleroPluginForAWS,
		ImagePullPolicy: corev1.PullAlways,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: "File",
		VolumeMounts: []corev1.VolumeMount{
			{MountPath: "/target", Name: "plugins"},
		},
	}
)

type ReconcileVeleroControllerScenario struct {
	namespace string
	dpaName   string
	envVar    corev1.EnvVar
}

var _ = ginkgo.Describe("Test ReconcileVeleroDeployment function", func() {
	var (
		ctx                 = context.Background()
		currentTestScenario ReconcileVeleroControllerScenario
		updateTestScenario  = func(scenario ReconcileVeleroControllerScenario) {
			currentTestScenario = scenario
		}
	)

	ginkgo.AfterEach(func() {
		os.Unsetenv(currentTestScenario.envVar.Name)

		deployment := &appsv1.Deployment{}
		if k8sClient.Get(
			ctx,
			types.NamespacedName{
				Name:      common.Velero,
				Namespace: currentTestScenario.namespace,
			},
			deployment,
		) == nil {
			gomega.Expect(k8sClient.Delete(ctx, deployment)).To(gomega.Succeed())
		}

		dpa := &oadpv1alpha1.DataProtectionApplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:      currentTestScenario.dpaName,
				Namespace: currentTestScenario.namespace,
			},
		}
		gomega.Expect(k8sClient.Delete(ctx, dpa)).To(gomega.Succeed())

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: currentTestScenario.namespace,
			},
		}
		gomega.Expect(k8sClient.Delete(ctx, namespace)).To(gomega.Succeed())
	})

	ginkgo.DescribeTable("Check if Subscription Config environment variables are passed to Velero Containers",
		func(scenario ReconcileVeleroControllerScenario) {
			updateTestScenario(scenario)

			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: scenario.namespace,
				},
			}
			gomega.Expect(k8sClient.Create(ctx, namespace)).To(gomega.Succeed())

			dpa := &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      scenario.dpaName,
					Namespace: scenario.namespace,
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							NoDefaultBackupLocation: true,
						},
					},
				},
			}
			gomega.Expect(k8sClient.Create(ctx, dpa)).To(gomega.Succeed())

			// Subscription Config environment variables are passed to controller-manager Pod
			// https://github.com/operator-framework/operator-lifecycle-manager/blob/d8500d88932b17aa9b1853f0f26086f6ee6b35f9/doc/design/subscription-config.md
			os.Setenv(scenario.envVar.Name, scenario.envVar.Value)

			event := record.NewFakeRecorder(5)
			r := &DPAReconciler{
				Client:  k8sClient,
				Scheme:  testEnv.Scheme,
				Context: ctx,
				NamespacedName: types.NamespacedName{
					Name:      scenario.dpaName,
					Namespace: scenario.namespace,
				},
				EventRecorder: event,
			}
			result, err := r.ReconcileVeleroDeployment(logr.Discard())

			gomega.Expect(result).To(gomega.BeTrue())
			gomega.Expect(err).To(gomega.Not(gomega.HaveOccurred()))

			gomega.Expect(len(event.Events)).To(gomega.Equal(1))
			message := <-event.Events
			for _, word := range []string{"Normal", "VeleroDeploymentReconciled", "created"} {
				gomega.Expect(message).To(gomega.ContainSubstring(word))
			}

			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{
					Name:      common.Velero,
					Namespace: scenario.namespace,
				},
				deployment,
			)
			gomega.Expect(err).To(gomega.Not(gomega.HaveOccurred()))

			if slices.Contains(proxy.ProxyEnvNames, scenario.envVar.Name) {
				for _, container := range deployment.Spec.Template.Spec.Containers {
					gomega.Expect(container.Env).To(gomega.ContainElement(scenario.envVar))
				}
			} else {
				for _, container := range deployment.Spec.Template.Spec.Containers {
					gomega.Expect(container.Env).To(gomega.Not(gomega.ContainElement(scenario.envVar)))
				}
			}
		},
		ginkgo.Entry("Should add HTTP_PROXY environment variable to Velero Containers", ReconcileVeleroControllerScenario{
			namespace: "test-velero-environment-variables-1",
			dpaName:   "test-velero-environment-variables-1-dpa",
			envVar: corev1.EnvVar{
				Name:  "HTTP_PROXY",
				Value: "http://proxy.example.com:8080",
			},
		}),
		ginkgo.Entry("Should add HTTPS_PROXY environment variable to Velero Containers", ReconcileVeleroControllerScenario{
			namespace: "test-velero-environment-variables-2",
			dpaName:   "test-velero-environment-variables-2-dpa",
			envVar: corev1.EnvVar{
				Name:  "HTTPS_PROXY",
				Value: "localhost",
			},
		}),
		ginkgo.Entry("Should add NO_PROXY environment variable to Velero Containers", ReconcileVeleroControllerScenario{
			namespace: "test-velero-environment-variables-3",
			dpaName:   "test-velero-environment-variables-3-dpa",
			envVar: corev1.EnvVar{
				Name:  "NO_PROXY",
				Value: "1.1.1.1",
			},
		}),
		ginkgo.Entry("Should NOT add WRONG environment variable to Velero Containers", ReconcileVeleroControllerScenario{
			namespace: "test-velero-environment-variables-4",
			dpaName:   "test-velero-environment-variables-4-dpa",
			envVar: corev1.EnvVar{
				Name:  "WRONG",
				Value: "I do not know what is happening here",
			},
		}),
	)
})

func pluginContainer(name, image string) corev1.Container {
	container := baseContainer
	container.Name = name
	container.Image = image
	return container
}

func TestDPAReconciler_buildVeleroDeployment(t *testing.T) {
	type fields struct {
		Client         client.Client
		Scheme         *runtime.Scheme
		Log            logr.Logger
		Context        context.Context
		NamespacedName types.NamespacedName
		EventRecorder  record.EventRecorder
	}

	trueVal := true
	tests := []struct {
		name                 string
		fields               fields
		veleroDeployment     *appsv1.Deployment
		dpa                  *oadpv1alpha1.DataProtectionApplication
		wantErr              bool
		wantVeleroDeployment *appsv1.Deployment
		clientObjects        []client.Object
		testProxy            bool
	}{
		{
			name: "DPA CR is nil",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: veleroLabelSelector,
				},
			},
			dpa:     nil,
			wantErr: true,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: veleroLabelSelector,
				},
			},
		},
		{
			name:                 "Velero Deployment is nil",
			veleroDeployment:     nil,
			dpa:                  &oadpv1alpha1.DataProtectionApplication{},
			wantErr:              true,
			wantVeleroDeployment: nil,
		},
		{
			name: "given valid DPA CR, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with PodConfig Env, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								Env: []corev1.EnvVar{
									{Name: "TEST_ENV", Value: "TEST_VALUE"},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env: []corev1.EnvVar{
										{Name: common.VeleroScratchDirEnvKey, Value: "/scratch"},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{Name: common.LDLibraryPathEnvKey, Value: "/plugins"},
										{Name: "TEST_ENV", Value: "TEST_VALUE"},
										{Name: "OPENSHIFT_IMAGESTREAM_BACKUP", Value: "true"},
									},
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, noDefaultBackupLocation, unsupportedOverrides operatorType MTC, vel deployment has secret volumes",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							NoDefaultBackupLocation: true,
							DefaultPlugins:          allDefaultPluginsList,
						},
					},
					UnsupportedOverrides: map[oadpv1alpha1.UnsupportedImageKey]string{
						oadpv1alpha1.OperatorTypeKey: oadpv1alpha1.OperatorTypeMTC,
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--features=EnableCSI",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: append(baseVolumeMounts, []corev1.VolumeMount{
										{Name: "cloud-credentials", MountPath: "/credentials"},
										{Name: "cloud-credentials-gcp", MountPath: "/credentials-gcp"},
										{Name: "cloud-credentials-azure", MountPath: "/credentials-azure"},
									}...),
									Env: append(baseEnvVars, []corev1.EnvVar{
										{Name: common.AWSSharedCredentialsFileEnvKey, Value: "/credentials/cloud"},
										{Name: common.GCPCredentialsEnvKey, Value: "/credentials-gcp/cloud"},
										{Name: common.AzureCredentialsFileEnvKey, Value: "/credentials-azure/cloud"},
									}...),
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{
								{
									Name: "cloud-credentials",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{
											SecretName: "cloud-credentials",
										},
									},
								},
								{
									Name: "cloud-credentials-gcp",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{
											SecretName: "cloud-credentials-gcp",
										},
									},
								},
								{
									Name: "cloud-credentials-azure",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{
											SecretName: "cloud-credentials-azure",
										},
									},
								},
							}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
								pluginContainer(common.VeleroPluginForGCP, common.GCPPluginImage),
								pluginContainer(common.VeleroPluginForAzure, common.AzurePluginImage),
								pluginContainer(common.KubeVirtPlugin, common.KubeVirtPluginImage),
								pluginContainer(common.VeleroPluginForOpenshift, common.OpenshiftPluginImage),
							},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with proxy env var, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			testProxy: true,
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,

									Env: []corev1.EnvVar{
										{Name: common.VeleroScratchDirEnvKey, Value: "/scratch"},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{Name: common.LDLibraryPathEnvKey, Value: "/plugins"},
										{Name: proxyEnvKey, Value: proxyEnvValue},
										{Name: strings.ToLower(proxyEnvKey), Value: proxyEnvValue},
										{Name: "OPENSHIFT_IMAGESTREAM_BACKUP", Value: "true"},
									},
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with podConfig label, appropriate Velero Deployment has template labels",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								Labels: map[string]string{
									"thisIsVelero": "yes",
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: common.AppendTTMapAsCopy(veleroDeploymentMatchLabels,
								map[string]string{
									"thisIsVelero": "yes",
								}),
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with Unsupported Server Args, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
					Annotations: map[string]string{
						"oadp.openshift.io/unsupported-velero-server-args": "unsupported-server-args-cm",
					},
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{},
					},
				},
			},
			clientObjects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "unsupported-server-args-cm",
						Namespace: "test-ns",
					},
					Data: map[string]string{
						"unsupported-arg":      "value1",
						"unsupported-bool-arg": "True",
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      veleroDeploymentMatchLabels,
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--unsupported-arg=value1",
										"--unsupported-bool-arg=true",
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with Empty String Unsupported Server Args Annotation, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
					Annotations: map[string]string{
						"oadp.openshift.io/unsupported-velero-server-args": "",
					},
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{},
					},
				},
			},
			clientObjects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "unsupported-server-args-cm",
						Namespace: "test-ns",
					},
					Data: map[string]string{
						"unsupported-arg":      "value1",
						"unsupported-bool-arg": "True",
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      veleroDeploymentMatchLabels,
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with Unsupported Server Args and user added args, appropriate Velero Deployment is built",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
					Annotations: map[string]string{
						"oadp.openshift.io/unsupported-velero-server-args": "unsupported-server-args-cm",
					},
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							Args: &server.Args{
								ServerConfig: server.ServerConfig{
									BackupSyncPeriod:            ptr.To(time.Duration(1)),
									PodVolumeOperationTimeout:   ptr.To(time.Duration(1)),
									ResourceTerminatingTimeout:  ptr.To(time.Duration(1)),
									DefaultBackupTTL:            ptr.To(time.Duration(1)),
									StoreValidationFrequency:    ptr.To(time.Duration(1)),
									ItemOperationSyncFrequency:  ptr.To(time.Duration(1)),
									RepoMaintenanceFrequency:    ptr.To(time.Duration(1)),
									GarbageCollectionFrequency:  ptr.To(time.Duration(1)),
									DefaultItemOperationTimeout: ptr.To(time.Duration(1)),
									ResourceTimeout:             ptr.To(time.Duration(1)),
								},
							},
						},
					},
				},
			},
			clientObjects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "unsupported-server-args-cm",
						Namespace: "test-ns",
					},
					Data: map[string]string{
						"unsupported-arg":      "value1",
						"unsupported-bool-arg": "True",
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      veleroDeploymentMatchLabels,
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--unsupported-arg=value1",
										"--unsupported-bool-arg=true",
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with Unsupported Server Args and missing ConfigMap, appropriate Velero Deployment is nil with error",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
					Annotations: map[string]string{
						"oadp.openshift.io/unsupported-velero-server-args": "missing-unsupported-server-args-cm",
					},
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{},
					},
				},
			},
			wantErr:              true,
			wantVeleroDeployment: nil,
		},
		{
			name: "given invalid DPA CR because invalid podConfig label, appropriate Velero Deployment is nil with error",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								Labels: map[string]string{
									"component": common.NodeAgent,
								},
							},
						},
					},
				},
			},
			wantErr:              true,
			wantVeleroDeployment: nil,
		},
		{
			name: "given valid DPA CR and ItemOperationSyncFrequency is defined correctly, ItemOperationSyncFrequency is set",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                   logrus.InfoLevel.String(),
							ItemOperationSyncFrequency: "5m",
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, SnapshotMovedata is set to false",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultSnapshotMoveData:     ptr.To(false),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-snapshot-move-data=false",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, defaultSnapshotMovedata is set to true",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultSnapshotMoveData:     ptr.To(true),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-snapshot-move-data=true",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, defaultVolumesToFSBackup is set to true",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(true),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-volumes-to-fs-backup=true",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, disableInformerCache is nil and default behaviour of false is supplied to velero",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(false),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-volumes-to-fs-backup=false",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, disableInformerCache is set to true",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(false),
							DisableInformerCache:        ptr.To(true),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-volumes-to-fs-backup=false",
										"--disable-informer-cache=true",
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, disableInformerCache is set to false",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(false),
							DisableInformerCache:        ptr.To(false),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-volumes-to-fs-backup=false",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, defaultVolumesToFSBackup is set to false",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(false),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-volumes-to-fs-backup=false",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and Velero Config is defined correctly, defaultVolumesToFSBackup is set to false, snapshotMoveData is true",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
							DefaultVolumesToFSBackup:    ptr.To(false),
							DefaultSnapshotMoveData:     ptr.To(true),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										"--default-snapshot-move-data=true",
										"--default-volumes-to-fs-backup=false",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and DefaultItemOperationTimeout is defined correctly, DefaultItemOperationTimeout is set",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:                    logrus.InfoLevel.String(),
							ItemOperationSyncFrequency:  "5m",
							DefaultItemOperationTimeout: "2h",
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--item-operation-sync-frequency=5m",
										"--default-item-operation-timeout=2h",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and log level is defined correctly, log level is set",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel: logrus.InfoLevel.String(),
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and ResourceTimeout is defined correctly, ResourceTimeout is set",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel:        logrus.InfoLevel.String(),
							ResourceTimeout: "5m",
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						"component":                    "velero",
						oadpv1alpha1.OadpOperatorLabel: "True",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name":       common.Velero,
							"app.kubernetes.io/instance":   "test-Velero-CR",
							"app.kubernetes.io/managed-by": common.OADPOperator,
							"app.kubernetes.io/component":  Server,
							"component":                    "velero",
							"deploy":                       "velero",
							oadpv1alpha1.OadpOperatorLabel: "True",
						},
					},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/name":       common.Velero,
								"app.kubernetes.io/instance":   "test-Velero-CR",
								"app.kubernetes.io/managed-by": common.OADPOperator,
								"app.kubernetes.io/component":  Server,
								"component":                    "velero",
								"deploy":                       "velero",
								oadpv1alpha1.OadpOperatorLabel: "True",
							},
							Annotations: map[string]string{
								"prometheus.io/scrape": "true",
								"prometheus.io/port":   "8085",
								"prometheus.io/path":   "/metrics",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports: []corev1.ContainerPort{
										{
											Name:          "metrics",
											ContainerPort: 8085,
										},
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--log-level",
										logrus.InfoLevel.String(),
										"--resource-timeout=5m",
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "plugins",
											MountPath: "/plugins",
										},
										{
											Name:      "scratch",
											MountPath: "/scratch",
										},
										{
											Name:      "certs",
											MountPath: "/etc/ssl/certs",
										},
									},
									Env: []corev1.EnvVar{
										{
											Name:  common.VeleroScratchDirEnvKey,
											Value: "/scratch",
										},
										{
											Name: common.VeleroNamespaceEnvKey,
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "metadata.namespace",
												},
											},
										},
										{
											Name:  common.LDLibraryPathEnvKey,
											Value: "/plugins",
										},
										{
											Name:  "OPENSHIFT_IMAGESTREAM_BACKUP",
											Value: "true",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "plugins",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "scratch",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								{
									Name: "certs",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR and log level is defined incorrectly error is returned",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							LogLevel: logrus.InfoLevel.String() + "typo",
						},
					},
				},
			},
			wantErr:              true,
			wantVeleroDeployment: nil,
		},
		{
			name: "given valid DPA CR, velero deployment resource customization",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("2"),
										corev1.ResourceMemory: resource.MustParse("700Mi"),
									},
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("2"),
											corev1.ResourceMemory: resource.MustParse("700Mi"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment resource customization only cpu limit",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("2"),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("2"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment resource customization only cpu request",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("2"),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("2"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment resource customization only memory request",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment resource customization only memory limit",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment tolerations",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("2"),
										corev1.ResourceMemory: resource.MustParse("700Mi"),
									},
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
								Tolerations: []corev1.Toleration{
									{
										Key:      "key1",
										Operator: "Equal",
										Value:    "value1",
										Effect:   "NoSchedule",
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						oadpv1alpha1.OadpOperatorLabel: "True",
						"component":                    "velero",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      veleroDeploymentMatchLabels,
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Tolerations: []corev1.Toleration{
								{
									Key:      "key1",
									Operator: "Equal",
									Value:    "value1",
									Effect:   "NoSchedule",
								},
							},
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("2"),
											corev1.ResourceMemory: resource.MustParse("700Mi"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, velero deployment nodeselector",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							PodConfig: &oadpv1alpha1.PodConfig{
								ResourceAllocations: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("2"),
										corev1.ResourceMemory: resource.MustParse("700Mi"),
									},
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
								NodeSelector: map[string]string{
									"foo": "bar",
								},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       common.Velero,
						"app.kubernetes.io/instance":   "test-Velero-CR",
						"app.kubernetes.io/managed-by": common.OADPOperator,
						"app.kubernetes.io/component":  Server,
						oadpv1alpha1.OadpOperatorLabel: "True",
						"component":                    "velero",
					},
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      veleroDeploymentMatchLabels,
							Annotations: veleroPodAnnotations,
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							NodeSelector: map[string]string{
								"foo": "bar",
							},
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources: corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("2"),
											corev1.ResourceMemory: resource.MustParse("700Mi"),
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
									Command: []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, appropriate velero deployment is build with aws plugin specific specs",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: append(baseVolumeMounts, []corev1.VolumeMount{
										{Name: "cloud-credentials", MountPath: "/credentials"},
									}...),
									Env: append(baseEnvVars, []corev1.EnvVar{
										{Name: common.AWSSharedCredentialsFileEnvKey, Value: "/credentials/cloud"},
									}...),
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{{
								Name: "cloud-credentials",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: "cloud-credentials",
									},
								},
							}}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
							},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR, appropriate velero deployment is build with aws and kubevirt plugin specific specs",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
								oadpv1alpha1.DefaultPluginKubeVirt,
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "plugins", MountPath: "/plugins"},
										{Name: "scratch", MountPath: "/scratch"},
										{Name: "certs", MountPath: "/etc/ssl/certs"},
										{Name: "cloud-credentials", MountPath: "/credentials"},
									},
									Env: append(baseEnvVars, []corev1.EnvVar{
										{Name: common.AWSSharedCredentialsFileEnvKey, Value: "/credentials/cloud"},
									}...),
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{{
								Name: "cloud-credentials",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: "cloud-credentials",
									},
								},
							}}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
								pluginContainer(common.KubeVirtPlugin, common.KubeVirtPluginImage),
							},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with annotations, appropriate velero deployment is build with aws plugin specific specs",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
					PodAnnotations: map[string]string{
						"test-annotation": "awesome annotation",
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: veleroDeploymentMatchLabels,
							Annotations: common.AppendTTMapAsCopy(veleroPodAnnotations,
								map[string]string{
									"test-annotation": "awesome annotation",
								},
							),
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "plugins", MountPath: "/plugins"},
										{Name: "scratch", MountPath: "/scratch"},
										{Name: "certs", MountPath: "/etc/ssl/certs"},
										{Name: "cloud-credentials", MountPath: "/credentials"},
									},
									Env: append(baseEnvVars, []corev1.EnvVar{
										{Name: common.AWSSharedCredentialsFileEnvKey, Value: "/credentials/cloud"},
									}...),
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{{
								Name: "cloud-credentials",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: "cloud-credentials",
									},
								},
							}}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
							},
						},
					},
				},
			},
		},
		{
			name: "given valid DPA CR with PodDNS Policy/Config, annotations, appropriate velero deployment is build with aws plugin specific specs",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
					PodAnnotations: map[string]string{
						"test-annotation": "awesome annotation",
					},
					PodDnsPolicy: "None",
					PodDnsConfig: corev1.PodDNSConfig{
						Nameservers: []string{
							"1.1.1.1",
							"8.8.8.8",
						},
						Options: []corev1.PodDNSConfigOption{
							{Name: "ndots", Value: ptr.To("2")},
							{
								Name: "edns0",
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: veleroDeploymentMatchLabels,
							Annotations: common.AppendTTMapAsCopy(veleroPodAnnotations,
								map[string]string{
									"test-annotation": "awesome annotation",
								},
							),
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							DNSPolicy:          "None",
							DNSConfig: &corev1.PodDNSConfig{
								Nameservers: []string{"1.1.1.1", "8.8.8.8"},
								Options: []corev1.PodDNSConfigOption{
									{Name: "ndots", Value: ptr.To("2")},
									{
										Name: "edns0",
									},
								},
							},
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "plugins", MountPath: "/plugins"},
										{Name: "scratch", MountPath: "/scratch"},
										{Name: "certs", MountPath: "/etc/ssl/certs"},
										{Name: "cloud-credentials", MountPath: "/credentials"},
									},
									Env: append(baseEnvVars, []corev1.EnvVar{
										{Name: common.AWSSharedCredentialsFileEnvKey, Value: "/credentials/cloud"},
									}...),
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{{
								Name: "cloud-credentials",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: "cloud-credentials",
									},
								},
							}}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
							},
						},
					},
				},
			},
		},
		{
			name: "given valid Velero CR with with aws plugin from bucket",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
					BackupLocations: []oadpv1alpha1.BackupLocation{
						{
							CloudStorage: &oadpv1alpha1.CloudStorageLocation{
								CloudStorageRef: corev1.LocalObjectReference{
									Name: "bucket-123",
								},
								Config: map[string]string{},
								Credential: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "cloud-credentials",
									},
									Key: "creds",
								},
								Default:          false,
								BackupSyncPeriod: &metav1.Duration{},
							},
						},
					},
				},
			},
			wantErr: false,
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "plugins", MountPath: "/plugins"},
										{Name: "scratch", MountPath: "/scratch"},
										{Name: "certs", MountPath: "/etc/ssl/certs"},
										{
											Name:      "bound-sa-token",
											MountPath: "/var/run/secrets/openshift/serviceaccount",
											ReadOnly:  true,
										},
									},
									Env: baseEnvVars,
								},
							},
							Volumes: append(baseVolumes, []corev1.Volume{
								{
									Name: "bound-sa-token",
									VolumeSource: corev1.VolumeSource{
										Projected: &corev1.ProjectedVolumeSource{
											DefaultMode: ptr.To(int32(420)),
											Sources: []corev1.VolumeProjection{
												{
													ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
														Audience:          "openshift",
														ExpirationSeconds: ptr.To(int64(3600)),
														Path:              "token",
													},
												},
											},
										},
									},
								}}...),
							InitContainers: []corev1.Container{
								pluginContainer(common.VeleroPluginForAWS, common.AWSPluginImage),
							},
						},
					},
				},
			},
			clientObjects: []client.Object{
				&oadpv1alpha1.CloudStorage{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bucket-123",
						Namespace: "test-ns",
					},
					Spec: oadpv1alpha1.CloudStorageSpec{
						EnableSharedConfig: &trueVal,
					},
				},
			},
		},
		{
			name: "velero with custom metrics address",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							Args: &server.Args{
								ServerConfig: server.ServerConfig{
									MetricsAddress: ":" + strconv.Itoa(int(argsMetricsPortTest)),
								},
							},
						},
					},
				},
			},
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: veleroDeploymentMatchLabels,
							Annotations: common.AppendTTMapAsCopy(veleroPodAnnotations, map[string]string{
								"prometheus.io/port": strconv.Itoa(int(argsMetricsPortTest)),
							}),
						},
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: argsMetricsPortTest}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--metrics-address=:" + strconv.Itoa(int(argsMetricsPortTest)),
										"--fs-backup-timeout=4h0m0s",
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "Override restore resource priorities",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							Args: &server.Args{
								ServerConfig: server.ServerConfig{
									RestoreResourcePriorities: "securitycontextconstraints,test",
								},
							},
						},
					},
				},
			},
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--fs-backup-timeout=4h0m0s",
										"--restore-resource-priorities=securitycontextconstraints,test",
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "Check values of time fields in Velero args",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							Args: &server.Args{
								ServerConfig: server.ServerConfig{
									BackupSyncPeriod:            ptr.To(time.Duration(1)),
									PodVolumeOperationTimeout:   ptr.To(time.Duration(1)),
									ResourceTerminatingTimeout:  ptr.To(time.Duration(1)),
									DefaultBackupTTL:            ptr.To(time.Duration(1)),
									StoreValidationFrequency:    ptr.To(time.Duration(1)),
									ItemOperationSyncFrequency:  ptr.To(time.Duration(1)),
									RepoMaintenanceFrequency:    ptr.To(time.Duration(1)),
									GarbageCollectionFrequency:  ptr.To(time.Duration(1)),
									DefaultItemOperationTimeout: ptr.To(time.Duration(1)),
									ResourceTimeout:             ptr.To(time.Duration(1)),
								},
							},
						},
					},
				},
			},
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--backup-sync-period=1ns",
										"--default-backup-ttl=1ns",
										"--default-item-operation-timeout=1ns",
										"--resource-timeout=1ns",
										"--default-repo-maintain-frequency=1ns",
										"--garbage-collection-frequency=1ns",
										"--fs-backup-timeout=1ns",
										"--item-operation-sync-frequency=1ns",
										defaultRestoreResourcePriorities,
										"--store-validation-frequency=1ns",
										"--terminating-resource-timeout=1ns",
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "Override burst and qps",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							ClientBurst: ptr.To(123),
							ClientQPS:   ptr.To(123),
						},
						NodeAgent: &oadpv1alpha1.NodeAgentConfig{
							UploaderType: "kopia",
						},
					},
				},
			},
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										"--uploader-type=kopia",
										defaultFileSystemBackupTimeout,
										defaultRestoreResourcePriorities,
										"--client-burst=123",
										"--client-qps=123",
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
		},
		{
			name: "Conflicting burst and qps",
			veleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
				},
			},
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							ClientBurst: ptr.To(123),
							ClientQPS:   ptr.To(123),
							Args: &server.Args{
								ServerConfig: server.ServerConfig{
									ClientBurst: ptr.To(321),
									ClientQPS:   ptr.To("321"),
								},
							},
						},
						NodeAgent: &oadpv1alpha1.NodeAgentConfig{
							UploaderType: "kopia",
						},
					},
				},
			},
			wantVeleroDeployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-velero-deployment",
					Namespace: "test-ns",
					Labels:    veleroDeploymentLabel,
				},
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: appsv1.SchemeGroupVersion.String(),
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: veleroDeploymentMatchLabels},
					Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: veleroPodObjectMeta,
						Spec: corev1.PodSpec{
							RestartPolicy:      corev1.RestartPolicyAlways,
							ServiceAccountName: common.Velero,
							Containers: []corev1.Container{
								{
									Name:            common.Velero,
									Image:           common.VeleroImage,
									ImagePullPolicy: corev1.PullAlways,
									Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8085}},
									Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
									Command:         []string{"/velero"},
									Args: []string{
										"server",
										// should be present... "--uploader-type=kopia",
										"--client-burst=321",
										"--client-qps=321",
										"--fs-backup-timeout=4h0m0s",
										defaultRestoreResourcePriorities,
										defaultDisableInformerCache,
									},
									VolumeMounts: baseVolumeMounts,
									Env:          baseEnvVars,
								},
							},
							Volumes:        baseVolumes,
							InitContainers: []corev1.Container{},
						},
					},
				},
			},
			wantErr: true, // TODO should error to avoid user confusion?
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(tt.clientObjects...)
			if err != nil {
				t.Errorf("error in creating fake client, likely programmer error")
			}
			r := DPAReconciler{
				Client: fakeClient,
			}
			oadpclient.SetClient(fakeClient)
			if tt.testProxy {
				os.Setenv(proxyEnvKey, proxyEnvValue)
				defer os.Unsetenv(proxyEnvKey)
			}
			if err := r.buildVeleroDeployment(tt.veleroDeployment, tt.dpa); err != nil {
				if !tt.wantErr {
					t.Errorf("buildVeleroDeployment() error = %v, wantErr %v", err, tt.wantErr)
				}
				if tt.wantErr && tt.wantVeleroDeployment == nil {
					// if we expect an error and we got one, and wantVeleroDeployment is not defined, we don't need to compare further.
					t.Skip()
				}
			}
			if tt.dpa != nil {
				setPodTemplateSpecDefaults(&tt.wantVeleroDeployment.Spec.Template)
				if len(tt.wantVeleroDeployment.Spec.Template.Spec.Containers) > 0 {
					setContainerDefaults(&tt.wantVeleroDeployment.Spec.Template.Spec.Containers[0])
				}
				if tt.wantVeleroDeployment.Spec.Strategy.Type == "" {
					tt.wantVeleroDeployment.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
				}
				if tt.wantVeleroDeployment.Spec.Strategy.RollingUpdate == nil {
					tt.wantVeleroDeployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
						MaxSurge:       &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
					}
				}
				if tt.wantVeleroDeployment.Spec.RevisionHistoryLimit == nil {
					tt.wantVeleroDeployment.Spec.RevisionHistoryLimit = ptr.To(int32(10))
				}
				if tt.wantVeleroDeployment.Spec.ProgressDeadlineSeconds == nil {
					tt.wantVeleroDeployment.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
				}
			}
			if !reflect.DeepEqual(tt.wantVeleroDeployment, tt.veleroDeployment) {
				t.Errorf("expected velero deployment spec to be \n%#v, got \n%#v\nDIFF:%v", tt.wantVeleroDeployment, tt.veleroDeployment, cmp.Diff(tt.wantVeleroDeployment, tt.veleroDeployment))
			}
		})
	}
}

func TestDPAReconciler_getVeleroImage(t *testing.T) {
	tests := []struct {
		name       string
		DpaCR      *oadpv1alpha1.DataProtectionApplication
		pluginName string
		wantImage  string
		setEnvVars map[string]string
	}{
		{
			name: "given Velero image override, custom Velero image should be returned",
			DpaCR: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					UnsupportedOverrides: map[oadpv1alpha1.UnsupportedImageKey]string{
						oadpv1alpha1.VeleroImageKey: "test-image",
					},
				},
			},
			pluginName: common.Velero,
			wantImage:  "test-image",
			setEnvVars: make(map[string]string),
		},
		{
			name: "given default DPA CR with no env var, default velero image should be returned",
			DpaCR: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
			},
			pluginName: common.Velero,
			wantImage:  common.VeleroImage,
			setEnvVars: make(map[string]string),
		},
		{
			name: "given default DPA CR with env var set, image should be built via env vars",
			DpaCR: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
			},
			pluginName: common.Velero,
			wantImage:  "quay.io/konveyor/velero:latest",
			setEnvVars: map[string]string{
				"REGISTRY":    "quay.io",
				"PROJECT":     "konveyor",
				"VELERO_REPO": "velero",
				"VELERO_TAG":  "latest",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.setEnvVars {
				os.Setenv(key, value)
				defer os.Unsetenv(key)
			}
			gotImage := getVeleroImage(tt.DpaCR)
			if gotImage != tt.wantImage {
				t.Errorf("Expected plugin image %v did not match %v", tt.wantImage, gotImage)
			}
		})
	}
}
func Test_removeDuplicateValues(t *testing.T) {
	type args struct {
		slice []string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{
			name: "nill slice",
			args: args{slice: nil},
			want: nil,
		},
		{
			name: "empty slice",
			args: args{slice: []string{}},
			want: []string{},
		},
		{
			name: "one item in slice",
			args: args{slice: []string{"yo"}},
			want: []string{"yo"},
		},
		{
			name: "duplicate item in slice",
			args: args{slice: []string{"ice", "ice", "baby"}},
			want: []string{"ice", "baby"},
		},
		{
			name: "maintain order of first appearance in slice",
			args: args{slice: []string{"ice", "ice", "baby", "ice"}},
			want: []string{"ice", "baby"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := common.RemoveDuplicateValues(tt.args.slice); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("removeDuplicateValues() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_validateVeleroPlugins(t *testing.T) {
	tests := []struct {
		name    string
		dpa     *oadpv1alpha1.DataProtectionApplication
		secret  *corev1.Secret
		wantErr bool
		want    bool
	}{

		{
			name: "given valid Velero default plugin, default secret gets mounted as volume mounts",
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: "test-ns",
				},
			},
			wantErr: false,
			want:    true,
		},
		{
			name: "given valid Velero default plugin that is not a cloud provider, no secrets get mounted",
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginOpenShift,
							},
						},
					},
				},
			},
			secret:  &corev1.Secret{},
			wantErr: false,
			want:    true,
		},
		{
			name: "given valid multiple Velero default plugins, default secrets gets mounted for each plugin if applicable",
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
								oadpv1alpha1.DefaultPluginOpenShift,
							},
						},
					},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cloud-credentials",
					Namespace: "test-ns",
				},
			},
			wantErr: false,
			want:    true,
		},
		{
			name: "given aws default plugin without bsl, the valid plugin check passes",
			dpa: &oadpv1alpha1.DataProtectionApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-Velero-CR",
					Namespace: "test-ns",
				},
				Spec: oadpv1alpha1.DataProtectionApplicationSpec{
					Configuration: &oadpv1alpha1.ApplicationConfig{
						Velero: &oadpv1alpha1.VeleroConfig{
							DefaultPlugins: []oadpv1alpha1.DefaultPlugin{
								oadpv1alpha1.DefaultPluginAWS,
							},
						},
					},
				},
			},
			secret:  &corev1.Secret{},
			wantErr: false,
			want:    true,
		},
	}
	for _, tt := range tests {
		fakeClient, err := getFakeClientFromObjects(tt.dpa, tt.secret)
		if err != nil {
			t.Errorf("error in creating fake client, likely programmer error")
		}
		r := &DPAReconciler{
			Client:  fakeClient,
			Scheme:  fakeClient.Scheme(),
			Log:     logr.Discard(),
			Context: newContextForTest(tt.name),
			NamespacedName: types.NamespacedName{
				Namespace: tt.dpa.Namespace,
				Name:      tt.dpa.Name,
			},
			EventRecorder: record.NewFakeRecorder(10),
		}
		t.Run(tt.name, func(t *testing.T) {
			result, err := r.ValidateVeleroPlugins(r.Log)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateVeleroPlugins() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("ValidateVeleroPlugins() = %v, want %v", result, tt.want)
			}
		})
	}
}

var allDefaultPluginsList = []oadpv1alpha1.DefaultPlugin{
	oadpv1alpha1.DefaultPluginAWS,
	oadpv1alpha1.DefaultPluginGCP,
	oadpv1alpha1.DefaultPluginMicrosoftAzure,
	oadpv1alpha1.DefaultPluginKubeVirt,
	oadpv1alpha1.DefaultPluginOpenShift,
	oadpv1alpha1.DefaultPluginCSI,
}

func TestDPAReconciler_noDefaultCredentials(t *testing.T) {
	type args struct {
		dpa oadpv1alpha1.DataProtectionApplication
	}
	tests := []struct {
		name                string
		args                args
		want                map[string]bool
		wantHasCloudStorage bool
		wantErr             bool
	}{
		{
			name: "dpa with all plugins but with noDefualtBackupLocation should not require default credentials",
			args: args{
				dpa: oadpv1alpha1.DataProtectionApplication{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-Velero-CR",
						Namespace: "test-ns",
					},
					Spec: oadpv1alpha1.DataProtectionApplicationSpec{
						Configuration: &oadpv1alpha1.ApplicationConfig{
							Velero: &oadpv1alpha1.VeleroConfig{
								DefaultPlugins:          allDefaultPluginsList,
								NoDefaultBackupLocation: true,
							},
						},
					},
				},
			},
			want: map[string]bool{
				"velero-plugin-for-aws":             false,
				"velero-plugin-for-gcp":             false,
				"velero-plugin-for-microsoft-azure": false,
			},
			wantHasCloudStorage: false,
			wantErr:             false,
		},
		{
			name: "dpa no default cloudprovider plugins should not require default credentials",
			args: args{
				dpa: oadpv1alpha1.DataProtectionApplication{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-Velero-CR",
						Namespace: "test-ns",
					},
					Spec: oadpv1alpha1.DataProtectionApplicationSpec{
						Configuration: &oadpv1alpha1.ApplicationConfig{
							Velero: &oadpv1alpha1.VeleroConfig{
								DefaultPlugins:          []oadpv1alpha1.DefaultPlugin{oadpv1alpha1.DefaultPluginOpenShift},
								NoDefaultBackupLocation: true,
							},
						},
					},
				},
			},
			want:                map[string]bool{},
			wantHasCloudStorage: false,
			wantErr:             false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(&tt.args.dpa)
			if err != nil {
				t.Errorf("error in creating fake client, likely programmer error")
			}
			r := DPAReconciler{
				Client: fakeClient,
			}
			got, got1, err := r.noDefaultCredentials(tt.args.dpa)
			if (err != nil) != tt.wantErr {
				t.Errorf("DPAReconciler.noDefaultCredentials() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DPAReconciler.noDefaultCredentials() got = \n%v, \nwant \n%v", got, tt.want)
			}
			if got1 != tt.wantHasCloudStorage {
				t.Errorf("DPAReconciler.noDefaultCredentials() got1 = %v, want %v", got1, tt.wantHasCloudStorage)
			}
		})
	}
}

func TestDPAReconciler_VeleroDebugEnvironment(t *testing.T) {
	tests := []struct {
		name     string
		replicas *int
		wantErr  bool
	}{
		{
			name:     "debug replica override not set",
			replicas: nil,
			wantErr:  false,
		},
		{
			name:     "debug replica override set to 1",
			replicas: ptr.To(1),
			wantErr:  false,
		},
		{
			name:     "debug replica override set to 0",
			replicas: ptr.To(0),
			wantErr:  false,
		},
	}
	for _, tt := range tests {
		var err error
		if tt.replicas != nil {
			err = os.Setenv(VeleroReplicaOverride, strconv.Itoa(*tt.replicas))
		} else {
			err = os.Unsetenv(VeleroReplicaOverride)
		}
		if err != nil {
			t.Errorf("DPAReconciler.VeleroDebugEnvironment failed to set debug override: %v", err)
			return
		}

		dpa := &oadpv1alpha1.DataProtectionApplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-Velero-CR",
				Namespace: "test-ns",
			},
			Spec: oadpv1alpha1.DataProtectionApplicationSpec{
				Configuration: &oadpv1alpha1.ApplicationConfig{
					Velero: &oadpv1alpha1.VeleroConfig{
						DefaultPlugins:          allDefaultPluginsList,
						NoDefaultBackupLocation: true,
					},
				},
			},
		}

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      common.Velero,
				Namespace: dpa.Namespace,
			},
		}

		t.Run(tt.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(dpa)
			if err != nil {
				t.Errorf("error in creating fake client, likely programmer error")
			}
			r := DPAReconciler{
				Client: fakeClient,
			}
			err = r.buildVeleroDeployment(deployment, dpa)
			if (err != nil) != tt.wantErr {
				t.Errorf("DPAReconciler.VeleroDebugEnvironment error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if deployment.Spec.Replicas == nil {
				t.Error("deployment replicas not set")
				return
			}
			if tt.replicas == nil {
				if *deployment.Spec.Replicas != 1 {
					t.Errorf("unexpected deployment replica count: %d", *deployment.Spec.Replicas)
					return
				}
			} else {
				if *deployment.Spec.Replicas != int32(*tt.replicas) {
					t.Error("debug replica override did not apply")
					return
				}
			}
		})
	}
}
