/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"os"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	guacv1alpha1 "github.com/gildub/guac-operator/api/v1alpha1"
)

const GuacFinalizer = "operator.trustification.io/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableGuac represents the status of the Deployment reconciliation
	typeAvailableGuac = "Available"
	// typeDegradedGuac represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedGuac = "Degraded"
)

// GuacReconciler reconciles a Guac object
type GuacReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=guac.trustification.io,resources=guaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=guac.trustification.io,resources=guaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=guac.trustification.io,resources=guaces/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *GuacReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Guac instance
	// The purpose is check if the Custom Resource for the Kind Guac
	// is applied on the cluster if not we return nil to stop the reconciliation
	guac := &guacv1alpha1.Guac{}
	err := r.Get(ctx, req.NamespacedName, guac)
	if err != nil {
		if errors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("guac resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get guac")
		return ctrl.Result{}, err
	}

	// Check if the deployment already exists, if not create a new deployment.
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: guac.Name, Namespace: guac.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		// Define a new deployment
		dep, _ := r.deploymentForGuac(guac)
		log.Info("Creating a new Deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			log.Error(err, "Failed to create new Deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}
		// Deployment created successfully - return and requeue
		return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		return ctrl.Result{}, err
	}

	// Ensure the deployment size is the same as the spec.
	size := guac.Spec.Replicas
	if found.Spec.Replicas != size {
		found.Spec.Replicas = size
		if err = r.Update(ctx, found); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
	}

	// Update the Guac status with the pod names.
	// List the pods for this CR's deployment.
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(guac.Namespace),
		client.MatchingLabels(labelsForApp(guac.Name)),
	}
	if err = r.List(ctx, podList, listOpts...); err != nil {
		return ctrl.Result{}, err
	}
	podNames := getPodNames(podList.Items)

	// Update status.Nodes if needed.
	if !reflect.DeepEqual(podNames, guac.Status.Nodes) {
		guac.Status.Nodes = &podNames
		if err := r.Status().Update(ctx, guac); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *GuacReconciler) serviceForGuac(guac *guacv1alpha1.Guac) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guac.Name + "-service",
			Namespace: guac.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/app": "guac",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/app": "guac",
			},
			Ports: []corev1.ServicePort{
				{
					Protocol:   corev1.ProtocolTCP,
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
				},
			},
		},
	}
}

// deploymentForGuac returns Deployment object
func (r *GuacReconciler) deploymentForGuac(guac *guacv1alpha1.Guac) (*appsv1.Deployment, error) {
	ls := labelsForGuac(guac.Name)
	replicas := guac.Spec.Replicas

	// Get the Operand image
	image, err := imageForGuac(*guac.Spec.ContainerImage)
	if err != nil {
		return nil, err
	}

	livenessProbe := &corev1.Probe{
		InitialDelaySeconds: 120,
		PeriodSeconds:       3,
		TimeoutSeconds:      5,
		FailureThreshold:    10,
	}

	livenessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path:   "/admin/health",
		Scheme: "HTTP",
		Port:   intstr.IntOrString{Type: intstr.Int, IntVal: int32(*guac.Spec.Port)},
	}

	readinessProbe := &corev1.Probe{
		InitialDelaySeconds: 150,
		PeriodSeconds:       5,
		TimeoutSeconds:      5,
		FailureThreshold:    10,
	}

	readinessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path:   "/admin/health",
		Scheme: "HTTP",
		Port:   intstr.IntOrString{Type: intstr.Int, IntVal: int32(*guac.Spec.Port)},
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guac.Name,
			Namespace: guac.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &[]bool{true}[0],
						// IMPORTANT: seccomProfile was introduced with Kubernetes 1.19
						// If you are looking for to produce solutions to be supported
						// on lower versions you must remove this option.
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Image: image,
						Name:  "guac",
						// Command:        []string{"guac"},
						ImagePullPolicy: corev1.PullIfNotPresent,
						// Ensure restrictive context for the container
						// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
						SecurityContext: &corev1.SecurityContext{
							// WARNING: Ensure that the image used defines an UserID in the Dockerfile
							// otherwise the Pod will not run and will fail with "container has runAsNonRoot and image has non-numeric user"".
							// If you want your workloads admitted in namespaces enforced with the restricted mode in OpenShift/OKD vendors
							// then, you MUST ensure that the Dockerfile defines a User ID OR you MUST leave the "RunAsNonRoot" and
							// "RunAsUser" fields empty.
							RunAsNonRoot: &[]bool{true}[0],
							// The guac image does not use a non-zero numeric user as the default user.
							// Due to RunAsNonRoot field being set to true, we need to force the user in the
							// container to a non-zero numeric user. We do this using the RunAsUser field.
							// However, if you are looking to provide solution for K8s vendors like OpenShift
							// be aware that you cannot run under its restricted-v2 SCC if you set this value.
							RunAsUser:                &[]int64{1001}[0],
							AllowPrivilegeEscalation: &[]bool{true}[0],
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{
									"ALL",
								},
							},
						},
						Ports: []corev1.ContainerPort{{
							ContainerPort: *guac.Spec.Port,
							Name:          "guac",
						}},
					}},
				},
			},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(guac, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForApp creates a simple set of labels for Memcached.
func labelsForApp(name string) map[string]string {
	return map[string]string{"cr_name": name}
}

// getPodNames returns the pod names of the array of pods passed in
func getPodNames(pods []corev1.Pod) []string {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
	}
	return podNames
}

// labelsForGuac returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForGuac(name string) map[string]string {

	imageTag := strings.Split("guac:latest", ":")[1]

	return map[string]string{"app.kubernetes.io/name": "Guac",
		"app.kubernetes.io/app":        "guac",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "guac-operator",
		"app.kubernetes.io/created-by": "controller-manager",
	}
}

// imageForGuac gets the Operand image which is managed by this controller
// if no image is specificed in guac.spec then envar can be used
// otherwise default to
func imageForGuac(imagePath string) (string, error) {
	if imagePath == "" {
		var imageEnvVar = "GUAC_IMAGE"
		image, found := os.LookupEnv(imageEnvVar)
		if !found {
			return "quay.io/centos7/httpd-24-centos7:latest", nil
		}
		return image, nil
	}
	return imagePath, nil

}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *GuacReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&guacv1alpha1.Guac{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
