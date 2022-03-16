// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubeapiserver

import (
	"context"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	secretutils "github.com/gardener/gardener/pkg/utils/secrets"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	secretOIDCCABundleNamePrefix   = "kube-apiserver-oidc-cabundle"
	secretOIDCCABundleDataKeyCaCrt = "ca.crt"

	secretServiceAccountSigningKeyNamePrefix = "kube-apiserver-sa-signing-key"
	// SecretServiceAccountSigningKeyDataKeySigningKey is a constant for a key in the data map that contains the key
	// which is used to sign service accounts.
	SecretServiceAccountSigningKeyDataKeySigningKey = "signing-key"

	// SecretEtcdEncryptionConfigurationDataKey is a constant for a key in the data map that contains the config
	// which is used to encrypt etcd data.
	SecretEtcdEncryptionConfigurationDataKey = "encryption-configuration.yaml"

	// SecretStaticTokenName is a constant for the name of the static-token secret.
	SecretStaticTokenName = "kube-apiserver-static-token"
	// SecretBasicAuthName is a constant for the name of the basic-auth secret.
	SecretBasicAuthName = "kube-apiserver-basic-auth"

	userNameClusterAdmin = "system:cluster-admin"
	userNameHealthCheck  = "health-check"
)

func (k *kubeAPIServer) emptySecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.namespace}}
}

func (k *kubeAPIServer) reconcileSecretOIDCCABundle(ctx context.Context, secret *corev1.Secret) error {
	if k.values.OIDC == nil || k.values.OIDC.CABundle == nil {
		// We don't delete the secret here as we don't know its name (as it's unique). Instead, we rely on the usual
		// garbage collection for unique secrets/configmaps.
		return nil
	}

	secret.Data = map[string][]byte{secretOIDCCABundleDataKeyCaCrt: []byte(*k.values.OIDC.CABundle)}
	utilruntime.Must(kutil.MakeUnique(secret))

	return kutil.IgnoreAlreadyExists(k.client.Client().Create(ctx, secret))
}

func (k *kubeAPIServer) reconcileSecretServiceAccountSigningKey(ctx context.Context, secret *corev1.Secret) error {
	if k.values.ServiceAccount.SigningKey == nil {
		// We don't delete the secret here as we don't know its name (as it's unique). Instead, we rely on the usual
		// garbage collection for unique secrets/configmaps.
		return nil
	}

	secret.Data = map[string][]byte{SecretServiceAccountSigningKeyDataKeySigningKey: k.values.ServiceAccount.SigningKey}
	utilruntime.Must(kutil.MakeUnique(secret))

	return kutil.IgnoreAlreadyExists(k.client.Client().Create(ctx, secret))
}

func (k *kubeAPIServer) reconcileSecretBasicAuth(ctx context.Context) (*corev1.Secret, error) {
	var (
		secret *corev1.Secret
		err    error
	)

	if k.values.BasicAuthenticationEnabled {
		secret, err = k.secretsManager.Generate(ctx, &secretutils.BasicAuthSecretConfig{
			Name:           SecretBasicAuthName,
			Format:         secretutils.BasicAuthFormatCSV,
			Username:       "admin",
			PasswordLength: 32,
		}, secretsmanager.Persist(), secretsmanager.Rotate(secretsmanager.InPlace))
		if err != nil {
			return nil, err
		}
	}

	// TODO(rfranzke): Remove this in a future release.
	return secret, kutil.DeleteObject(ctx, k.client.Client(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver-basic-auth", Namespace: k.namespace}})
}

func (k *kubeAPIServer) reconcileSecretStaticToken(ctx context.Context) (*corev1.Secret, error) {
	secret, err := k.secretsManager.Generate(ctx, &secretutils.StaticTokenSecretConfig{
		Name: SecretStaticTokenName,
		Tokens: map[string]secretutils.TokenConfig{
			userNameClusterAdmin: {
				Username: userNameClusterAdmin,
				UserID:   userNameClusterAdmin,
				Groups:   []string{user.SystemPrivilegedGroup},
			},
			userNameHealthCheck: {
				Username: userNameHealthCheck,
				UserID:   userNameHealthCheck,
			},
		},
	}, secretsmanager.Persist(), secretsmanager.Rotate(secretsmanager.InPlace))
	if err != nil {
		return nil, err
	}

	// TODO(rfranzke): Remove this in a future release.
	return secret, kutil.DeleteObject(ctx, k.client.Client(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "static-token", Namespace: k.namespace}})
}

func (k *kubeAPIServer) reconcileSecretUserKubeconfig(ctx context.Context, secretStaticToken, secretBasicAuth *corev1.Secret) error {
	caBundleSecret, err := k.secretsManager.Get(v1beta1constants.SecretNameCACluster)
	if err != nil {
		return err
	}

	var basicAuth *secretutils.BasicAuth
	if secretBasicAuth != nil {
		basicAuth, err = secretutils.LoadBasicAuthFromCSV(SecretBasicAuthName, secretBasicAuth.Data[secretutils.DataKeyCSV])
		if err != nil {
			return err
		}
	}

	var token *secretutils.Token
	if secretStaticToken != nil {
		staticToken, err := secretutils.LoadStaticTokenFromCSV(SecretStaticTokenName, secretStaticToken.Data[secretutils.DataKeyStaticTokenCSV])
		if err != nil {
			return err
		}

		token, err = staticToken.GetTokenForUsername(userNameClusterAdmin)
		if err != nil {
			return err
		}
	}

	// TODO: In the future when we no longer support basic auth (dropped support for Kubernetes < 1.18) then we can
	//  switch from ControlPlaneSecretConfig to KubeconfigSecretConfig.
	if _, err := k.secretsManager.Generate(ctx, &secretutils.ControlPlaneSecretConfig{
		Name:      SecretNameUserKubeconfig,
		BasicAuth: basicAuth,
		Token:     token,
		KubeConfigRequests: []secretutils.KubeConfigRequest{{
			ClusterName:   k.namespace,
			APIServerHost: k.values.ExternalServer,
			CAData:        caBundleSecret.Data[secretutils.DataKeyCertificateBundle],
		}},
	}, secretsmanager.Rotate(secretsmanager.InPlace)); err != nil {
		return err
	}

	// TODO(rfranzke): Remove this in a future release.
	return kutil.DeleteObject(ctx, k.client.Client(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kubecfg", Namespace: k.namespace}})
}
