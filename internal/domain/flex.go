package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexBool acepta JSON bool, numero (0/1) o string ("0","1","true","false").
// n8n puede enviar cualquiera de estos formatos indistintamente.
type FlexBool struct {
	Valid bool
	Value bool
}

func (f *FlexBool) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		f.Valid = false
		return nil
	}
	// bool nativo
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		f.Valid, f.Value = true, b
		return nil
	}
	// numero
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		f.Valid, f.Value = true, n != 0
		return nil
	}
	// string
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.ToLower(strings.TrimSpace(str))
		f.Valid = true
		f.Value = str == "1" || str == "true" || str == "yes"
		return nil
	}
	return fmt.Errorf("FlexBool: cannot parse %s", s)
}

// FlexInt acepta JSON numero o string numerico ("15", "3796").
type FlexInt struct {
	Valid bool
	Value int
}

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		f.Valid = false
		return nil
	}
	// numero nativo
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		f.Valid, f.Value = true, n
		return nil
	}
	// float (por si viene 30.0)
	var fl float64
	if err := json.Unmarshal(data, &fl); err == nil {
		f.Valid, f.Value = true, int(fl)
		return nil
	}
	// string numerico
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.TrimSpace(str)
		if v, err := strconv.Atoi(str); err == nil {
			f.Valid, f.Value = true, v
			return nil
		}
		if v, err := strconv.ParseFloat(str, 64); err == nil {
			f.Valid, f.Value = true, int(v)
			return nil
		}
	}
	return fmt.Errorf("FlexInt: cannot parse %s", s)
}
