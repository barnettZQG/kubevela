/*
Copyright 2022 The KubeVela Authors.

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

package bcode

var (
	// ErrSensitiveConfig means the config can not be read
	ErrSensitiveConfig = NewBcode(400, 16001, "the config is sensitive")

	// ErrNoConfigOrTarget means there is no target or config when creating the distribution.
	ErrNoConfigOrTarget = NewBcode(400, 16002, "the config list or the target list can not be empty")
)