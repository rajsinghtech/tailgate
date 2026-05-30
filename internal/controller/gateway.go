package controller

import (
	"fmt"
	"hash/fnv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// stateDir is the node-local hostPath holding the gateway's tailscaled state. It is keyed
// by group AND a short hash of the backing tailnet: a stable tailnet keeps the same dir
// (so node identity survives pod restarts), but pointing the operator at a DIFFERENT
// tailnet yields a fresh dir — avoiding a stale node key being replayed at a control
// plane that no longer knows it ("node already exists").
func stateDir(group, tailnet string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tailnet))
	return fmt.Sprintf("/var/lib/tailgate/%s-%08x", group, h.Sum32())
}

func gatewayName(group string) string { return "tailgate-gw-" + group }

func authKeySecretName(group string) string { return "tailgate-" + group + "-authkey" }

func gatewayConfigName(group string) string { return "tailgate-gw-" + group + "-config" }

// gatewayConfigMap renders the declarative tailscaled config for a group into a
// ConfigMap the gateway mounts (and watches for hot-reload).
func gatewayConfigMap(eg *egressv1.EgressGroup, ns string) (*corev1.ConfigMap, error) {
	conf, err := renderGatewayConfig(eg)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayConfigName(eg.Name), Namespace: ns, Labels: gatewayLabels(eg.Name)},
		Data:       map[string]string{"tailscaled.json": string(conf)},
	}, nil
}

func gatewayLabels(group string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "tailgate",
		"app.kubernetes.io/component":  "gateway",
		"tailgate.dev/group":           group,
	}
}

func tagsFor(eg *egressv1.EgressGroup) []string {
	if len(eg.Spec.Tags) > 0 {
		return eg.Spec.Tags
	}
	return []string{"tag:egress-" + eg.Name}
}

// gatewayDaemonSet builds the per-group gateway: a node-local DaemonSet running
// tailscaled in kernel-TUN mode (our tailgate-gateway entrypoint) in its OWN pod
// netns (not hostNetwork — so each group's tailscale0 is isolated and the agent can
// stitch member veths into it). It forwards + MASQUERADEs onto tailscale0 (SNAT-to-tag).
// Node-local so member pods always have a same-node gateway to be wired to.
func gatewayDaemonSet(eg *egressv1.EgressGroup, ns, gwImage, tailnet string) *appsv1.DaemonSet {
	l := gatewayLabels(eg.Name)
	// Node config (accept-routes, exit-node, DNS, hostname) and the authkey are delivered
	// as files under /etc/tailgate, not env: the config is a watched ConfigMap (hot-reload)
	// and tags ride on the authkey. TS_GROUP is informational.
	env := []corev1.EnvVar{
		{Name: "TS_GROUP", Value: eg.Name},
	}
	hpType := corev1.HostPathDirectoryOrCreate
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName(eg.Name), Namespace: ns, Labels: l},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: l},
			// One node's gateway at a time; a node's member egress is briefly interrupted
			// only while its own gateway pod rolls (the agent re-stitches onto the new netns).
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type:          appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1))},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: corev1.PodSpec{
					// tailscaled needs no long drain; a short grace shrinks the rolling-update gap.
					TerminationGracePeriodSeconds: ptr.To(int64(15)),
					Containers: []corev1.Container{{
						Name:  "gateway",
						Image: gwImage,
						Env:   env,
						SecurityContext: &corev1.SecurityContext{
							Privileged: ptr.To(true), // kernel TUN + iptables + sysctls
						},
						// persist tailscaled state (node-local) so the identity is stable across
						// restarts; share a host dir for coordination.
						VolumeMounts: []corev1.VolumeMount{
							{Name: "run", MountPath: "/run/tailgate"},
							{Name: "state", MountPath: "/var/lib/tailscale"},
							// projected (NOT subPath) so ConfigMap/Secret updates propagate for hot-reload
							{Name: "conf", MountPath: gwConfigDir, ReadOnly: true},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/usr/local/bin/tailgate-gateway", "ready"}}},
							InitialDelaySeconds: 3,
							PeriodSeconds:       5,
							FailureThreshold:    30,
						},
						// Restart a wedged/dead tailscaled (daemon unresponsive), but not on a
						// transient tailnet disconnect — `live` checks only LocalAPI liveness.
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/usr/local/bin/tailgate-gateway", "live"}}},
							InitialDelaySeconds: 30,
							PeriodSeconds:       10,
							FailureThreshold:    3,
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "run", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/tailgate", Type: &hpType}}},
						{Name: "state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: stateDir(eg.Name, tailnet), Type: &hpType}}},
						// /etc/tailgate = tailscaled.json (from ConfigMap) + authkey (from Secret).
						{Name: "conf", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
							{ConfigMap: &corev1.ConfigMapProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: gatewayConfigName(eg.Name)},
								Items:                []corev1.KeyToPath{{Key: "tailscaled.json", Path: "tailscaled.json"}},
							}},
							{Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: authKeySecretName(eg.Name)},
								Items:                []corev1.KeyToPath{{Key: "TS_AUTHKEY", Path: "authkey"}},
							}},
						}}}},
					},
				},
			},
		},
	}
}
