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

package upgrade

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The version string build.sh injects into a customized build:
// sfx_release+<git describe>+<build date>. It must never be mistaken for an
// official release tag, otherwise the in-app self-upgrade would replace the
// customized binary with the official one and drop every local patch.
const customVersion = "sfx_release+v4.24.0-37-gcb05f10b+20261028"

func TestParseVersion(t *testing.T) {
	assert.Equal(t, []int{4, 24, 0}, parseVersion("v4.24.0"))
	assert.Equal(t, []int{3, 154, 4}, parseVersion("v3.154.4"))
	assert.Equal(t, []int{0, 0, 1}, parseVersion("v0.0.1"))

	// Anything that is not a plain release tag must be rejected, so that a
	// customized or development build is never treated as an official release.
	// The date suffix a customized build carries must not change that either.
	for _, version := range []string{
		customVersion, "sfx_release+v4.24.0+20261028", "sfx_release+v4.24.0", "sfx_release+20261028",
		"dev", "", "v4.24", "v4.24.0-1-gc4591a5f", "4.24.0", "v4.24.0-dirty", "v4.24.0+20261028",
	} {
		assert.Nil(t, parseVersion(version), "parseVersion(%q) must be nil", version)
	}
}

func TestIsNewerVersionAcrossMajorBump(t *testing.T) {
	// The production environment runs v3 while this tree already carries v4,
	// so the major bump has to be detected as an upgrade.
	assert.True(t, isNewerVersion("v4.24.0", "v3.154.4"))
	assert.True(t, isNewerVersion("v4.0.0", "v3.999.999"))

	// Comparison must be numeric, not lexicographic: 24 > 9.
	assert.True(t, isNewerVersion("v4.24.0", "v4.9.0"))
	assert.True(t, isNewerVersion("v4.24.10", "v4.24.9"))

	// Same version and downgrades are not upgrades.
	assert.False(t, isNewerVersion("v4.24.0", "v4.24.0"))
	assert.False(t, isNewerVersion("v4.23.0", "v4.24.0"))
	assert.False(t, isNewerVersion("v3.154.4", "v4.24.0"))
}

func TestIsNewerVersionWithCustomBuildVersion(t *testing.T) {
	// A customized build reports sfx_release+... which is not parseable, so no
	// official release can ever look "newer". The in-app upgrade stays inert and
	// upgrades go through deploy-systemd.sh --upgrade instead.
	assert.False(t, isNewerVersion("v4.24.0", customVersion))
	assert.False(t, isNewerVersion("v9.9.9", customVersion))
	assert.False(t, isNewerVersion(customVersion, "v4.24.0"))
	assert.False(t, isNewerVersion(customVersion, customVersion))

	// Development builds are equally inert.
	assert.False(t, isNewerVersion("v4.24.0", "dev"))
}

func TestIsSelfUpgradeableReleaseGate(t *testing.T) {
	// An official release binary may replace itself with a newer official asset.
	assert.True(t, isSelfUpgradeableRelease("v4.24.0", "release"))

	// A customized build must never be self-upgradeable, even when it is stamped
	// with BuildType=release, otherwise the official asset would overwrite every
	// local patch. Its version is not a plain release tag, so the gate stays shut.
	assert.False(t, isSelfUpgradeableRelease(customVersion, "release"))
	assert.False(t, isSelfUpgradeableRelease("sfx_release+v4.24.0+20261028", "release"))
	assert.False(t, isSelfUpgradeableRelease("sfx_release+v4.24.0", "release"))
	assert.False(t, isSelfUpgradeableRelease("v4.24.0+20261028", "release"))
	assert.False(t, isSelfUpgradeableRelease("v4.24.0-1-gc4591a5f", "release"))
	assert.False(t, isSelfUpgradeableRelease("v4.24.0-dirty", "release"))

	// Binaries not produced by the release workflow are not self-upgradeable either.
	assert.False(t, isSelfUpgradeableRelease("v4.24.0", ""))
	assert.False(t, isSelfUpgradeableRelease("dev", ""))
}

func TestGetAssetName(t *testing.T) {
	name := getAssetName()
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		assert.Equal(t, fmt.Sprintf("casdoor_Linux_%s.tar.gz", map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]), name)
		assert.Equal(t, "casdoor_4.24.0_checksums.txt", getChecksumsName("v4.24.0"))
	} else {
		t.Logf("unsupported platform %s/%s, asset name: %q", runtime.GOOS, runtime.GOARCH, name)
	}
}

func TestIsUpgrading(t *testing.T) {
	assert.True(t, isUpgrading(StateDownloading))
	assert.True(t, isUpgrading(StateInstalling))
	assert.True(t, isUpgrading(StateRestarting))
	assert.False(t, isUpgrading(StateFailed))
	assert.False(t, isUpgrading(""))
}

func TestSetStatusIsIsolated(t *testing.T) {
	original := getStatus()
	defer func() {
		statusMutex.Lock()
		status = original
		statusMutex.Unlock()
	}()

	setStatus(func(status *UpgradeStatus) {
		status.State = StateDownloading
		status.Version = "v9.9.9"
	})

	snapshot := getStatus()
	assert.Equal(t, StateDownloading, snapshot.State)
	assert.Equal(t, "v9.9.9", snapshot.Version)

	// Mutating the returned snapshot must not leak back into the shared status.
	snapshot.State = StateFailed
	assert.Equal(t, StateDownloading, getStatus().State)
}
