package connbigquery

import (
	"testing"

	"cloud.google.com/go/bigquery"
	"github.com/stretchr/testify/require"

	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

func fieldNames(schema bigquery.Schema) []string {
	names := make([]string, 0, len(schema))
	for _, f := range schema {
		names = append(names, f.Name)
	}
	return names
}

func TestAlignQRepSchemaToSource(t *testing.T) {
	dst := bigquery.Schema{
		{Name: "upddat", Type: bigquery.TimestampFieldType},
		{Name: "usrcod", Type: bigquery.StringFieldType},
		{Name: "rolcod", Type: bigquery.StringFieldType},
		{Name: "dropped_at_source", Type: bigquery.StringFieldType},
		{Name: "_PEERDB_SYNCED_AT", Type: bigquery.TimestampFieldType},
	}

	t.Run("reorders by name to source order", func(t *testing.T) {
		src := types.NewQRecordSchema([]types.QField{
			{Name: "rolcod", Type: types.QValueKindString},
			{Name: "USRCOD", Type: types.QValueKindString},
			{Name: "upddat", Type: types.QValueKindTimestamp},
		})
		aligned, err := alignQRepSchemaToSource(dst, src, "_PEERDB_SYNCED_AT", "")
		require.NoError(t, err)
		require.Equal(t, []string{"rolcod", "usrcod", "upddat", "_PEERDB_SYNCED_AT"}, fieldNames(aligned))
	})

	t.Run("leaves out destination columns the source no longer sends", func(t *testing.T) {
		src := types.NewQRecordSchema([]types.QField{{Name: "usrcod", Type: types.QValueKindString}})
		aligned, err := alignQRepSchemaToSource(dst, src, "", "")
		require.NoError(t, err)
		require.Equal(t, []string{"usrcod"}, fieldNames(aligned))
	})

	t.Run("errors when a source column has no destination column", func(t *testing.T) {
		src := types.NewQRecordSchema([]types.QField{{Name: "brand_new", Type: types.QValueKindString}})
		_, err := alignQRepSchemaToSource(dst, src, "", "")
		require.ErrorContains(t, err, "brand_new")
	})
}
