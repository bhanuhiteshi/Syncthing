// Copyright (C) 2016 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// +build !solaris,!darwin solaris,cgo darwin,cgo

package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/zillode/notify"
)

func TestMain(m *testing.M) {
	if err := os.RemoveAll(testDir); err != nil {
		panic(err)
	}

	dir, err := filepath.Abs(".")
	if err != nil {
		panic("Cannot get absolute path to working dir")
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		panic("Cannot get real path to working dir")
	}
	testDirAbs = filepath.Join(dir, testDir)
	testFs = NewFilesystem(FilesystemTypeBasic, testDirAbs)

	backendBuffer = 10
	defer func() {
		backendBuffer = 500
	}()
	os.Exit(m.Run())
}

const (
	testDir = "temporary_test_root"
)

var (
	testDirAbs string
	testFs     Filesystem
)

func TestWatchIgnore(t *testing.T) {
	name := "ignore"

	file := "file"
	ignored := "ignored"

	testCase := func() {
		createTestFile(name, file)
		createTestFile(name, ignored)
	}

	expectedEvents := []Event{
		{file, NonRemove},
	}
	allowedEvents := []Event{
		{name, NonRemove},
	}

	testScenario(t, name, testCase, expectedEvents, allowedEvents, ignored)
}

func TestWatchRename(t *testing.T) {
	name := "rename"

	old := createTestFile(name, "oldfile")
	new := "newfile"

	testCase := func() {
		renameTestFile(name, old, new)
	}

	destEvent := Event{new, Remove}
	// Only on these platforms the removed file can be differentiated from
	// the created file during renaming
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "solaris" {
		destEvent = Event{new, NonRemove}
	}
	expectedEvents := []Event{
		{old, Remove},
		destEvent,
	}
	allowedEvents := []Event{
		{name, NonRemove},
	}

	// set the "allow others" flag because we might get the create of
	// "oldfile" initially
	testScenario(t, name, testCase, expectedEvents, allowedEvents, "")
}

// TestWatchOutside checks that no changes from outside the folder make it in
func TestWatchOutside(t *testing.T) {
	outChan := make(chan Event)
	backendChan := make(chan notify.EventInfo, backendBuffer)

	ctx, cancel := context.WithCancel(context.Background())

	// testFs is Filesystem, but we need BasicFilesystem here
	fs := newBasicFilesystem(testDirAbs)

	go func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("Watch did not panic on receiving event outside of folder")
			}
			cancel()
		}()
		fs.watchLoop(".", testDirAbs, backendChan, outChan, fakeMatcher{}, ctx)
	}()

	backendChan <- fakeEventInfo(filepath.Join(filepath.Dir(testDirAbs), "outside"))
}

func TestWatchSubpath(t *testing.T) {
	outChan := make(chan Event)
	backendChan := make(chan notify.EventInfo, backendBuffer)

	ctx, cancel := context.WithCancel(context.Background())

	// testFs is Filesystem, but we need BasicFilesystem here
	fs := newBasicFilesystem(testDirAbs)

	abs, _ := fs.rooted("sub")
	go fs.watchLoop("sub", abs, backendChan, outChan, fakeMatcher{}, ctx)

	backendChan <- fakeEventInfo(filepath.Join(abs, "file"))

	timeout := time.NewTimer(2 * time.Second)
	select {
	case <-timeout.C:
		t.Errorf("Timed out before receiving an event")
		cancel()
	case ev := <-outChan:
		if ev.Name != filepath.Join("sub", "file") {
			t.Errorf("While watching a subfolder, received an event with unexpected path %v", ev.Name)
		}
	}

	cancel()
}

// TestWatchOverflow checks that an event at the root is sent when maxFiles is reached
func TestWatchOverflow(t *testing.T) {
	name := "overflow"

	expectedEvents := []Event{
		{".", NonRemove},
	}

	allowedEvents := []Event{
		{name, NonRemove},
	}

	testCase := func() {
		for i := 0; i < 5*backendBuffer; i++ {
			file := "file" + strconv.Itoa(i)
			createTestFile(name, file)
			allowedEvents = append(allowedEvents, Event{file, NonRemove})
		}
	}

	testScenario(t, name, testCase, expectedEvents, allowedEvents, "")
}

// path relative to folder root, also creates parent dirs if necessary
func createTestFile(name string, file string) string {
	joined := filepath.Join(name, file)
	if err := testFs.MkdirAll(filepath.Dir(joined), 0755); err != nil {
		panic(fmt.Sprintf("Failed to create parent directory for %s: %s", joined, err))
	}
	handle, err := testFs.Create(joined)
	if err != nil {
		panic(fmt.Sprintf("Failed to create test file %s: %s", joined, err))
	}
	handle.Close()
	return file
}

func renameTestFile(name string, old string, new string) {
	old = filepath.Join(name, old)
	new = filepath.Join(name, new)
	if err := testFs.Rename(old, new); err != nil {
		panic(fmt.Sprintf("Failed to rename %s to %s: %s", old, new, err))
	}
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func testScenario(t *testing.T, name string, testCase func(), expectedEvents, allowedEvents []Event, ignored string) {
	if err := testFs.MkdirAll(name, 0755); err != nil {
		panic(fmt.Sprintf("Failed to create directory %s: %s", name, err))
	}
	defer testFs.RemoveAll(name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if ignored != "" {
		ignored = filepath.Join(name, ignored)
	}

	eventChan, err := testFs.Watch(name, fakeMatcher{ignored}, ctx, false)
	if err != nil {
		panic(err)
	}

	go testWatchOutput(t, name, eventChan, expectedEvents, allowedEvents, ctx, cancel)

	testCase()

	select {
	case <-time.NewTimer(time.Minute).C:
		t.Errorf("Timed out before receiving all expected events")

	case <-ctx.Done():
	}
}

func testWatchOutput(t *testing.T, name string, in <-chan Event, expectedEvents, allowedEvents []Event, ctx context.Context, cancel context.CancelFunc) {
	var expected = make(map[Event]struct{})
	for _, ev := range expectedEvents {
		ev.Name = filepath.Join(name, ev.Name)
		expected[ev] = struct{}{}
	}

	var received Event
	var last Event
	for {
		if len(expected) == 0 {
			cancel()
			return
		}

		select {
		case <-ctx.Done():
			return
		case received = <-in:
		}

		// apparently the backend sometimes sends repeat events
		if last == received {
			continue
		}

		if _, ok := expected[received]; !ok {
			if len(allowedEvents) > 0 {
				sleepMs(100) // To facilitate overflow
				continue
			}
			t.Errorf("Received unexpected event %v expected one of %v", received, expected)
			cancel()
			return
		}
		delete(expected, received)
		last = received
	}
}

type fakeMatcher struct{ match string }

func (fm fakeMatcher) ShouldIgnore(name string) bool {
	return name == fm.match
}

type fakeEventInfo string

func (e fakeEventInfo) Path() string {
	return string(e)
}

func (e fakeEventInfo) Event() notify.Event {
	return notify.Write
}

func (e fakeEventInfo) Sys() interface{} {
	return nil
}
