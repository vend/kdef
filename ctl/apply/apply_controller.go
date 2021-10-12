package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ghodss/yaml"
	"github.com/peter-evans/kdef/cli/log"
	"github.com/peter-evans/kdef/core/client"
	"github.com/peter-evans/kdef/core/model/def"
	"github.com/peter-evans/kdef/core/model/opt"
	"github.com/peter-evans/kdef/core/model/res"
	"github.com/peter-evans/kdef/core/operators/broker"
	"github.com/peter-evans/kdef/core/operators/brokers"
	"github.com/peter-evans/kdef/core/operators/topic"
	"github.com/peter-evans/kdef/ctl/apply/docparse"
)

type applier interface {
	Execute() *res.ApplyResult
}

// Options to configure an apply controller
type ApplyControllerOptions struct {
	// ApplierOptions
	DefinitionFormat  opt.DefinitionFormat
	DryRun            bool
	ReassAwaitTimeout int

	// Apply controller specific
	ContinueOnError bool
	ExitCode        bool
	JsonOutput      bool
}

// An apply controller
type applyController struct {
	// constructor params
	cl   *client.Client
	args []string
	opts ApplyControllerOptions
}

// Create a new apply controller
func NewApplyController(
	cl *client.Client,
	args []string,
	opts ApplyControllerOptions,
) *applyController {
	return &applyController{
		cl:   cl,
		args: args,
		opts: opts,
	}
}

// Execute the apply controller
func (a *applyController) Execute() error {
	var results res.ApplyResults

	if a.args[0] == "-" {
		log.Info("Reading definition(s) from stdin")
		defDocs, err := docparse.FromStdin(docparse.Format(a.opts.DefinitionFormat))
		if err != nil {
			return err
		}

		resourceDefs, err := getResourceDefinitions(defDocs, a.opts.DefinitionFormat)
		if err != nil {
			return err
		}

		results = a.applyDefinitions(defDocs, resourceDefs)
	} else {
	mainloop:
		for _, arg := range a.args {
			matchCount := 0

			matches, err := filepath.Glob(arg)
			if err != nil {
				return err
			}

			for _, match := range matches {
				matchCount++

				log.Info("Reading definition(s) from file %q", match)
				defDocs, err := docparse.FromFile(match, docparse.Format(a.opts.DefinitionFormat))
				if err != nil {
					return err
				}

				resourceDefs, err := getResourceDefinitions(defDocs, a.opts.DefinitionFormat)
				if err != nil {
					return err
				}

				res := a.applyDefinitions(defDocs, resourceDefs)
				results = append(results, res...)
				if res.ContainsErr() && !a.opts.ContinueOnError {
					break mainloop
				}

			}

			if matchCount == 0 {
				return errors.New("no definition files found")
			}
		}
	}

	if a.opts.JsonOutput {
		out, err := results.JSON()
		if err != nil {
			return err
		}
		fmt.Print(out)
	}

	// Check the apply results for any errors
	if results.ContainsErr() {
		return fmt.Errorf("apply completed with errors")
	}

	// Cause the program to exit with 1 if there are unapplied changes
	if a.opts.ExitCode && results.ContainsUnappliedChanges() {
		return fmt.Errorf("unapplied changes exist")
	}

	return nil
}

// Apply resource definitions using an applier
func (a *applyController) applyDefinitions(
	defDocs []string,
	resourceDefs []def.ResourceDefinition,
) res.ApplyResults {
	var results res.ApplyResults

	for i, resourceDef := range resourceDefs {
		var applier applier

		switch resourceDef.Kind {
		case "broker":
			applier = broker.NewApplier(a.cl, defDocs[i], broker.ApplierOptions{
				DefinitionFormat: a.opts.DefinitionFormat,
				DryRun:           a.opts.DryRun,
			})
		case "brokers":
			applier = brokers.NewApplier(a.cl, defDocs[i], brokers.ApplierOptions{
				DefinitionFormat: a.opts.DefinitionFormat,
				DryRun:           a.opts.DryRun,
			})
		case "topic":
			applier = topic.NewApplier(a.cl, defDocs[i], topic.ApplierOptions{
				DefinitionFormat:  a.opts.DefinitionFormat,
				DryRun:            a.opts.DryRun,
				ReassAwaitTimeout: a.opts.ReassAwaitTimeout,
			})
		}

		res := applier.Execute()
		results = append(results, res)
		if err := res.GetErr(); err != nil && !a.opts.ContinueOnError {
			return results
		}
	}

	return results
}

// Get resource definitions for the definition documents
func getResourceDefinitions(defDocs []string, format opt.DefinitionFormat) ([]def.ResourceDefinition, error) {
	kinds := make([]def.ResourceDefinition, len(defDocs))

	for i, defDoc := range defDocs {
		var resourceDef def.ResourceDefinition

		switch format {
		case opt.YamlFormat:
			if err := yaml.Unmarshal([]byte(defDoc), &resourceDef); err != nil {
				return nil, err
			}
		case opt.JsonFormat:
			if err := json.Unmarshal([]byte(defDoc), &resourceDef); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported format")
		}

		if err := resourceDef.ValidateResource(); err != nil {
			return nil, err
		}

		kinds[i] = resourceDef
	}

	return kinds, nil
}
