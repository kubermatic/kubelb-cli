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
	"strconv"
	"strings"
	"time"

	"k8c.io/kubelb-cli/internal/config"
	"k8c.io/kubelb-cli/internal/constants"
	kubelb "k8c.io/kubelb/api/ee/kubelb.k8c.io/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CreateOptions struct {
	Name      string
	Endpoints string
	Type      string
	Protocol  string
	Route     bool
	Hostname  string
	Wait      bool
	Output    string
}

func Create(ctx context.Context, k8s client.Client, cfg *config.Config, opts CreateOptions) error {
	endpoints, ports, err := parseEndpoints(opts.Endpoints)
	if err != nil {
		return fmt.Errorf("invalid endpoints: %w", err)
	}

	if opts.Protocol == "" {
		opts.Protocol = constants.DefaultProtocol
	}

	// Validate protocol
	if opts.Protocol != constants.ProtocolHTTP {
		return fmt.Errorf("invalid protocol: %s. Only 'http' is supported", opts.Protocol)
	}

	if opts.Type != string(corev1.ServiceTypeClusterIP) && opts.Type != string(corev1.ServiceTypeLoadBalancer) {
		return fmt.Errorf("invalid load balancer type: %s", opts.Type)
	}
	lbType := corev1.ServiceType(opts.Type)

	// Create LoadBalancer resource with hostname or wildcard annotation
	lb := generateLoadBalancer(opts.Name, cfg.TenantNamespace, endpoints, ports, lbType, opts.Route, opts.Hostname)

	if err := k8s.Create(ctx, lb); err != nil {
		return fmt.Errorf("failed to create load balancer: %w", err)
	}

	// Wait for LoadBalancer if requested
	var updatedLB *kubelb.LoadBalancer
	if opts.Wait {
		var err error
		updatedLB, err = waitForLoadBalancer(ctx, k8s, cfg, lb.Name, opts.Route)
		if err != nil {
			return fmt.Errorf("load balancer did not become ready: %w", err)
		}
	} else {
		updatedLB = lb
	}

	switch opts.Output {
	case "yaml":
		return outputYAML(ctx, k8s, cfg, updatedLB.Name)
	default:
		return outputSummary(updatedLB)
	}
}

func parseEndpoints(endpointStr string) ([]kubelb.LoadBalancerEndpoints, []int32, error) {
	parts := strings.Split(endpointStr, ",")

	ipMap := make(map[string]bool)
	portMap := make(map[int32]bool)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		host, portStr, found := strings.Cut(part, ":")
		if !found {
			return nil, nil, fmt.Errorf("invalid endpoint format: %s (expected IP:port)", part)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, nil, fmt.Errorf("invalid port in %s", part)
		}

		ipMap[host] = true
		portMap[int32(port)] = true
	}

	var addresses []kubelb.EndpointAddress
	for ip := range ipMap {
		addresses = append(addresses, kubelb.EndpointAddress{IP: ip})
	}

	var ports []int32
	for port := range portMap {
		ports = append(ports, port)
	}

	endpoints := []kubelb.LoadBalancerEndpoints{
		{
			Name:      constants.DefaultEndpointName,
			Addresses: addresses,
		},
	}

	return endpoints, ports, nil
}

func generateLoadBalancer(name, namespace string, endpoints []kubelb.LoadBalancerEndpoints, ports []int32, lbType corev1.ServiceType, hasRoute bool, hostname string) *kubelb.LoadBalancer {
	var lbPorts []kubelb.LoadBalancerPort
	for i, port := range ports {
		portName := fmt.Sprintf("port-%d", i+1)
		if len(ports) == 1 {
			portName = constants.DefaultPortName
		}

		lbPorts = append(lbPorts, kubelb.LoadBalancerPort{
			Name:     portName,
			Port:     port,
			Protocol: corev1.ProtocolTCP,
		})
	}

	annotations := map[string]string{
		kubelb.CLIResourceAnnotation: "true",
	}

	// If route is requested but no explicit hostname provided, request wildcard domain
	if hasRoute && hostname == "" {
		annotations["kubelb.k8c.io/request-wildcard-domain"] = "true"
	}

	lb := &kubelb.LoadBalancer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubelb-cli",
			},
		},
		Spec: kubelb.LoadBalancerSpec{
			Type:      lbType,
			Endpoints: endpoints,
			Ports:     lbPorts,
		},
	}

	// Only set hostname if route is requested
	if hasRoute && hostname != "" {
		lb.Spec.Hostname = hostname
	}

	return lb
}

func waitForLoadBalancer(ctx context.Context, k8s client.Client, cfg *config.Config, name string, hasRoute bool) (*kubelb.LoadBalancer, error) {
	fmt.Print("⏳ Waiting for LoadBalancer to be ready")

	ticker := time.NewTicker(constants.ProgressTickerInterval)
	defer ticker.Stop()

	var lb *kubelb.LoadBalancer
	err := wait.PollUntilContextTimeout(ctx, constants.DefaultPollInterval, constants.DefaultWaitTimeout, true, func(ctx context.Context) (bool, error) {
		select {
		case <-ticker.C:
			fmt.Print(".")
		default:
		}

		var currentLB kubelb.LoadBalancer
		key := client.ObjectKey{Name: name, Namespace: cfg.TenantNamespace}

		if err := k8s.Get(ctx, key, &currentLB); err != nil {
			return false, err
		}

		lb = &currentLB

		if hasRoute {
			if lb.Spec.Hostname != "" {
				return true, nil
			}
		}

		// For LoadBalancer type, check if it has ingress points
		if lb.Spec.Type == corev1.ServiceTypeLoadBalancer && len(lb.Status.LoadBalancer.Ingress) > 0 {
			return true, nil
		}

		return false, nil
	})
	return lb, err
}

func outputYAML(ctx context.Context, k8s client.Client, cfg *config.Config, name string) error {
	return Get(ctx, k8s, cfg, name)
}

func outputSummary(lb *kubelb.LoadBalancer) error {
	fmt.Printf("✅ LoadBalancer '%s' created successfully\n", lb.Name)

	if lb.Spec.Hostname != "" {
		fmt.Printf("🌐 Route URL: https://%s\n", lb.Spec.Hostname)
		fmt.Println("\nYour application will be accessible at the URL above once DNS propagation completes.")
	}

	fmt.Printf("\nUse 'kubelb lb get %s' to see the full resource details.\n", lb.Name)
	return nil
}
