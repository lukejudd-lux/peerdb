package model

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

// TestMarshalJSONMatchesToMap proves the streaming encoder (writeJSON, used by
// MarshalJSONWithOptions) produces output semantically identical to the reference
// json.Marshal(toMap(...)) across a broad set of QValue kinds, including the ones
// that ride the WriteVal fallback (bytes/base64, int256, uuid arrays, inet, etc.).
func TestMarshalJSONMatchesToMap(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	tz := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 2*3600))
	u := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	items := NewRecordItems(0)
	items.AddColumn("null_col", nil)
	items.AddColumn("str", types.QValueString{Val: `he said "hi"` + "\n<tag>"})
	items.AddColumn("json", types.QValueJSON{Val: `{"a":1,"b":[1,2,{"n":1000000000000000000000}]}`})
	items.AddColumn("hstore", types.QValueHStore{Val: `"k"=>"v", "n"=>NULL`})
	items.AddColumn("uuid", types.QValueUUID{Val: u})
	items.AddColumn("qchar", types.QValueQChar{Val: 'x'})
	items.AddColumn("bytes", types.QValueBytes{Val: []byte{0, 1, 2, 250, 255}})
	items.AddColumn("ts", types.QValueTimestamp{Val: ts})
	items.AddColumn("tstz", types.QValueTimestampTZ{Val: tz})
	items.AddColumn("date", types.QValueDate{Val: ts})
	items.AddColumn("numeric", types.QValueNumeric{Val: decimal.RequireFromString("1234.50")})
	items.AddColumn("arr_numeric", types.QValueArrayNumeric{Val: []decimal.Decimal{
		decimal.RequireFromString("1.1"), decimal.RequireFromString("2.2"),
	}})
	items.AddColumn("f64", types.QValueFloat64{Val: 2.5})
	items.AddColumn("f64_nan", types.QValueFloat64{Val: math.NaN()})
	items.AddColumn("f32", types.QValueFloat32{Val: 1.5})
	items.AddColumn("arr_f64", types.QValueArrayFloat64{Val: []float64{1.5, math.Inf(1), 3.5}})
	items.AddColumn("arr_date", types.QValueArrayDate{Val: []time.Time{ts, ts}})
	// default / WriteVal fallback kinds
	items.AddColumn("i32", types.QValueInt32{Val: -7})
	items.AddColumn("i64", types.QValueInt64{Val: 9223372036854775807})
	items.AddColumn("i256", types.QValueInt256{Val: new(big.Int).SetInt64(123456789)})
	items.AddColumn("bool", types.QValueBoolean{Val: true})
	items.AddColumn("enum", types.QValueEnum{Val: "active"})
	items.AddColumn("inet", types.QValueINET{Val: "10.0.0.1/32"})
	items.AddColumn("cidr", types.QValueCIDR{Val: "10.0.0.0/24"})
	items.AddColumn("macaddr", types.QValueMacaddr{Val: "08:00:2b:01:02:03"})
	items.AddColumn("interval", types.QValueInterval{Val: "1 day"})
	items.AddColumn("geo", types.QValueGeography{Val: "POINT(1 2)"})
	items.AddColumn("point", types.QValuePoint{Val: "POINT(3 4)"})
	items.AddColumn("arr_uuid", types.QValueArrayUUID{Val: []uuid.UUID{u, u}})
	items.AddColumn("arr_str", types.QValueArrayString{Val: []string{"a", "b"}})
	items.AddColumn("arr_i32", types.QValueArrayInt32{Val: []int32{1, 2, 3}})

	for _, opts := range []ToJSONOptions{
		NewToJSONOptions(nil, true),
		{HStoreAsJSON: true, ClearValuesOverBytes: 5},
	} {
		reference, err := items.toMap(opts)
		require.NoError(t, err)
		want, err := json.Marshal(reference)
		require.NoError(t, err)

		got, err := items.MarshalJSONWithOptions(opts)
		require.NoError(t, err)

		require.JSONEq(t, string(want), string(got),
			"streaming output must match json.Marshal(toMap) for opts %+v", opts)
	}
}

func benchItems() RecordItems {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	it := NewRecordItems(0)
	it.AddColumn("id", types.QValueInt64{Val: 42})
	it.AddColumn("name", types.QValueString{Val: "a moderately sized string value here"})
	it.AddColumn("payload", types.QValueJSON{Val: `{"a":1,"b":[1,2,3],"c":{"d":"e"}}`})
	it.AddColumn("amount", types.QValueNumeric{Val: decimal.RequireFromString("1234.56")})
	it.AddColumn("created_at", types.QValueTimestamp{Val: ts})
	it.AddColumn("active", types.QValueBoolean{Val: true})
	return it
}

func BenchmarkMarshalStreaming(b *testing.B) {
	it := benchItems()
	opts := NewToJSONOptions(nil, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = it.MarshalJSONWithOptions(opts)
	}
}

func BenchmarkMarshalReference(b *testing.B) {
	it := benchItems()
	opts := NewToJSONOptions(nil, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, _ := it.toMap(opts)
		_, _ = json.Marshal(m)
	}
}
