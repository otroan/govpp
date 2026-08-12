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

package statsclient

import (
	"sync/atomic"
	"testing"
	"unsafe"

	"go.fd.io/govpp/adapter"
)

// A synthetic v2 stats segment, laid out exactly as VPP lays out the real one, so
// the client's unsafe pointer walking can be exercised without a running VPP. It
// mirrors the shape VPP uses for error counters - one vector, plus a symlink naming
// each of its items:
//
//	index 0: /sys/fake-scalar            filler, see fakeTargetIndex
//	index 1: /node/errors                simple counter vector, one thread
//	index 2+: /err/fake-node/<reason>    symlinks, one per item of that vector
//
// Pointers stored inside the segment are VPP-side addresses (fakeBase + offset),
// which is what adjust() expects to translate back into the mapped region.
const fakeBase = uint64(0x7f0000000000)

// v2 stat segment directory types, per dirTypeMapping.
const (
	fakeTypeScalarIndex         = 1
	fakeTypeSimpleCounterVector = 2
	fakeTypeSymlink             = 6
)

// fakeTargetIndex is the directory index of /node/errors. It is deliberately not 0:
// CopyEntryData treats a directory entry whose union data is zero as having no data,
// so a symlink to item 0 of directory index 0 is unrepresentable. Real VPP never
// lands there either, but a fake that did would fail for that reason alone.
const fakeTargetIndex = 1

// shared header field offsets, per sharedHeaderV2.
const (
	fakeOffVersion     = 0
	fakeOffBase        = 8
	fakeOffEpoch       = 16
	fakeOffInProgress  = 24
	fakeOffDirVector   = 32
	fakeOffErrorVector = 40
)

type fakeSegment struct {
	buf []byte
	// counters is the offset of the backing counter data for thread 0.
	counters int
}

// newFakeSegment builds a segment holding a /node/errors vector with the given
// counter values, plus one /err/fake-node/rN symlink per value, in reverse order so
// that a symlink's own directory index is never its item index (which would let an
// off-by-one confusion pass unnoticed).
func newFakeSegment(t *testing.T, values []uint64) *fakeSegment {
	t.Helper()

	const (
		hdrSize   = 64 // sharedHeaderV2 rounded up
		vecHdr    = 8  // vector length precedes the data
		ptrSize   = 8
		threads   = 1
		trailer   = 8 // adjust() rejects pointers to the very last byte
		dirEntLen = int(unsafe.Sizeof(statSegDirectoryEntryV2{}))
	)
	nDir := fakeTargetIndex + 1 + len(values) // filler + /node/errors + one symlink per value

	dirLenOff := hdrSize
	dirOff := dirLenOff + vecHdr
	ptLenOff := dirOff + nDir*dirEntLen // per-thread vector of pointers
	ptOff := ptLenOff + vecHdr
	ctLenOff := ptOff + threads*ptrSize // thread 0 counter vector
	ctOff := ctLenOff + vecHdr
	total := ctOff + len(values)*ptrSize + trailer

	f := &fakeSegment{buf: make([]byte, total), counters: ctOff}

	// Shared header. errorVector stays zero: adjust() then rejects it, which is how
	// the client decides a segment uses the modern (non-legacy) type mapping.
	f.putU64(fakeOffVersion, 2)
	f.putU64(fakeOffBase, fakeBase)
	f.putU64(fakeOffEpoch, 1)
	f.putU64(fakeOffInProgress, 0)
	f.putU64(fakeOffDirVector, fakeBase+uint64(dirOff))
	f.putU64(fakeOffErrorVector, 0)

	// Vector lengths.
	f.putU64(dirLenOff, uint64(nDir))
	f.putU64(ptLenOff, threads)
	f.putU64(ctLenOff, uint64(len(values)))

	// Counter data, and the per-thread vector pointing at it.
	f.putU64(ptOff, fakeBase+uint64(ctOff))
	for i, v := range values {
		f.putU64(ctOff+i*ptrSize, v)
	}

	// Directory entry 0: filler, so the backing vector is not at index 0.
	f.putDirEntry(dirOff, 0, fakeTypeScalarIndex, 7, "/sys/fake-scalar")

	// Directory entry 1: the backing vector.
	f.putDirEntry(dirOff, fakeTargetIndex, fakeTypeSimpleCounterVector, fakeBase+uint64(ptOff), "/node/errors")

	// The remaining entries are symlinks into it, named in reverse item order.
	for i := range values {
		item := uint32(len(values) - 1 - i)
		union := uint64(fakeTargetIndex) | uint64(item)<<32
		f.putDirEntry(dirOff, fakeTargetIndex+1+i, fakeTypeSymlink, union, fakeErrName(item))
	}
	return f
}

func fakeErrName(item uint32) string {
	return "/err/fake-node/r" + string(rune('a'+item))
}

// fakeErrItem is the inverse of fakeErrName, so a test can tell which counter a
// symlink entry should be showing without relying on the API under test.
func fakeErrItem(t *testing.T, name []byte) uint32 {
	t.Helper()
	for item := uint32(0); item < 32; item++ {
		if fakeErrName(item) == string(name) {
			return item
		}
	}
	t.Fatalf("%s is not a fake symlink name", name)
	return 0
}

func (f *fakeSegment) putU64(off int, v uint64) {
	*(*uint64)(unsafe.Pointer(&f.buf[off])) = v
}

func (f *fakeSegment) putDirEntry(dirOff, index int, typ dirType, union uint64, name string) {
	e := (*statSegDirectoryEntryV2)(unsafe.Pointer(&f.buf[dirOff+index*int(unsafe.Sizeof(statSegDirectoryEntryV2{}))]))
	e.directoryType = typ
	e.unionData = union
	copy(e.name[:], name)
	e.name[len(name)] = 0
}

// setCounter changes a backing counter value, as VPP would between two reads.
func (f *fakeSegment) setCounter(item int, v uint64) {
	f.putU64(f.counters+item*8, v)
}

// bumpEpoch simulates a directory re-layout.
func (f *fakeSegment) bumpEpoch() {
	f.putU64(fakeOffEpoch, *(*uint64)(unsafe.Pointer(&f.buf[fakeOffEpoch]))+1)
}

// client returns a StatsClient reading this segment, without a socket.
func (f *fakeSegment) client() *StatsClient {
	sc := &StatsClient{statSegment: newStatSegmentV2(f.buf, int64(len(f.buf)))}
	atomic.StoreUint32(&sc.connected, 1)
	return sc
}

// symlinkValue returns the single counter value an entry resolved through a symlink
// carries.
func symlinkValue(t *testing.T, e adapter.StatEntry) uint64 {
	t.Helper()
	s, ok := e.Data.(adapter.SimpleCounterStat)
	if !ok {
		t.Fatalf("%s: expected SimpleCounterStat, got %T", e.Name, e.Data)
	}
	if len(s) != 1 || len(s[0]) != 1 {
		t.Fatalf("%s: expected a single resolved item, got %v", e.Name, s)
	}
	return uint64(s[0][0])
}

func TestListSymlinks(t *testing.T) {
	values := []uint64{10, 20, 30}
	f := newFakeSegment(t, values)
	sc := f.client()

	symlinks, err := sc.ListSymlinks()
	if err != nil {
		t.Fatal("ListSymlinks failed:", err)
	}
	if len(symlinks) != len(values) {
		t.Fatalf("expected %d symlinks, got %d", len(values), len(symlinks))
	}
	for _, s := range symlinks {
		if got, want := string(s.TargetName), "/node/errors"; got != want {
			t.Errorf("%s: target name = %q, want %q", s.Name, got, want)
		}
		if s.TargetIndex != fakeTargetIndex {
			t.Errorf("%s: target index = %d, want %d", s.Name, s.TargetIndex, fakeTargetIndex)
		}
		// The item index is the whole point: it must name the counter this symlink
		// aliases, independent of the symlink's own directory index.
		if got, want := string(s.Name), fakeErrName(s.ItemIndex); got != want {
			t.Errorf("item index %d resolved to %q, want %q", s.ItemIndex, want, got)
		}
	}
}

// The mapping ListSymlinks reports must agree with what resolving the symlink
// individually yields - otherwise a caller reading the backing vector directly and
// labelling it from ListSymlinks would mislabel every item.
func TestListSymlinksAgreesWithResolvedValues(t *testing.T) {
	values := []uint64{11, 22, 33, 44}
	f := newFakeSegment(t, values)
	sc := f.client()

	symlinks, err := sc.ListSymlinks()
	if err != nil {
		t.Fatal("ListSymlinks failed:", err)
	}
	entries, err := sc.DumpStats("^/err/")
	if err != nil {
		t.Fatal("DumpStats failed:", err)
	}
	byName := make(map[string]adapter.StatEntry, len(entries))
	for _, e := range entries {
		byName[string(e.Name)] = e
	}
	for _, s := range symlinks {
		e, ok := byName[string(s.Name)]
		if !ok {
			t.Fatalf("%s: not returned by DumpStats", s.Name)
		}
		if got, want := symlinkValue(t, e), values[s.ItemIndex]; got != want {
			t.Errorf("%s: resolved value %d, but item index %d holds %d", s.Name, got, s.ItemIndex, want)
		}
	}
}

// UpdateDir must re-resolve symlink entries. Before the fix the type check in
// updateStatOnIndex skipped them - a symlink's directory type never equals the
// resolved type of its data - so a prepared dir kept returning its PrepareDir values.
func TestUpdateDirRefreshesSymlinks(t *testing.T) {
	values := []uint64{1, 2, 3}
	f := newFakeSegment(t, values)
	sc := f.client()

	dir, err := sc.PrepareDir()
	if err != nil {
		t.Fatal("PrepareDir failed:", err)
	}

	// Change every backing counter, exactly as VPP would while counting.
	updated := []uint64{100, 200, 300}
	for i, v := range updated {
		f.setCounter(i, v)
	}

	if err := sc.UpdateDir(dir); err != nil {
		t.Fatal("UpdateDir failed:", err)
	}

	var seen int
	for i := range dir.Entries {
		e := dir.Entries[i]
		if !e.Symlink {
			continue
		}
		seen++
		item := fakeErrItem(t, e.Name)
		if got, want := symlinkValue(t, e), updated[item]; got != want {
			t.Errorf("%s: value after UpdateDir = %d, want %d (stale value was %d)",
				e.Name, got, want, values[item])
		}
	}
	if seen != len(values) {
		t.Fatalf("expected %d symlink entries in the prepared dir, got %d", len(values), seen)
	}
}

// The non-symlink path must keep working, in place, as before.
func TestUpdateDirRefreshesCounterVector(t *testing.T) {
	f := newFakeSegment(t, []uint64{1, 2, 3})
	sc := f.client()

	dir, err := sc.PrepareDir("^/node/errors$")
	if err != nil {
		t.Fatal("PrepareDir failed:", err)
	}
	if len(dir.Entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(dir.Entries))
	}
	f.setCounter(1, 42)
	if err := sc.UpdateDir(dir); err != nil {
		t.Fatal("UpdateDir failed:", err)
	}
	s, ok := dir.Entries[0].Data.(adapter.SimpleCounterStat)
	if !ok {
		t.Fatalf("expected SimpleCounterStat, got %T", dir.Entries[0].Data)
	}
	if got := uint64(s[0][1]); got != 42 {
		t.Errorf("counter after UpdateDir = %d, want 42", got)
	}
}

func TestUpdateDirStaleEpoch(t *testing.T) {
	f := newFakeSegment(t, []uint64{1, 2, 3})
	sc := f.client()

	dir, err := sc.PrepareDir()
	if err != nil {
		t.Fatal("PrepareDir failed:", err)
	}
	f.bumpEpoch()
	if err := sc.UpdateDir(dir); err != adapter.ErrStatsDirStale {
		t.Fatalf("UpdateDir after epoch change = %v, want %v", err, adapter.ErrStatsDirStale)
	}
}

// v1 has no symlink target encoding, so nothing must be reported for it.
func TestGetSymlinkIndexesV1(t *testing.T) {
	ss := &statSegmentV1{}
	if _, _, ok := ss.GetSymlinkIndexes(nil); ok {
		t.Error("statSegmentV1 reported symlink indexes")
	}
}

// A non-symlink segment must not be reported as one, even though its union data
// would decode into a plausible-looking pair of indexes.
func TestGetSymlinkIndexesNonSymlink(t *testing.T) {
	f := newFakeSegment(t, []uint64{1})
	ss := newStatSegmentV2(f.buf, int64(len(f.buf)))

	vector := ss.GetDirectoryVector()
	segment, name, _ := ss.GetStatDirOnIndex(vector, fakeTargetIndex)
	if string(name) != "/node/errors" {
		t.Fatalf("index %d is %q, want /node/errors", fakeTargetIndex, name)
	}
	if _, _, ok := ss.GetSymlinkIndexes(segment); ok {
		t.Error("counter vector entry reported as a symlink")
	}
}
