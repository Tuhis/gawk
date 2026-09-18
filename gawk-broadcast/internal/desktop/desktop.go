// Package desktop is the Linux desktop identity of the GUI (R44, docs/53).
//
// Gio has no window-icon API on X11 or Wayland. The desktop resolves an
// application's icon from its Wayland app_id / X11 class hint through a
// desktop entry of the same name and the hicolor icon theme — so the ID is
// the entire mechanism, and every mirror of it must agree: gioui.org/app.ID,
// the desktop entry's file name, its Icon= and StartupWMClass=, and the
// installed icon's file name. This package holds the one constant; the tests
// hold the desktop entry to it.
package desktop

import (
	"os"
	"path/filepath"
	"strings"
)

// AppID is the reverse-DNS application ID (docs/53 D3). It is what GNOME
// Shell and KDE match `<AppID>.desktop` against, exactly, so it is also the
// desktop entry's base name and the hicolor icon name.
const AppID = "fi.ioio.gawk.broadcast"

// StockIcon is the freedesktop icon name notifications fall back to when the
// desktop entry is not installed. A notification naming an icon the theme
// cannot resolve shows no icon at all, which is worse than a generic one.
const StockIcon = "video-display"

// IconName returns the notification icon to use: AppID when the hicolor
// theme in any XDG data directory has it, StockIcon otherwise (docs/53 D5).
func IconName() string {
	return iconNameIn(os.Getenv("HOME"), os.Getenv("XDG_DATA_HOME"), os.Getenv("XDG_DATA_DIRS"))
}

// iconNameIn is IconName with the environment as arguments. The search
// order and defaults are the XDG base directory specification's:
// $XDG_DATA_HOME (default ~/.local/share), then each of $XDG_DATA_DIRS
// (default /usr/local/share:/usr/share).
func iconNameIn(home, dataHome, dataDirs string) string {
	if dataHome == "" && home != "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	if dataDirs == "" {
		dataDirs = "/usr/local/share:/usr/share"
	}
	dirs := []string{}
	if dataHome != "" {
		dirs = append(dirs, dataHome)
	}
	for _, d := range strings.Split(dataDirs, ":") {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if hasHicolorIcon(d) {
			return AppID
		}
	}
	return StockIcon
}

// hasHicolorIcon reports whether <dataDir>/icons/hicolor/<size>/apps/ holds
// the icon at any size, scalable included.
func hasHicolorIcon(dataDir string) bool {
	sizes, err := os.ReadDir(filepath.Join(dataDir, "icons", "hicolor"))
	if err != nil {
		return false
	}
	for _, s := range sizes {
		if !s.IsDir() {
			continue
		}
		for _, ext := range []string{".svg", ".png"} {
			if _, err := os.Stat(filepath.Join(dataDir, "icons", "hicolor", s.Name(), "apps", AppID+ext)); err == nil {
				return true
			}
		}
	}
	return false
}
