//go:build e2e

package report

import "encoding/json"

// WaitSpec describes observations collected by one client actor invocation.
type WaitSpec struct {
	Status         int             `json:"status,omitempty"`
	Contains       []string        `json:"contains,omitempty"`
	RejectContains []string        `json:"reject_contains,omitempty"`
	JSON           []JSONCondition `json:"json,omitempty"`
	Consecutive    int             `json:"consecutive,omitempty"`
}

// JSONCondition compares one RFC 6901 pointer using exactly one comparison.
// Equal accepts a JSON scalar, including explicit null. Missing values never match.
type JSONCondition struct {
	Pointer     string          `json:"pointer"`
	Equal       json.RawMessage `json:"equal,omitempty"`
	Min         *json.Number    `json:"min,omitempty"`
	GreaterThan *json.Number    `json:"greater_than,omitempty"`
	Length      *int            `json:"length,omitempty"`
}
