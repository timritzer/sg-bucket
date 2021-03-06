package sgbucket

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Validates a design document.
func CheckDDoc(value interface{}) (*DesignDoc, error) {
	source, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var design DesignDoc
	if err := json.Unmarshal(source, &design); err != nil {
		return nil, err
	}

	if design.Language != "" && design.Language != "javascript" {
		return nil, fmt.Errorf("Lolrus design docs don't support language %q",
			design.Language)
	}

	return &design, nil
}

// Applies view params (startkey/endkey, limit, etc) against a ViewResult.
func ProcessViewResult(result ViewResult, params map[string]interface{},
	bucket Bucket, reduceFunction string) (ViewResult, error) {
	includeDocs := false
	limit := 0
	reverse := false
	reduce := true

	if params != nil {
		includeDocs, _ = params["include_docs"].(bool)
		plimit, ok := params["limit"].(uint64)
		if ok {
			limit = int(plimit)
		}
		reverse, _ = params["reverse"].(bool)
		if reduceParam, found := params["reduce"].(bool); found {
			reduce = reduceParam
		}
	}

	if reverse {
		//TODO: Apply "reverse" option
		return result, fmt.Errorf("Reverse is not supported yet, sorry")
	}

	startkey := params["startkey"]
	if startkey == nil {
		startkey = params["start_key"] // older synonym
	}
	endkey := params["endkey"]
	if endkey == nil {
		endkey = params["end_key"]
	}
	inclusiveEnd := true
	if key := params["key"]; key != nil {
		startkey = key
		endkey = key
	} else {
		if value, ok := params["inclusive_end"].(bool); ok {
			inclusiveEnd = value
		}
	}

	var collator JSONCollator

	if startkey != nil {
		i := sort.Search(len(result.Rows), func(i int) bool {
			return collator.Collate(result.Rows[i].Key, startkey) >= 0
		})
		result.Rows = result.Rows[i:]
	}

	if limit > 0 && len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
	}

	if endkey != nil {
		limit := 0
		if !inclusiveEnd {
			limit = -1
		}
		i := sort.Search(len(result.Rows), func(i int) bool {
			return collator.Collate(result.Rows[i].Key, endkey) > limit
		})
		result.Rows = result.Rows[:i]
	}

	if includeDocs {
		newRows := make(ViewRows, len(result.Rows))
		for i, row := range result.Rows {
			//OPT: This may unmarshal the same doc more than once
			raw, _, err := bucket.GetRaw(row.ID)
			if err != nil {
				return result, err
			}
			var parsedDoc interface{}
			json.Unmarshal(raw, &parsedDoc)
			newRows[i] = row
			newRows[i].Doc = &parsedDoc
		}
		result.Rows = newRows
	}

	if reduce && reduceFunction != "" {
		if err := ReduceViewResult(reduceFunction, params, &result); err != nil {
			return result, err
		}
	}

	result.TotalRows = len(result.Rows)
	logg("\t... view returned %d rows", result.TotalRows)
	return result, nil
}

func ReduceViewResult(reduceFunction string, params map[string]interface{}, result *ViewResult) error {
	reduceFun, compileErr := ReduceFunc(reduceFunction)
	if compileErr != nil {
		return compileErr
	}
	groupLevel := 0
	if params["group"] != nil && params["group"].(bool) == true {
		groupLevel = -1
	} else if params["group_level"] != nil {
		groupLevel = int(params["group_level"].(uint64))
	}
	if groupLevel != 0 {
		var collator JSONCollator
		key := result.Rows[0].Key
		if groupLevel != -1 {
			// don't try to cast key as a slice if group=true
			key = keyPrefix(groupLevel, key)
		}
		inRows := []*ViewRow{}
		outRows := []*ViewRow{}
		for _, row := range result.Rows {
			inKey := row.Key
			if groupLevel != -1 {
				// don't try to cast key as a slice if group=true
				inKey = keyPrefix(groupLevel, inKey)
			}
			collated := collator.Collate(inKey, key)
			if collated == 0 {
				inRows = append(inRows, row)
			} else {
				outRow, outErr := reduceFun(inRows)
				if outErr != nil {
					return outErr
				}
				outRow.Key = key
				outRows = append(outRows, outRow)
				// reset for next key
				inRows = []*ViewRow{row}
				key = inKey
			}
		}
		// do last key
		outRow, outErr := reduceFun(inRows)
		if outErr != nil {
			return outErr
		}
		outRow.Key = key
		result.Rows = append(outRows, outRow)
	} else {
		row, err := reduceFun(result.Rows)
		if err != nil {
			return err
		}
		result.Rows = []*ViewRow{row}
	}
	return nil
}

func keyPrefix(groupLevel int, key interface{}) []interface{} {
	return key.([]interface{})[0:groupLevel]
}

func ReduceFunc(reduceFunction string) (func([]*ViewRow) (*ViewRow, error), error) {
	switch reduceFunction {
	case "_count":
		return func(rows []*ViewRow) (*ViewRow, error) {
			return &ViewRow{Value: float64(len(rows))}, nil
		}, nil
	case "_sum":
		return func(rows []*ViewRow) (*ViewRow, error) {
			total := float64(0)
			for _, row := range rows {
				// This could theoretically know how to unwrap our [channels, value]
				// design_doc emit wrapper, but even so reduce would remain admin only.
				total += collationToFloat64(row.Value)
			}
			return &ViewRow{Value: total}, nil
		}, nil
	default:
		// TODO: Implement other reduce functions!
		return nil, fmt.Errorf("Sgbucket only supports _count and _sum reduce functions")
	}
}

//////// VIEW RESULT: (implementation of sort.Interface interface)

func (result *ViewResult) Len() int {
	return len(result.Rows)
}

func (result *ViewResult) Swap(i, j int) {
	temp := result.Rows[i]
	result.Rows[i] = result.Rows[j]
	result.Rows[j] = temp
}

func (result *ViewResult) Less(i, j int) bool {
	return result.Collator.Collate(result.Rows[i].Key, result.Rows[j].Key) < 0
}
