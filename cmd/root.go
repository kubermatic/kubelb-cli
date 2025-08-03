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

package cmd

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"k8c.io/kubelb-cli/internal/config"
	"k8c.io/kubelb-cli/internal/constants"
	kubelbce "k8c.io/kubelb/api/ce/kubelb.k8c.io/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// List of commands that don't require kubeconfig or tenant name etc.
const (
	CmdNameVersion    = "version"
	CmdNameHelp       = "help"
	CmdNameCompletion = "completion"
	CmdNameDocs       = "docs"
)

var (
	kubeconfig string
	tenant     string
	timeout    time.Duration

	cfg       *config.Config
	k8sClient client.Client

	skipConfigCommands = []string{
		CmdNameVersion,
		CmdNameHelp,
		CmdNameCompletion,
		CmdNameDocs,
	}
)

var rootCmd = &cobra.Command{
	Use:   "kubelb",
	Short: "KubeLB CLI - Manage load balancers and create secure tunnels",
	Long: `KubeLB CLI provides tools to manage KubeLB load balancers and create secure tunnels
to expose local services through the KubeLB infrastructure.`,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		// Skip config loading for commands that don't need it
		if shouldSkipConfig(cmd) {
			return nil
		}

		// Load configuration
		var err error
		cfg, err = config.LoadConfig(kubeconfig, tenant)
		if err != nil {
			return err
		}

		// Create Kubernetes client
		k8sClient, err = createKubernetesClient(cfg)
		if err != nil {
			return err
		}

		return nil
	},
}

func Execute() error {
	// Create base context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Apply timeout if specified
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return rootCmd.ExecuteContext(ctx)
}

func createKubernetesClient(cfg *config.Config) (client.Client, error) {
	restConfig, err := config.CreateKubernetesConfig(cfg.KubeConfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// Add CE API types for tunnel and loadbalancer support
	if err := kubelbce.AddToScheme(scheme); err != nil {
		return nil, err
	}
	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return k8sClient, nil
}

// shouldSkipConfig determines if a command should skip configuration loading
func shouldSkipConfig(cmd *cobra.Command) bool {
	// Skip if it's the root command (no parent)
	if cmd.Parent() == nil {
		return true
	}

	cmdName := cmd.Name()
	for _, skipCmd := range skipConfigCommands {
		if cmdName == skipCmd {
			return true
		}
	}

	return false
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig for the tenant")
	rootCmd.PersistentFlags().StringVarP(&tenant, "tenant", "t", "", "Name of the tenant")
	rootCmd.PersistentFlags().DurationVar(&timeout, "timeout", constants.DefaultWaitTimeout, "Timeout for the command (e.g., 30s, 5m)")

	rootCmd.AddCommand(
		versionCmd(),
		loadbalancerCmd,
		tunnelCmd,
		exposeCmd(),
		docsCmd(),
	)
}
