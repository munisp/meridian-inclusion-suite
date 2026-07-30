package main

import "encoding/json"

// jsonMarshal/jsonUnmarshal wrap encoding/json so workflow code can re-decode
// loosely-typed input maps without importing encoding/json everywhere.
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
