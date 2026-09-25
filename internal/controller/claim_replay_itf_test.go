/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// A decoder for the traces claim_replay_test.go replays: quint run's
// Informal Trace Format (https://apalache-mc.org/docs/adr/015adr-trace.html)
// with quint's --mbt metadata, decoded into the parts of claims.qnt's state
// that the replay compares.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// itfInt is an ITF integer: {"#bigint": "n"}, or a plain JSON number.
type itfInt int64

func (n *itfInt) UnmarshalJSON(b []byte) error {
	var big struct {
		V *string `json:"#bigint"`
	}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		if err := json.Unmarshal(b, &big); err != nil {
			return err
		}
		if big.V == nil {
			return fmt.Errorf("ITF integer %s has no #bigint", b)
		}
		v, err := strconv.ParseInt(*big.V, 10, 64)
		*n = itfInt(v)
		return err
	}
	var v int64
	err := json.Unmarshal(b, &v)
	*n = itfInt(v)
	return err
}

// itfSet is an ITF set: {"#set": [...]}.
type itfSet[T comparable] map[T]bool

func (s *itfSet[T]) UnmarshalJSON(b []byte) error {
	var w struct {
		Set []T `json:"#set"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*s = make(itfSet[T], len(w.Set))
	for _, v := range w.Set {
		(*s)[v] = true
	}
	return nil
}

// itfMap is an ITF map: {"#map": [[key, value], ...]}.
type itfMap[K comparable, V any] map[K]V

func (m *itfMap[K, V]) UnmarshalJSON(b []byte) error {
	var w struct {
		Map [][2]json.RawMessage `json:"#map"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*m = make(itfMap[K, V], len(w.Map))
	for _, kv := range w.Map {
		var k K
		var v V
		if err := json.Unmarshal(kv[0], &k); err != nil {
			return err
		}
		if err := json.Unmarshal(kv[1], &v); err != nil {
			return err
		}
		(*m)[k] = v
	}
	return nil
}

// itfVariant is a value of a Quint sum type: {"tag": "T", "value": v}. A
// tag with no value, such as Pending, has the empty tuple as its value.
type itfVariant struct {
	Tag   string          `json:"tag"`
	Value json.RawMessage `json:"value"`
}

// into decodes the variant's value into v.
func (x itfVariant) into(v any) error { return json.Unmarshal(x.Value, v) }

// itfOption is types.qnt's Option: Some(v) or None.
type itfOption[T any] struct {
	Some bool
	V    T
}

func (o *itfOption[T]) UnmarshalJSON(b []byte) error {
	var x itfVariant
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	switch x.Tag {
	case "Some":
		o.Some = true
		return x.into(&o.V)
	case "None":
		o.Some = false
		return nil
	default:
		return fmt.Errorf("an Option tagged %q", x.Tag)
	}
}

// modelCondition is types.qnt's Condition.
type modelCondition struct {
	Status itfVariant `json:"status"`
	Reason itfVariant `json:"reason"`
}

// modelClaim is types.qnt's MicroVMClaim.
type modelClaim struct {
	Spec struct {
		RenewTime itfInt `json:"renewTime"`
	} `json:"spec"`
	Status struct {
		Phase          itfVariant        `json:"phase"`
		LeaseID        itfOption[itfInt] `json:"leaseId"`
		MicroVM        itfOption[string] `json:"microVM"`
		NodeName       itfOption[string] `json:"nodeName"`
		BoundTime      itfOption[itfInt] `json:"boundTime"`
		LeaseExpiresAt itfOption[itfInt] `json:"leaseExpiresAt"`
		Bound          modelCondition    `json:"bound"`
		Synced         modelCondition    `json:"synced"`
	} `json:"status"`
	HasFinalizer      bool   `json:"hasFinalizer"`
	Deleting          bool   `json:"deleting"`
	ObservedRenewTime itfInt `json:"observedRenewTime"`
}

// modelLease is types.qnt's Lease.
type modelLease struct {
	ID        itfInt `json:"id"`
	VM        string `json:"vm"`
	Host      string `json:"host"`
	ClaimedAt itfInt `json:"claimedAt"`
	ExpiresAt itfInt `json:"expiresAt"`
}

// modelBattery is claims.qnt's BatteryState.
type modelBattery struct {
	Up        bool                       `json:"up"`
	UpSince   itfInt                     `json:"upSince"`
	Unborn    itfSet[string]             `json:"unborn"`
	Warm      itfSet[string]             `json:"warm"`
	Leases    itfMap[itfInt, modelLease] `json:"leases"`
	NextLease itfInt                     `json:"nextLease"`
	Events    itfSet[string]             `json:"events"`
}

// modelHeartbeatAnswer is the value of claims.qnt's HeartbeatAnswered.
type modelHeartbeatAnswer struct {
	LeaseID   itfInt `json:"leaseId"`
	ExpiresAt itfInt `json:"expiresAt"`
	RenewTime itfInt `json:"renewTime"`
}

// modelState is one state of claims.qnt, with the step that led to it.
type modelState struct {
	// Action is the step that led to this state, "init" for the first.
	Action string `json:"mbt::actionTaken"`
	// Picks are the step's nondeterministic picks, by name.
	Picks   map[string]itfOption[json.RawMessage] `json:"mbt::nondetPicks"`
	Now     itfInt                                `json:"now"`
	Claims  itfMap[string, itfVariant]            `json:"claims"`
	Battery modelBattery                          `json:"battery"`
	Ctrl    struct {
		Mem        itfMap[string, itfVariant] `json:"mem"`
		Recovering bool                       `json:"recovering"`
		Deleted    itfSet[string]             `json:"deleted"`
	} `json:"ctrl"`
}

// pick is the step's pick of name, as a string.
func (s *modelState) pick(name string) (string, error) {
	p, ok := s.Picks[name]
	if !ok || !p.Some {
		return "", fmt.Errorf("step %s has no pick %q", s.Action, name)
	}
	var v string
	if err := json.Unmarshal(p.V, &v); err != nil {
		return "", fmt.Errorf("step %s: pick %q: %w", s.Action, name, err)
	}
	return v, nil
}

// claim is the claim in slot name: its slot's tag (Unborn, Live or Gone),
// and the claim it holds or held last.
func (s *modelState) claim(name string) (string, modelClaim, error) {
	var c modelClaim
	slot, ok := s.Claims[name]
	if !ok {
		return "", c, fmt.Errorf("no claim %q in the state", name)
	}
	if slot.Tag == "Unborn" {
		return slot.Tag, c, nil
	}
	err := slot.into(&c)
	return slot.Tag, c, err
}

// itfTrace is a trace as quint run writes it.
type itfTrace struct {
	States []modelState `json:"states"`
}

// readTrace reads a trace from path, gunzipping it if its name ends in .gz.
func readTrace(path string) (*itfTrace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if len(path) > 3 && path[len(path)-3:] == ".gz" {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	var t itfTrace
	if err := json.NewDecoder(r).Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}
