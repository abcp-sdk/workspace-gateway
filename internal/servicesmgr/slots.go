package servicesmgr

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Slot names for a blue-green service. Blue is the stable/production slot and
// is the default; green is the candidate slot.
const (
	SlotBlue  = "blue"
	SlotGreen = "green"
)

// ValidSlot reports whether s names a blue-green slot ("" is allowed and means
// "not a slotted deploy").
func ValidSlot(s string) bool { return s == "" || s == SlotBlue || s == SlotGreen }

// Slot names a blue-green slot plus its resolved k8s object names.
type Slot struct {
	// Name is "blue" or "green".
	Name string
	// DeployName is the Deployment name: `<service>` for blue, `<service>-green`
	// for green (so an existing plain Deployment is adopted as blue).
	DeployName string
}

// slotFor resolves a slot name to its object names for a service.
func slotFor(service, slot string) Slot {
	if slot == SlotGreen {
		return Slot{Name: SlotGreen, DeployName: service + "-" + SlotGreen}
	}
	return Slot{Name: SlotBlue, DeployName: service}
}

// SlotDeployName is the Deployment name a slot writes to (`<name>` for blue,
// `<name>-green` for green). Exported so the gateway can wait on the concrete
// Deployment after a slot deploy.
func SlotDeployName(service, slot string) string {
	return slotFor(service, slot).DeployName
}

// ServiceSlot is one slot's live view (see Service.Slots).
type ServiceSlot struct {
	Slot          string
	Image         string
	Ready         bool
	Replicas      int32
	ReadyReplicas int32
	URL           string
	CreatedAt     int64
	PodPhase      string
	Restarts      int32
	Message       string
}

// IsSlotted reports whether the service is blue-green (its primary Service
// carries an active-slot annotation).
func (c *Client) IsSlotted(ctx context.Context, name string) bool {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return svc.Annotations[AnnoActiveSlot] != ""
}

// DeploySlot creates or updates one slot of a blue-green service and keeps the
// primary (router) Service selecting the ACTIVE slot. It is the slot-mode
// analogue of Deploy: the plain (non-slotted) path is untouched.
//
//   - blue  -> Deployment `<name>` + Service `<name>-blue`; the primary Service
//     `<name>` is created (selecting blue) when absent.
//   - green -> Deployment `<name>-green` + Service `<name>-green`; the primary
//     Service is left on its current active slot (blue by default).
//
// Both slot Services carry the service's exposed ports, so each slot has its
// own address (`<name>-blue` / `<name>-green`).
func (c *Client) DeploySlot(ctx context.Context, s Spec) (Service, error) {
	if s.Name == "" || s.Image == "" {
		return Service{}, fmt.Errorf("name and image required")
	}
	if !ValidSlot(s.Slot) || s.Slot == "" {
		return Service{}, fmt.Errorf("slot must be blue|green")
	}
	if s.ContainerPort == 0 {
		s.ContainerPort = 8080
	}
	ports, groups, order, err := normalizePorts(s)
	if err != nil {
		return Service{}, err
	}
	// Conflict precheck: the slot Deployment/Service must be absent or ours.
	for _, suffix := range order {
		for _, slot := range []string{SlotBlue, SlotGreen} {
			svcName := slotServiceName(s.Name, slot, suffix)
			if cur, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, svcName, metav1.GetOptions{}); err == nil {
				if cur.Labels[LabelManaged] != "1" || (s.Creator != "" && cur.Annotations[AnnoCreator] != s.Creator) {
					return Service{}, fmt.Errorf("service %q already exists and is not managed by this tenant", svcName)
				}
			} else if !apierrors.IsNotFound(err) {
				return Service{}, err
			}
		}
	}

	slot := slotFor(s.Name, s.Slot)
	reps := s.Replicas
	if reps <= 0 {
		reps = 1
	}
	stage := s.Stage
	if stage == "" {
		stage = StageRelease
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name, LabelSlot: slot.Name}
	annotations := map[string]string{
		AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session,
		AnnoStage: stage, AnnoSlot: slot.Name,
	}
	if s.ExpiresAt > 0 {
		annotations[AnnoExpiresAt] = fmt.Sprint(s.ExpiresAt)
	}
	podSpec, err := c.buildPodSpec(s, ports)
	if err != nil {
		return Service{}, err
	}
	depSpec := appsv1.DeploymentSpec{
		Replicas: &reps,
		// The pod selector is slot-scoped so blue/green pods are distinct.
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.Name, LabelSlot: slot.Name}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
			Spec:       podSpec,
		},
	}
	if ru := buildRollout(s.Rollout); ru != nil {
		depSpec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: ru,
		}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: slot.DeployName, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec:       depSpec,
	}
	if err := c.applyDeployment(ctx, dep); err != nil {
		return Service{}, err
	}

	// One slot Service per suffix, selecting only this slot's pods.
	for _, suffix := range order {
		svcName := slotServiceName(s.Name, slot.Name, suffix)
		svcLabels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name, LabelSlot: slot.Name}
		svcAnnotations := map[string]string{
			AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session,
			AnnoStage: stage, AnnoPrimary: s.Name, AnnoSlot: slot.Name,
		}
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: c.namespace, Labels: svcLabels, Annotations: svcAnnotations},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": s.Name, LabelSlot: slot.Name},
				Ports:    corePorts(groups[suffix]),
			},
		}
		if err := c.applyService(ctx, svc); err != nil {
			return Service{}, err
		}
	}

	// Ensure the primary router Service exists and targets the active slot. A
	// brand-new service activates the slot just deployed (blue by default); an
	// existing router keeps its active slot (green deploys do not auto-promote).
	primary, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		active := slot.Name
		if err := c.applyRouter(ctx, s, active, groups, order, stage); err != nil {
			return Service{}, err
		}
	} else if err != nil {
		return Service{}, err
	} else if primary.Annotations[AnnoActiveSlot] == "" {
		// Adopt an existing plain Service as the router on blue (migration).
		if err := c.applyRouter(ctx, s, SlotBlue, groups, order, stage); err != nil {
			return Service{}, err
		}
	}
	return c.Get(ctx, s.Name)
}

// Promote switches the primary Service to the READY non-active slot. When
// requireReady is true (default) the target slot must have all replicas ready.
func (c *Client) Promote(ctx context.Context, name string, requireReady bool) (Service, error) {
	active, ok, err := c.activeSlot(ctx, name)
	if err != nil {
		return Service{}, err
	}
	if !ok {
		return Service{}, fmt.Errorf("service %q is not blue-green (no active slot)", name)
	}
	target := SlotGreen
	if active == SlotGreen {
		target = SlotBlue
	}
	dep, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, slotFor(name, target).DeployName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Service{}, fmt.Errorf("slot %q does not exist", target)
	}
	if err != nil {
		return Service{}, err
	}
	if requireReady {
		reps := int32(0)
		if dep.Spec.Replicas != nil {
			reps = *dep.Spec.Replicas
		}
		if reps == 0 || dep.Status.ReadyReplicas < reps {
			return Service{}, fmt.Errorf("slot %q is not ready (%d/%d)", target, dep.Status.ReadyReplicas, reps)
		}
	}
	return c.setActiveSlot(ctx, name, target)
}

// Rollback switches the primary Service back to the other slot (the reverse of
// Promote).
func (c *Client) Rollback(ctx context.Context, name string) (Service, error) {
	active, ok, err := c.activeSlot(ctx, name)
	if err != nil {
		return Service{}, err
	}
	if !ok {
		return Service{}, fmt.Errorf("service %q is not blue-green (no active slot)", name)
	}
	target := SlotGreen
	if active == SlotGreen {
		target = SlotBlue
	}
	if _, gerr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, slotFor(name, target).DeployName, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
		return Service{}, fmt.Errorf("slot %q does not exist", target)
	}
	return c.setActiveSlot(ctx, name, target)
}

// setActiveSlot repoints the router Service selector at `target` and records it.
func (c *Client) setActiveSlot(ctx context.Context, name, target string) (Service, error) {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, err
	}
	svc.Spec.Selector = map[string]string{"app": name, LabelSlot: target}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[AnnoActiveSlot] = target
	if _, err := c.cs.CoreV1().Services(c.namespace).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		return Service{}, err
	}
	return c.Get(ctx, name)
}

// activeDeployName is the Deployment that Pause/Resume/Scale/Manifest act on:
// the ACTIVE slot's Deployment for a blue-green service, else `name` itself.
func (c *Client) activeDeployName(ctx context.Context, name string) string {
	if active, ok, err := c.activeSlot(ctx, name); err == nil && ok {
		return slotFor(name, active).DeployName
	}
	return name
}

// activeSlot reads the primary Service's active slot annotation.
func (c *Client) activeSlot(ctx context.Context, name string) (string, bool, error) {
	svc, err := c.cs.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	a := svc.Annotations[AnnoActiveSlot]
	return a, a != "", nil
}

// applyRouter creates/updates the primary Service selecting `active`, carrying
// the service's exposed ports.
func (c *Client) applyRouter(ctx context.Context, s Spec, active string, groups map[string][]Port, order []string, stage string) error {
	// The primary router keeps the service's ports (union across suffixes is not
	// used here: the primary Service carries the "" group; siblings carry
	// their own). To stay compatible with the plain model we give the router
	// the primary (empty-suffix) ports; a service with only a suffixed port
	// still gets a router that selects the active slot with those ports.
	primaryPorts := groups[""]
	if len(primaryPorts) == 0 && len(order) > 0 {
		primaryPorts = groups[order[0]]
	}
	labels := map[string]string{LabelManaged: "1", LabelName: s.Name, "app": s.Name}
	annotations := map[string]string{
		AnnoImage: s.Image, AnnoCreator: s.Creator, AnnoSession: s.Session,
		AnnoStage: stage, AnnoActiveSlot: active,
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": s.Name, LabelSlot: active},
			Ports:    corePorts(primaryPorts),
		},
	}
	return c.applyService(ctx, svc)
}

// Slots returns the live blue/green slots of a service (empty for a plain
// service).
func (c *Client) Slots(ctx context.Context, name string) ([]ServiceSlot, string, error) {
	active, ok, err := c.activeSlot(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", nil
	}
	var out []ServiceSlot
	for _, slot := range []string{SlotBlue, SlotGreen} {
		d, gerr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, slotFor(name, slot).DeployName, metav1.GetOptions{})
		if apierrors.IsNotFound(gerr) {
			continue
		}
		if gerr != nil {
			return nil, "", gerr
		}
		reps := int32(0)
		if d.Spec.Replicas != nil {
			reps = *d.Spec.Replicas
		}
		phase := "Pending"
		if d.Status.ReadyReplicas > 0 {
			phase = "Running"
		} else if d.Status.UnavailableReplicas > 0 {
			phase = "Progressing"
		}
		urlPort := int32(80)
		for _, p := range c.portsFor(ctx, slotServiceName(name, slot, "")) {
			if p.Suffix == "" && p.Protocol == "tcp" {
				urlPort = p.Port
				break
			}
		}
		host := fmt.Sprintf("%s.%s.svc.cluster.local", slotServiceName(name, slot, ""), c.namespace)
		svcSlot := ServiceSlot{
			Slot: slot, Image: d.Annotations[AnnoImage], Ready: d.Status.ReadyReplicas > 0,
			Replicas: reps, ReadyReplicas: d.Status.ReadyReplicas,
			URL:       fmt.Sprintf("http://%s:%d", host, urlPort),
			CreatedAt: d.CreationTimestamp.UnixMilli(),
			PodPhase:  phase,
		}
		svcSlot.PodPhase, svcSlot.Restarts, svcSlot.Message = c.podDiagnostics(ctx, slotFor(name, slot).DeployName)
		out = append(out, svcSlot)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, active, nil
}

// slotServiceName is the k8s Service name for a slot + port suffix.
func slotServiceName(service, slot, suffix string) string {
	base := service + "-" + slot
	if suffix == "" {
		return base
	}
	return base + "-" + suffix
}

// normalizePorts validates and groups the requested ports (shared by Deploy and
// DeploySlot).
func normalizePorts(s Spec) ([]Port, map[string][]Port, []string, error) {
	ports := s.Ports
	if len(ports) == 0 {
		ports = []Port{{Port: 80, Protocol: "tcp", TargetPort: s.ContainerPort}}
	}
	groups := map[string][]Port{}
	order := []string{}
	for i := range ports {
		p := &ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return nil, nil, nil, fmt.Errorf("port %d: protocol must be tcp|udp", i)
		}
		if p.Port != 80 && p.Port != 443 {
			return nil, nil, nil, fmt.Errorf("port %d: service port must be 80 or 443", i)
		}
		if p.TargetPort == 0 {
			p.TargetPort = s.ContainerPort
		}
		if _, dup := groups[p.Suffix]; !dup {
			order = append(order, p.Suffix)
		}
		groups[p.Suffix] = append(groups[p.Suffix], *p)
	}
	return ports, groups, order, nil
}

// buildPodSpec assembles the pod spec (shared by the plain and slot paths).
func (c *Client) buildPodSpec(s Spec, ports []Port) (corev1.PodSpec, error) {
	env := []corev1.EnvVar{}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for k, v := range s.Runtime.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	for i, e := range s.EnvRefs {
		if e.Name == "" {
			return corev1.PodSpec{}, fmt.Errorf("env_refs[%d]: name is required", i)
		}
		switch {
		case e.ConfigMap != "" && e.ConfigKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap}, Key: e.ConfigKey,
				},
			}})
		case e.Secret != "" && e.SecretKey != "":
			env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret}, Key: e.SecretKey,
				},
			}})
		default:
			return corev1.PodSpec{}, fmt.Errorf("env_refs[%d]: set (config_map+config_key) or (secret+secret_key)", i)
		}
	}
	envFrom := []corev1.EnvFromSource{}
	for i, e := range s.EnvFrom {
		switch {
		case e.ConfigMap != "":
			envFrom = append(envFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.ConfigMap},
			}})
		case e.Secret != "":
			envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: e.Secret},
			}})
		default:
			return corev1.PodSpec{}, fmt.Errorf("env_from[%d]: set config_map or secret", i)
		}
	}
	rr := c.resources(s.CPU, s.Memory)
	if !s.Resources.Empty() {
		rr = buildResources(s.Resources)
	}
	if s.Runtime.Resources != nil {
		rr = *s.Runtime.Resources
	}
	for k, v := range s.Runtime.DeviceLimits {
		if rr.Limits == nil {
			rr.Limits = corev1.ResourceList{}
		}
		rr.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	containerPorts := []corev1.ContainerPort{}
	seen := map[int32]bool{}
	for _, p := range ports {
		if seen[p.TargetPort] {
			continue
		}
		seen[p.TargetPort] = true
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name: fmt.Sprintf("p%d", p.TargetPort), ContainerPort: p.TargetPort,
		})
	}
	container := corev1.Container{
		Name: "svc", Image: s.Image, Env: env, EnvFrom: envFrom, Command: s.Command,
		Ports: containerPorts, Resources: rr, SecurityContext: s.Runtime.SecurityContext,
	}
	if pr := buildProbe(s.ReadinessProbe); pr != nil {
		container.ReadinessProbe = pr
	}
	if pl := buildProbe(s.LivenessProbe); pl != nil {
		container.LivenessProbe = pl
	}
	if ps := buildProbe(s.StartupProbe); ps != nil {
		container.StartupProbe = ps
	}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
	initContainers := []corev1.Container{}
	extraContainers := []corev1.Container{}
	for i, cm := range s.ConfigMounts {
		if cm.MountPath == "" {
			return corev1.PodSpec{}, fmt.Errorf("config_mounts[%d]: mount_path is required", i)
		}
		volName := fmt.Sprintf("cfg-%d", i)
		vol := corev1.Volume{Name: volName}
		switch {
		case cm.ConfigMap != "":
			src := &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.ConfigMap}}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.ConfigMap = src
		case cm.Secret != "":
			src := &corev1.SecretVolumeSource{SecretName: cm.Secret}
			for _, it := range cm.Items {
				src.Items = append(src.Items, corev1.KeyToPath{Key: it.Key, Path: it.Path})
			}
			vol.Secret = src
		default:
			return corev1.PodSpec{}, fmt.Errorf("config_mounts[%d]: set config_map or secret", i)
		}
		podSpec.Volumes = append(podSpec.Volumes, vol)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: cm.MountPath,
		})
	}
	for i, sc := range s.Sidecars {
		if sc.Name == "" || sc.Image == "" {
			return corev1.PodSpec{}, fmt.Errorf("sidecars[%d]: name and image are required", i)
		}
		cc := corev1.Container{Name: sc.Name, Image: sc.Image, Command: sc.Command, Resources: c.resources(sc.CPU, sc.Memory)}
		for k, v := range sc.Env {
			cc.Env = append(cc.Env, corev1.EnvVar{Name: k, Value: v})
		}
		if sc.Init {
			initContainers = append(initContainers, cc)
		} else {
			extraContainers = append(extraContainers, cc)
		}
	}
	if len(initContainers) > 0 {
		podSpec.InitContainers = initContainers
	}
	if len(extraContainers) > 0 {
		podSpec.Containers = append(podSpec.Containers, extraContainers...)
	}
	for i, t := range s.Tolerations {
		if t.Key == "" {
			return corev1.PodSpec{}, fmt.Errorf("tolerations[%d]: key is required", i)
		}
		podSpec.Tolerations = append(podSpec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	if len(s.NodeSelector) > 0 {
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = map[string]string{}
		}
		for k, v := range s.NodeSelector {
			podSpec.NodeSelector[k] = v
		}
	}
	if s.Runtime.RuntimeClass != "" {
		rc := s.Runtime.RuntimeClass
		podSpec.RuntimeClassName = &rc
	}
	if len(s.Runtime.NodeSelector) > 0 {
		podSpec.NodeSelector = s.Runtime.NodeSelector
	}
	for _, sec := range s.Runtime.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: sec})
	}
	if s.Runtime.NeedsTun {
		t := corev1.HostPathCharDev
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "devtun",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: &t}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "devtun", MountPath: "/dev/net/tun",
		})
	}
	for i, vm := range s.Volumes {
		if vm.PVC == "" || vm.MountPath == "" {
			return corev1.PodSpec{}, fmt.Errorf("volumes[%d]: pvc and mount_path are required", i)
		}
		volName := fmt.Sprintf("pvc-%d", i)
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: vm.PVC, ReadOnly: vm.ReadOnly,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vm.MountPath, ReadOnly: vm.ReadOnly, SubPath: vm.SubPath,
		})
	}
	return podSpec, nil
}

// DeleteSlot removes a whole blue-green service: the router Service, both slot
// Services, and both slot Deployments.
func (c *Client) DeleteSlot(ctx context.Context, name string) (bool, error) {
	// Router + all `<name>-blue*` / `<name>-green*` Services.
	list, err := c.cs.CoreV1().Services(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManaged + "=1," + LabelName + "=" + name,
	})
	if err != nil {
		return false, err
	}
	for i := range list.Items {
		if err := c.cs.CoreV1().Services(c.namespace).Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	for _, slot := range []string{SlotBlue, SlotGreen} {
		dn := slotFor(name, slot).DeployName
		if err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, dn, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

// slotLabelsPresent reports whether a Deployment already carries a slot label.
func slotLabelsPresent(d *appsv1.Deployment) bool {
	return d.Labels[LabelSlot] != "" || d.Annotations[AnnoActiveSlot] != ""
}

var _ = strings.TrimSpace
