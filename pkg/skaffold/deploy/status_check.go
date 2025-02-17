/*
Copyright 2019 The Skaffold Authors

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

package deploy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	kubernetesutil "github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	runcontext "github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/context"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// TODO: Move this to a flag or global config.
	// Default deadline set to 10 minutes. This is default value for progressDeadlineInSeconds
	// See: https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/api/apps/v1/types.go#L305
	defaultStatusCheckDeadlineInSeconds int32 = 600
	// Poll period for checking set to 100 milliseconds
	defaultPollPeriodInMilliseconds = 100

	// For testing
	executeRolloutStatus = getRollOutStatus
)

func StatusCheck(ctx context.Context, defaultLabeller *DefaultLabeller, runCtx *runcontext.RunContext) error {
	client, err := kubernetesutil.GetClientset()
	if err != nil {
		return err
	}
	dMap, err := getDeployments(client, runCtx.Opts.Namespace, defaultLabeller)
	if err != nil {
		return errors.Wrap(err, "could not fetch deployments")
	}

	wg := sync.WaitGroup{}
	// Its safe to use sync.Map without locks here as each subroutine adds a different key to the map.
	syncMap := &sync.Map{}
	kubeCtl := &kubectl.CLI{
		Namespace:   runCtx.Opts.Namespace,
		KubeContext: runCtx.KubeContext,
	}

	for dName, deadline := range dMap {
		deadlineDuration := time.Duration(deadline) * time.Second
		wg.Add(1)
		go func(dName string, deadlineDuration time.Duration) {
			defer wg.Done()
			pollDeploymentRolloutStatus(ctx, kubeCtl, dName, deadlineDuration, syncMap)
		}(dName, deadlineDuration)
	}

	// Wait for all deployment status to be fetched
	wg.Wait()
	return getSkaffoldDeployStatus(syncMap)
}

func getDeployments(client kubernetes.Interface, ns string, l *DefaultLabeller) (map[string]int32, error) {
	deps, err := client.AppsV1().Deployments(ns).List(metav1.ListOptions{
		LabelSelector: l.K8sManagedByLabelKeyValueString(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not fetch deployments")
	}

	depMap := map[string]int32{}

	for _, d := range deps.Items {
		var deadline int32
		if d.Spec.ProgressDeadlineSeconds == nil {
			logrus.Debugf("no progressDeadlineSeconds config found for deployment %s. Setting deadline to %d seconds", d.Name, defaultStatusCheckDeadlineInSeconds)
			deadline = defaultStatusCheckDeadlineInSeconds
		} else {
			deadline = *d.Spec.ProgressDeadlineSeconds
		}
		depMap[d.Name] = deadline
	}

	return depMap, nil
}

func pollDeploymentRolloutStatus(ctx context.Context, k *kubectl.CLI, dName string, deadline time.Duration, syncMap *sync.Map) {
	pollDuration := time.Duration(defaultPollPeriodInMilliseconds) * time.Millisecond
	// Add poll duration to account for one last attempt after progressDeadlineSeconds.
	timeoutContext, cancel := context.WithTimeout(ctx, deadline+pollDuration)
	logrus.Debugf("checking rollout status %s", dName)
	defer cancel()
	for {
		select {
		case <-timeoutContext.Done():
			syncMap.Store(dName, errors.Wrap(timeoutContext.Err(), fmt.Sprintf("deployment rollout status could not be fetched within %v", deadline)))
			return
		case <-time.After(pollDuration):
			status, err := executeRolloutStatus(timeoutContext, k, dName)
			if err != nil {
				syncMap.Store(dName, err)
				return
			}
			if strings.Contains(status, "successfully rolled out") {
				syncMap.Store(dName, nil)
				return
			}
		}
	}
}

func getSkaffoldDeployStatus(m *sync.Map) error {
	errorStrings := []string{}
	m.Range(func(k, v interface{}) bool {
		if t, ok := v.(error); ok {
			errorStrings = append(errorStrings, fmt.Sprintf("deployment %s failed due to %s", k, t.Error()))
		}
		return true
	})

	if len(errorStrings) == 0 {
		return nil
	}
	return fmt.Errorf("following deployments are not stable:\n%s", strings.Join(errorStrings, "\n"))
}

func getRollOutStatus(ctx context.Context, k *kubectl.CLI, dName string) (string, error) {
	b, err := k.RunOut(ctx, nil, "rollout", []string{"status", "deployment", dName},
		"--watch=false")
	return string(b), err
}
