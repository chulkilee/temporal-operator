// Licensed to Alexandre VILAIN under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Alexandre VILAIN licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alexandrevilain/temporal-operator/api/v1beta1"
	kubernetesutil "github.com/alexandrevilain/temporal-operator/tests/e2e/util/kubernetes"
	"github.com/alexandrevilain/temporal-operator/tests/e2e/util/networking"
	"go.temporal.io/server/common"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

func deployAndWaitForTemporalWithPostgres(ctx context.Context, cfg *envconf.Config, namespace, version string) (*v1beta1.TemporalCluster, error) {
	// create the postgres
	err := deployAndWaitForPostgres(ctx, cfg, namespace)
	if err != nil {
		return nil, err
	}

	connectAddr := fmt.Sprintf("postgres.%s:5432", namespace) // create the temporal cluster
	cluster := &v1beta1.TemporalCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: namespace,
		},
		Spec: v1beta1.TemporalClusterSpec{
			NumHistoryShards:           1,
			JobTtlSecondsAfterFinished: &jobTtl,
			Version:                    version,
			MTLS: &v1beta1.MTLSSpec{
				Provider: v1beta1.CertManagerMTLSProvider,
				Internode: &v1beta1.InternodeMTLSSpec{
					Enabled: true,
				},
				Frontend: &v1beta1.FrontendMTLSSpec{
					Enabled: true,
				},
			},
			Persistence: v1beta1.TemporalPersistenceSpec{
				DefaultStore: &v1beta1.DatastoreSpec{
					SQL: &v1beta1.SQLSpec{
						User:            "temporal",
						PluginName:      "postgres",
						DatabaseName:    "temporal",
						ConnectAddr:     connectAddr,
						ConnectProtocol: "tcp",
					},
					PasswordSecretRef: v1beta1.SecretKeyReference{
						Name: "postgres-password",
						Key:  "PASSWORD",
					},
				},
				VisibilityStore: &v1beta1.DatastoreSpec{
					SQL: &v1beta1.SQLSpec{
						User:            "temporal",
						PluginName:      "postgres",
						DatabaseName:    "temporal_visibility",
						ConnectAddr:     connectAddr,
						ConnectProtocol: "tcp",
					},
					PasswordSecretRef: v1beta1.SecretKeyReference{
						Name: "postgres-password",
						Key:  "PASSWORD",
					},
				},
			},
		},
	}
	err = cfg.Client().Resources(namespace).Create(ctx, cluster)
	if err != nil {
		return nil, err
	}

	return cluster, nil

}

func klientToControllerRuntimeClient(k klient.Client) (client.Client, error) {
	return client.New(k.RESTConfig(), client.Options{})
}

func deployAndWaitForMySQL(ctx context.Context, cfg *envconf.Config, namespace string) error {
	return deployAndWaitFor(ctx, cfg, "mysql", namespace)
}

func deployAndWaitForPostgres(ctx context.Context, cfg *envconf.Config, namespace string) error {
	return deployAndWaitFor(ctx, cfg, "postgres", namespace)
}

func deployAndWaitForCassandra(ctx context.Context, cfg *envconf.Config, namespace string) error {
	name := "cassandra"
	err := deployTestManifest(ctx, cfg, name, namespace)
	if err != nil {
		return err
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-0", name), Namespace: namespace},
	}

	return wait.For(conditions.New(cfg.Client().Resources()).PodReady(&pod), wait.WithTimeout(10*time.Minute))
}

func deployAndWaitFor(ctx context.Context, cfg *envconf.Config, name, namespace string) error {
	err := deployTestManifest(ctx, cfg, name, namespace)
	if err != nil {
		return err
	}

	dep := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	// wait for the deployment to become available
	return waitForDeployment(ctx, cfg, &dep)
}

func deployTestManifest(ctx context.Context, cfg *envconf.Config, name, namespace string) error {
	path := fmt.Sprintf("testdata/%s", name)
	return decoder.ApplyWithManifestDir(ctx, cfg.Client().Resources(namespace), path, "*", []resources.CreateOption{}, decoder.MutateNamespace(namespace))
}

func waitForDeployment(ctx context.Context, cfg *envconf.Config, dep *appsv1.Deployment) error {
	err := wait.For(
		conditions.New(cfg.Client().Resources()).ResourcesFound(&appsv1.DeploymentList{Items: []appsv1.Deployment{*dep}}),
		wait.WithTimeout(time.Minute*10),
	)
	if err != nil {
		return err
	}
	return wait.For(conditions.New(cfg.Client().Resources()).DeploymentConditionMatch(dep, appsv1.DeploymentAvailable, corev1.ConditionTrue), wait.WithTimeout(time.Minute*10))
}

// waitForCluster waits for the temporal cluster's components to be up and running (reporting Ready condition).
func waitForCluster(ctx context.Context, cfg *envconf.Config, cluster *v1beta1.TemporalCluster) error {
	cond := conditions.New(cfg.Client().Resources()).ResourceMatch(cluster, func(object k8s.Object) bool {
		for _, condition := range object.(*v1beta1.TemporalCluster).Status.Conditions {
			if condition.Type == v1beta1.ReadyCondition && condition.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	})
	return wait.For(cond, wait.WithTimeout(time.Minute*10))
}

func waitForClusterClient(ctx context.Context, cfg *envconf.Config, clusterClient *v1beta1.TemporalClusterClient) error {
	cond := conditions.New(cfg.Client().Resources()).ResourceMatch(clusterClient, func(object k8s.Object) bool {
		return object.(*v1beta1.TemporalClusterClient).Status.SecretRef.Name != ""
	})
	return wait.For(cond, wait.WithTimeout(time.Minute*10))
}

type testLogWriter struct {
	t *testing.T
}

func (t *testLogWriter) Write(p []byte) (n int, err error) {
	t.t.Logf("%s", p)
	return len(p), nil
}

func forwardPortToTemporalFrontend(ctx context.Context, cfg *envconf.Config, t *testing.T, cluster *v1beta1.TemporalCluster) (string, func(), error) {
	selector, err := metav1.LabelSelectorAsSelector(
		&metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "app.kubernetes.io/name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{cluster.GetName()},
				},
				{
					Key:      "app.kubernetes.io/component",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{common.FrontendServiceName},
				},
				{
					Key:      "app.kubernetes.io/version",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{cluster.Spec.Version},
				},
			},
		},
	)
	if err != nil {
		return "", nil, err
	}

	podList := &corev1.PodList{}
	err = cfg.Client().Resources(cluster.GetNamespace()).List(ctx, podList, resources.WithLabelSelector(selector.String()))
	if err != nil {
		return "", nil, err
	}

	if len(podList.Items) == 0 {
		return "", nil, errors.New("no frontend port found")
	}

	selectedPod := podList.Items[0]

	localPort, err := networking.GetFreePort()
	if err != nil {
		return "", nil, err
	}

	// stopCh control the port forwarding lifecycle. When it gets closed the
	// port forward will terminate
	stopCh := make(chan struct{}, 1)
	// readyCh communicate when the port forward is ready to get traffic
	readyCh := make(chan struct{})

	out := &testLogWriter{t}

	go func() {
		err := kubernetesutil.ForwardPortToPod(cfg.Client().RESTConfig(), &selectedPod, localPort, out, stopCh, readyCh)
		if err != nil {
			panic(err)
		}
	}()

	<-readyCh
	t.Log("Port forwarding is ready to get traffic.")

	connectAddr := fmt.Sprintf("localhost:%d", localPort)
	return connectAddr, func() { close(stopCh) }, nil
}
