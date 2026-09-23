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

// Package battery is the operator's connection to battery's gRPC API.
//
// It holds nothing yet. The import below pins battery v0.1.0, whose
// generated stubs the controllers will call.
package battery

import (
	// battery's generated gRPC stubs, pinned until the first controller uses them.
	_ "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
)
