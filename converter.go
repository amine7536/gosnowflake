// Package gosnowflake is a Go Snowflake Driver for Go's database/sql
//
// Copyright (c) 2017 Snowflake Computing Inc. All right reserved.
//
package gosnowflake

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
)

func goTypeToSnowflake(v interface{}, tsmode string) string {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "FIXED"
	case bool:
		return "BOOLEAN"
	case float32, float64:
		return "REAL"
	case time.Time:
		return tsmode
	case string:
		return "TEXT"
	case []byte:
		if bd, ok := v.([]byte); ok {
			if bd != nil && len(bd) == 1 {
				return "CHANGE_MODE"
			}
		}
	default:
		return "TEXT"
	}
	return "TEXT"
}

// valueToString converts arbitrary golang type to a string. This is mainly used in binding data with placeholders
// in queries.
func valueToString(v interface{}, tsmode string) (*string, error) {
	glog.V(2).Infof("TYPE: %v, %v", reflect.TypeOf(v), reflect.ValueOf(v))
	if v == nil {
		return nil, nil
	}
	v1 := reflect.ValueOf(v)
	switch v1.Kind() {
	case reflect.Bool:
		s := strconv.FormatBool(v1.Bool())
		return &s, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := strconv.FormatInt(v1.Int(), 10)
		return &s, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s := strconv.FormatUint(v1.Uint(), 10)
		return &s, nil
	case reflect.Float32, reflect.Float64:
		s := strconv.FormatFloat(v1.Float(), 'g', -1, 32)
		return &s, nil
	case reflect.String:
		s := v1.String()
		return &s, nil
	case reflect.Slice, reflect.Map:
		if v1.IsNil() {
			return nil, nil
		}
		if bd, ok := v.([]byte); ok {
			if tsmode == "BINARY" {
				s := hex.EncodeToString(bd)
				return &s, nil
			}
		}
		// TODO: is this good enough?
		s := v1.String()
		return &s, nil
	case reflect.Struct:
		if tm, ok := v.(time.Time); ok {
			switch tsmode {
			case "DATE":
				s := fmt.Sprintf("%d", tm.Unix()*1000)
				return &s, nil
			case "TIME":
				s := fmt.Sprintf("%d",
					(tm.Hour()*3600+tm.Minute()*60+tm.Second())*1e9+tm.Nanosecond())
				return &s, nil
			case "TIMESTAMP_NTZ":
				s := fmt.Sprintf("%d", tm.UnixNano())
				return &s, nil
			case "TIMESTAMP_LTZ":
				_, offset := tm.Zone()
				tm = tm.Add(time.Second * time.Duration(offset))
				s := fmt.Sprintf("%d", tm.UnixNano())
				return &s, nil
			case "TIMESTAMP_TZ":
				s := fmt.Sprintf("%d", tm.UnixNano())
				return &s, nil
			}
		}
	}
	return nil, fmt.Errorf("unsupported type: %v", v1.Kind())
}

// extractTimestamp extracts the internal timestamp data to epoch time in seconds and milliseconds
func extractTimestamp(srcValue *string) (sec int64, nsec int64, err error) {
	glog.V(2).Infof("SRC: %v", srcValue)
	var i int
	for i = 0; i < len(*srcValue); i++ {
		if (*srcValue)[i] == '.' {
			sec, err = strconv.ParseInt((*srcValue)[0:i], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			break
		}
	}
	if i == len(*srcValue) {
		// no fraction
		sec, err = strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		nsec = 0
	} else {
		s := (*srcValue)[i+1:]
		nsec, err = strconv.ParseInt(s+strings.Repeat("0", 9-len(s)), 10, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	if err != nil {
		return 0, 0, err
	}
	glog.V(2).Infof("sec: %v, nsec: %v", sec, nsec)
	return sec, nsec, nil
}

// stringToValue converts a pointer of string data to an arbitrary golang variable. This is mainly used in fetching
// data.
func stringToValue(dest *driver.Value, srcColumnMeta execResponseRowType, srcValue *string) error {
	// glog.V(2).Infof("DATA TYPE: %s, VALUE: % s", srcColumnMeta.Type, srcValue)
	if srcValue == nil {
		dest = nil
		return nil
	}
	switch srcColumnMeta.Type {
	case "text", "fixed", "real", "variant", "object":
		*dest = *srcValue
		return nil
	case "date":
		v, err := strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return err
		}
		*dest = time.Unix(v*86400, 0).UTC()
		return nil
	case "time":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		t0 := time.Time{}
		*dest = t0.Add(time.Duration(sec*1e9 + nsec))
		return nil
	case "timestamp_ntz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		*dest = time.Unix(sec, nsec).UTC()
		return nil
	case "timestamp_ltz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		tt := time.Unix(sec, nsec)
		zone, offset := tt.Zone() // get timezone for the given datetime
		glog.V(2).Infof("local: %v, %v", zone, offset)
		*dest = tt.Add(time.Second * time.Duration(-offset))
		return nil
	case "timestamp_tz":
		glog.V(2).Infof("tz: %v", *srcValue)

		tm := strings.Split(*srcValue, " ")
		if len(tm) != 2 {
			return &SnowflakeError{
				Number:  ErrInvalidTimestampTz,
				Message: fmt.Sprintf("invalid TIMESTAMP_TZ data: %v", *srcValue),
			}
		}
		sec, nsec, err := extractTimestamp(&tm[0])
		if err != nil {
			return err
		}
		offset, err := strconv.ParseInt(tm[1], 10, 64)
		if err != nil {
			return &SnowflakeError{
				Number:  ErrInvalidTimestampTz,
				Message: fmt.Sprintf("invalid TIMESTAMP_TZ data: %v", *srcValue),
			}
		}
		loc := Location(int(offset) - 1440)
		tt := time.Unix(sec, nsec)
		*dest = tt.In(loc)
		return nil
	case "binary":
		glog.V(2).Infof("bin: %v", *srcValue)
		b, err := hex.DecodeString(*srcValue)
		if err != nil {
			return err
		}
		*dest = b
		return nil
	}
	*dest = *srcValue
	return nil
}
