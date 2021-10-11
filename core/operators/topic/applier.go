package topic

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ghodss/yaml"
	"github.com/google/go-cmp/cmp"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/peter-evans/kdef/cli/log"
	"github.com/peter-evans/kdef/client"
	"github.com/peter-evans/kdef/core/helpers/assignments"
	"github.com/peter-evans/kdef/core/helpers/jsondiff"
	"github.com/peter-evans/kdef/core/kafka"
	"github.com/peter-evans/kdef/core/model/def"
	"github.com/peter-evans/kdef/core/model/meta"
	"github.com/peter-evans/kdef/core/model/opt"
	"github.com/peter-evans/kdef/core/model/res"
)

// Options to configure an applier
type ApplierOptions struct {
	DefinitionFormat  opt.DefinitionFormat
	DryRun            bool
	ReassAwaitTimeout int
}

// Create a new applier
func NewApplier(
	cl *client.Client,
	defDoc string,
	opts ApplierOptions,
) *applier {
	return &applier{
		srv:    kafka.NewService(cl),
		defDoc: defDoc,
		opts:   opts,
	}
}

// Applier operations
type applierOps struct {
	create            bool
	createAssignments def.PartitionAssignments
	config            kafka.ConfigOperations
	partitions        def.PartitionAssignments
	assignments       def.PartitionAssignments
}

// Determine if there are any pending operations
func (a applierOps) pending() bool {
	return a.create ||
		len(a.config) > 0 ||
		len(a.partitions) > 0 ||
		len(a.assignments) > 0
}

// An applier handling the apply operation
type applier struct {
	// constructor params
	srv    *kafka.Service
	defDoc string
	opts   ApplierOptions

	// internal
	localDef      def.TopicDefinition
	remoteDef     *def.TopicDefinition
	remoteConfigs def.Configs
	brokers       meta.Brokers
	ops           applierOps

	// result
	res           res.ApplyResult
	reassignments meta.PartitionReassignments
}

// Execute the applier
func (a *applier) Execute() *res.ApplyResult {
	if err := a.apply(); err != nil {
		a.res.Err = err.Error()
		log.Error(err)
	} else {
		// Consider the definition applied if there were ops and this is not a dry run
		if a.ops.pending() && !a.opts.DryRun {
			a.res.Applied = true
		}
	}

	a.res.Data = res.TopicApplyResultData{
		PartitionReassignments: a.reassignments,
	}

	return &a.res
}

// Perform the apply operation sequence
func (a *applier) apply() error {
	// Create the local definition
	if err := a.createLocal(); err != nil {
		return err
	}

	log.Debug("Validating topic definition")
	if err := a.localDef.Validate(); err != nil {
		return err
	}

	// Fetch the remote definition and necessary metadata
	if err := a.tryFetchRemote(); err != nil {
		return err
	}

	// Perform further validations with metadata
	log.Debug("Validating topic definition using cluster metadata")
	if err := a.localDef.ValidateWithMetadata(a.brokers); err != nil {
		return err
	}

	// Build topic operations
	if err := a.buildOps(); err != nil {
		return err
	}

	// Update the apply result with the remote definition and human readable diff
	if err := a.updateApplyResult(); err != nil {
		return err
	}

	if a.ops.pending() {
		// Display diff and pending operations
		if !log.Quiet {
			a.displayPendingOps()
		}

		// Execute operations
		if err := a.executeOps(); err != nil {
			return err
		}

		// Check for in-progress partition reassignments as a result of operations
		if len(a.ops.assignments) > 0 {
			if err := a.fetchPartitionReassignments(false); err != nil {
				return err
			}
			if len(a.reassignments) > 0 {
				if a.opts.ReassAwaitTimeout > 0 {
					// Wait for reassignments to complete
					if err := a.awaitReassignments(a.opts.ReassAwaitTimeout); err != nil {
						return err
					}
				} else if !log.Quiet {
					// Display in-progress partition reassignments
					a.displayPartitionReassignments()
				}
			}
		}

		log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Completed apply for topic %q", a.localDef.Metadata.Name)
	} else {
		log.Info("No changes to apply for topic %q", a.localDef.Metadata.Name)
	}

	return nil
}

// Create the local definition
func (a *applier) createLocal() error {
	switch a.opts.DefinitionFormat {
	case opt.YamlFormat:
		if err := yaml.Unmarshal([]byte(a.defDoc), &a.localDef); err != nil {
			return err
		}
	case opt.JsonFormat:
		if err := json.Unmarshal([]byte(a.defDoc), &a.localDef); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported format")
	}

	// Set the local definition on the apply result
	a.res.LocalDef = &a.localDef

	return nil
}

// Fetch the remote definition and necessary metadata
func (a *applier) tryFetchRemote() error {
	log.Info("Checking if topic %q exists...", a.localDef.Metadata.Name)
	var err error
	a.remoteDef, a.remoteConfigs, a.brokers, err = a.srv.TryRequestTopic(a.localDef.Metadata.Name)
	if err != nil {
		return err
	}

	a.ops.create = (a.remoteDef == nil)
	if a.ops.create {
		log.Debug("Topic %q does not exist", a.localDef.Metadata.Name)
	}

	return nil
}

// Build topic operations
func (a *applier) buildOps() error {
	if a.ops.create {
		a.buildCreateOp()
	} else {
		if err := a.buildConfigOps(); err != nil {
			return err
		}
		if err := a.buildPartitionsOp(); err != nil {
			return err
		}
		a.buildAssignmentsOp()
	}
	return nil
}

// Update the apply result with the remote definition and human readable diff
func (a *applier) updateApplyResult() error {
	// Copy the remote definition
	var remoteCopy *def.TopicDefinition
	if !a.ops.create {
		c := a.remoteDef.Copy()
		remoteCopy = &c
	}

	// Modify the remote definition to remove optional properties not specified in local
	// Further, set properties that are local only and have no remote state
	if remoteCopy != nil {
		// Remove assignments if not specified in local
		if !a.localDef.Spec.HasAssignments() {
			remoteCopy.Spec.Assignments = nil
		}
		if !a.localDef.Spec.HasRackAssignments() {
			remoteCopy.Spec.RackAssignments = nil
		}

		// The only configs we want to see are those specified in local and those in configOps
		// configOps could contain key deletions that should be shown in the diff
		for k := range remoteCopy.Spec.Configs {
			_, existsInLocal := a.localDef.Spec.Configs[k]
			existsInOps := a.ops.config.Contains(k)

			if !existsInLocal && !existsInOps {
				delete(remoteCopy.Spec.Configs, k)
			}
		}

		// Set properties that are local only and have no remote state
		remoteCopy.Spec.DeleteMissingConfigs = a.localDef.Spec.DeleteMissingConfigs
	}

	// Compute diff
	diff, err := jsondiff.Diff(remoteCopy, &a.localDef)
	if err != nil {
		return fmt.Errorf("failed to compute diff: %v", err)
	}

	// Check the diff against the pending operations
	if diffExists := (len(diff) > 0); diffExists != a.ops.pending() {
		return fmt.Errorf("existence of diff was %v, but expected %v", diffExists, a.ops.pending())
	}

	// Update the apply result
	a.res.RemoteDef = remoteCopy
	a.res.Diff = diff

	return nil
}

// Display pending operations
func (a *applier) displayPendingOps() {
	if a.ops.create {
		log.Info("Topic %q does not exist and will be created", a.localDef.Metadata.Name)
	}

	log.Info("Topic %q diff (local -> remote):", a.localDef.Metadata.Name)
	fmt.Print(a.res.Diff)
}

// Execute topic update operations
func (a *applier) executeOps() error {
	if a.ops.create {
		if err := a.createTopic(); err != nil {
			return err
		}
	}

	if len(a.ops.config) > 0 {
		if err := a.updateConfigs(); err != nil {
			return err
		}
	}

	if len(a.ops.partitions) > 0 {
		if err := a.updatePartitions(); err != nil {
			return err
		}
	}

	if len(a.ops.assignments) > 0 {
		if err := a.updateAssignments(); err != nil {
			return err
		}
	}

	return nil
}

// Build create operation
func (a *applier) buildCreateOp() {
	if a.localDef.Spec.HasRackAssignments() {
		// Make an empty set of assignments
		newAssignments := make(def.PartitionAssignments, len(a.localDef.Spec.RackAssignments))
		for i := range newAssignments {
			newAssignments[i] = make([]int32, len(a.localDef.Spec.RackAssignments[0]))
		}
		// Populate local assignments from defined rack assignments
		a.ops.createAssignments = assignments.SyncRackAssignments(
			newAssignments,
			a.localDef.Spec.RackAssignments,
			a.brokers.BrokersByRack(),
		)
	} else {
		a.ops.createAssignments = a.localDef.Spec.Assignments
	}
}

// Execute a request to create a topic
func (a *applier) createTopic() error {
	log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Creating topic %q...", a.localDef.Metadata.Name)

	if err := a.srv.CreateTopic(
		a.localDef,
		a.ops.createAssignments,
		a.opts.DryRun,
	); err != nil {
		return err
	} else {
		log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Created topic %q", a.localDef.Metadata.Name)
	}

	return nil
}

// Build alter configs operations
func (a *applier) buildConfigOps() error {
	log.Debug("Comparing local and remote definition configs for topic %q", a.localDef.Metadata.Name)

	var err error
	a.ops.config, err = a.srv.NewConfigOps(
		a.localDef.Spec.Configs,
		a.remoteDef.Spec.Configs,
		a.remoteConfigs,
		a.localDef.Spec.DeleteMissingConfigs,
	)
	if err != nil {
		return err
	}

	return nil
}

// Update topic configs
func (a *applier) updateConfigs() error {
	if a.ops.config.ContainsOp(kafka.DeleteConfigOperation) && !a.localDef.Spec.DeleteMissingConfigs {
		// This case should only occur when using non-incremental alter configs
		return errors.New("cannot apply configs because deletion of missing configs is not enabled")
	}

	log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Altering configs...")
	if err := a.srv.AlterTopicConfigs(
		a.localDef.Metadata.Name,
		a.ops.config,
		a.opts.DryRun,
	); err != nil {
		return err
	} else {
		log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Altered configs for topic %q", a.localDef.Metadata.Name)
	}

	return nil
}

// Build partitions operation
func (a *applier) buildPartitionsOp() error {
	if a.localDef.Spec.Partitions < a.remoteDef.Spec.Partitions {
		return fmt.Errorf("decreasing the number of partitions is not supported")
	}

	if a.localDef.Spec.Partitions > a.remoteDef.Spec.Partitions {
		log.Debug(
			"The number of partitions has changed and will be increased from %d to %d",
			a.remoteDef.Spec.Partitions,
			a.localDef.Spec.Partitions,
		)
		// Note: It's not necessary to cater specifically for rack assignments here because any miss-placements
		// will be reassigned and migrate to the correct broker very quickly in the cluster.
		a.ops.partitions = assignments.AddPartitions(
			a.remoteDef.Spec.Assignments,
			a.localDef.Spec.Partitions,
			a.brokers.Ids(),
		)
	}

	return nil
}

// Execute a request to create partitions
func (a *applier) updatePartitions() error {
	log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Creating partitions...")

	if err := a.srv.CreatePartitions(
		a.localDef.Metadata.Name,
		a.localDef.Spec.Partitions,
		a.ops.partitions,
		a.opts.DryRun,
	); err != nil {
		return err
	} else {
		log.InfoMaybeWithKey("dry-run", a.opts.DryRun, "Created partitions for topic %q", a.localDef.Metadata.Name)
	}

	return nil
}

// Build assignments operation
func (a *applier) buildAssignmentsOp() {
	if a.localDef.Spec.HasAssignments() {
		if !cmp.Equal(a.remoteDef.Spec.Assignments, a.localDef.Spec.Assignments) {
			log.Debug("Partition assignments have changed and will be updated")
			a.ops.assignments = a.localDef.Spec.Assignments
		}
	} else if a.localDef.Spec.HasRackAssignments() {
		var newAssignments def.PartitionAssignments
		newAssignments = assignments.Copy(a.remoteDef.Spec.Assignments)
		if len(a.ops.partitions) > 0 {
			// Assignments will include new partitions that will be added
			newAssignments = append(newAssignments, a.ops.partitions...)
		}
		// Sync remote assignments with local rack assignments
		newAssignments = assignments.SyncRackAssignments(
			newAssignments,
			a.localDef.Spec.RackAssignments,
			a.brokers.BrokersByRack(),
		)
		if !cmp.Equal(a.remoteDef.Spec.Assignments, newAssignments) {
			log.Debug("Partition assignments are out of sync with defined racks and will be updated")
			a.ops.assignments = newAssignments
		}
	} else {
		if a.localDef.Spec.ReplicationFactor != a.remoteDef.Spec.ReplicationFactor {
			log.Debug("Replication factor has changed and will be updated")
			var newAssignments def.PartitionAssignments
			newAssignments = assignments.Copy(a.remoteDef.Spec.Assignments)
			if len(a.ops.partitions) > 0 {
				// Assignments will include new partitions that will be added
				newAssignments = append(newAssignments, a.ops.partitions...)
			}
			a.ops.assignments = assignments.AlterReplicationFactor(
				newAssignments,
				a.localDef.Spec.ReplicationFactor,
				a.brokers.Ids(),
			)
		}
	}
}

// Execute a request to list partition reassignments
func (a *applier) fetchPartitionReassignments(suppressLog bool) error {
	if !(suppressLog) {
		log.Debug("Fetching in-progress partition reassignments for topic %q", a.localDef.Metadata.Name)
	}

	partitions := make([]int32, a.localDef.Spec.Partitions)
	for i := range partitions {
		partitions[i] = int32(i)
	}

	var err error
	a.reassignments, err = a.srv.ListPartitionReassignments(
		a.localDef.Metadata.Name,
		partitions,
	)
	if err != nil {
		return err
	}

	return nil
}

// Display in-progress partition reassignments
func (a *applier) displayPartitionReassignments() {
	log.Info("In-progress partition reassignments for topic %q:", a.localDef.Metadata.Name)
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(table.Row{"Partition", "Replicas", "Adding Replicas", "Removing Replicas"})
	for _, r := range a.reassignments {
		t.AppendRow([]interface{}{
			fmt.Sprint(r.Partition),
			fmt.Sprint(r.Replicas),
			fmt.Sprint(r.AddingReplicas),
			fmt.Sprint(r.RemovingReplicas),
		})
	}
	t.SetStyle(table.StyleLight)
	t.Render()
}

// Execute a request to alter assignments
func (a *applier) updateAssignments() error {
	if a.opts.DryRun {
		// AlterPartitionAssignments has no 'ValidateOnly' for dry-run mode so we check
		// in-progress partition reassignments and error if found.
		if err := a.fetchPartitionReassignments(false); err != nil {
			return err
		}
		if len(a.reassignments) > 0 {
			// Kafka would return a very similiar error if we attempted to execute the reassignment
			return fmt.Errorf("a partition reassignment is in progress for the topic %q", a.localDef.Metadata.Name)
		}

		log.Info("Skipped altering partition assignments (dry-run not available)")
	} else {
		log.Info("Altering partition assignments...")

		if err := a.srv.AlterPartitionAssignments(
			a.localDef.Metadata.Name,
			a.ops.assignments,
		); err != nil {
			return err
		} else {
			log.Info("Altered partition assignments for topic %q", a.localDef.Metadata.Name)
		}
	}

	return nil
}

// Await the completion of in-progress partition reassignments
func (a *applier) awaitReassignments(timeoutSec int) error {
	log.Info("Awaiting completion of partition reassignments (timeout: %d seconds)...", timeoutSec)
	timeout := time.After(time.Duration(timeoutSec) * time.Second)

	remaining := 0
	for {
		select {
		case <-timeout:
			log.Info("Awaiting completion of partition reassignments timed out after %d seconds", timeoutSec)
			return nil
		default:
			if err := a.fetchPartitionReassignments(true); err != nil {
				return err
			}
			if len(a.reassignments) > 0 {
				if !log.Quiet && len(a.reassignments) != remaining {
					a.displayPartitionReassignments()
				}
				remaining = len(a.reassignments)
			} else {
				log.Info("Partition reassignments completed")
				return nil
			}

			time.Sleep(5 * time.Second)
			continue
		}
	}
}
