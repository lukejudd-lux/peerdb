package utils

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

// _peerdb_match_data (entries[5]) carries the old row and is only consumed by
// ClickHouse. BigQuery stores the column but never reads it, so the update path
// must skip serializing old items for BigQuery (empty string) while still
// producing them for destinations that consume it.
func TestRecordToQRecordUpdateMatchData(t *testing.T) {
	newItems := model.NewRecordItems(1)
	newItems.AddColumn("id", types.QValueInt32{Val: 2})
	oldItems := model.NewRecordItems(1)
	oldItems.AddColumn("id", types.QValueInt32{Val: 1})

	rec := &model.UpdateRecord[model.RecordItems]{
		NewItems:             newItems,
		OldItems:             oldItems,
		SourceTableName:      "src",
		DestinationTableName: "dst",
	}
	truncator := model.NewStreamNumericTruncator(nil, map[string]struct{}{})

	bqEntries, err := recordToQRecordOrError(1, rec, protos.DBType_BIGQUERY, false, truncator)
	require.NoError(t, err)
	require.Equal(t, "", bqEntries[5].Value(), "BigQuery should not serialize match_data")
	require.NotEmpty(t, bqEntries[3].Value(), "new items are still serialized")

	chEntries, err := recordToQRecordOrError(1, rec, protos.DBType_CLICKHOUSE, false, truncator)
	require.NoError(t, err)
	chMatch, ok := chEntries[5].Value().(string)
	require.True(t, ok)
	require.Contains(t, chMatch, "id", "ClickHouse still gets old items in match_data")
}
