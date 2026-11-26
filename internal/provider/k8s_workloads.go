package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/local/image-prepuller/internal/model"
)

// Kubernetes workload kinds as rendered in model.Workload.Kind.
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
)

// listWorkloads lists Deployments, StatefulSets and DaemonSets in the configured
// namespaces. nodeCount fills DaemonSet replicas: a DaemonSet has no replica
// field, its desired pod count is the number of eligible nodes.
func (p *K8sProvider) listWorkloads(ctx context.Context, nodeCount int32) ([]model.Workload, error) {
	opts := metav1.ListOptions{LabelSelector: p.labelSelector}
	var (
		out  []model.Workload
		errs []error
	)

	for _, ns := range p.namespaceScope() {
		deployments, err := p.clientset.AppsV1().Deployments(ns).List(ctx, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("list deployments in %q: %w", ns, err))
		} else {
			for i := range deployments.Items {
				out = append(out, workloadFromDeployment(&deployments.Items[i]))
			}
		}

		statefulSets, err := p.clientset.AppsV1().StatefulSets(ns).List(ctx, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("list statefulsets in %q: %w", ns, err))
		} else {
			for i := range statefulSets.Items {
				out = append(out, workloadFromStatefulSet(&statefulSets.Items[i]))
			}
		}

		daemonSets, err := p.clientset.AppsV1().DaemonSets(ns).List(ctx, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("list daemonsets in %q: %w", ns, err))
		} else {
			for i := range daemonSets.Items {
				out = append(out, workloadFromDaemonSet(&daemonSets.Items[i], nodeCount))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, errors.Join(errs...)
}

// namespaceScope returns the namespaces to list; metav1.NamespaceAll is "".
func (p *K8sProvider) namespaceScope() []string {
	if len(p.namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return p.namespaces
}

func workloadFromDeployment(d *appsv1.Deployment) model.Workload {
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return model.Workload{
		Namespace:        d.Namespace,
		Kind:             kindDeployment,
		Name:             d.Name,
		Replicas:         replicas,
		ImagePullSecrets: imagePullSecretNames(d.Spec.Template.Spec.ImagePullSecrets),
		Images:           workloadImages(d.Spec.Template.Spec),
	}
}

func workloadFromStatefulSet(s *appsv1.StatefulSet) model.Workload {
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	return model.Workload{
		Namespace:        s.Namespace,
		Kind:             kindStatefulSet,
		Name:             s.Name,
		Replicas:         replicas,
		ImagePullSecrets: imagePullSecretNames(s.Spec.Template.Spec.ImagePullSecrets),
		Images:           workloadImages(s.Spec.Template.Spec),
	}
}

func workloadFromDaemonSet(d *appsv1.DaemonSet, nodeCount int32) model.Workload {
	return model.Workload{
		Namespace:        d.Namespace,
		Kind:             kindDaemonSet,
		Name:             d.Name,
		Replicas:         nodeCount,
		ImagePullSecrets: imagePullSecretNames(d.Spec.Template.Spec.ImagePullSecrets),
		Images:           workloadImages(d.Spec.Template.Spec),
	}
}

// workloadImages collects init containers first, then regular containers, in pod
// spec order. Duplicates are kept: the container name tells them apart.
func workloadImages(spec corev1.PodSpec) []model.WorkloadImage {
	images := make([]model.WorkloadImage, 0, len(spec.InitContainers)+len(spec.Containers))
	for _, c := range spec.InitContainers {
		images = append(images, workloadImage(c, true))
	}
	for _, c := range spec.Containers {
		images = append(images, workloadImage(c, false))
	}
	return images
}

func workloadImage(c corev1.Container, isInit bool) model.WorkloadImage {
	policy := resolvePullPolicy(string(c.ImagePullPolicy), c.Image)
	return model.WorkloadImage{
		Image:            c.Image,
		Ref:              model.ParseImageRef(c.Image),
		ContainerName:    c.Name,
		IsInit:           isInit,
		PullPolicy:       policy,
		PolicyViolations: EvaluatePolicyViolations(c.Image, policy),
	}
}

// resolvePullPolicy applies the Kubernetes defaulting rule the API server itself
// would apply: an omitted imagePullPolicy means Always for :latest/apparently
// untagged images and IfNotPresent for everything else.
func resolvePullPolicy(raw, image string) model.PullPolicy {
	if strings.TrimSpace(raw) == "" {
		return k8sDefaultPullPolicy(image)
	}
	return model.ParsePullPolicy(raw)
}

func k8sDefaultPullPolicy(image string) model.PullPolicy {
	ref := model.ParseImageRef(image)
	if ref.Digest != "" {
		return model.PullPolicyIfNotPresent
	}
	if ref.Tag == "" || strings.EqualFold(ref.Tag, "latest") {
		return model.PullPolicyAlways
	}
	return model.PullPolicyIfNotPresent
}

func imagePullSecretNames(secrets []corev1.LocalObjectReference) []string {
	if len(secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		names = append(names, s.Name)
	}
	return names
}
