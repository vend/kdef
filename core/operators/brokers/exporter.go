package brokers

import (
	"github.com/peter-evans/kdef/cli/log"
	"github.com/peter-evans/kdef/client"
	"github.com/peter-evans/kdef/core/kafka"
	"github.com/peter-evans/kdef/core/model/def"
	"github.com/peter-evans/kdef/core/model/res"
)

// Create a new exporter
func NewExporter(
	cl *client.Client,
) *exporter {
	return &exporter{
		srv: kafka.NewService(cl),
	}
}

// An exporter handling the export operation
type exporter struct {
	// constructor params
	srv *kafka.Service
}

// Execute the export operation
func (e *exporter) Execute() (res.ExportResults, error) {
	log.Info("Fetching cluster-wide broker configuration...")
	brokersDef, err := e.getBrokersDefinition()
	if err != nil {
		return nil, err
	}

	results := make(res.ExportResults, 1)
	results[0] = res.ExportResult{
		Id:  brokersDef.Metadata.Name,
		Def: brokersDef,
	}

	return results, nil
}

// Return the brokers definition
func (e *exporter) getBrokersDefinition() (*def.BrokersDefinition, error) {
	brokerConfigs, err := e.srv.DescribeAllBrokerConfigs()
	if err != nil {
		return nil, err
	}

	brokersDef := def.NewBrokersDefinition(brokerConfigs.ToExportableMap())

	return &brokersDef, nil
}