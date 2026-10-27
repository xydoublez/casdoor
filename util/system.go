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

package util

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// versionSearchCutoff bounds the history walk done by GetVersionInfo(), the same
// way git describe caps its own search, so a large repository cannot turn a
// version lookup into a full history scan.
const versionSearchCutoff = 2000

type SystemInfo struct {
	CpuUsage     []float64 `json:"cpuUsage"`
	MemoryUsed   uint64    `json:"memoryUsed"`
	MemoryTotal  uint64    `json:"memoryTotal"`
	DiskUsed     uint64    `json:"diskUsed"`
	DiskTotal    uint64    `json:"diskTotal"`
	NetworkSent  uint64    `json:"networkSent"`
	NetworkRecv  uint64    `json:"networkRecv"`
	NetworkTotal uint64    `json:"networkTotal"`
}

type VersionInfo struct {
	Version      string `json:"version"`
	CommitId     string `json:"commitId"`
	CommitOffset int    `json:"commitOffset"`
}

// getCpuUsage get cpu usage
func getCpuUsage() ([]float64, error) {
	usage, err := cpu.Percent(time.Second, true)
	return usage, err
}

// getMemoryUsage get memory usage
func getMemoryUsage() (uint64, uint64, error) {
	virtualMem, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return 0, 0, err
	}

	memInfo, err := proc.MemoryInfo()
	if err != nil {
		return 0, 0, err
	}

	return memInfo.RSS, virtualMem.Total, nil
}

// getDiskUsage gets disk usage for Casdoor's data directory
func getDiskUsage() (uint64, uint64, error) {
	_, filename, _, _ := runtime.Caller(0)
	rootPath := path.Dir(path.Dir(filename))
	dataPath := filepath.Join(rootPath, "data")

	var size uint64
	err := filepath.Walk(dataPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	diskStat, err := disk.Usage(dataPath)
	if err != nil {
		diskStat, err = disk.Usage("/")
		if err != nil {
			return 0, 0, err
		}
	}

	return size, diskStat.Total, nil
}

// getNetworkUsage gets Casdoor process's own I/O usage
func getNetworkUsage() (uint64, uint64, uint64, error) {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return 0, 0, 0, err
	}

	ioCounters, err := proc.IOCounters()
	if err != nil {
		return 0, 0, 0, err
	}

	bytesSent := ioCounters.WriteBytes
	bytesRecv := ioCounters.ReadBytes
	bytesTotal := bytesSent + bytesRecv

	return bytesSent, bytesRecv, bytesTotal, nil
}

func GetSystemInfo() (*SystemInfo, error) {
	cpuUsage, err := getCpuUsage()
	if err != nil {
		return nil, err
	}

	memoryUsed, memoryTotal, err := getMemoryUsage()
	if err != nil {
		return nil, err
	}

	diskUsed, diskTotal, err := getDiskUsage()
	if err != nil {
		return nil, err
	}

	networkSent, networkRecv, networkTotal, err := getNetworkUsage()
	if err != nil {
		return nil, err
	}

	res := &SystemInfo{
		CpuUsage:     cpuUsage,
		MemoryUsed:   memoryUsed,
		MemoryTotal:  memoryTotal,
		DiskUsed:     diskUsed,
		DiskTotal:    diskTotal,
		NetworkSent:  networkSent,
		NetworkRecv:  networkRecv,
		NetworkTotal: networkTotal,
	}
	return res, nil
}

// GetVersionInfo get git current commit and repo release version
//
// It follows the same semantics as `git describe --tags`: walk the history
// breadth-first from HEAD and report the reachable tag whose commit is the
// newest one, together with how many commits away it is. A plain depth-first
// walk is not enough here, because a fork that merges upstream keeps two lineages
// alive and DFS would happily return an old v3.x tag from the fork side while the
// merged-in upstream history is already at v4.x.
func GetVersionInfo() (*VersionInfo, error) {
	res := &VersionInfo{
		Version:      "",
		CommitId:     "",
		CommitOffset: -1,
	}

	_, filename, _, _ := runtime.Caller(0)
	rootPath := path.Dir(path.Dir(filename))
	r, err := git.PlainOpen(rootPath)
	if err != nil {
		return res, err
	}
	ref, err := r.Head()
	if err != nil {
		return res, err
	}
	tags, err := r.Tags()
	if err != nil {
		return res, err
	}
	tagMap := make(map[plumbing.Hash]string)
	err = tags.ForEach(func(t *plumbing.Reference) error {
		// This technique should work for both lightweight and annotated tags.
		revHash, err := r.ResolveRevision(plumbing.Revision(t.Name()))
		if err != nil {
			// A shallow clone can hold a tag whose target object was never
			// fetched. Skip it rather than failing the whole lookup.
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return nil
			}
			return err
		}
		tagMap[*revHash] = t.Name().Short()
		return nil
	})
	if err != nil {
		return res, err
	}

	head, err := r.CommitObject(ref.Hash())
	if err != nil {
		return res, err
	}

	// Breadth-first walk, bounded like git describe's own cutoff so that a huge
	// history cannot make this call expensive.
	type entry struct {
		commit *object.Commit
		depth  int
	}

	visited := make(map[plumbing.Hash]struct{}, versionSearchCutoff)
	queue := []entry{{commit: head, depth: 0}}
	visited[head.Hash] = struct{}{}

	var (
		bestTag  string
		bestWhen time.Time
		bestAt   int
		found    bool
	)

	for len(queue) > 0 && len(visited) < versionSearchCutoff {
		cur := queue[0]
		queue = queue[1:]

		if tag, ok := tagMap[cur.commit.Hash]; ok {
			// The newest tagged commit wins, so a stale tag on a side branch
			// never shadows the release the code actually contains.
			if !found || cur.commit.Committer.When.After(bestWhen) {
				found = true
				bestTag = tag
				bestWhen = cur.commit.Committer.When
				bestAt = cur.depth
			}
		}

		err = cur.commit.Parents().ForEach(func(parent *object.Commit) error {
			if _, seen := visited[parent.Hash]; seen {
				return nil
			}
			visited[parent.Hash] = struct{}{}
			queue = append(queue, entry{commit: parent, depth: cur.depth + 1})
			return nil
		})
		if err != nil {
			// A shallow clone stops at a boundary commit whose parents are not in
			// the object database, walk as far as the history actually goes.
			if errors.Is(err, io.EOF) || errors.Is(err, plumbing.ErrObjectNotFound) {
				break
			}
			return res, err
		}
	}

	if !found {
		return res, nil
	}

	res = &VersionInfo{
		Version:      bestTag,
		CommitId:     ref.Hash().String(),
		CommitOffset: bestAt,
	}
	return res, nil
}

func GetBuiltInVersionInfo() *VersionInfo {
	return &VersionInfo{
		Version:      Version,
		CommitId:     CommitId,
		CommitOffset: CommitOffset,
	}
}
