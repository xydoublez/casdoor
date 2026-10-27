// Copyright 2022 The Casdoor Authors. All Rights Reserved.
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

//go:build !skipCi

package util

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
)

func TestGetCpuUsage(t *testing.T) {
	usage, err := getCpuUsage()
	assert.Nil(t, err)
	t.Log(usage)
}

func TestGetMemoryUsage(t *testing.T) {
	used, total, err := getMemoryUsage()
	assert.Nil(t, err)
	t.Log(used, total)
}

func TestGetDiskUsage(t *testing.T) {
	used, total, err := getDiskUsage()
	assert.Nil(t, err)
	t.Log(used, total)
}

func TestGetNetworkUsage(t *testing.T) {
	sent, recv, total, err := getNetworkUsage()
	assert.Nil(t, err)
	t.Log(sent, recv, total)
}

func TestGetVersionInfo(t *testing.T) {
	versionInfo, err := GetVersionInfo()
	if errors.Is(err, git.ErrRepositoryNotExists) {
		t.Skip("no git repository, keeping the version info in variable.go")
	}
	assert.NoError(t, err)
	assert.NotNil(t, versionInfo)

	_, filename, _, _ := runtime.Caller(0)
	variablePath := path.Join(path.Dir(filename), "variable.go")

	content, err := os.ReadFile(variablePath)
	assert.NoError(t, err)

	replacements := map[*regexp.Regexp]string{
		regexp.MustCompile(`Version\s*=\s*".*"`):       fmt.Sprintf(`Version      = "%s"`, versionInfo.Version),
		regexp.MustCompile(`CommitId\s*=\s*".*"`):      fmt.Sprintf(`CommitId     = "%s"`, versionInfo.CommitId),
		regexp.MustCompile(`CommitOffset\s*=\s*-?\d+`): fmt.Sprintf(`CommitOffset = %d`, versionInfo.CommitOffset),
	}
	updated := string(content)
	for re, val := range replacements {
		updated = re.ReplaceAllString(updated, val)
	}

	err = os.WriteFile(variablePath, []byte(updated), 0o644)
	assert.NoError(t, err)
}

func TestGetVersion(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	root := path.Dir(path.Dir(filename))
	r, err := git.PlainOpen(root)
	if err != nil {
		t.Log(err)
	}
	tags, err := r.Tags()
	if err != nil {
		t.Log(err)
	}
	tagMap := make(map[plumbing.Hash]string)
	err = tags.ForEach(func(t *plumbing.Reference) error {
		// This technique should work for both lightweight and annotated tags.
		revHash, err := r.ResolveRevision(plumbing.Revision(t.Name()))
		if err != nil {
			return err
		}
		tagMap[*revHash] = t.Name().Short()
		return nil
	})

	testHash := plumbing.NewHash("f8bc87eb4e5ba3256424cf14aafe0549f812f1cf")
	cIter, err := r.Log(&git.LogOptions{From: testHash})
	if err != nil {
		t.Log(err)
	}

	aheadCnt := 0
	releaseVersion := ""
	// iterates over the commits
	err = cIter.ForEach(func(c *object.Commit) error {
		tag, ok := tagMap[c.Hash]
		if ok {
			if releaseVersion == "" {
				releaseVersion = tag
			}
		}
		if releaseVersion == "" {
			aheadCnt++
		}
		return nil
	})

	assert.Equal(t, 3, aheadCnt)
	assert.Equal(t, "v1.257.0", releaseVersion)
}

// TestGetVersionInfoReportsHighestReleaseTag guards the upgrade path: a fork that
// merges upstream keeps both lineages reachable, and picking the first tag met by
// a depth-first walk used to report a stale v3.x tag while the tree actually
// contained v4.x. GetVersionInfo() must report the highest reachable release tag,
// so cross-check it against an independently computed one.
func TestGetVersionInfoReportsHighestReleaseTag(t *testing.T) {
	versionInfo, err := GetVersionInfo()
	if errors.Is(err, git.ErrRepositoryNotExists) {
		t.Skip("no git repository")
	}
	assert.NoError(t, err)
	if versionInfo.Version == "" {
		t.Skip("no tag is reachable from HEAD")
	}

	assert.Regexp(t, regexp.MustCompile(`^v\d+\.\d+\.\d+$`), versionInfo.Version)
	assert.GreaterOrEqual(t, versionInfo.CommitOffset, 0)

	_, filename, _, _ := runtime.Caller(0)
	r, err := git.PlainOpen(path.Dir(path.Dir(filename)))
	assert.NoError(t, err)

	ref, err := r.Head()
	assert.NoError(t, err)
	head, err := r.CommitObject(ref.Hash())
	assert.NoError(t, err)

	tags, err := r.Tags()
	assert.NoError(t, err)
	tagMap := make(map[plumbing.Hash]string)
	assert.NoError(t, tags.ForEach(func(tag *plumbing.Reference) error {
		revHash, err := r.ResolveRevision(plumbing.Revision(tag.Name()))
		if err != nil {
			// Same tolerance as GetVersionInfo(): a shallow clone can hold a tag
			// whose target object was never fetched.
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return nil
			}
			return err
		}
		tagMap[*revHash] = tag.Name().Short()
		return nil
	}))

	// Breadth-first walk collecting every reachable release tag, then pick the
	// numerically highest one. The walk is written out here instead of reusing
	// GetVersionInfo() so that a regression in its selection rule still fails.
	visited := make(map[plumbing.Hash]struct{})
	queue := []*object.Commit{head}
	visited[head.Hash] = struct{}{}

	var (
		bestVersion string
		bestParts   []int
	)
	for len(queue) > 0 && len(visited) < versionSearchCutoff {
		current := queue[0]
		queue = queue[1:]

		if tag, ok := tagMap[current.Hash]; ok {
			parts := parseReleaseVersion(tag)
			if parts != nil && (bestParts == nil || compareVersionParts(parts, bestParts) > 0) {
				bestParts = parts
				bestVersion = tag
			}
		}

		err := current.Parents().ForEach(func(parent *object.Commit) error {
			if _, seen := visited[parent.Hash]; seen {
				return nil
			}
			visited[parent.Hash] = struct{}{}
			queue = append(queue, parent)
			return nil
		})
		if err != nil {
			// Same tolerance as GetVersionInfo(): a shallow clone stops at a
			// boundary commit whose parents are not in the object database.
			if !errors.Is(err, io.EOF) && !errors.Is(err, plumbing.ErrObjectNotFound) {
				assert.NoError(t, err)
			}
			break
		}
	}

	if bestVersion == "" {
		t.Skip("no release tag is reachable from HEAD")
	}

	assert.Equal(t, bestVersion, versionInfo.Version,
		"GetVersionInfo() must report the highest reachable release tag, not a stale one from a merged side branch")
}

func compareVersionParts(a []int, b []int) int {
	for i := range a {
		if i >= len(b) {
			return 1
		}
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// TestIsBetterVersionTag pins the rule that keeps a merged-in upstream release from
// being shadowed by a stale tag on the fork side, including the case where the
// stale tag was committed later.
func TestIsBetterVersionTag(t *testing.T) {
	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// The v4 release wins over a v3 tag even when the v3 tag is the newer commit,
	// which is exactly what a fork merging upstream runs into.
	assert.True(t, isBetterVersionTag("v4.24.0", older, "v3.154.4", newer))
	assert.False(t, isBetterVersionTag("v3.154.4", newer, "v4.24.0", older))

	// Comparison is numeric, not lexicographic.
	assert.True(t, isBetterVersionTag("v4.24.0", older, "v4.9.0", newer))
	assert.True(t, isBetterVersionTag("v4.24.10", older, "v4.24.9", newer))

	// Equal versions fall back to the newer commit date.
	assert.True(t, isBetterVersionTag("v4.24.0", newer, "v4.24.0", older))
	assert.False(t, isBetterVersionTag("v4.24.0", older, "v4.24.0", newer))

	// A release tag outranks any non-release tag, and among non-release tags the
	// newer commit wins.
	assert.True(t, isBetterVersionTag("v1.2.3", older, "sfx-2026", newer))
	assert.False(t, isBetterVersionTag("sfx-2026", newer, "v1.2.3", older))
	assert.True(t, isBetterVersionTag("sfx-2026", newer, "sfx-2025", older))
}

func TestParseReleaseVersion(t *testing.T) {
	assert.Equal(t, []int{4, 24, 0}, parseReleaseVersion("v4.24.0"))
	assert.Equal(t, []int{3, 154, 4}, parseReleaseVersion("v3.154.4"))

	for _, version := range []string{"", "dev", "v4.24", "4.24.0", "v4.24.0-dirty", "v4.24.0-1-gabc", "sfx_release+v4.24.0"} {
		assert.Nil(t, parseReleaseVersion(version), "parseReleaseVersion(%q) must be nil", version)
	}
}
