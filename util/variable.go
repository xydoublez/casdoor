// Copyright 2026 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

var (
	Version      = "v3.154.4"
	CommitId     = "fe30a6a086bce75bfc9da64e63ddb715f3e4afc6"
	CommitOffset = 10
	// BuildType is set to "release" by .goreleaser.yaml, only such binaries can upgrade themselves
	BuildType = ""
)
