package connpostgres

import (
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/otel_metrics"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

func newTestCDCSource(t *testing.T) *PostgresCDCSource {
	t.Helper()
	return &PostgresCDCSource{
		PostgresConnector: &PostgresConnector{typeMap: pgtype.NewMap()},
		jsonApi:           createExtendedJSONUnmarshaler(),
		otelManager:       &otel_metrics.OtelManager{},
	}
}

func cdcTestTableSchema(columnName string, kind types.QValueKind) *protos.TableSchema {
	return &protos.TableSchema{
		Columns: []*protos.FieldDescription{
			{Name: columnName, Type: string(kind)},
		},
	}
}

func TestProcessMessageInvalidMessage(t *testing.T) {
	t.Parallel()

	p := newTestCDCSource(t)
	batch := model.NewCDCStream[model.RecordItems](0)

	xld := pglogrepl.XLogData{
		WALStart:     pglogrepl.LSN(0x01),
		ServerWALEnd: pglogrepl.LSN(0x02),
		// 'S' is a v2 Stream Start tag — not handled by pglogrepl.Parse (v1)
		WALData: []byte{'S', 0, 1, 0, 1 /*arbitrary bytes*/},
	}

	rec, err := processMessage(t.Context(), p, batch, xld, xld.WALStart, 0, qProcessor{}, map[string]struct{}{})
	require.Nil(t, rec)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error parsing logical message (msgType=\"S\", walStart=0/1)")
}

// TestDefaultExprFromPostgresMissingValue feeds in scalar values extracted from attmissingval; want
// is empty for the types and values we decline to translate.
func TestDefaultExprFromPostgresMissingValue(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value string
		qkind types.QValueKind
		want  string
	}{
		{name: "int", value: "5", qkind: types.QValueKindInt32, want: "5"},
		{name: "int negative", value: "-1", qkind: types.QValueKindInt32, want: "-1"},
		{name: "int max", value: "2147483647", qkind: types.QValueKindInt32, want: "2147483647"},
		{name: "smallint negative", value: "-32768", qkind: types.QValueKindInt16, want: "-32768"},
		{name: "bigint", value: "9223372036854775807", qkind: types.QValueKindInt64, want: "9223372036854775807"},
		{name: "oid", value: "42", qkind: types.QValueKindUInt32, want: "42"},

		{name: "numeric", value: "1.50", qkind: types.QValueKindNumeric, want: "1.50"},
		{name: "double", value: "2.5", qkind: types.QValueKindFloat64, want: "2.5"},
		{name: "real negative", value: "-1.5", qkind: types.QValueKindFloat32, want: "-1.5"},
		{name: "double exponent", value: "1e-05", qkind: types.QValueKindFloat64, want: "1e-05"},

		{name: "bool true", value: "true", qkind: types.QValueKindBoolean, want: "true"},
		{name: "bool false", value: "false", qkind: types.QValueKindBoolean, want: "false"},

		// strings, requoted with SQL doubling for ClickHouse
		{name: "text", value: "hello", qkind: types.QValueKindString, want: "'hello'"},
		{name: "text empty", value: "", qkind: types.QValueKindString, want: "''"},
		{name: "text with quote", value: "it's", qkind: types.QValueKindString, want: "'it''s'"},
		{name: "text with backslash n", value: "\\n", qkind: types.QValueKindString, want: "'\\\\n'"},
		{name: "text with backslash", value: "\\", qkind: types.QValueKindString, want: "'\\\\'"},
		{name: "text with two backslashes", value: "\\\\", qkind: types.QValueKindString, want: "'\\\\\\\\'"},
		{
			name: "text with mixed escapes", value: "\\n\\\\'", qkind: types.QValueKindString,
			want: "'\\\\n\\\\\\\\'''",
		},
		{name: "text with cast text", value: "a::b", qkind: types.QValueKindString, want: "'a::b'"},
		{name: "qchar", value: "x", qkind: types.QValueKindQChar, want: "'x'"},
		{name: "enum", value: "ok", qkind: types.QValueKindEnum, want: "'ok'"},
		{
			name: "uuid", value: "00000000-0000-0000-0000-000000000001", qkind: types.QValueKindUUID,
			want: "'00000000-0000-0000-0000-000000000001'",
		},
		{name: "jsonb", value: `{"a": 1}`, qkind: types.QValueKindJSONB, want: `'{"a": 1}'`},
		{
			name: "hstore canonicalized", value: `{"z": "<&>", "a": null}`,
			qkind: types.QValueKindHStore, want: `'{"a":null,"z":"\\u003c\\u0026\\u003e"}'`,
		},
		{name: "inet", value: "10.0.0.1", qkind: types.QValueKindINET, want: "'10.0.0.1'"},
		{name: "date", value: "2020-01-02", qkind: types.QValueKindDate, want: "'2020-01-02'"},
		{
			name: "timestamp", value: "2020-01-02T03:04:05.678",
			qkind: types.QValueKindTimestamp, want: "'2020-01-02 03:04:05.678'",
		},
		{
			name: "timestamptz", value: "2020-01-02T01:04:05+00:00",
			qkind: types.QValueKindTimestampTZ, want: "'2020-01-02 01:04:05'",
		},
		{name: "text with backslash b", value: `a\b`, qkind: types.QValueKindString, want: "'a\\\\b'"},
		{name: "text with newline", value: "a\nb", qkind: types.QValueKindString, want: "'a\nb'"},

		// declined: types whose text form does not carry over
		{name: "array", value: "[1,2]", qkind: types.QValueKindArrayInt32},
		{name: "bytea", value: `\x0001`, qkind: types.QValueKindBytes},
		{name: "hstore non-string value", value: `{"a": 1}`, qkind: types.QValueKindHStore},
		{name: "interval", value: "1 day", qkind: types.QValueKindInterval},
		{name: "time", value: "13:14:15", qkind: types.QValueKindTime},
		{name: "point", value: "(1,2)", qkind: types.QValueKindPoint},
		{name: "numeric nan", value: "NaN", qkind: types.QValueKindNumeric},
		{name: "double infinity", value: "Infinity", qkind: types.QValueKindFloat64},
		{
			name: "timestamptz non utc offset", value: "2020-01-02T01:04:05+02:00",
			qkind: types.QValueKindTimestampTZ,
		},
		// declined: value of the wrong shape for the column
		{name: "quoted bool", value: "'true'", qkind: types.QValueKindBoolean},
		{name: "text for number", value: "abc", qkind: types.QValueKindInt32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			literal, ok := defaultExprFromPostgresMissingValue(tc.value, tc.qkind)
			require.Equal(t, tc.want != "", ok)
			require.Equal(t, tc.want, literal)
		})
	}
}

func TestCDCDecodeJSONPassthroughPreservesRawNestedJSON(t *testing.T) {
	t.Parallel()

	p := newTestCDCSource(t)
	rawJSON := `{"items":[{"n":1000000000000000000000000000000000000000}],"deep":{"array":[1,2,{"ok":true}]}}`

	qvalue, err := p.decodeColumnData(
		[]byte(rawJSON),
		pgtype.JSONOID,
		-1,
		pgtype.TextFormatCode,
		nil,
		0,
		true,
	)

	require.NoError(t, err)
	jsonValue, ok := qvalue.(types.QValueJSON)
	require.True(t, ok)
	require.Equal(t, rawJSON, jsonValue.Val)
	require.False(t, jsonValue.IsArray)
}

func TestValidateCDCJSONPassthroughConfig(t *testing.T) {
	t.Parallel()

	tableMapping := map[string]model.NameAndExclude{
		"public.events": model.NewNameAndExclude("dataset.events", nil, []string{"payload"}),
	}

	tests := []struct {
		name          string
		destination   protos.DBType
		schemaMapping map[string]*protos.TableSchema
		wantErr       string
	}{
		{
			name:        "allows scalar json",
			destination: protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindJSON),
			},
		},
		{
			name:        "allows scalar jsonb",
			destination: protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindJSONB),
			},
		},
		{
			name:        "rejects non BigQuery destination",
			destination: protos.DBType_POSTGRES,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindJSON),
			},
			wantErr: "non-BigQuery destination",
		},
		{
			name:        "rejects json array schema",
			destination: protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindArrayJSON),
			},
			wantErr: "unsupported JSON array type",
		},
		{
			name:        "rejects non JSON schema",
			destination: protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindString),
			},
			wantErr: "maps to non-JSON destination type",
		},
		{
			name:        "rejects missing column",
			destination: protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("other_payload", types.QValueKindJSON),
			},
			wantErr: "destination column was not found",
		},
		{
			name:          "rejects missing table schema",
			destination:   protos.DBType_BIGQUERY,
			schemaMapping: map[string]*protos.TableSchema{},
			wantErr:       "destination table schema was not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCDCJSONPassthroughConfig(tt.destination, tableMapping, tt.schemaMapping)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestShouldPassthroughJSONColumnOIDValidation(t *testing.T) {
	t.Parallel()

	nameAndExclude := model.NewNameAndExclude("dataset.events", nil, []string{"payload"})

	tests := []struct {
		name            string
		columnName      string
		oid             uint32
		wantPassthrough bool
		wantErr         string
	}{
		{
			name:            "allows json",
			columnName:      "payload",
			oid:             pgtype.JSONOID,
			wantPassthrough: true,
		},
		{
			name:            "allows jsonb",
			columnName:      "payload",
			oid:             pgtype.JSONBOID,
			wantPassthrough: true,
		},
		{
			name:       "rejects json array",
			columnName: "payload",
			oid:        pgtype.JSONArrayOID,
			wantErr:    "unsupported Postgres JSON array type OID",
		},
		{
			name:       "rejects non JSON",
			columnName: "payload",
			oid:        pgtype.Int4OID,
			wantErr:    "non-JSON Postgres type OID",
		},
		{
			name:       "ignores columns outside allowlist",
			columnName: "other_payload",
			oid:        pgtype.Int4OID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newTestCDCSource(t)
			p.destinationType = protos.DBType_BIGQUERY
			p.tableNameSchemaMapping = map[string]*protos.TableSchema{
				"dataset.events": cdcTestTableSchema("payload", types.QValueKindJSON),
			}

			passthrough, err := p.shouldPassthroughJSONColumn(nameAndExclude, &pglogrepl.RelationMessageColumn{
				Name:     tt.columnName,
				DataType: tt.oid,
			})
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, tt.wantPassthrough, passthrough)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
