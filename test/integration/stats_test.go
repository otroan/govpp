//  Copyright (c) 2022 Cisco and/or its affiliates.
//  Copyright (c) 2026 Meter, Inc.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at:
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package integration

import (
	"testing"

	"go.fd.io/govpp/adapter"
	"go.fd.io/govpp/adapter/statsclient"
	"go.fd.io/govpp/api"
	"go.fd.io/govpp/test/vpptesting"
)

func TestStatClientAll(t *testing.T) {
	test := vpptesting.SetupVPP(t)

	c := test.StatsConn()

	var err error
	t.Run("SystemStats", func(t *testing.T) {
		stats := new(api.SystemStats)
		if err = c.GetSystemStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%+v", stats)
	})
	t.Run("NodeStats", func(t *testing.T) {
		stats := new(api.NodeStats)
		if err = c.GetNodeStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%d node stats", len(stats.Nodes))
	})
	t.Run("ErrorStats", func(t *testing.T) {
		stats := new(api.ErrorStats)
		if err = c.GetErrorStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%d error stats", len(stats.Errors))
	})
	t.Run("InterfaceStats", func(t *testing.T) {
		stats := new(api.InterfaceStats)
		if err = c.GetInterfaceStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%d interface stats", len(stats.Interfaces))
	})
	t.Run("MemoryStats", func(t *testing.T) {
		stats := new(api.MemoryStats)
		if err = c.GetMemoryStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%d main, %d stat memory stats", len(stats.Main), len(stats.Stat))
	})
	t.Run("BufferStats", func(t *testing.T) {
		stats := new(api.BufferStats)
		if err = c.GetBufferStats(stats); err != nil {
			t.Fatal("getting stats failed:", err)
		}
		t.Logf("%d buffers stats", len(stats.Buffer))
	})
}

func TestStatClientNodeStats(t *testing.T) {
	test := vpptesting.SetupVPP(t)

	c := test.StatsConn()

	stats := new(api.NodeStats)

	if err := c.GetNodeStats(stats); err != nil {
		t.Fatal("getting node stats failed:", err)
	}
}

func TestStatClientNodeStatsAgain(t *testing.T) {
	test := vpptesting.SetupVPP(t)

	c := test.StatsConn()

	stats := new(api.NodeStats)

	if err := c.GetNodeStats(stats); err != nil {
		t.Fatal("getting node stats failed:", err)
	}
	if err := c.GetNodeStats(stats); err != nil {
		t.Fatal("getting node stats failed:", err)
	}
}

// TestStatClientSymlinks exercises ListSymlinks against a live VPP: that the
// reported (target, item) mapping is self-consistent, and that it actually agrees
// with what resolving each symlink individually yields - which is what makes it safe
// to read a backing vector once and label its items from the mapping.
//
// /err/<node>/<reason> is the case that motivates the API: every one of them is a
// symlink into a single /node/errors vector, and an item's position there comes from
// a heap allocation in vlib_register_errors, so it is not derivable from anything a
// caller can compute.
//
// The unit tests in adapter/statsclient cover the pointer walking and the UpdateDir
// symlink refresh deterministically, against a synthetic segment; this test is about
// agreeing with a real VPP's directory layout.
func TestStatClientSymlinks(t *testing.T) {
	test := vpptesting.SetupVPP(t)

	// create an interface so the directory carries per-interface symlink entries
	// (/interfaces/* aliasing into /if/*) alongside the /err/* ones
	test.MustCli("create loopback interface", "set interface state loop0 up")

	client := statsclient.NewStatsClient("")
	if err := client.Connect(); err != nil {
		t.Fatal("connecting stats client failed:", err)
	}
	defer func() { _ = client.Disconnect() }()

	symlinks, err := client.ListSymlinks()
	if err != nil {
		t.Fatal("ListSymlinks failed:", err)
	}
	if len(symlinks) == 0 {
		t.Fatal("expected at least one symlink entry in the stats directory")
	}

	all, err := client.DumpStats()
	if err != nil {
		t.Fatal("DumpStats failed:", err)
	}
	byIndex := make(map[uint32]adapter.StatEntry, len(all))
	for _, e := range all {
		byIndex[e.Index] = e
	}

	var errSymlinks int
	for _, s := range symlinks {
		target, ok := byIndex[s.TargetIndex]
		if !ok {
			t.Fatalf("%s: target index %d not present in directory", s.Name, s.TargetIndex)
		}
		if string(target.Name) != string(s.TargetName) {
			t.Fatalf("%s: target index %d is %q, but ListSymlinks reported %q",
				s.Name, s.TargetIndex, target.Name, s.TargetName)
		}
		// A symlink must alias a real counter, never another symlink.
		if target.Symlink {
			t.Fatalf("%s: target %q is itself a symlink", s.Name, target.Name)
		}

		// The value the mapping points at must equal the value obtained by resolving
		// the symlink itself. This is the property a caller relies on when it reads
		// the backing vector directly and labels its items from ListSymlinks.
		resolved, ok := byIndex[s.Index]
		if !ok {
			t.Fatalf("%s: symlink index %d not present in directory", s.Name, s.Index)
		}
		want, ok := itemValue(t, target, s.ItemIndex)
		if !ok {
			continue // target type carries no per-item counters to compare
		}
		got, ok := itemValue(t, resolved, 0)
		if !ok {
			t.Fatalf("%s: resolved to unexpected type %T", s.Name, resolved.Data)
		}
		if got != want {
			t.Fatalf("%s: resolves to %d, but %s item %d holds %d",
				s.Name, got, target.Name, s.ItemIndex, want)
		}

		if string(target.Name) == "/node/errors" {
			errSymlinks++
		}
	}
	if errSymlinks == 0 {
		t.Fatal("expected /err/* symlinks aliasing /node/errors")
	}
	t.Logf("validated %d symlinks (%d of them error counters)", len(symlinks), errSymlinks)
}

// itemValue returns the item at index of a counter vector entry, summed over threads,
// and whether the entry has such items at all.
func itemValue(t *testing.T, e adapter.StatEntry, index uint32) (uint64, bool) {
	t.Helper()
	switch d := e.Data.(type) {
	case adapter.SimpleCounterStat:
		if len(d) == 0 || int(index) >= len(d[0]) {
			return 0, false
		}
		return adapter.ReduceSimpleCounterStatIndex(d, int(index)), true
	case adapter.CombinedCounterStat:
		if len(d) == 0 || int(index) >= len(d[0]) {
			return 0, false
		}
		return adapter.CombinedCounter(adapter.ReduceCombinedCounterStatIndex(d, int(index))).Packets(), true
	}
	return 0, false
}

// TestStatClientUpdateDirStaleEpoch checks that a dir prepared under one directory
// layout is rejected once the layout changes, and can be re-prepared afterwards.
func TestStatClientUpdateDirStaleEpoch(t *testing.T) {
	test := vpptesting.SetupVPP(t)

	test.MustCli("create loopback interface", "set interface state loop0 up")

	client := statsclient.NewStatsClient("")
	if err := client.Connect(); err != nil {
		t.Fatal("connecting stats client failed:", err)
	}
	defer func() { _ = client.Disconnect() }()

	dir, err := client.PrepareDir("/if", "/interfaces")
	if err != nil {
		t.Fatal("PrepareDir failed:", err)
	}
	// Under an unchanged layout UpdateDir must succeed.
	if err := client.UpdateDir(dir); err != nil {
		t.Fatal("UpdateDir failed:", err)
	}

	// Adding an interface changes the directory layout, which bumps the epoch.
	test.MustCli("create loopback interface")

	if err := client.UpdateDir(dir); err != adapter.ErrStatsDirStale {
		t.Fatalf("UpdateDir after layout change = %v, want %v", err, adapter.ErrStatsDirStale)
	}
	if _, err := client.PrepareDir("/if", "/interfaces"); err != nil {
		t.Fatal("re-PrepareDir after layout change failed:", err)
	}
}
