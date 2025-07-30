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
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

func docsCmd() *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Generate markdown documentation for all commands",
		Long: `Generate markdown documentation for all CLI commands and their parameters.
This creates individual markdown files for each command with complete usage information.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("failed to create output directory: %w", err)
			}

			// Generate markdown documentation
			err := doc.GenMarkdownTree(rootCmd, outputDir)
			if err != nil {
				return fmt.Errorf("failed to generate documentation: %w", err)
			}

			fmt.Printf("Documentation generated successfully in: %s\n", outputDir)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output", "o", "./docs", "Output directory for generated documentation")
	return cmd
}
