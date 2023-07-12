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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pingcap/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// PodReconciler reconciles a Pod object
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=pods/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("pod", req.NamespacedName)
	fmt.Printf("\nEssential container exited, injecting process kill container in the pod %s in namespace %s\n", req.Name, req.Namespace)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Pod not found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Pod")
		return ctrl.Result{}, err
	}

	// Terminate the pid 1 in pod
	// @todo add the pod deletion as events in namespace. Useful for debugging.
	err := r.injectEphemeralContainer(ctx, pod, &log)

	if err != nil {
		log.Error(err, "Failed to inject ephemeral container into pod")
		return ctrl.Result{}, err
	}

	log.Info("Pod killed successfully")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		// WORKAROUND - Since we can't easily get the old object in reconciler itself, easiest way for us
		// is to do this check in event filters. Filter everything out and pass event to reconciler only
		// if our logic matches.
		// Another approach is to check for old object in local informer cache, which i felt is not so better than
		// this approach. Reason being, not sure if the old object exists or not in cache.
		//
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Retrieve the old and new pod objects
				oldPod := e.ObjectOld.(*corev1.Pod)
				newPod := e.ObjectNew.(*corev1.Pod)

				// Check if the container status has changed from Running to Terminated
				// @todo get the container essetnial name dynamically
				oldStatus := getState(getContainerStatus(oldPod, "main"))

				newState := getContainerStatus(newPod, "main")
				newStatus := getState(newState)
				statusReason := getStateReason(newState)

				// @todo Check if given essenrtial container name is present in list of containers in pod
				return (oldStatus == "Running" && newStatus == "Terminated") && statusReason == "Completed"
			},
			CreateFunc: func(ce event.CreateEvent) bool {
				return false
			},
			DeleteFunc: func(de event.DeleteEvent) bool {
				return false
			},
		}).
		Complete(r)
}

// getContainerStatus retrieves the status of a container within a pod.
func getContainerStatus(pod *corev1.Pod, containerName string) corev1.ContainerState {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return status.State
		}
	}
	return corev1.ContainerState{}
}

func getState(state corev1.ContainerState) string {

	if state.Running != nil {
		return "Running"
	} else if state.Waiting != nil {
		return "Waiting"
	} else if state.Terminated != nil {
		return "Terminated"
	} else {
		return "unknown"
	}
}

func getStateReason(state corev1.ContainerState) string {
	if state.Running != nil {
		return ""
	} else if state.Waiting != nil {
		return state.Waiting.Reason
	} else if state.Terminated != nil {
		return state.Terminated.Reason
	} else {
		return ""
	}
}

// injectEphemeralContainer injects an ephemeral container into the running Pod and sets the entry point as "kill 1".
func (r *PodReconciler) injectEphemeralContainer(ctx context.Context, pod *corev1.Pod, log *logr.Logger) error {
	ec := &corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:      "essential-container-sidecar",
			Image:     "busybox",
			Command:   []string{"/bin/sh"},
			Args:      []string{"-c", "kill -INT 1"},
			TTY:       false,
			Stdin:     false,
			Resources: corev1.ResourceRequirements{},
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{
						"SYS_PTRACE",
					},
				},
			},
		},
	}

	podJS, _ := json.Marshal(pod)
	copied := pod.DeepCopy()
	copied.Spec.EphemeralContainers = append(copied.Spec.EphemeralContainers, *ec)

	debugJS, err := json.Marshal(copied)
	if err != nil {
		return fmt.Errorf("error creating JSON for debug container: %v", err)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(podJS, debugJS, pod)
	if err != nil {
		return fmt.Errorf("error creating patch to add debug container: %v", err)
	}

	// fmt.Printf("generated strategic merge patch for debug container: %s \n", patch)

	src := r.SubResource("ephemeralcontainers")
	err = src.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, patch))
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("pod not found: %s/%s", pod.Namespace, pod.Name)
		}
		return err
	}

	log.Info("Ephemeral container injected successfully")
	return nil
}
