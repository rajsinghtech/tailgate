package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// ensureAuthKeySecret mints a per-group tagged ephemeral authkey once and stores it
// in a Secret the gateway consumes. Idempotent: if the Secret already holds a
// non-empty TS_AUTHKEY it does nothing (the gateway re-auths from it across restarts).
func (r *EgressGroupReconciler) ensureAuthKeySecret(ctx context.Context, eg *egressv1.EgressGroup) error {
	name := authKeySecretName(eg.Name)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, &existing)
	if err == nil && len(existing.Data["TS_AUTHKEY"]) > 0 {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if r.TS == nil {
		return fmt.Errorf("no tailscale client configured")
	}
	key, err := r.TS.MintAuthKey(ctx, tagsFor(eg))
	if err != nil {
		return fmt.Errorf("mint authkey for %q: %w", eg.Name, err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace},
		Data:       map[string][]byte{"TS_AUTHKEY": []byte(key)},
	}
	if r.Scheme != nil {
		_ = controllerutil.SetControllerReference(eg, s, r.Scheme)
	}
	if apierrors.IsNotFound(err) || existing.Name == "" {
		return r.Create(ctx, s)
	}
	existing.Data = s.Data
	existing.OwnerReferences = s.OwnerReferences
	return r.Update(ctx, &existing)
}
