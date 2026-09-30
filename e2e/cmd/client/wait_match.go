//go:build e2e

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"statute.kjanat.dev/e2e/report"
)

type waitCondition struct {
	tokens []string
	equal  any
	spec   report.JSONCondition
}

type waitPredicate struct {
	spec       report.WaitSpec
	conditions []waitCondition
}

func decodeWaitJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func parseWaitSpec(raw string) (*waitPredicate, error) {
	var spec report.WaitSpec
	if raw != "" {
		if strings.TrimSpace(raw) == "null" {
			return nil, errors.New("wait: spec must be an object")
		}
		if err := decodeWaitJSON([]byte(raw), &spec); err != nil {
			return nil, fmt.Errorf("wait: invalid spec: %w", err)
		}
	}
	return compileWaitSpec(spec)
}

func compileWaitSpec(spec report.WaitSpec) (*waitPredicate, error) {
	if spec.Status == 0 {
		spec.Status = 200
	}
	if spec.Status < 100 || spec.Status > 599 || spec.Consecutive < 0 {
		return nil, errors.New("wait: invalid status or consecutive count")
	}
	if spec.Consecutive == 0 {
		spec.Consecutive = 1
	}
	if slices.Contains(spec.RejectContains, "") {
		return nil, errors.New("wait: rejected substring must be nonempty")
	}
	p := &waitPredicate{spec: spec}
	for _, condition := range spec.JSON {
		compiled, err := compileWaitCondition(condition)
		if err != nil {
			return nil, fmt.Errorf("wait: pointer %q: %w", condition.Pointer, err)
		}
		p.conditions = append(p.conditions, compiled)
	}
	return p, nil
}

func compileWaitCondition(spec report.JSONCondition) (waitCondition, error) {
	c := waitCondition{spec: spec}
	var err error
	if c.tokens, err = parseJSONPointer(spec.Pointer); err != nil {
		return c, err
	}
	comparisons := 0
	if spec.Equal != nil {
		comparisons++
		if c.equal, err = waitScalar(spec.Equal); err != nil {
			return c, err
		}
	}
	for _, number := range []*json.Number{spec.Min, spec.GreaterThan} {
		if number != nil {
			comparisons++
			if _, ok := waitNumber(*number); !ok {
				return c, errors.New("invalid numeric comparison")
			}
		}
	}
	if spec.Length != nil {
		comparisons++
		if *spec.Length < 0 {
			return c, errors.New("length must be nonnegative")
		}
	}
	if comparisons != 1 {
		return c, errors.New("exactly one comparison required")
	}
	return c, nil
}

func waitScalar(raw json.RawMessage) (any, error) {
	var value any
	if err := decodeWaitJSON(raw, &value); err != nil {
		return nil, err
	}
	switch scalar := value.(type) {
	case nil, string, bool:
		return scalar, nil
	case json.Number:
		if _, ok := waitNumber(scalar); ok {
			return scalar, nil
		}
		return nil, errors.New("numeric comparison exceeds supported precision range")
	default:
		return nil, errors.New("equal must be a JSON scalar")
	}
}

func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("JSON pointer must start with /")
	}
	tokens := strings.Split(pointer[1:], "/")
	for i, token := range tokens {
		for j := 0; j < len(token); j++ {
			if token[j] != '~' {
				continue
			}
			j++
			if j == len(token) || (token[j] != '0' && token[j] != '1') {
				return nil, errors.New("invalid JSON pointer escape")
			}
		}
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

func pointerValue(root any, tokens []string) (any, bool) {
	value := root
	for _, token := range tokens {
		switch node := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = node[token]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) || strconv.Itoa(index) != token {
				return nil, false
			}
			value = node[index]
		default:
			return nil, false
		}
	}
	return value, true
}

func (c waitCondition) matches(value any) bool {
	if c.spec.Length != nil {
		return waitLength(value) == *c.spec.Length
	}
	if c.spec.Min != nil {
		cmp, ok := compareJSONNumbers(value, *c.spec.Min)
		return ok && cmp >= 0
	}
	if c.spec.GreaterThan != nil {
		cmp, ok := compareJSONNumbers(value, *c.spec.GreaterThan)
		return ok && cmp > 0
	}
	if number, ok := c.equal.(json.Number); ok {
		cmp, valid := compareJSONNumbers(value, number)
		return valid && cmp == 0
	}
	switch value.(type) {
	case nil, string, bool:
		return value == c.equal
	default:
		return false
	}
}

func waitLength(value any) int {
	switch value := value.(type) {
	case []any:
		return len(value)
	case map[string]any:
		return len(value)
	default:
		return -1
	}
}

func waitNumber(number json.Number) (*big.Rat, bool) {
	raw := string(number)
	if len(raw) > 128 || !json.Valid([]byte(raw)) {
		return nil, false
	}
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(raw[index+1:])
		if err != nil || exponent < -512 || exponent > 512 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(raw)
}

func compareJSONNumbers(value any, expected json.Number) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	actual, ok := waitNumber(number)
	if !ok {
		return 0, false
	}
	want, ok := waitNumber(expected)
	if !ok {
		return 0, false
	}
	return actual.Cmp(want), true
}

func (p *waitPredicate) rejectBody(body []byte) error {
	for _, rejected := range p.spec.RejectContains {
		if bytes.Contains(body, []byte(rejected)) {
			return errors.New("wait: response contains a forbidden substring")
		}
	}
	return nil
}

func (p *waitPredicate) matches(status int, body []byte) (bool, error) {
	if err := p.rejectBody(body); err != nil {
		return false, err
	}
	if status != p.spec.Status {
		return false, nil
	}
	var document any
	if len(p.conditions) > 0 {
		if err := decodeWaitJSON(body, &document); err != nil {
			return false, fmt.Errorf("wait: invalid JSON observation: %w", err)
		}
	}
	for _, required := range p.spec.Contains {
		if !bytes.Contains(body, []byte(required)) {
			return false, nil
		}
	}
	for _, c := range p.conditions {
		value, exists := pointerValue(document, c.tokens)
		if !exists || !c.matches(value) {
			return false, nil
		}
	}
	return true, nil
}
