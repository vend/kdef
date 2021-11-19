// Package brokers implements the export brokers command and executes the controller.
package brokers

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peter-evans/kdef/cli/config"
	"github.com/peter-evans/kdef/cli/ctl/export"
	"github.com/peter-evans/kdef/core/model/opt"
)

func Command(cOpts *config.Options) *cobra.Command {
	opts := export.ControllerOptions{}
	var defFormat string

	cmd := &cobra.Command{
		Use:   "brokers [options]",
		Short: "Export cluster-wide broker configuration to a definition",
		Long: `Export cluster-wide broker configuration to a definition (Kafka 0.11.0+).

Exports to stdout by default. Supply the --output-dir option to create definition files.

Documentation: https://peter-evans.github.io/kdef`,
		Example: `# export brokers definition to the directory "brokers"
kdef export brokers --output-dir "brokers"

# export brokers definition to stdout
kdef export brokers --quiet`,
		SilenceUsage:          true,
		SilenceErrors:         true,
		DisableFlagsInUseLine: true,
		PreRunE: func(_ *cobra.Command, args []string) error {
			opts.DefinitionFormat = opt.ParseDefinitionFormat(defFormat)
			if opts.DefinitionFormat == opt.UnsupportedFormat {
				return fmt.Errorf("\"format\" must be one of %q", strings.Join(opt.DefinitionFormatValidValues, "|"))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			cl, err := config.NewClient(cOpts)
			if err != nil {
				return err
			}

			ctl := export.NewExportController(cl, args, opts, "brokers")
			if err := ctl.Execute(); err != nil {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(
		&defFormat,
		"format",
		"f",
		"yaml",
		fmt.Sprintf("resource definition format [%s]", strings.Join(opt.DefinitionFormatValidValues, "|")),
	)
	cmd.Flags().StringVarP(&opts.OutputDir, "output-dir", "o", "", "output directory (must exist)")
	cmd.Flags().BoolVarP(&opts.Overwrite, "overwrite", "w", false, "overwrite existing files in output directory")

	return cmd
}
