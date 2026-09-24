package main

// Fonts are the loudest thing a page can learn about the machine after the
// user agent: a site lays text out in the first family of its stack that
// exists, and a fingerprinting script measures a list of families to see
// which do. A device profile therefore gets a font world of its own — a
// directory of links to the image's fonts, and a fontconfig configuration
// (FONTCONFIG_FILE on the Chrome process) that is the whole of what Chrome
// can see, with the system's own configuration deliberately not included.
//
// Each font in that directory is renamed, as fontconfig scans it, to the
// device families it stands in for: the file behind DejaVu Sans answers to
// Verdana, Tahoma and Trebuchet MS, and to nothing else. So the device's
// families are present because a font really carries those names, and the
// image's own names are absent because no font carries them any more and
// no directory holding one is on the list. Nothing is hidden by sleight of
// hand, which is what makes it hold.
//
// It used to be done the other way about — the system fonts left in place,
// device families aliased onto them with <prefer>, and the image's names
// hidden by rewriting any request that led with one. That rested on Chrome
// refusing a match whose family was not the one asked for, and Chrome 154
// no longer refuses it: every hidden name came back visible while the
// aliases went on working. Renaming the fonts needs no such cooperation.
//
// One tell survives and cannot be removed here: Skia keeps its own table of
// metric-compatible pairs (Arial/Liberation Sans, Times New Roman/Liberation
// Serif, Courier New/Liberation Mono, Calibri/Carlito, Cambria/Caladea) and
// answers a request for either with the other, whatever fontconfig says. A
// profile that must have Arial therefore also answers to Liberation Sans.
// Only the unpaired names — DejaVu, Noto, Selawik, the rest — can be made
// to disappear.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// `fantasy` generic (Blink resolves it to Impact) measure narrow — the
	// tell that keeps a font-fingerprinting script reading the box as Chrome
	// and not Firefox. Liberation Sans Narrow (fonts-liberation-sans-narrow,
	// a separate package from fonts-liberation) is the closest match; Carlito
	// is a narrower-than-body fallback that is always in the image, so the
	// generic stays under the threshold even if the narrow package is absent.
	narrow   = []string{"Liberation Sans Narrow", "Carlito", "Liberation Sans"}
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

// fontSet is what fontconfig knows about this machine: the files behind
// each family name. A profile's directory is built from it, so a family
// with no files is a family the profile cannot present.
type fontSet struct {
	files map[string][]string
}

// installedFonts asks fontconfig what is on the machine. It returns nil
// when fc-list cannot be run, and a profile with no font set gets no font
// configuration at all: presenting the device's families means having
// files to present, and guessing at them would leave Chrome with a
// directory of names backed by nothing.
func installedFonts() *fontSet {
	out, err := exec.Command("fc-list", "--format", "%{family}\t%{file}\n").Output()
	if err != nil {
		return nil
	}
	s := &fontSet{files: map[string][]string{}}
	for _, line := range strings.Split(string(out), "\n") {
		families, file, ok := strings.Cut(line, "\t")
		if !ok || file == "" {
			continue
		}
		for _, name := range strings.Split(families, ",") {
			if name = strings.TrimSpace(name); name != "" && !slices.Contains(s.files[name], file) {
				s.files[name] = append(s.files[name], file)
			}
		}
	}
	return s
}

func (s *fontSet) has(family string) bool { return s != nil && len(s.files[family]) > 0 }

// fontPlan is a profile's font world: the configuration Chrome reads, and
// the files its directory must hold for that configuration to mean
// anything.
type fontPlan struct {
	Conf  string
	Files []string
}

// windowsFontsPlan presents the machine's fonts as a Windows 11 machine's.
// Each device family is answered by the first of its substitutes that is
// installed; the substitutes that end up used are linked into fontDir and
// renamed, as they are scanned, to every family they answer for. A
// substitute nothing needs is not linked, so its own name goes with it.
func windowsFontsPlan(fonts *fontSet, fontDir, cacheDir string) fontPlan {
	// Which substitute answers for each device family, and in turn every
	// family that substitute must answer to, in the order they are listed
	// so the configuration is the same from one run to the next.
	answers := map[string][]string{}
	var used []string
	for _, a := range windowsFonts {
		for _, sub := range a.Substitute {
			if !fonts.has(sub) {
				continue
			}
			if len(answers[sub]) == 0 {
				used = append(used, sub)
			}
			answers[sub] = append(answers[sub], a.Family)
			break
		}
	}
	if len(used) == 0 {
		return fontPlan{}
	}

	var files []string
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "fonts.dtd">
<!-- chromemcp: the windows device profile's font world. The system
     configuration is deliberately not included: this directory is the
     whole of what Chrome can see. -->
<fontconfig>
`)
	fmt.Fprintf(&sb, "  <dir>%s</dir>\n  <cachedir>%s</cachedir>\n", xmlEscape(fontDir), xmlEscape(cacheDir))
	for _, sub := range used {
		files = append(files, fonts.files[sub]...)
		fmt.Fprintf(&sb, "  <match target=\"scan\"><test name=\"family\"><string>%s</string></test>", xmlEscape(sub))
		for i, family := range answers[sub] {
			mode := "append"
			if i == 0 {
				mode = "assign_replace"
			}
			fmt.Fprintf(&sb, "<edit name=\"family\" mode=\"%s\"><string>%s</string></edit>", mode, xmlEscape(family))
		}
		sb.WriteString("</match>\n")
	}
	slices.Sort(files)
	files = slices.Compact(files)

	// A font file often carries several families — one CJK collection holds
	// the JP, SC, TC, KR and HK faces — and a rename only reaches the faces
	// whose family it names. The rest would arrive in the directory still
	// carrying their own names, so the faces that were not renamed are
	// dropped: this profile presents the families it means to and no others.
	linked := map[string]bool{}
	for _, f := range files {
		linked[f] = true
	}
	var strays []string
	for family, ff := range fonts.files {
		if len(answers[family]) > 0 {
			continue
		}
		if slices.ContainsFunc(ff, func(f string) bool { return linked[f] }) {
			strays = append(strays, family)
		}
	}
	slices.Sort(strays)
	for _, family := range strays {
		fmt.Fprintf(&sb, "  <selectfont><rejectfont><pattern><patelt name=\"family\"><string>%s</string></patelt></pattern></rejectfont></selectfont>\n", xmlEscape(family))
	}

	// The CSS generics, under the names the fonts now carry: sans-serif is
	// what Chrome resolves system-ui through, and system-ui is Segoe UI on
	// a Windows machine.
	for _, g := range []struct {
		generic string
		want    []fontAlias
	}{
		{"sans-serif", []fontAlias{{"Segoe UI", segoe}, {"Arial", arial}}},
		{"serif", []fontAlias{{"Times New Roman", times}}},
		{"monospace", []fontAlias{{"Consolas", consolas}, {"Courier New", courier}}},
	} {
		var prefer []string
		for _, a := range g.want {
			for _, sub := range a.Substitute {
				if fonts.has(sub) {
					prefer = append(prefer, a.Family)
					break
				}
			}
		}
		if len(prefer) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "  <alias binding=\"strong\"><family>%s</family><prefer>", g.generic)
		for _, f := range prefer {
			fmt.Fprintf(&sb, "<family>%s</family>", xmlEscape(f))
		}
		sb.WriteString("</prefer></alias>\n")
	}
	sb.WriteString("</fontconfig>\n")

	// The files are named in the configuration too, so that replacing a
	// font on disk rewrites the directory rather than leaving it stale.
	var list strings.Builder
	for _, f := range files {
		fmt.Fprintf(&list, "<!-- %s -->\n", xmlEscape(f))
	}
	return fontPlan{Conf: sb.String() + list.String(), Files: files}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// fontsConfFile builds the profile's font world under dir and returns the
// path of its configuration, or "" for a profile that presents the
// machine's own fonts. The directory is rebuilt only when the plan changes
// — the font cache inside it is what makes a session start quickly, and it
// is only valid for the configuration that produced it.
func (d *deviceProfile) fontsConfFile(dir string, fonts *fontSet) (string, error) {
	if d.FontsPlan == nil || fonts == nil {
		return "", nil
	}
	var (
		conf    = filepath.Join(dir, "fonts-"+d.Name+".conf")
		fontDir = filepath.Join(dir, "fonts-"+d.Name+".d")
		cache   = filepath.Join(dir, "fonts-"+d.Name+".cache")
	)
	plan := d.FontsPlan(fonts, fontDir, cache)
	if plan.Conf == "" {
		return "", nil
	}
	if b, err := os.ReadFile(conf); err == nil && string(b) == plan.Conf {
		return conf, nil
	}
	if err := os.RemoveAll(fontDir); err != nil {
		return "", err
	}
	for _, d := range []string{fontDir, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("font directory: %w", err)
		}
	}
	for _, f := range plan.Files {
		// Flattened: two fonts of the same base name in different
		// directories would otherwise collide.
		link := filepath.Join(fontDir, strings.ReplaceAll(strings.TrimPrefix(f, "/"), "/", "_"))
		if err := os.Symlink(f, link); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	if err := os.WriteFile(conf+".tmp", []byte(plan.Conf), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(conf+".tmp", conf); err != nil {
		os.Remove(conf + ".tmp")
		return "", err
	}
	// Scanning is where the renaming happens, so the cache is built here
	// rather than left for the first Chrome to pay for. Chrome scans the
	// directory itself if fc-cache is not on the machine.
	cmd := exec.Command("fc-cache", fontDir)
	cmd.Env = append(os.Environ(), "FONTCONFIG_FILE="+conf)
	if out, err := cmd.CombinedOutput(); err != nil {
		logf("fc-cache for the %s profile: %v (%s)", d.Name, err, strings.TrimSpace(string(out)))
	}
	return conf, nil
}
