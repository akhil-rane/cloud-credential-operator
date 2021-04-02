/*
Copyright 2021 The OpenShift Authors.

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

package openstack

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	schemeutils "github.com/openshift/cloud-credential-operator/pkg/util"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

// A minimal definition of the clouds.yaml sufficient for the values used in
// this test
type openstackCloud struct {
	Auth               map[string]string `json:"auth"`
	CACert             string            `json:"cacert,omitempty"`
	IdentityAPIVersion string            `json:"identity_api_version"`
	RegionName         string            `json:"region_name"`
	Verify             string            `json:"verify"`
}

type openstackClouds struct {
	Clouds struct {
		OpenStack openstackCloud `json:"openstack"`
	} `json:"clouds"`
}

const (
	cloudsNoCACert = `
clouds:
  openstack:
    auth:
      auth_url: http://1.2.3.4:5000
      password: password
      project_domain_name: Default
      project_name: openshift
      user_domain_name: Default
      username: openshift
    identity_api_version: "3"
    region_name: regionOne
    verify: true
`
	cloudsWithCACert = `
clouds:
  openstack:
    auth:
      auth_url: http://1.2.3.4:5000
      password: password
      project_domain_name: Default
      project_name: openshift
      user_domain_name: Default
      username: openshift
    cacert: %s
    identity_api_version: "3"
    region_name: regionOne
    verify: true
`
)

func TestReconcileCloudCredSecret_Reconcile(t *testing.T) {
	schemeutils.SetupScheme(scheme.Scheme)

	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: configv1.InfrastructureStatus{
			Platform:           configv1.OpenStackPlatformType,
			InfrastructureName: "test-cluster",
		},
	}

	/*
		Test parsing of CCO configuration and the resulting annotation of the
		root secret. Most of this is boilerplate behaviour.

		* An empty string mode means default behaviour: don't generate an error,
		  don't annotate the secret
		* If a legacy config map exists and conflicts with the configuration of CCO
		  we should return an error
		* If an invalid mode is specified we should return an error
		* If the Passthrough mode is specified explicitly we should annotate the
		  secret with this mode.
		* If the Mint mode is specified explicitly we should return an error,
		  because this is not supported by OpenStack.

	*/
	t.Run("Test operating mode", func(t *testing.T) {
		legacyDisabledCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cloud-credential-operator-config",
				Namespace: "openshift-cloud-credential-operator",
			},
			Data: map[string]string{
				"disabled": "true",
			},
		}

		for _, tc := range [...]struct {
			name           string
			mode           string
			existing       []runtime.Object
			wantAnnotation string
			wantErr        bool
		}{
			{
				name:           "empty string",
				mode:           "",
				existing:       nil,
				wantAnnotation: "",
				wantErr:        false,
			},
			{
				name:           "configuration conflict",
				mode:           "Passthrough",
				existing:       []runtime.Object{legacyDisabledCM},
				wantAnnotation: "",
				wantErr:        true,
			},
			{
				name:           "invalid mode",
				mode:           "invalid",
				existing:       nil,
				wantAnnotation: "",
				wantErr:        true,
			},
			{
				name:           "Passthrough",
				mode:           "Passthrough",
				existing:       nil,
				wantAnnotation: "passthrough",
				wantErr:        false,
			},
			{
				name:           "Mint",
				mode:           "Mint",
				existing:       nil,
				wantAnnotation: "",
				wantErr:        true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				secret := testSecret("")
				existing := append(tc.existing, infra, secret, testOperatorConfig(tc.mode))
				fakeClient := fake.NewFakeClient(existing...)

				r := &ReconcileCloudCredSecret{
					Client: fakeClient,
					Logger: log.WithField("controller", "testController"),
				}
				_, err := r.Reconcile(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      "openstack-credentials",
					Namespace: "kube-system",
				}})

				if tc.wantErr {
					require.Error(t, err, "ReconcileCloudCredSecret.Reconcile() did not return expected error")
				} else {
					require.NoError(t, err, "ReconcileCloudCredSecret.Reconcile() returned unexpected error")
				}

				reconciledSecret := &corev1.Secret{}
				err = fakeClient.Get(context.TODO(), client.ObjectKey{
					Namespace: secret.Namespace,
					Name:      secret.Name,
				}, reconciledSecret)
				require.NoError(t, err, "Failed to fetch secret after ReconcileCloudCredSecret.Reconcile()")

				annotation := reconciledSecret.Annotations["cloudcredential.openshift.io/mode"]
				assert.Equal(t, annotation, tc.wantAnnotation, "Secret annotation not set correctly")
			})
		}
	})

	/*
		Test fixing an invalid cacert detected in the root secret.

		* If the root secret clouds.yaml contains invalid YAML we should return an
		  error
		* If the root secret clouds.yaml does not contain a CA Cert we should not
		  modify it
		* If the root secret clouds.yaml contains the incorrect CA Cert path we
		  should update it
		* If the root secret clouds.yaml contains the correct CA Cert path we
		  should not modify it

	*/
	t.Run("Test fix cacert path", func(t *testing.T) {
		parseClouds := func(secret *corev1.Secret) (*openstackClouds, error) {
			clouds := &openstackClouds{}
			err := yaml.Unmarshal([]byte(secret.Data["clouds.yaml"]), clouds)
			return clouds, err
		}

		passthrough := testOperatorConfig("Passthrough")

		const incorrectCACertFile = "/incorrect/path/to/ca-bundle.pem"
		const correctCACertFile = "/etc/kubernetes/static-pod-resources/configmaps/cloud-config/ca-bundle.pem"

		for _, tc := range [...]struct {
			name           string
			cacert         string
			expectedCACert string
			skipDiff       bool
			wantErr        bool
		}{
			{
				name:     "invalid YAML",
				cacert:   "\"",
				skipDiff: true, // Ignore YAML parse error diffing updated secret
				wantErr:  true,
			},
			{
				name:           "No CA Cert",
				cacert:         "",
				expectedCACert: "",
				wantErr:        false,
			},
			{
				name:           "Incorrect CA Cert",
				cacert:         incorrectCACertFile,
				expectedCACert: correctCACertFile,
				wantErr:        false,
			},
			{
				name:           "Correct CA Cert",
				cacert:         correctCACertFile,
				expectedCACert: correctCACertFile,
				wantErr:        false,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				secret := testSecret(tc.cacert)
				fakeClient := fake.NewFakeClient(infra, passthrough, secret)

				r := &ReconcileCloudCredSecret{
					Client: fakeClient,
					Logger: log.WithField("controller", "testController"),
				}
				_, err := r.Reconcile(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      "openstack-credentials",
					Namespace: "kube-system",
				}})

				if tc.wantErr {
					require.Error(t, err, "ReconcileCloudCredSecret.Reconcile() did not return expected error")
				} else {
					require.NoError(t, err, "ReconcileCloudCredSecret.Reconcile() returned unexpected error")
				}

				reconciledSecret := &corev1.Secret{}
				err = fakeClient.Get(context.TODO(), client.ObjectKey{
					Namespace: secret.Namespace,
					Name:      secret.Name,
				}, reconciledSecret)
				require.NoError(t, err, "Failed to fetch secret after ReconcileCloudCredSecret.Reconcile()")

				if !tc.skipDiff {
					// Update cacert in the input secret to the expected value,
					// and compare the resulting parsed clouds.yamls
					origClouds, err := parseClouds(secret)
					require.NoError(t, err, "Unexpected error parsing original clouds.yaml")
					origClouds.Clouds.OpenStack.CACert = tc.expectedCACert

					reconciledClouds, err := parseClouds(reconciledSecret)
					require.NoError(t, err, "Unexpected error parsing updated clouds.yaml")
					assert.Equal(t, origClouds, reconciledClouds, "Secret was not updated as expected")
				}
			})
		}
	})
}

func testOperatorConfig(mode string) *operatorv1.CloudCredential {
	return &operatorv1.CloudCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: operatorv1.CloudCredentialSpec{
			CredentialsMode: operatorv1.CloudCredentialsMode(mode),
		},
	}
}

func testSecret(caCert string) *corev1.Secret {
	var clouds string
	if caCert == "" {
		clouds = cloudsNoCACert
	} else {
		clouds = fmt.Sprintf(cloudsWithCACert, caCert)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openstack-credentials",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"clouds.yaml": []byte(clouds),
		},
	}
}
