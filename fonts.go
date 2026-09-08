package main

// Fonts are the loudest thing a page can learn about the machine after the
// user agent: a site lays text out in the first family of its stack that
// exists, and a fingerprinting script measures a list of families to see
// which do. A device profile therefore gets a fontconfig configuration of
// its own (FONTCONFIG_FILE on the Chrome process — the browser resolves
// fonts for its renderers) that presents the image's fonts under the names
// the device would have.
//
// Chrome accepts a substitute for a named family only if the substituted
// pattern's first family is the font fontconfig found (or the pair is in
// Skia's short metric-compatible table: Arial/Liberation Sans, Calibri/
// Carlito, …), so the aliases here are <prefer> rules, which prepend, and
// the families a Windows machine would not have are hidden by rewriting a
// request whose first family is one of them — the site gets its next
// fallback, as it would there. Nothing is renamed; the fonts stay what they
// are, this only changes which names find them.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fontAlias presents the substitutes, in order of preference, under a
// family name of the device.
type fontAlias struct {
	Family     string
	Substitute []string
}

var (
	// The image's fonts (Dockerfile): Selawik is Microsoft's own
	// metric-compatible stand-in for Segoe UI, Carlito and Caladea Google's
	// for Calibri and Cambria, Liberation Red Hat's for Arial, Times New
	// Roman and Courier New; DejaVu is the Verdana/Tahoma lookalike, and
	// Noto CJK covers the East Asian families.
	segoe   = []string{"Selawik", "DejaVu Sans", "Liberation Sans"}
	verdana = []string{"DejaVu Sans", "Selawik", "Liberation Sans"}
	arial   = []string{"Liberation Sans", "Arimo", "DejaVu Sans"}
	// Impact and Arial Narrow are condensed; the narrow stand-in keeps them
	// visibly tighter than the body sans, which is also what makes the CSS
	// `fantasy` generic (Blink resolves it to Impact) measure narrow.
	narrow   = []string{"Liberation Sans Narrow", "Liberation Sans"}
	times    = []string{"Liberation Serif", "Tinos", "DejaVu Serif"}
	georgia  = []string{"DejaVu Serif", "Liberation Serif"}
	courier  = []string{"Liberation Mono", "Cousine", "DejaVu Sans Mono"}
	consolas = []string{"DejaVu Sans Mono", "Liberation Mono"}
	calibri  = []string{"Carlito", "Liberation Sans Narrow", "Selawik", "Liberation Sans"}
	cambria  = []string{"Caladea", "Liberation Serif"}
	jpSans   = []string{"Noto Sans CJK JP"}
	jpSerif  = []string{"Noto Serif CJK JP"}
	scSans   = []string{"Noto Sans CJK SC"}
	scSerif  = []string{"Noto Serif CJK SC"}
	tcSans   = []string{"Noto Sans CJK TC"}
	tcSerif  = []string{"Noto Serif CJK TC"}
	krSans   = []string{"Noto Sans CJK KR"}
	krSerif  = []string{"Noto Serif CJK KR"}
	emoji    = []string{"Noto Color Emoji"}
)

// windowsFonts are the families a Windows 11 machine has that a site or a
// fingerprinting script is likely to ask for by name.
var windowsFonts = []fontAlias{
	{"Segoe UI", segoe}, {"Segoe UI Light", segoe}, {"Segoe UI Semilight", segoe}, {"Segoe UI Semibold", segoe},
	{"Segoe UI Black", segoe}, {"Segoe UI Variable", segoe}, {"Segoe UI Variable Text", segoe}, {"Segoe UI Symbol", verdana},
	{"Segoe UI Emoji", emoji}, {"Segoe UI Historic", verdana}, {"Segoe Print", verdana}, {"Segoe Script", verdana},
	{"Arial", arial}, {"Arial Black", arial}, {"Arial Narrow", narrow},
	{"Helvetica", arial}, {"Microsoft Sans Serif", arial}, {"MS Sans Serif", arial}, {"MS Reference Sans Serif", arial},
	{"Times New Roman", times}, {"Times", times}, {"Courier New", courier}, {"Courier", courier},
	{"Tahoma", verdana}, {"Verdana", verdana}, {"Trebuchet MS", verdana}, {"Lucida Sans Unicode", verdana}, {"Lucida Sans", verdana},
	{"Century Gothic", verdana}, {"Candara", verdana}, {"Corbel", verdana}, {"Franklin Gothic Medium", verdana}, {"Franklin Gothic", verdana},
	{"Bahnschrift", verdana}, {"Ebrima", verdana}, {"Gadugi", verdana}, {"Leelawadee UI", verdana}, {"Nirmala UI", verdana},
	{"Sylfaen", georgia}, {"Comic Sans MS", verdana}, {"Impact", narrow}, {"Myanmar Text", verdana}, {"Javanese Text", verdana},
	{"Calibri", calibri}, {"Calibri Light", calibri}, {"Cambria", cambria}, {"Cambria Math", cambria},
	{"Georgia", georgia}, {"Constantia", georgia}, {"Palatino Linotype", georgia}, {"Book Antiqua", georgia},
	{"Garamond", georgia}, {"Sitka", georgia}, {"Sitka Text", georgia}, {"Sitka Small", georgia}, {"Gabriola", georgia},
	{"Consolas", consolas}, {"Lucida Console", consolas}, {"Cascadia Mono", consolas}, {"Cascadia Code", consolas},
	{"MS Gothic", jpSans}, {"MS PGothic", jpSans}, {"MS UI Gothic", jpSans}, {"Meiryo", jpSans}, {"Meiryo UI", jpSans},
	{"Yu Gothic", jpSans}, {"Yu Gothic UI", jpSans}, {"MS Mincho", jpSerif}, {"MS PMincho", jpSerif}, {"Yu Mincho", jpSerif},
	{"Microsoft YaHei", scSans}, {"Microsoft YaHei UI", scSans}, {"SimHei", scSans}, {"DengXian", scSans},
	{"SimSun", scSerif}, {"NSimSun", scSerif}, {"SimSun-ExtB", scSerif}, {"FangSong", scSerif}, {"KaiTi", scSerif},
	{"Microsoft JhengHei", tcSans}, {"Microsoft JhengHei UI", tcSans}, {"PMingLiU", tcSerif}, {"MingLiU", tcSerif},
	{"MingLiU-ExtB", tcSerif}, {"PMingLiU-ExtB", tcSerif},
	{"Malgun Gothic", krSans}, {"Gulim", krSans}, {"Dotum", krSans}, {"Batang", krSerif}, {"Gungsuh", krSerif},
}

// linuxFonts are the families a Windows machine would not have, as a site
// or script names them: the image's own fonts (and the usual suspects of
// other Linux desktops, in case a deployment adds them). Only requests
// naming one of them first are affected; the substitutions above still
// reach them.
var linuxFonts = []string{
	"DejaVu Sans", "DejaVu Serif", "DejaVu Sans Mono", "DejaVu Sans Condensed", "DejaVu Serif Condensed",
	"Liberation Sans", "Liberation Serif", "Liberation Mono", "Liberation Sans Narrow",
	"Selawik", "Selawik Light", "Selawik Semibold", "Selawik Semilight", "Carlito", "Caladea", "Arimo", "Tinos", "Cousine",
	"Noto Sans", "Noto Serif", "Noto Sans Mono", "Noto Color Emoji", "Noto Emoji", "Noto Sans Symbols", "Noto Sans Symbols2",
	"Noto Sans CJK JP", "Noto Sans CJK SC", "Noto Sans CJK TC", "Noto Sans CJK KR", "Noto Sans CJK HK",
	"Noto Serif CJK JP", "Noto Serif CJK SC", "Noto Serif CJK TC", "Noto Serif CJK KR", "Noto Serif CJK HK",
	"Noto Sans Mono CJK JP", "Noto Sans Mono CJK SC", "Noto Sans Mono CJK TC", "Noto Sans Mono CJK KR", "Noto Sans Mono CJK HK",
	"Ubuntu", "Ubuntu Mono", "Cantarell", "Droid Sans", "Droid Serif", "Droid Sans Mono", "Bitstream Vera Sans",
	"FreeSans", "FreeSerif", "FreeMono", "Nimbus Sans", "Nimbus Roman", "Nimbus Mono PS", "URW Gothic", "Open Sans",
	"Source Sans Pro", "Source Code Pro", "Fira Sans", "Inter", "Lato", "Roboto",
}

// installedFonts is the set of family names fontconfig knows, from
// fc-list; nil when that cannot be asked, in which case every substitute
// is assumed present.
func installedFonts() map[string]bool {
	out, err := exec.Command("fc-list", "--format", "%{family}\n").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		for _, name := range strings.Split(line, ",") {
			if name = strings.TrimSpace(name); name != "" {
				set[name] = true
			}
		}
	}
	return set
}

// windowsFontsConf is the fontconfig configuration of the windows profile
// for a machine with the given fonts. Chrome takes a substitute only when
// the substituted request's first family is the font found, so each alias
// prefers the substitutes that exist, first one first; a family with none
// is simply absent. Chrome also takes any match whose family is the name
// requested, so a family is hidden by replacing the request with a name
// that matches nothing followed by a decoy of a different family — the
// site gets its next fallback. The generic sans-serif — what Chrome
// resolves system-ui through — is the Segoe stand-in, since system-ui is
// Segoe UI there; the CSS generic families are Chrome's own defaults
// (Arial, Times New Roman, monospace) already.
func windowsFontsConf(installed map[string]bool) string {
	has := func(name string) bool { return installed == nil || installed[name] }
	present := func(names []string) []string {
		var out []string
		for _, n := range names {
			if has(n) {
				out = append(out, n)
			}
		}
		return out
	}
	decoy := func(hidden string) string {
		for _, d := range []string{"DejaVu Sans", "Noto Sans CJK JP", "Liberation Serif", "Noto Sans", "Liberation Sans"} {
			if has(d) && strings.Fields(d)[0] != strings.Fields(hidden)[0] {
				return d
			}
		}
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "fonts.dtd">
<!-- chromemcp: the windows device profile's view of this machine's fonts -->
<fontconfig>
  <include ignore_missing="yes">/etc/fonts/fonts.conf</include>
`)
	for _, f := range linuxFonts {
		if !has(f) {
			continue
		}
		fmt.Fprintf(&sb, "  <match target=\"pattern\"><test qual=\"first\" name=\"family\"><string>%s</string></test><edit name=\"family\" mode=\"assign_replace\"><string>chromemcp-absent</string>", xmlEscape(f))
		if d := decoy(f); d != "" {
			fmt.Fprintf(&sb, "<string>%s</string>", xmlEscape(d))
		}
		sb.WriteString("</edit></match>\n")
	}
	for _, a := range windowsFonts {
		subs := present(a.Substitute)
		if len(subs) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "  <alias binding=\"same\"><family>%s</family><prefer>", xmlEscape(a.Family))
		for _, s := range subs {
			fmt.Fprintf(&sb, "<family>%s</family>", xmlEscape(s))
		}
		sb.WriteString("</prefer></alias>\n")
	}
	generic := func(name string, prefer []string) {
		if p := present(prefer); len(p) > 0 {
			fmt.Fprintf(&sb, "  <alias binding=\"strong\"><family>%s</family><prefer>", name)
			for _, s := range p {
				fmt.Fprintf(&sb, "<family>%s</family>", xmlEscape(s))
			}
			sb.WriteString("</prefer></alias>\n")
		}
	}
	generic("sans-serif", segoe)
	generic("serif", times)
	generic("monospace", consolas)
	sb.WriteString("</fontconfig>\n")
	return sb.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// fontsConfFile writes the profile's fontconfig file under dir (once per
// process; the sessions directory is ephemeral) and returns its path, or
// "" for a profile with no fonts of its own.
func (d *deviceProfile) fontsConfFile(dir string, installed map[string]bool) (string, error) {
	if d.FontsConf == nil {
		return "", nil
	}
	path := filepath.Join(dir, "fonts-"+d.Name+".conf")
	want := d.FontsConf(installed)
	if b, err := os.ReadFile(path); err == nil && string(b) == want {
		return path, nil
	}
	if err := os.WriteFile(path+".tmp", []byte(want), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(path+".tmp", path)
}
