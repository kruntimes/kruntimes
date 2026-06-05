package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const (
	runtimeLabel         = "runtime"
	runtimedDefaultImage = "kruntimes-runtimed:latest"
	workspaceVolume      = "workspace"
	workspacePath        = "/workspace"
)

// RuntimeReconciler watches Runtime CRs and creates Deployments with runtimed sidecar.
type RuntimeReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	DefaultDaemonImage string
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *RuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("runtime", req.NamespacedName)

	var rt v1alpha1.Runtime
	if err := r.Get(ctx, req.NamespacedName, &rt); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get runtime: %w", err)
	}

	deploy := r.buildDeployment(&rt)

	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, &existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get deployment: %w", err)
		}
		if err := controllerutil.SetControllerReference(&rt, deploy, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner ref: %w", err)
		}
		if err := r.Create(ctx, deploy); err != nil {
			return ctrl.Result{}, fmt.Errorf("create deployment: %w", err)
		}
		log.Info("Created Deployment", "deployment", deploy.Name)
		return ctrl.Result{}, nil
	}

	if !equality.Semantic.DeepEqual(existing.Labels, deploy.Labels) ||
		!equality.Semantic.DeepEqual(existing.Spec, deploy.Spec) {
		existing.Labels = deploy.Labels
		existing.Spec = deploy.Spec
		if err := controllerutil.SetControllerReference(&rt, &existing, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner ref: %w", err)
		}
		if err := r.Update(ctx, &existing); err != nil {
			return ctrl.Result{}, fmt.Errorf("update deployment: %w", err)
		}
		log.Info("Updated Deployment", "deployment", existing.Name)
		return ctrl.Result{}, nil
	}

	// Propagate Deployment status back to Runtime.
	if rt.Status.ReadyReplicas != existing.Status.ReadyReplicas {
		rt.Status.ReadyReplicas = existing.Status.ReadyReplicas
		if err := r.Status().Update(ctx, &rt); err != nil {
			return ctrl.Result{}, fmt.Errorf("update runtime status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *RuntimeReconciler) buildDeployment(rt *v1alpha1.Runtime) *appsv1.Deployment {
	name := rt.Name
	ns := rt.Namespace
	runtimeLabelVal := name
	replicas := rt.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	port := rt.Spec.Port
	if port == 0 {
		port = 9091
	}
	daemonImage := rt.Spec.DaemonImage
	if daemonImage == "" {
		daemonImage = runtimedDefaultImage
	}
	if r.DefaultDaemonImage != "" {
		daemonImage = r.DefaultDaemonImage
	}

	labels := map[string]string{
		runtimeLabel: runtimeLabelVal,
		"app":        "kruntimes-" + name,
	}
	annotations := runtimepod.CapacityAnnotations(rt)
	runsCapacity := runtimepod.RunsCapacityFromRuntime(rt, 0)

	runtimeContainer := corev1.Container{
		Name:            "runtime",
		Image:           rt.Spec.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            rt.Spec.Command,
		Ports: []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		},
		Env: rt.Spec.Env,
		Resources: corev1.ResourceRequirements{
			Requests: rt.Spec.Resources.Requests,
			Limits:   rt.Spec.Resources.Limits,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspacePath},
		},
	}
	if runtimeContainer.Resources.Requests == nil {
		runtimeContainer.Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		}
	}
	if runtimeContainer.Resources.Limits == nil {
		runtimeContainer.Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
	}

	daemonContainer := corev1.Container{
		Name:            "runtimed",
		Image:           daemonImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			fmt.Sprintf("--runtime-endpoint=localhost:%d", port),
			"--status-addr=:9093",
		},
		Env: []corev1.EnvVar{
			{
				Name: "HOSTNAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspacePath},
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   ptr(true),
			RunAsNonRoot:             ptr(true),
			AllowPrivilegeEscalation: ptr(false),
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	if runsCapacity > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--workers=%d", runsCapacity))
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-" + name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					ServiceAccountName: "kruntimes-runtimed",
					Containers:         []corev1.Container{runtimeContainer, daemonContainer},
					Volumes: []corev1.Volume{
						{
							Name: workspaceVolume,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *RuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runtime{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func ptr[T any](v T) *T { return &v }
