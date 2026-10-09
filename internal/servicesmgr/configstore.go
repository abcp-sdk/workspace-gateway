// ---- configmaps / secrets ----
package servicesmgr

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelConfig         = "workspace/configmap"
	LabelConfigName     = "workspace/configmap-name"
	AnnoConfigCreator   = "workspace/configmap-creator"
	AnnoConfigNamespace = "workspace/configmap-namespace"
	LabelSecret         = "workspace/secret"
	LabelSecretName     = "workspace/secret-name"
	AnnoSecretCreator   = "workspace/secret-creator"
	AnnoSecretNamespace = "workspace/secret-namespace"
)

// ConfigMap is a live ConfigMap view (values exposed).
type ConfigMap struct {
	Name      string
	Data      map[string]string
	Creator   string
	Namespace string
	CreatedAt int64
}

// Secret is a live Secret view (decoded values).
type Secret struct {
	Name      string
	Data      map[string]string
	Creator   string
	Namespace string
	CreatedAt int64
}

func (c *Client) PutConfigMap(ctx context.Context, name string, data map[string]string, creator, namespace string) (ConfigMap, error) {
	if name == "" {
		return ConfigMap{}, fmt.Errorf("name required")
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: c.namespace,
			Labels:      map[string]string{LabelConfig: "1", LabelConfigName: name},
			Annotations: map[string]string{AnnoConfigCreator: creator, AnnoConfigNamespace: namespace},
		},
		Data: data,
	}
	if existing, gerr := c.GetConfigMap(ctx, name); gerr == nil {
		// A1: only a NON-EMPTY creator equal to `creator` may be overwritten;
		// an unowned object (creator "") is NOT writable by anyone.
		if !(existing.Creator != "" && existing.Creator == creator) {
			return ConfigMap{}, fmt.Errorf("configmap %q is not writable by this tenant", name)
		}
		if _, uerr := c.cs.CoreV1().ConfigMaps(c.namespace).Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
			return ConfigMap{}, fmt.Errorf("update configmap: %w", uerr)
		}
		return c.GetConfigMap(ctx, name)
	} else if !apierrors.IsNotFound(gerr) {
		return ConfigMap{}, gerr
	}
	if _, err := c.cs.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return ConfigMap{}, fmt.Errorf("create configmap: %w", err)
	}
	return c.GetConfigMap(ctx, name)
}

func (c *Client) GetConfigMap(ctx context.Context, name string) (ConfigMap, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ConfigMap{}, err
	}
	return toConfigMap(cm), nil
}

func (c *Client) ListConfigMaps(ctx context.Context) ([]ConfigMap, error) {
	list, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelConfig + "=1"})
	if err != nil {
		return nil, err
	}
	out := make([]ConfigMap, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toConfigMap(&list.Items[i]))
	}
	return out, nil
}

func (c *Client) DeleteConfigMap(ctx context.Context, name string) (bool, error) {
	err := c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func toConfigMap(cm *corev1.ConfigMap) ConfigMap {
	data := map[string]string{}
	for k, v := range cm.Data {
		data[k] = v
	}
	return ConfigMap{Name: cm.Name, Data: data, Creator: cm.Annotations[AnnoConfigCreator],
		Namespace: cm.Annotations[AnnoConfigNamespace], CreatedAt: cm.CreationTimestamp.UnixMilli()}
}

func (c *Client) PutSecret(ctx context.Context, name string, data map[string]string, creator, namespace string) (Secret, error) {
	if name == "" {
		return Secret{}, fmt.Errorf("name required")
	}
	s := secretObject(name, c.namespace, data, creator, namespace)
	if existing, gerr := c.GetSecret(ctx, name); gerr == nil {
		// A1: only a NON-EMPTY creator equal to `creator` may be overwritten.
		if !(existing.Creator != "" && existing.Creator == creator) {
			return Secret{}, fmt.Errorf("secret %q is not writable by this tenant", name)
		}
		if _, uerr := c.cs.CoreV1().Secrets(c.namespace).Update(ctx, s, metav1.UpdateOptions{}); uerr != nil {
			return Secret{}, fmt.Errorf("update secret: %w", uerr)
		}
		return c.GetSecret(ctx, name)
	} else if !apierrors.IsNotFound(gerr) {
		return Secret{}, gerr
	}
	if _, err := c.cs.CoreV1().Secrets(c.namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
		return Secret{}, fmt.Errorf("create secret: %w", err)
	}
	return c.GetSecret(ctx, name)
}

func secretObject(name, namespace string, data map[string]string, creator, orgNS string) *corev1.Secret {
	b := map[string][]byte{}
	for k, v := range data {
		b[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      map[string]string{LabelSecret: "1", LabelSecretName: name},
			Annotations: map[string]string{AnnoSecretCreator: creator, AnnoSecretNamespace: orgNS},
		},
		Type: corev1.SecretTypeOpaque, Data: b,
	}
}

func (c *Client) GetSecret(ctx context.Context, name string) (Secret, error) {
	s, err := c.cs.CoreV1().Secrets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Secret{}, err
	}
	return toSecret(s), nil
}

func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	list, err := c.cs.CoreV1().Secrets(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelSecret + "=1"})
	if err != nil {
		return nil, err
	}
	out := make([]Secret, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toSecret(&list.Items[i]))
	}
	return out, nil
}

func (c *Client) DeleteSecret(ctx context.Context, name string) (bool, error) {
	err := c.cs.CoreV1().Secrets(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func toSecret(s *corev1.Secret) Secret {
	data := map[string]string{}
	for k, v := range s.Data {
		data[k] = string(v)
	}
	return Secret{Name: s.Name, Data: data, Creator: s.Annotations[AnnoSecretCreator],
		Namespace: s.Annotations[AnnoSecretNamespace], CreatedAt: s.CreationTimestamp.UnixMilli()}
}
