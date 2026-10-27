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
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"testing"

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

// TestGetVersionInfoReportsNewestReleaseTag guards the upgrade path: a fork that
// merges upstream keeps both lineages reachable, and picking the first tag met by
// a depth-first walk used to report a stale v3.x tag while the tree actually
// contained v4.x. GetVersionInfo() must report the newest release tag instead, so
// cross-check it against an independently computed highest release tag.
func TestGetVersionInfoReportsNewestReleaseTag(t *testing.T) {
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
			return nil
		}
		tagMap[*revHash] = tag.Name().Short()
		return nil
	}))

	releaseTag := regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	parse := func(version string) []int {
		match := releaseTag.FindStringSubmatch(version)
		if match == nil {
			return nil
		}
		res := make([]int, 0, len(match)-1)
		for _, part := range match[1:] {
			num, err := strconv.Atoi(part)
			if err != nil {
				return nil
			}
			res = append(res, num)
		}
		return res
	}

	// Breadth-first walk collecting every reachable release tag, then pick the
	// numerically highest one. This is a different selection rule than the one
	// GetVersionInfo() uses (newest committer date), so a disagreement means a
	// stale tag has been reported.
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
			parts := parse(tag)
			if parts != nil && (bestParts == nil || compareVersionParts(parts, bestParts) > 0) {
				bestParts = parts
				bestVersion = tag
			}
		}

		assert.NoError(t, current.Parents().ForEach(func(parent *object.Commit) error {
			if _, seen := visited[parent.Hash]; seen {
				return nil
			}
			visited[parent.Hash] = struct{}{}
			queue = append(queue, parent)
			return nil
		}))
	}

	if bestVersion == "" {
		t.Skip("no release tag is reachable from HEAD")
	}

	assert.Equal(t, bestVersion, versionInfo.Version,
		"GetVersionInfo() must report the newest reachable release tag, not a stale one from a merged side branch")
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
