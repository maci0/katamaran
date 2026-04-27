package controller

import "encoding/json"

// jsonMarshal is a thin wrapper for testability and to keep encoding/json
// out of the controller's main file (so the dependency surface there stays
// focused on k8s types).
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
