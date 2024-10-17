/*
 * Copyright 2024 Redpanda Data, Inc.
 *
 * Licensed as a Redpanda Enterprise file under the Redpanda Community
 * License (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * https://github.com/redpanda-data/redpanda/blob/master/licenses/rcl.md
 */

package streaming

import (
	"fmt"
	"maps"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/parquet-go/parquet-go"
	"github.com/redpanda-data/connect/v4/internal/impl/snowflake/streaming/int128"
)

type dataTransformer struct {
	converter dataConverter
	stats     *statsBuffer
	column    *columnMetadata
	buf       typedBuffer
}

func convertFixedType(column columnMetadata) (parquet.Node, dataConverter, error) {
	var scale int32
	var precision int32
	if column.Scale != nil {
		scale = *column.Scale
	}
	if column.Precision != nil {
		precision = *column.Precision
	}
	isDecimal := column.Scale != nil && column.Precision != nil
	if (column.Scale != nil && *column.Scale != 0) || strings.ToUpper(column.PhysicalType) == "SB16" {
		c := numberConverter{nullable: column.Nullable, scale: scale, precision: precision}
		if isDecimal {
			return parquet.Decimal(int(scale), int(precision), parquet.FixedLenByteArrayType(16)), c, nil
		}
		return parquet.Leaf(parquet.FixedLenByteArrayType(16)), c, nil
	}
	var ptype parquet.Type
	var defaultPrecision int32
	switch strings.ToUpper(column.PhysicalType) {
	case "SB1":
		ptype = parquet.Int32Type
		defaultPrecision = maxPrecisionForByteWidth(1)
	case "SB2":
		ptype = parquet.Int32Type
		defaultPrecision = maxPrecisionForByteWidth(2)
	case "SB4":
		ptype = parquet.Int32Type
		defaultPrecision = maxPrecisionForByteWidth(4)
	case "SB8":
		ptype = parquet.Int64Type
		defaultPrecision = maxPrecisionForByteWidth(8)
	default:
		return nil, nil, fmt.Errorf("unsupported physical column type: %s", column.PhysicalType)
	}
	validationPrecision := precision
	if column.Precision == nil {
		validationPrecision = defaultPrecision
	}
	c := numberConverter{nullable: column.Nullable, scale: scale, precision: validationPrecision}
	if isDecimal {
		return parquet.Decimal(int(scale), int(precision), ptype), c, nil
	}
	return parquet.Leaf(ptype), c, nil
}

// maxJSONSize is the size that any kind of semi-structured data can be, which is 16MiB minus a small overhead
const maxJSONSize = 16*humanize.MiByte - 64

// See ParquetTypeGenerator
func constructParquetSchema(columns []columnMetadata) (*parquet.Schema, map[string]*dataTransformer, map[string]string, error) {
	groupNode := parquet.Group{}
	transformers := map[string]*dataTransformer{}
	typeMetadata := map[string]string{"sfVer": "1,1"}
	var err error
	for _, column := range columns {
		id := int(column.Ordinal)
		var n parquet.Node
		var converter dataConverter
		logicalType := strings.ToLower(column.LogicalType)
		switch logicalType {
		case "fixed":
			n, converter, err = convertFixedType(column)
			if err != nil {
				return nil, nil, nil, err
			}
		case "array":
			typeMetadata[fmt.Sprintf("%d:obj_enc", id)] = "1"
			n = parquet.String()
			converter = jsonArrayConverter{jsonConverter{column.Nullable, maxJSONSize}}
		case "object":
			typeMetadata[fmt.Sprintf("%d:obj_enc", id)] = "1"
			n = parquet.String()
			converter = jsonObjectConverter{jsonConverter{column.Nullable, maxJSONSize}}
		case "variant":
			typeMetadata[fmt.Sprintf("%d:obj_enc", id)] = "1"
			n = parquet.String()
			converter = jsonConverter{column.Nullable, maxJSONSize}
		case "any", "text", "char":
			n = parquet.String()
			byteLength := 16 * humanize.MiByte
			if column.ByteLength != nil {
				byteLength = int(*column.ByteLength)
			}
			byteLength = min(byteLength, 16*humanize.MiByte)
			converter = binaryConverter{nullable: column.Nullable, maxLength: byteLength, utf8: true}
		case "binary":
			n = parquet.Leaf(parquet.ByteArrayType)
			// Why binary data defaults to 8MiB instead of the 16MiB for strings... ¯\_(ツ)_/¯
			byteLength := 8 * humanize.MiByte
			if column.ByteLength != nil {
				byteLength = int(*column.ByteLength)
			}
			byteLength = min(byteLength, 16*humanize.MiByte)
			converter = binaryConverter{nullable: column.Nullable, maxLength: byteLength}
		case "boolean":
			n = parquet.Leaf(parquet.BooleanType)
			converter = boolConverter{column.Nullable}
		case "real":
			n = parquet.Leaf(parquet.DoubleType)
			converter = doubleConverter{column.Nullable}
		case "timestamp_tz", "timestamp_ltz", "timestamp_ntz":
			if column.PhysicalType == "SB8" {
				n = parquet.Leaf(parquet.Int64Type)
			} else {
				n = parquet.Leaf(parquet.FixedLenByteArrayType(16))
			}
			var scale int32
			if column.Scale != nil {
				scale = *column.Scale
			}
			tz := logicalType != "timestamp_ntz"
			converter = timestampConverter{column.Nullable, scale, tz}
		case "time":
			t := parquet.Int32Type
			precision := 9
			if column.PhysicalType == "SB8" {
				t = parquet.Int64Type
				precision = 18
			}
			scale := int32(9)
			if column.Scale != nil {
				scale = *column.Scale
			}
			n = parquet.Decimal(int(scale), precision, t)
			converter = timeConverter{column.Nullable, scale}
		case "date":
			n = parquet.Leaf(parquet.Int32Type)
			converter = dateConverter{column.Nullable}
		default:
			return nil, nil, nil, fmt.Errorf("unsupported logical column type: %s", column.LogicalType)
		}
		if column.Nullable {
			n = parquet.Optional(n)
		}
		n = parquet.FieldID(n, id)
		// Use plain encoding for now as there seems to be compatibility issues with the default settings
		// we might be able to tune this more.
		n = parquet.Encoded(n, &parquet.Plain)
		typeMetadata[strconv.Itoa(id)] = fmt.Sprintf(
			"%d,%d",
			logicalTypeOrdinal(column.LogicalType),
			physicalTypeOrdinal(column.PhysicalType),
		)
		name := normalizeColumnName(column.Name)
		groupNode[name] = n
		transformers[name] = &dataTransformer{
			name:      column.Name,
			converter: converter,
			stats:     &statsBuffer{columnID: id},
			column:    &column,
		}
	}
	return parquet.NewSchema("bdec", groupNode), transformers, typeMetadata, nil
}

// So snowflake has a storage optimization where physical types are narrowed to the smallest possible storage
// value, so after we collect stats, this narrows the schema to smallest possible numeric types.
func narrowPhysicalTypes(
	schema *parquet.Schema,
	transformers map[string]*dataTransformer,
	fileMetadata map[string]string) (*parquet.Schema, map[string]string) {
	mapped := parquet.Group{}
	mappedMeta := maps.Clone(fileMetadata)
	for _, field := range schema.Fields() {
		name := field.Name()
		t := transformers[name]
		if !canCompatNumber(t.column) {
			mapped[field.Name()] = field
			continue
		}
		stats := transformers[field.Name()].stats
		byteWidth := max(int128.ByteWidth(stats.maxIntVal), int128.ByteWidth(stats.minIntVal))
		n := parquet.Int(byteWidth * 8)
		if field.Type().LogicalType() != nil && field.Type().LogicalType().Decimal != nil {
			d := field.Type().LogicalType().Decimal
			n = parquet.Decimal(
				int(d.Scale),
				int(min(d.Precision, maxPrecisionForByteWidth(byteWidth))),
				n.Type(),
			)
		}
		if field.Optional() {
			n = parquet.Optional(n)
		}
		n = parquet.FieldID(n, field.ID())
		n = parquet.Compressed(n, field.Compression())
		n = parquet.Encoded(n, field.Encoding())
		mapped[field.Name()] = n
		mappedMeta[strconv.Itoa(field.ID())] = fmt.Sprintf(
			"%d,%d",
			logicalTypeOrdinal(t.column.LogicalType),
			physicalTypeOrdinal(fmt.Sprintf("SB%d", byteWidth)),
		)
	}
	if debug {
		fmt.Println("=== original ===")
		_ = parquet.PrintSchema(os.Stdout, schema.Name(), schema)
		fmt.Println("\n=== mapped ===")
		_ = parquet.PrintSchema(os.Stdout, schema.Name(), mapped)
		fmt.Println()
	}
	return parquet.NewSchema(schema.Name(), mapped), mappedMeta
}

type statsBuffer struct {
	columnID               int
	minIntVal, maxIntVal   int128.Int128
	minRealVal, maxRealVal float64
	minStrVal, maxStrVal   []byte
	maxStrLen              int
	nullCount              int64
	first                  bool
}

func (s *statsBuffer) Reset() {
	s.first = true
	s.minIntVal = int128.Int64(0)
	s.maxIntVal = int128.Int64(0)
	s.minRealVal = 0
	s.maxRealVal = 0
	s.minStrVal = nil
	s.maxStrVal = nil
	s.maxStrLen = 0
	s.nullCount = 0
}

func computeColumnEpInfo(stats map[string]*dataTransformer) map[string]fileColumnProperties {
	info := map[string]fileColumnProperties{}
	for _, transformer := range stats {
		stat := transformer.stats
		var minStrVal *string = nil
		if stat.minStrVal != nil {
			s := truncateBytesAsHex(stat.minStrVal, false)
			minStrVal = &s
		}
		var maxStrVal *string = nil
		if stat.maxStrVal != nil {
			s := truncateBytesAsHex(stat.maxStrVal, true)
			maxStrVal = &s
		}
		info[transformer.name] = fileColumnProperties{
			ColumnOrdinal:  int32(stat.columnID),
			NullCount:      stat.nullCount,
			MinStrValue:    minStrVal,
			MaxStrValue:    maxStrVal,
			MaxLength:      int64(stat.maxStrLen),
			MinIntValue:    stat.minIntVal,
			MaxIntValue:    stat.maxIntVal,
			MinRealValue:   stat.minRealVal,
			MaxRealValue:   stat.maxRealVal,
			DistinctValues: -1,
		}
	}
	return info
}

func canCompatNumber(column *columnMetadata) bool {
	// We leave out SB1 because it's already as small
	// as possible and we'd have to special case booleans
	switch strings.ToUpper(column.PhysicalType) {
	case "SB2", "SB4", "SB8", "SB16":
		return true
	}
	return false
}

func physicalTypeOrdinal(str string) int {
	switch strings.ToUpper(str) {
	case "ROWINDEX":
		return 9
	case "DOUBLE":
		return 7
	case "SB1":
		return 1
	case "SB2":
		return 2
	case "SB4":
		return 3
	case "SB8":
		return 4
	case "SB16":
		return 5
	case "LOB":
		return 8
	case "ROW":
		return 10
	}
	return -1
}

func logicalTypeOrdinal(str string) int {
	switch strings.ToUpper(str) {
	case "BOOLEAN":
		return 1
	case "NULL":
		return 15
	case "REAL":
		return 8
	case "FIXED":
		return 2
	case "TEXT":
		return 9
	case "BINARY":
		return 10
	case "DATE":
		return 7
	case "TIME":
		return 6
	case "TIMESTAMP_LTZ":
		return 3
	case "TIMESTAMP_NTZ":
		return 4
	case "TIMESTAMP_TZ":
		return 5
	case "ARRAY":
		return 13
	case "OBJECT":
		return 12
	case "VARIANT":
		return 11
	}
	return -1
}

func byteWidth(v int64) int {
	if v < 0 {
		switch {
		case v >= math.MinInt8:
			return 1
		case v >= math.MinInt16:
			return 2
		case v >= math.MinInt32:
			return 4
		}
		return 8
	}
	switch {
	case v <= math.MaxInt8:
		return 1
	case v <= math.MaxInt16:
		return 2
	case v <= math.MaxInt32:
		return 4
	}
	return 8
}

func maxPrecisionForByteWidth(byteWidth int) int32 {
	switch byteWidth {
	case 1:
		return 3
	case 2:
		return 5
	case 4:
		return 9
	case 8:
		return 18
	case 16:
		return 38
	}
	panic(fmt.Errorf("unexpected byteWidth=%d", byteWidth))
}
