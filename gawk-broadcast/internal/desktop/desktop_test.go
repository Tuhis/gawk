package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func install(t *testing.T, dataDir, size, ext string) {
	t.Helper()
	dir := filepath.Join(dataDir, "icons", "hicolor", size, "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AppID+ext), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIconNameFallsBackToTheStockIconWhenNothingIsInstalled(t *testing.T) {
	empty := t.TempDir()
	if got := iconNameIn("", empty, empty); got != StockIcon {
		t.Fatalf("got %q, want %q", got, StockIcon)
	}
	// Directories that do not exist are not an error either.
	if got := iconNameIn("", filepath.Join(empty, "nope"), filepath.Join(empty, "nor")); got != StockIcon {
		t.Fatalf("got %q, want %q", got, StockIcon)
	}
}

func TestIconNameFindsTheIconInXDGDataHome(t *testing.T) {
	home := t.TempDir()
	install(t, home, "scalable", ".svg")
	if got := iconNameIn("", home, t.TempDir()); got != AppID {
		t.Fatalf("got %q, want %q", got, AppID)
	}
}

func TestIconNameDefaultsDataHomeToLocalShare(t *testing.T) {
	home := t.TempDir()
	install(t, filepath.Join(home, ".local", "share"), "48x48", ".png")
	if got := iconNameIn(home, "", t.TempDir()); got != AppID {
		t.Fatalf("got %q, want %q", got, AppID)
	}
}

func TestIconNameSearchesEveryXDGDataDir(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	install(t, second, "256x256", ".png")
	if got := iconNameIn("", t.TempDir(), first+":"+second); got != AppID {
		t.Fatalf("got %q, want %q", got, AppID)
	}
}

func TestIconNameIgnoresOtherAppsIcons(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "icons", "hicolor", "scalable", "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "something-else.svg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := iconNameIn("", home, t.TempDir()); got != StockIcon {
		t.Fatalf("got %q, want %q", got, StockIcon)
	}
}

// The desktop entry is the other half of the mechanism (docs/53 D3): its file
// name, Icon= and StartupWMClass= must all be AppID, or the desktop shows the
// stock icon with no error anywhere.
func TestDesktopEntryAgreesWithAppID(t *testing.T) {
	path := filepath.Join("..", "..", "desktop", AppID+".desktop")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the desktop entry must be named after AppID: %v", err)
	}
	keys := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			keys[k] = v
		}
	}
	for k, want := range map[string]string{
		"Icon":           AppID,
		"StartupWMClass": AppID,
		"Type":           "Application",
	} {
		if keys[k] != want {
			t.Errorf("%s=%q, want %q", k, keys[k], want)
		}
	}
	if !strings.HasPrefix(keys["Exec"], "gawk-broadcast-gui") {
		t.Errorf("Exec=%q must launch gawk-broadcast-gui (the install script makes it absolute)", keys["Exec"])
	}
	if keys["Categories"] == "" || !strings.HasSuffix(keys["Categories"], ";") {
		t.Errorf("Categories=%q must be a ;-terminated list", keys["Categories"])
	}
}
