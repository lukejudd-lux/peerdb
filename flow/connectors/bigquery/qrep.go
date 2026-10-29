package connbigquery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"cloud.google.com/go/bigquery"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/shared"
	"github.com/PeerDB-io/peerdb/flow/shared/datatypes"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

func (c *BigQueryConnector) SyncQRepRecords(
	ctx context.Context,
	config *protos.QRepConfig,
	partition *protos.QRepPartition,
	stream *model.QRecordStream,
) (int64, shared.QRepWarnings, error) {
	// Ensure the destination table is available.
	destTable := config.DestinationTableIdentifier
	srcSchema, err := stream.Schema()
	if err != nil {
		return 0, nil, err
	}

	tblMetadata, err := c.replayTableSchemaDeltasQRep(ctx, config, partition, srcSchema)
	if err != nil {
		return 0, nil, err
	}

	if err := validateQRepJSONPassthroughColumns(config.JsonPassthroughColumns, srcSchema, tblMetadata.Schema); err != nil {
		return 0, nil, err
	}

	alignedSchema, err := alignQRepSchemaToSource(tblMetadata.Schema, srcSchema, config.SyncedAtColName, config.SoftDeleteColName)
	if err != nil {
		return 0, nil, fmt.Errorf("destination table %s: %w", destTable, err)
	}
	alignedMetadata := *tblMetadata
	alignedMetadata.Schema = alignedSchema

	c.logger.Info(fmt.Sprintf("QRep sync function called and partition existence checked for"+
		" partition %s of destination table %s",
		partition.PartitionId, destTable))

	avroSync := NewQRepAvroSyncMethod(c, config.StagingPath, config.FlowJobName)
	result, err := avroSync.SyncQRepRecords(ctx, config.Env, config.FlowJobName, destTable, partition,
		&alignedMetadata, stream, config.SyncedAtColName, config.SoftDeleteColName)
	if err != nil {
		return result, nil, err
	}
	return result, nil, nil
}

// alignQRepSchemaToSource returns the destination schema reordered to follow the source's columns.
// The Avro stage is written positionally from source records, so the schema used for the Avro file,
// the INSERT column list and the select list must follow the source order, matched by name.
// Without this, a source whose column order differs from the destination table (e.g. a view that was
// recreated with columns in a different order) either fails on a type mismatch or, when the swapped
// columns share a type, silently loads values into the wrong columns.
// Destination columns the source no longer sends are left out of the load, so they land as NULL.
// The synced-at and soft-delete columns are filled in by the INSERT itself and are kept at the end.
func alignQRepSchemaToSource(
	dst bigquery.Schema,
	src types.QRecordSchema,
	syncedAtCol string,
	softDeleteCol string,
) (bigquery.Schema, error) {
	aligned := make(bigquery.Schema, 0, len(dst))
	for _, srcField := range src.Fields {
		var match *bigquery.FieldSchema
		for _, dstField := range dst {
			if strings.EqualFold(srcField.Name, dstField.Name) {
				match = dstField
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("source column %s has no matching destination column", srcField.Name)
		}
		aligned = append(aligned, match)
	}
	for _, dstField := range dst {
		if (syncedAtCol != "" && dstField.Name == syncedAtCol) || (softDeleteCol != "" && dstField.Name == softDeleteCol) {
			aligned = append(aligned, dstField)
		}
	}
	return aligned, nil
}

func (c *BigQueryConnector) replayTableSchemaDeltasQRep(
	ctx context.Context,
	config *protos.QRepConfig,
	partition *protos.QRepPartition,
	srcSchema types.QRecordSchema,
) (*bigquery.TableMetadata, error) {
	destDatasetTable, _ := c.convertToDatasetTable(config.DestinationTableIdentifier)
	bqTable := c.client.DatasetInProject(c.projectID, destDatasetTable.dataset).Table(destDatasetTable.table)
	dstTableMetadata, err := bqTable.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of table %s: %w", destDatasetTable, err)
	}

	tableSchemaDelta := &protos.TableSchemaDelta{
		SrcTableName: config.WatermarkTable,
		DstTableName: config.DestinationTableIdentifier,
	}

	for _, col := range srcSchema.Fields {
		hasColumn := false
		// check ignoring case
		for _, dstCol := range dstTableMetadata.Schema {
			if strings.EqualFold(col.Name, dstCol.Name) {
				hasColumn = true
				break
			}
		}

		if !hasColumn {
			c.logger.Info(fmt.Sprintf("adding column %s to destination table %s",
				col.Name, config.DestinationTableIdentifier),
				slog.String(string(shared.PartitionIDKey), partition.PartitionId))
			tableSchemaDelta.AddedColumns = append(tableSchemaDelta.AddedColumns, &protos.FieldDescription{
				Name:         col.Name,
				Type:         string(col.Type),
				TypeModifier: datatypes.MakeNumericTypmod(int32(col.Precision), int32(col.Scale)),
			})
		}
	}

	if err := c.ReplayTableSchemaDeltas(
		ctx, config.Env, config.FlowJobName, nil, []*protos.TableSchemaDelta{tableSchemaDelta}, nil,
	); err != nil {
		return nil, fmt.Errorf("failed to add columns to destination table: %w", err)
	}
	dstTableMetadata, err = bqTable.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of table %s: %w", destDatasetTable, err)
	}
	return dstTableMetadata, nil
}

func validateQRepJSONPassthroughColumns(
	jsonPassthroughColumns []string,
	srcSchema types.QRecordSchema,
	dstSchema bigquery.Schema,
) error {
	if len(jsonPassthroughColumns) == 0 {
		return nil
	}

	sourceTypes := make(map[string]types.QValueKind, len(srcSchema.Fields))
	for _, field := range srcSchema.Fields {
		sourceTypes[field.Name] = field.Type
	}
	destinationTypes := make(map[string]bigquery.FieldType, len(dstSchema))
	for _, field := range dstSchema {
		destinationTypes[field.Name] = field.Type
	}

	for _, columnName := range jsonPassthroughColumns {
		sourceType, ok := sourceTypes[columnName]
		if !ok {
			return fmt.Errorf("json passthrough column %s was not found in source schema", columnName)
		}
		switch sourceType {
		case types.QValueKindJSON, types.QValueKindJSONB:
		case types.QValueKindArrayJSON, types.QValueKindArrayJSONB:
			return fmt.Errorf("json passthrough column %s has unsupported JSON array source type %s",
				columnName, sourceType)
		default:
			return fmt.Errorf("json passthrough column %s has non-JSON source type %s", columnName, sourceType)
		}

		destinationType, ok := destinationTypes[columnName]
		if !ok {
			return fmt.Errorf("json passthrough column %s was not found in BigQuery destination schema", columnName)
		}
		if destinationType != bigquery.JSONFieldType {
			return fmt.Errorf("json passthrough column %s has non-JSON BigQuery destination type %s",
				columnName, destinationType)
		}
	}
	return nil
}

func (c *BigQueryConnector) SetupQRepMetadataTables(ctx context.Context, config *protos.QRepConfig) error {
	if config.WriteMode.WriteType == protos.QRepWriteType_QREP_WRITE_MODE_OVERWRITE {
		query := c.queryWithLogging("TRUNCATE TABLE " + config.DestinationTableIdentifier)
		query.DefaultDatasetID = c.datasetID
		query.DefaultProjectID = c.projectID
		if _, err := query.Read(ctx); err != nil {
			return fmt.Errorf("failed to TRUNCATE table before query replication: %w", err)
		}
	}

	return nil
}
