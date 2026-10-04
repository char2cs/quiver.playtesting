// Package recorder records each playtest session to an mp4 by watching the VM over its own VNC connection.
package recorder

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	slugRe   = regexp.MustCompile(`[^a-z0-9]+`)
	unsafeID = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// Slug reduces a label to [a-z0-9-], at most 40 characters, so it can never form a path.
func Slug(label string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// LinkDir is the per-link folder. The numeric ID keeps it unique and stable when labels repeat or change.
func LinkDir(root string, linkID int64, label string) string {
	name := "link-" + strconv.FormatInt(linkID, 10)
	if s := Slug(label); s != "" {
		name += "-" + s
	}
	return filepath.Join(root, name)
}

// FileName is the mp4 for one segment of a session. Segment 1 has no suffix.
func FileName(started time.Time, sessionID string, segment int) string {
	base := started.UTC().Format("20060102T150405Z") + "_" + unsafeID.ReplaceAllString(sessionID, "")
	if segment > 1 {
		base += "_part" + strconv.Itoa(segment)
	}
	return base + ".mp4"
}
