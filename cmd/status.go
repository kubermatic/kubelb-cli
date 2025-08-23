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
	"github.com/spf13/cobra"

	"k8c.io/kubelb-cli/internal/status"
)

func statusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display current status of KubeLB",
		Long:  `Display the current status of KubeLB including version information, configuration, and state`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return status.Run(cmd.Context(), k8sClient, cfg)
		},
		Example: `  # Display status for current tenant
  kubelb status`,
	}

	return cmd
}
