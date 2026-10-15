package model

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math"

	jsoniter "github.com/json-iterator/go"

	"github.com/PeerDB-io/peerdb/flow/shared/datatypes"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

// rawTableJSON streams the _peerdb_data / _peerdb_match_data JSON without the
// intermediate map[string]any that toMap builds. It is configured to match the
// standard library's encoding (notably []byte -> base64) so the output is
// semantically identical to json.Marshal(toMap(...)); downstream warehouses read
// the blob by JSON key path, so key order and escaping differences are immaterial.
var rawTableJSON = jsoniter.ConfigCompatibleWithStandardLibrary

type Items interface {
	json.Marshaler
	UpdateIfNotExists(Items) []string
	UpdateWithBaseRecord(BaseRecord)
	GetBytesByColName(string) ([]byte, error)
	ToJSONWithOptions(ToJSONOptions) (string, error)
	DeleteColName(string)
}

func ItemsToJSON(items Items) (string, error) {
	bytes, err := items.MarshalJSON()
	return string(bytes), err
}

// encoding/gob cannot encode unexported fields
type RecordItems struct {
	ColToVal map[string]types.QValue
}

func NewRecordItems(capacity int) RecordItems {
	return RecordItems{
		ColToVal: make(map[string]types.QValue, capacity),
	}
}

func (r RecordItems) AddColumn(col string, val types.QValue) {
	r.ColToVal[col] = val
}

func (r RecordItems) GetColumnValue(col string) types.QValue {
	return r.ColToVal[col]
}

// UpdateIfNotExists takes in a RecordItems as input and updates the values of the
// current RecordItems with the values from the input RecordItems for the columns
// that are present in the input RecordItems but not in the current RecordItems.
// We return the slice of col names that were updated.
func (r RecordItems) UpdateIfNotExists(input_ Items) []string {
	input := input_.(RecordItems)
	updatedCols := make([]string, 0, len(input.ColToVal))
	for col, val := range input.ColToVal {
		if _, ok := r.ColToVal[col]; !ok {
			r.ColToVal[col] = val
			updatedCols = append(updatedCols, col)
		}
	}
	return updatedCols
}

func (r RecordItems) UpdateWithBaseRecord(baseRecord BaseRecord) {
	r.AddColumn("_peerdb_origin_transaction_id", types.QValueUInt64{Val: baseRecord.GetTransactionID()})
	r.AddColumn("_peerdb_origin_checkpoint_id", types.QValueInt64{Val: baseRecord.GetCheckpointID()})
	r.AddColumn("_peerdb_origin_commit_time_nano", types.QValueInt64{Val: baseRecord.GetCommitTime().UnixNano()})
}

func (r RecordItems) GetValueByColName(colName string) (types.QValue, error) {
	val, ok := r.ColToVal[colName]
	if !ok {
		return nil, fmt.Errorf("column name %s not found", colName)
	}
	return val, nil
}

func (r RecordItems) GetBytesByColName(colName string) ([]byte, error) {
	val, err := r.GetValueByColName(colName)
	if err != nil {
		return nil, err
	}
	return fmt.Append(nil, val.Value()), nil
}

func (r RecordItems) Len() int {
	return len(r.ColToVal)
}

func (r RecordItems) toMap(opts ToJSONOptions) (map[string]any, error) {
	jsonStruct := make(map[string]any, len(r.ColToVal))
	for col, qv := range r.ColToVal {
		if qv == nil {
			jsonStruct[col] = nil
			continue
		}

		switch v := qv.(type) {
		case types.QValueUUID:
			jsonStruct[col] = v.Val
		case types.QValueQChar:
			jsonStruct[col] = string(v.Val)
		case types.QValueString:
			strVal := v.Val
			if opts.ClearValuesOverBytes > 0 && len(strVal) > opts.ClearValuesOverBytes {
				jsonStruct[col] = ""
			} else {
				jsonStruct[col] = strVal
			}
		case types.QValueJSON:
			if opts.ClearValuesOverBytes > 0 && len(v.Val) > opts.ClearValuesOverBytes {
				jsonStruct[col] = "{}"
			} else if _, ok := opts.UnnestColumns[col]; ok {
				var unnestStruct map[string]any
				if err := json.Unmarshal([]byte(v.Val), &unnestStruct); err != nil {
					return nil, err
				}

				maps.Copy(jsonStruct, unnestStruct)
			} else {
				jsonStruct[col] = v.Val
			}
		case types.QValueHStore:
			hstoreVal := v.Val

			if !opts.HStoreAsJSON {
				jsonStruct[col] = hstoreVal
			} else {
				jsonVal, err := datatypes.ParseHstore(hstoreVal)
				if err != nil {
					return nil, fmt.Errorf("unable to convert hstore column %s to json for value %T: %w", col, v, err)
				}
				if opts.ClearValuesOverBytes > 0 && len(jsonVal) > opts.ClearValuesOverBytes {
					jsonStruct[col] = ""
				} else {
					jsonStruct[col] = jsonVal
				}
			}

		case types.QValueTimestamp:
			jsonStruct[col] = v.Val.Format("2006-01-02 15:04:05.999999")
		case types.QValueTimestampTZ:
			jsonStruct[col] = v.Val.Format("2006-01-02 15:04:05.999999-0700")
		case types.QValueDate:
			jsonStruct[col] = v.Val.Format("2006-01-02")
		case types.QValueTime:
			jsonStruct[col] = types.FormatExtendedTimeDuration(v.Val)
		case types.QValueTimeTZ:
			jsonStruct[col] = types.FormatExtendedTimeDuration(v.Val)
		case types.QValueArrayDate:
			dateArr := v.Val
			formattedDateArr := make([]string, 0, len(dateArr))
			for _, val := range dateArr {
				formattedDateArr = append(formattedDateArr, val.Format("2006-01-02"))
			}
			jsonStruct[col] = formattedDateArr
		case types.QValueArrayTime:
			timeArr := v.Val
			formattedTimeArr := make([]string, 0, len(timeArr))
			for _, val := range timeArr {
				formattedTimeArr = append(formattedTimeArr, types.FormatExtendedTimeDuration(val))
			}
			jsonStruct[col] = formattedTimeArr
		case types.QValueNumeric:
			jsonStruct[col] = v.Val.String()
		case types.QValueArrayNumeric:
			numericArr := v.Val
			strArr := make([]any, 0, len(numericArr))
			for _, val := range numericArr {
				strArr = append(strArr, val.String())
			}
			jsonStruct[col] = strArr
		case types.QValueFloat64:
			if math.IsNaN(v.Val) || math.IsInf(v.Val, 0) {
				jsonStruct[col] = nil
			} else {
				jsonStruct[col] = v.Val
			}
		case types.QValueFloat32:
			if math.IsNaN(float64(v.Val)) || math.IsInf(float64(v.Val), 0) {
				jsonStruct[col] = nil
			} else {
				jsonStruct[col] = v.Val
			}
		case types.QValueArrayFloat64:
			floatArr := v.Val
			nullableFloatArr := make([]any, 0, len(floatArr))
			for _, val := range floatArr {
				if math.IsNaN(val) || math.IsInf(val, 0) {
					nullableFloatArr = append(nullableFloatArr, nil)
				} else {
					nullableFloatArr = append(nullableFloatArr, val)
				}
			}
			jsonStruct[col] = nullableFloatArr
		case types.QValueArrayFloat32:
			floatArr := v.Val
			nullableFloatArr := make([]any, 0, len(floatArr))
			for _, val := range floatArr {
				if math.IsNaN(float64(val)) || math.IsInf(float64(val), 0) {
					nullableFloatArr = append(nullableFloatArr, nil)
				} else {
					nullableFloatArr = append(nullableFloatArr, val)
				}
			}
			jsonStruct[col] = nullableFloatArr
		default:
			jsonStruct[col] = v.Value()
		}
	}

	return jsonStruct, nil
}

func (r RecordItems) ToJSONWithOptions(options ToJSONOptions) (string, error) {
	bytes, err := r.MarshalJSONWithOptions(options)
	return string(bytes), err
}

func (r RecordItems) MarshalJSON() ([]byte, error) {
	return r.MarshalJSONWithOptions(NewToJSONOptions(nil, true))
}

func (r RecordItems) MarshalJSONWithOptions(opts ToJSONOptions) ([]byte, error) {
	// The unnest path splices sub-object keys into the parent and is only used by
	// non-CDC flows; keep it on the map-based encoder where it is well exercised.
	if len(opts.UnnestColumns) > 0 {
		jsonStruct, err := r.toMap(opts)
		if err != nil {
			return nil, err
		}
		return json.Marshal(jsonStruct)
	}

	stream := rawTableJSON.BorrowStream(nil)
	defer rawTableJSON.ReturnStream(stream)
	if err := r.writeJSON(stream, opts); err != nil {
		return nil, err
	}
	if stream.Error != nil {
		return nil, stream.Error
	}
	// The stream's buffer is reused after ReturnStream, so copy it out.
	buf := stream.Buffer()
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// writeJSON streams the record as a flat {column: value} object, mirroring the
// per-kind rendering in toMap exactly but without allocating the intermediate map.
// Kinds not special-cased fall back to stream.WriteVal(v.Value()), which encodes
// them the same way encoding/json would (bytes are written via base64 explicitly
// so this never depends on the fallback for binary columns).
func (r RecordItems) writeJSON(stream *jsoniter.Stream, opts ToJSONOptions) error {
	stream.WriteObjectStart()
	first := true
	field := func(col string) {
		if !first {
			stream.WriteMore()
		}
		first = false
		stream.WriteObjectField(col)
	}

	for col, qv := range r.ColToVal {
		if qv == nil {
			field(col)
			stream.WriteNil()
			continue
		}

		switch v := qv.(type) {
		case types.QValueUUID:
			field(col)
			stream.WriteString(v.Val.String())
		case types.QValueQChar:
			field(col)
			stream.WriteString(string(v.Val))
		case types.QValueString:
			field(col)
			if opts.ClearValuesOverBytes > 0 && len(v.Val) > opts.ClearValuesOverBytes {
				stream.WriteString("")
			} else {
				stream.WriteString(v.Val)
			}
		case types.QValueJSON:
			field(col)
			if opts.ClearValuesOverBytes > 0 && len(v.Val) > opts.ClearValuesOverBytes {
				stream.WriteString("{}")
			} else {
				stream.WriteString(v.Val)
			}
		case types.QValueHStore:
			field(col)
			if !opts.HStoreAsJSON {
				stream.WriteString(v.Val)
			} else {
				jsonVal, err := datatypes.ParseHstore(v.Val)
				if err != nil {
					return fmt.Errorf("unable to convert hstore column %s to json for value %T: %w", col, v, err)
				}
				if opts.ClearValuesOverBytes > 0 && len(jsonVal) > opts.ClearValuesOverBytes {
					stream.WriteString("")
				} else {
					stream.WriteString(jsonVal)
				}
			}
		case types.QValueBytes:
			field(col)
			stream.WriteString(base64.StdEncoding.EncodeToString(v.Val))
		case types.QValueTimestamp:
			field(col)
			stream.WriteString(v.Val.Format("2006-01-02 15:04:05.999999"))
		case types.QValueTimestampTZ:
			field(col)
			stream.WriteString(v.Val.Format("2006-01-02 15:04:05.999999-0700"))
		case types.QValueDate:
			field(col)
			stream.WriteString(v.Val.Format("2006-01-02"))
		case types.QValueTime:
			field(col)
			stream.WriteString(types.FormatExtendedTimeDuration(v.Val))
		case types.QValueTimeTZ:
			field(col)
			stream.WriteString(types.FormatExtendedTimeDuration(v.Val))
		case types.QValueArrayDate:
			field(col)
			stream.WriteArrayStart()
			for i, val := range v.Val {
				if i > 0 {
					stream.WriteMore()
				}
				stream.WriteString(val.Format("2006-01-02"))
			}
			stream.WriteArrayEnd()
		case types.QValueArrayTime:
			field(col)
			stream.WriteArrayStart()
			for i, val := range v.Val {
				if i > 0 {
					stream.WriteMore()
				}
				stream.WriteString(types.FormatExtendedTimeDuration(val))
			}
			stream.WriteArrayEnd()
		case types.QValueNumeric:
			field(col)
			stream.WriteString(v.Val.String())
		case types.QValueArrayNumeric:
			field(col)
			stream.WriteArrayStart()
			for i, val := range v.Val {
				if i > 0 {
					stream.WriteMore()
				}
				stream.WriteString(val.String())
			}
			stream.WriteArrayEnd()
		case types.QValueFloat64:
			field(col)
			if math.IsNaN(v.Val) || math.IsInf(v.Val, 0) {
				stream.WriteNil()
			} else {
				stream.WriteFloat64(v.Val)
			}
		case types.QValueFloat32:
			field(col)
			if math.IsNaN(float64(v.Val)) || math.IsInf(float64(v.Val), 0) {
				stream.WriteNil()
			} else {
				stream.WriteFloat32(v.Val)
			}
		case types.QValueArrayFloat64:
			field(col)
			stream.WriteArrayStart()
			for i, val := range v.Val {
				if i > 0 {
					stream.WriteMore()
				}
				if math.IsNaN(val) || math.IsInf(val, 0) {
					stream.WriteNil()
				} else {
					stream.WriteFloat64(val)
				}
			}
			stream.WriteArrayEnd()
		case types.QValueArrayFloat32:
			field(col)
			stream.WriteArrayStart()
			for i, val := range v.Val {
				if i > 0 {
					stream.WriteMore()
				}
				if math.IsNaN(float64(val)) || math.IsInf(float64(val), 0) {
					stream.WriteNil()
				} else {
					stream.WriteFloat32(val)
				}
			}
			stream.WriteArrayEnd()
		default:
			field(col)
			stream.WriteVal(v.Value())
		}
	}

	stream.WriteObjectEnd()
	return nil
}

func (r RecordItems) DeleteColName(colName string) {
	delete(r.ColToVal, colName)
}
