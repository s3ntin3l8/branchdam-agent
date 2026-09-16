package tray

import "testing"

// TestRegistryTitlesAreUnique guards against #187's regression: two
// independent registries -- Integrations() (catalog sync) and
// HookDescriptors() (render-hook installers) -- both got an entry titled
// "DaVinci Resolve" (IntegrationResolveDB and HookResolve respectively),
// and both used to feed a flat top-level tray menu item built straight
// from Title with no disambiguating context. The result was two identical
// sibling menu entries with no way to tell them apart.
//
// This test asserts uniqueness WITHIN each registry separately, NOT across
// their union. That is deliberate, not an oversight: since the
// per-integration/per-hook tray submenus were removed in favor of the
// Wails window's equivalent actions, "DaVinci Resolve" legitimately
// appears in BOTH registries -- once under the window's/status page's
// "Integrations" section, once under "Render hooks". Those are separately
// headed sections, so the repeated title is unambiguous there; the
// original bug was specifically two flat, unheaded top-level siblings in
// a systray menu. A union check would fail today, correctly rejecting a
// state this repo has deliberately chosen.
//
// Be honest about what this covers: a per-registry check would NOT have
// caught #187, because each registry individually had unique titles then
// too -- IntegrationResolveDB was the only Resolve entry in Integrations(),
// HookResolve the only one in HookDescriptors(). The actual guard against
// that regression recurring is structural: the flat top-level menu files
// (integrationsmenu.go, hooksmenu.go) are gone, so no surface renders
// descriptors from both registries as unheaded siblings any more. What
// this test covers is the narrower, still-worth-having property that one
// registry never renders two indistinguishable rows in its OWN section.
func TestRegistryTitlesAreUnique(t *testing.T) {
	t.Run("Integrations", func(t *testing.T) {
		checkDescriptorTitles(t, "Integrations()", len(Integrations()), func(i int) (id, title string) {
			d := Integrations()[i]
			return string(d.ID), d.Title
		})
	})
	t.Run("HookDescriptors", func(t *testing.T) {
		checkDescriptorTitles(t, "HookDescriptors()", len(HookDescriptors()), func(i int) (id, title string) {
			d := HookDescriptors()[i]
			return string(d.ID), d.Title
		})
	})
}

func checkDescriptorTitles(t *testing.T, registryName string, n int, at func(i int) (id, title string)) {
	t.Helper()
	seenIDs := make(map[string]bool, n)
	seenTitles := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, title := at(i)
		if id == "" {
			t.Errorf("%s[%d] has an empty ID", registryName, i)
		}
		if title == "" {
			t.Errorf("%s[%d] (ID %q) has an empty Title", registryName, i, id)
		}
		if seenIDs[id] {
			t.Errorf("%s has a duplicate ID %q", registryName, id)
		}
		seenIDs[id] = true
		if seenTitles[title] {
			t.Errorf("%s has a duplicate Title %q -- two entries in the SAME registry with the same Title is exactly #187's regression", registryName, title)
		}
		seenTitles[title] = true
	}
}

// TestDisplayNameFallsBackToID pins IntegrationStatus.DisplayName and
// HookStatus.DisplayName's Title-else-ID fallback. This is load-bearing
// for statusserver_test.go, which builds IntegrationStatus/HookStatus
// literals directly (not via Status()) and never sets Title -- those
// fixtures must keep rendering exactly what they render today (the raw
// ID) once the status page template switches from {{.ID}} to
// {{.DisplayName}}.
func TestDisplayNameFallsBackToID(t *testing.T) {
	t.Run("IntegrationStatus with Title", func(t *testing.T) {
		is := IntegrationStatus{ID: IntegrationLuminar, Title: "Luminar Neo"}
		if got := is.DisplayName(); got != "Luminar Neo" {
			t.Errorf("DisplayName() = %q, want %q", got, "Luminar Neo")
		}
	})
	t.Run("IntegrationStatus without Title falls back to ID", func(t *testing.T) {
		is := IntegrationStatus{ID: IntegrationLuminar}
		if got := is.DisplayName(); got != string(IntegrationLuminar) {
			t.Errorf("DisplayName() = %q, want %q", got, IntegrationLuminar)
		}
	})
	t.Run("HookStatus with Title", func(t *testing.T) {
		hs := HookStatus{ID: HookResolve, Title: "DaVinci Resolve"}
		if got := hs.DisplayName(); got != "DaVinci Resolve" {
			t.Errorf("DisplayName() = %q, want %q", got, "DaVinci Resolve")
		}
	})
	t.Run("HookStatus without Title falls back to ID", func(t *testing.T) {
		hs := HookStatus{ID: HookResolve}
		if got := hs.DisplayName(); got != string(HookResolve) {
			t.Errorf("DisplayName() = %q, want %q", got, HookResolve)
		}
	})
}
