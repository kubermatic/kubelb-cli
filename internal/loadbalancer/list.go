/*
Copyright 2025 The KubeLB Authors.

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

package loadbalancer

import (
	"context"
	"fmt"

	"k8c.io/kubelb-cli/internal/config"
	kubelb "k8c.io/kubelb/api/ee/kubelb.k8c.io/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func List(ctx context.Context, k8sClient client.Client, cfg *config.Config) error {
	var loadbalancers kubelb.LoadBalancerList
	err := k8sClient.List(ctx, &loadbalancers, client.InNamespace(cfg.TenantNamespace))
	if err != nil {
		return fmt.Errorf("failed to list load balancers: %w", err)
	}

	DisplayLoadbalancerList(loadbalancers.Items)
	return nil
}
