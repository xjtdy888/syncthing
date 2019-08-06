// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package versioner

import (
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/d4l3k/messagediff"
	"github.com/syncthing/syncthing/lib/fs"
)

func TestStaggeredVersioningVersionCount(t *testing.T) {
	/* Default settings:

	{30, 3600},       // first hour -> 30 sec between versions
	{3600, 86400},    // next day -> 1 h between versions
	{86400, 592000},  // next 30 days -> 1 day between versions
	{604800, maxAge}, // next year -> 1 week between versions
	*/

	now := parseTime("20160415-140000")
	versionsWithMtime := []string{
		// 14:00:00 is "now"
		"test~20160415-140000", // 0 seconds ago
		"test~20160415-135959", // 1 second ago
		"test~20160415-135958", // 2 seconds ago
		"test~20160415-135900", // 1 minute ago
		"test~20160415-135859", // 1 minute 1 second ago
		"test~20160415-135830", // 1 minute 30 seconds ago
		"test~20160415-135829", // 1 minute 31 seconds ago
		"test~20160415-135700", // 3 minutes ago
		"test~20160415-135630", // 3 minutes 30 seconds ago
		"test~20160415-133000", // 30 minutes ago
		"test~20160415-132900", // 31 minutes ago
		"test~20160415-132500", // 35 minutes ago
		"test~20160415-132000", // 40 minutes ago
		"test~20160415-130000", // 60 minutes ago
		"test~20160415-124000", // 80 minutes ago
		"test~20160415-122000", // 100 minutes ago
		"test~20160415-110000", // 120 minutes ago
	}

	delete := []string{
		"test~20160415-140000", // 0 seconds ago
		"test~20160415-135959", // 1 second ago
		"test~20160415-135900", // 1 minute ago
		"test~20160415-135830", // 1 minute 30 second ago
		"test~20160415-130000", // 60 minutes ago
		"test~20160415-124000", // 80 minutes ago
	}
	sort.Strings(delete)

	v := NewStaggered("", fs.NewFilesystem(fs.FilesystemTypeFake, "testdata"), map[string]string{
		"maxAge": strconv.Itoa(365 * 86400),
	}).(*Staggered)
	rem := v.toRemove(versionsWithMtime, now)
	sort.Strings(rem)

	if diff, equal := messagediff.PrettyDiff(delete, rem); !equal {
		t.Errorf("Incorrect deleted files; got %v, expected %v\n%v", rem, delete, diff)
	}
}

func parseTime(in string) time.Time {
	t, err := time.ParseInLocation(TimeFormat, in, time.Local)
	if err != nil {
		panic(err.Error())
	}
	return t
}
