package connpostgres

import (
	"fmt"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/pkg/common"
	"github.com/PeerDB-io/peerdb/flow/shared"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

// TestProcessInsertMessage_ChildTableDifferentColumnType verifies that when we process
// an InsertMessage from a child table, we decode the tuple using the *child's* relation
// schema, not the parent's. This is the fix for the bug where a child table had a column
// as TEXT (e.g. Stripe ID "ch_3T1KxCFUtwYrZPVC0M6OeTpy") while the parent declared the same
// column as UUID, causing "cannot parse UUID" when decoding with the parent's schema.
//
// In standard PostgreSQL inheritance, the same column cannot have different types in
// parent vs child. This test simulates the scenario by:
// 1. Creating real parent + child tables (same schema so pg_inherits works).
// 2. Injecting synthetic RelationMessages: parent has "id" as UUID OID, child has "id" as TEXT OID.
// 3. Building an InsertMessage from the child with tuple data "ch_3T1KxCFUtwYrZPVC0M6OeTpy".
// 4. Asserting we decode to QValueString (success); using parent's schema would try UUID parse and fail.
func TestProcessInsertMessage_ChildTableDifferentColumnType(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	connector, schemaName := setupDB(t, "cdc_inherit_diff_type")
	defer connector.Close()
	defer teardownDB(t, connector.conn, schemaName)

	parentTable := common.QuoteIdentifier(schemaName) + ".parent"
	childTable := common.QuoteIdentifier(schemaName) + ".child"

	// Create parent and child (same column types in PG so inheritance works)
	_, err := connector.conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (id SERIAL PRIMARY KEY, name TEXT);
		CREATE TABLE %s () INHERITS (%s);
	`, parentTable, childTable, parentTable))
	require.NoError(t, err)

	// Get parent and child relation OIDs
	var parentOID, childOID uint32
	err = connector.conn.QueryRow(ctx,
		`SELECT c.oid FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid WHERE n.nspname = $1 AND c.relname = 'parent'`,
		schemaName).Scan(&parentOID)
	require.NoError(t, err)
	err = connector.conn.QueryRow(ctx,
		`SELECT c.oid FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid WHERE n.nspname = $1 AND c.relname = 'child'`,
		schemaName).Scan(&childOID)
	require.NoError(t, err)

	tableName := schemaName + ".parent"
	srcTableIDNameMapping := map[uint32]string{parentOID: tableName}
	tableNameMapping := map[string]model.NameAndExclude{
		tableName: {Name: "dest_table", Exclude: nil},
	}

	cdc, err := connector.NewPostgresCDCSource(ctx, &PostgresCDCConfig{
		SrcTableIDNameMapping:                    srcTableIDNameMapping,
		TableNameMapping:                         tableNameMapping,
		TableNameSchemaMapping:                   nil,
		RelationMessageMapping:                   connector.relationMessageMapping,
		FlowJobName:                              "test_flow",
		Slot:                                     "test_slot",
		Publication:                              "test_pub",
		HandleInheritanceForNonPartitionedTables: true,
		InternalVersion:                          shared.InternalVersion_Latest,
	})
	require.NoError(t, err)

	// Simulate "child has id as TEXT" while "parent has id as UUID" by injecting relation messages.
	// Child relation: one column "id" with TEXT OID so we decode the tuple as string.
	childRelMsg := &pglogrepl.RelationMessage{
		RelationID: childOID,
		Columns: []*pglogrepl.RelationMessageColumn{
			{Name: "id", DataType: pgtype.TextOID, TypeModifier: -1},
		},
	}
	parentRelMsg := &pglogrepl.RelationMessage{
		RelationID: parentOID,
		Columns: []*pglogrepl.RelationMessageColumn{
			{Name: "id", DataType: pgtype.UUIDOID, TypeModifier: -1},
		},
	}
	connector.relationMessageMapping[childOID] = childRelMsg
	connector.relationMessageMapping[parentOID] = parentRelMsg

	// InsertMessage from child: one column, text value that is not a valid UUID
	stripeLikeID := "ch_3T1KxCFUtwYrZPVC0M6OeTpy"
	insertMsg := &pglogrepl.InsertMessage{
		RelationID: childOID,
		Tuple: &pglogrepl.TupleData{
			Columns: []*pglogrepl.TupleDataColumn{
				{DataType: 't', Data: []byte(stripeLikeID)},
			},
		},
	}

	record, err := processInsertMessage(cdc, pglogrepl.LSN(0), insertMsg, qProcessor{}, nil)
	require.NoError(t, err)
	require.NotNil(t, record)

	insertRec, ok := record.(*model.InsertRecord[model.RecordItems])
	require.True(t, ok)
	require.Equal(t, tableName, insertRec.SourceTableName)
	require.Equal(t, "dest_table", insertRec.DestinationTableName)

	val := insertRec.Items.GetColumnValue("id")
	require.NotNil(t, val)
	require.Equal(t, types.QValueKindString, val.Kind())
	require.Equal(t, stripeLikeID, val.Value(), "id must be decoded as string (child schema), not UUID")
}
