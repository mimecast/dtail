package clients

import (
	"fmt"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

func listingAnswer(truncated bool, names ...string) protocol.ProbeAnswer {
	answer := protocol.ProbeAnswer{Truncated: truncated}
	for _, name := range names {
		answer.Entries = append(answer.Entries, protocol.ProbeEntry{Name: name})
	}
	return answer
}

// TestFindListing covers which cached listing completes a base name and
// which request is sent when none does.
func TestFindListing(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tests := []struct {
		name         string
		cached       map[string]protocol.ProbeAnswer
		age          time.Duration
		dir, base    string
		wantFound    bool
		wantComplete bool
		wantPrefix   string // of the listing found
		wantFetch    string
	}{
		{name: "nothing cached", dir: "/var/log/", base: "ap", wantFetch: "/var/log/"},
		{name: "nothing cached, hidden", dir: "/var/log/", base: ".a", wantFetch: "/var/log/."},
		{name: "whole directory", cached: map[string]protocol.ProbeAnswer{"/var/log/": listingAnswer(false, "app.log")},
			dir: "/var/log/", base: "ap", wantFound: true, wantComplete: true, wantPrefix: "/var/log/"},
		{name: "whole directory lacks hidden names", cached: map[string]protocol.ProbeAnswer{
			"/var/log/": listingAnswer(false, "app.log")},
			dir: "/var/log/", base: ".a", wantFetch: "/var/log/."},
		{name: "narrower listing does not cover a shorter base", cached: map[string]protocol.ProbeAnswer{
			"/var/log/app": listingAnswer(false, "app.log")},
			dir: "/var/log/", base: "a", wantFetch: "/var/log/"},
		{name: "other directory", cached: map[string]protocol.ProbeAnswer{"/var/": listingAnswer(false, "log")},
			dir: "/var/log/", base: "", wantFetch: "/var/log/"},
		{name: "subdirectory listing is no match", cached: map[string]protocol.ProbeAnswer{
			"/var/log/sub/": listingAnswer(false, "x")},
			dir: "/var/log/", base: "sub", wantFetch: "/var/log/"},
		{name: "truncated asks for a narrower one", cached: map[string]protocol.ProbeAnswer{
			"/var/log/": listingAnswer(true, "a.log")},
			dir: "/var/log/", base: "app", wantFound: true, wantPrefix: "/var/log/", wantFetch: "/var/log/app"},
		{name: "truncated narrowest is complete", cached: map[string]protocol.ProbeAnswer{
			"/var/log/": listingAnswer(true, "a.log"), "/var/log/app": listingAnswer(true, "app.log")},
			dir: "/var/log/", base: "app", wantFound: true, wantComplete: true, wantPrefix: "/var/log/app"},
		{name: "complete wins over truncated narrower", cached: map[string]protocol.ProbeAnswer{
			"/var/log/a": listingAnswer(false, "app.log"), "/var/log/app": listingAnswer(true, "app.log")},
			dir: "/var/log/", base: "app", wantFound: true, wantComplete: true, wantPrefix: "/var/log/a"},
		{name: "stale listing", cached: map[string]protocol.ProbeAnswer{"/var/log/": listingAnswer(false, "app.log")},
			age: pathListingMaxAge + time.Second, dir: "/var/log/", base: "ap", wantFetch: "/var/log/"},
		{name: "journal units", cached: map[string]protocol.ProbeAnswer{"journal:": listingAnswer(false, "app.service")},
			dir: "journal:", base: "a", wantFound: true, wantComplete: true, wantPrefix: "journal:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFieldCatalog()
			for prefix, answer := range tt.cached {
				c.storeListing("srv1", prefix, answer, now.Add(-tt.age))
			}
			got := c.findListing(tt.dir, tt.base, now)
			if got.found != tt.wantFound || got.complete != tt.wantComplete || got.fetch != tt.wantFetch ||
				(tt.wantFound && got.listing.prefix != tt.wantPrefix) {
				t.Fatalf("findListing = found %v complete %v prefix %q fetch %q; want %v %v %q %q",
					got.found, got.complete, got.listing.prefix, got.fetch,
					tt.wantFound, tt.wantComplete, tt.wantPrefix, tt.wantFetch)
			}
		})
	}
}

// TestStoreListing: a busy-only answer is not cached (it says nothing about
// the directory), an error reason is kept, and the cache keeps at most
// pathListingMaxCached listings, dropping the oldest.
func TestStoreListing(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newFieldCatalog()
	busy := protocol.ProbeAnswer{Errors: []protocol.ProbeError{{Error: protocol.ProbeErrorBusy}}}
	c.storeListing("srv1", "/var/log/", busy, now)
	if got := c.findListing("/var/log/", "", now); got.found {
		t.Fatalf("busy answer cached: %+v", got.listing)
	}
	disabled := protocol.ProbeAnswer{Errors: []protocol.ProbeError{{Error: protocol.ProbeErrorListDisabled}}}
	c.storeListing("srv1", "/var/log/", disabled, now)
	if got := c.findListing("/var/log/", "", now); !got.found || got.listing.err != protocol.ProbeErrorListDisabled {
		t.Fatalf("disabled answer = %+v", got)
	}

	for i := range pathListingMaxCached + 1 {
		c.storeListing("srv1", fmt.Sprintf("/d%d/", i), listingAnswer(false, "f"), now.Add(time.Duration(i+1)*time.Millisecond))
	}
	if len(c.listings) != pathListingMaxCached {
		t.Fatalf("cached listings = %d, want %d", len(c.listings), pathListingMaxCached)
	}
	if _, ok := c.listings["/var/log/"]; ok {
		t.Fatal("the oldest listing was not dropped")
	}
	if _, ok := c.listings[fmt.Sprintf("/d%d/", pathListingMaxCached)]; !ok {
		t.Fatal("the newest listing was dropped")
	}
}

// TestTransientListingErrors: an answer whose listing failed for the moment
// (probe: timed out, probe: unable to list) is used only briefly, by the
// Tab that waited for it, and is never complete for a narrower prefix, so a
// later Tab asks again instead of showing the error for pathListingMaxAge.
// An answer that says the listing is not possible (disabled) is cached like
// any listing.
func TestTransientListingErrors(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	failed := func(reason string) protocol.ProbeAnswer {
		return protocol.ProbeAnswer{Errors: []protocol.ProbeError{{Error: reason}}}
	}
	for _, reason := range []string{protocol.ProbeErrorTimeout, protocol.ProbeErrorListFailed} {
		c := newFieldCatalog()
		c.storeListing("srv1", "journal:", failed(reason), now)
		if match := c.findListing("journal:", "", now); !match.found || !match.complete || match.listing.err != reason {
			t.Fatalf("%s: right away = %+v, want the failed listing for its own prefix", reason, match)
		}
		if match := c.findListing("journal:", "ss", now); match.complete || match.fetch != "journal:ss" {
			t.Fatalf("%s: narrower prefix = %+v, want a fetch of journal:ss", reason, match)
		}
		later := now.Add(30 * time.Second)
		if match := c.findListing("journal:", "", later); match.found || match.fetch != "journal:" {
			t.Fatalf("%s: 30 s later = %+v, want a new fetch", reason, match)
		}
	}

	c := newFieldCatalog()
	c.storeListing("srv1", "/var/log/", failed(protocol.ProbeErrorListDisabled), now)
	if match := c.findListing("/var/log/", "a", now.Add(time.Minute)); !match.found || !match.complete {
		t.Fatalf("disabled listing a minute later = %+v, want it cached and complete", match)
	}
}

func TestListingDirKey(t *testing.T) {
	for _, tt := range []struct{ entry, dir, base string }{
		{"/var/log/app", "/var/log/", "app"},
		{"/var/log/", "/var/log/", ""},
		{"/", "/", ""},
		{"app.log", pathListingWorkingDir, "app.log"},
		{"", pathListingWorkingDir, ""},
		{"logs/a", "logs/", "a"},
		{"journal:ss", "journal:", "ss"},
		{"journal:", "journal:", ""},
	} {
		if dir, base := listingDirKey(tt.entry); dir != tt.dir || base != tt.base {
			t.Errorf("listingDirKey(%q) = %q, %q; want %q, %q", tt.entry, dir, base, tt.dir, tt.base)
		}
	}
}
