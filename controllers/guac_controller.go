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
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("guac resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get guac")
		return ctrl.Result{}, err
	}

	log.Info("status: %+v\n", guac.Status)
	// Let's just set the status as Unknown when no status are available
	if guac.Status.Conditions == nil || len(guac.Status.Conditions) == 0 {
		meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeAvailableGuac, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, guac); err != nil {
			log.Error(err, "Failed to update Guac status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the guac Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, guac); err != nil {
			log.Error(err, "Failed to re-fetch guac")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(guac, GuacFinalizer) {
		log.Info("Adding Finalizer for guac")
		if ok := controllerutil.AddFinalizer(guac, GuacFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			// return ctrl.Result{Requeue: true}, nil
			return ctrl.Result{}, nil
		}

		if err = r.Update(ctx, guac); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the Guac instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isGuacMarkedToBeDeleted := guac.GetDeletionTimestamp() != nil
	if isGuacMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(guac, GuacFinalizer) {
			log.Info("Performing Finalizer Operations for Guac before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeDegradedGuac,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", guac.Name)})

			if err := r.Status().Update(ctx, guac); err != nil {
				log.Error(err, "Failed to update Guac status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForGuac(guac)

			// TODO(user): If you add operations to the doFinalizerOperationsForGuac method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the guac Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, guac); err != nil {
				log.Error(err, "Failed to re-fetch guac")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeDegradedGuac,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", guac.Name)})

			if err := r.Status().Update(ctx, guac); err != nil {
				log.Error(err, "Failed to update Guac status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Guac after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(guac, GuacFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Guac")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, guac); err != nil {
				log.Error(err, "Failed to remove finalizer for Guac")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: guac.Name, Namespace: guac.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new deployment
		dep, err := r.deploymentForGuac(guac)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Guac")

			// The following implementation will update the status
			meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeAvailableGuac,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Deployment for the custom resource (%s): (%s)", guac.Name, err)})

			if err := r.Status().Update(ctx, guac); err != nil {
				log.Error(err, "Reconcile existing deployment: Failed to update Guac status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Deployment",
			"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API is defining that the Guac type, have a GuacSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	replicas := guac.Spec.Replicas
	if *found.Spec.Replicas != *replicas {
		found.Spec.Replicas = replicas
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Reconcile replicas: Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the guac Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, guac); err != nil {
				log.Error(err, "Failed to re-fetch guac")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeAvailableGuac,
				Status: metav1.ConditionFalse, Reason: "Resizing",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", guac.Name, err)})

			if err := r.Status().Update(ctx, guac); err != nil {
				log.Error(err, "Update status after reconcile replicas failes: Failed to update Guac status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// The following implementation will update the status
	meta.SetStatusCondition(&guac.Status.Conditions, metav1.Condition{Type: typeAvailableGuac,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", guac.Name, replicas)})

	if err := r.Status().Update(ctx, guac); err != nil {
		log.Error(err, "Failed to update Guac status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// finalizeGuac will perform the required operations before delete the CR.
func (r *GuacReconciler) doFinalizerOperationsForGuac(cr *guacv1alpha1.Guac) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// deploymentForGuac returns Deployment object
func (r *GuacReconciler) deploymentForGuac(
	guac *guacv1alpha1.Guac) (*appsv1.Deployment, error) {
	ls := labelsForGuac(guac.Name)
	replicas := guac.Spec.Replicas

	// Get the Operand image
	image, err := imageForNgxinx(*guac.Spec.ContainerImage)
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
						Image:           image,
						Name:            "guac",
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
						// Command:        []string{"guac"},

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

// labelsForGuac returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForGuac(name string) map[string]string {
	// var imageTag string
	// image, err := imageForNgxinx()
	// if err == nil {
	imageTag := strings.Split("guac:latest", ":")[1]
	// }
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
func imageForNgxinx(imagePath string) (string, error) {
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
